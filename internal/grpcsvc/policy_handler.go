// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"strconv"
	"time"

	log "github.com/Bugs5382/go-log"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/errcodes"
	"github.com/Steward-GRC/steward-core/internal/lifecycle"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/Steward-GRC/steward-core/internal/validate"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// storeUnavailable codes a store fault. The cause is only logged, never sent; op names the failed
// call.
func storeUnavailable(ctx context.Context, op string, cause error) error {
	logger.Ctx(ctx).Debug("core: store op failed", log.F("error", cause.Error()), log.F("op", op))
	return errcodes.Error(ctx, errcodes.StoreUnavailable(op, cause))
}

// PolicyStorer is the store interface the PolicyHandler depends on.
type PolicyStorer interface {
	CreatePolicy(ctx context.Context, p domain.Policy) (domain.Policy, error)
	GetPolicy(ctx context.Context, id uuid.UUID) (domain.Policy, error)
	ListPolicies(ctx context.Context, categoryID uuid.UUID, includeDescendants bool, docType domain.DocumentType) ([]domain.Policy, error)
	GetPolicyVersion(ctx context.Context, id uuid.UUID) (domain.PolicyVersion, error)
	ListPublishedVersions(ctx context.Context, policyID uuid.UUID) ([]domain.PolicyVersion, error)
	UpsertDraft(ctx context.Context, pv domain.PolicyVersion) (domain.PolicyVersion, store.UpsertDraftResult, error)
	UpdateDraftContent(ctx context.Context, policyVersionID uuid.UUID, contentJSON string) error
	PublishDraft(ctx context.Context, policyID, actorUserID uuid.UUID) (domain.PolicyVersion, error)
	DiscardDraft(ctx context.Context, policyID uuid.UUID) error
	DeletePolicy(ctx context.Context, policyID uuid.UUID) error
	RetirePolicy(ctx context.Context, id uuid.UUID) (domain.Policy, error)
	SetVersionStatus(ctx context.Context, policyVersionID uuid.UUID, target domain.PolicyVersionStatus) (domain.PolicyVersion, error)
	SetOwner(ctx context.Context, policyID, ownerUserID uuid.UUID) (domain.Policy, error)
	SetSensitivity(ctx context.Context, policyID uuid.UUID, sensitivity domain.Sensitivity) (domain.Policy, error)
	SetTitle(ctx context.Context, policyID uuid.UUID, title string) (domain.Policy, error)
	SetProposedTitle(ctx context.Context, policyVersionID uuid.UUID, proposedTitle *string) error
	SetHomeCategory(ctx context.Context, policyID, targetCategoryID uuid.UUID) (domain.Policy, error)
	IsTemplateUpdateAvailable(ctx context.Context, policyVersionID, templateID uuid.UUID) (bool, error)
	SetAck(ctx context.Context, policyID uuid.UUID, ack *domain.AckTrigger, override []uuid.UUID) (domain.Policy, error)
	SetTemplate(ctx context.Context, policyID, templateID uuid.UUID, templateNone bool) (domain.Policy, error)
	ListAllPolicies(ctx context.Context) ([]domain.Policy, error)
	ListPoliciesByOwner(ctx context.Context, ownerUserID uuid.UUID, includeRetired bool) ([]domain.Policy, error)
	ReassignUserPolicies(ctx context.Context, fromUserID, toUserID uuid.UUID) ([]uuid.UUID, int, int, error)
}

// appendixCopier copies a published version's appendices into a freshly created
// draft. Satisfied by *store.AppendixStore.
type appendixCopier interface {
	CopyForward(ctx context.Context, fromVersionID, toVersionID uuid.UUID) error
}

// PolicyHandler implements corev1.PolicyServiceServer.
type PolicyHandler struct {
	corev1.UnimplementedPolicyServiceServer
	store         PolicyStorer
	categoryStore CategoryStorer
	tplStore      TemplateStorer
	validator     domain.ContentValidator
	auditor       auditEmitter
	// lifecyclePub sends worker events on the jobs exchange, apart from audit. nil skips them.
	lifecyclePub *lifecycle.Emitter
	// appendixCopier copies appendices into a new draft. nil skips the copy.
	appendixCopier appendixCopier
	// versionCache holds published versions only. nil turns caching off.
	versionCache versionCache
}

// versionCache is the optional byte cache for published, immutable data.
type versionCache interface {
	GetBytes(ctx context.Context, key string) ([]byte, bool)
	SetBytes(ctx context.Context, key string, val []byte)
	Del(ctx context.Context, key string)
}

// NewPolicyHandler returns a PolicyHandler. auditor may be nil.
func NewPolicyHandler(ps PolicyStorer, gs CategoryStorer, ts TemplateStorer, v domain.ContentValidator, auditor auditEmitter) *PolicyHandler {
	return &PolicyHandler{store: ps, categoryStore: gs, tplStore: ts, validator: v, auditor: auditor}
}

// WithVersionCache sets the optional read cache.
func (h *PolicyHandler) WithVersionCache(c versionCache) *PolicyHandler {
	h.versionCache = c
	return h
}

// WithLifecycleEmitter sets the jobs-exchange publisher; nil turns it off.
func (h *PolicyHandler) WithLifecycleEmitter(em *lifecycle.Emitter) *PolicyHandler {
	h.lifecyclePub = em
	return h
}

// WithAppendixCopier sets the appendix copy into a newly created draft.
func (h *PolicyHandler) WithAppendixCopier(c appendixCopier) *PolicyHandler {
	h.appendixCopier = c
	return h
}

func sensitivityFromProto(s corev1.Sensitivity) domain.Sensitivity {
	switch s {
	case corev1.Sensitivity_SENSITIVITY_SENSITIVE:
		return domain.SensitivitySensitive
	default:
		// Unspecified is read as standard, the safe default.
		return domain.SensitivityStandard
	}
}

func sensitivityToProto(s domain.Sensitivity) corev1.Sensitivity {
	switch s {
	case domain.SensitivitySensitive:
		return corev1.Sensitivity_SENSITIVITY_SENSITIVE
	case domain.SensitivityStandard:
		return corev1.Sensitivity_SENSITIVITY_STANDARD
	default:
		return corev1.Sensitivity_SENSITIVITY_UNSPECIFIED
	}
}

// docTypeFromProto reads an unset or unknown type as a policy, so callers that don't send one keep
// creating policies.
func docTypeFromProto(t corev1.DocumentType) domain.DocumentType {
	switch t {
	case corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE:
		return domain.DocumentTypeProcedure
	default:
		return domain.DocumentTypePolicy
	}
}

// docTypeToProto never sends UNSPECIFIED: a stored row always has a concrete type.
func docTypeToProto(t domain.DocumentType) corev1.DocumentType {
	switch t {
	case domain.DocumentTypeProcedure:
		return corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE
	default:
		return corev1.DocumentType_DOCUMENT_TYPE_POLICY
	}
}

func policyVersionStatusToProto(s domain.PolicyVersionStatus) corev1.PolicyVersionStatus {
	switch s {
	case domain.PolicyVersionStatusDraft:
		return corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_DRAFT
	case domain.PolicyVersionStatusPublished:
		return corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_PUBLISHED
	case domain.PolicyVersionStatusSuperseded:
		return corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_SUPERSEDED
	case domain.PolicyVersionStatusArchived:
		return corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_ARCHIVED
	default:
		return corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_UNSPECIFIED
	}
}

func policyToProto(p domain.Policy) *corev1.Policy {
	ackOverride := make([]string, 0, len(p.AckAudienceOverride))
	for _, id := range p.AckAudienceOverride {
		ackOverride = append(ackOverride, id.String())
	}
	proto := &corev1.Policy{
		Id:                        p.ID.String(),
		HomeCategoryId:            nilableUUID(p.HomeCategoryID),
		Number:                    p.Number,
		Title:                     p.Title,
		Sensitivity:               sensitivityToProto(p.Sensitivity),
		DocumentType:              docTypeToProto(p.DocumentType),
		OwnerUserId:               nilableUUID(p.OwnerUserID),
		CurrentPublishedVersionId: nilableUUID(p.CurrentPublishedVersionID),
		CurrentDraftVersionId:     nilableUUID(p.CurrentDraftVersionID),
		TemplateId:                nilableUUID(p.TemplateID),
		TemplateNone:              p.TemplateNone,
		AckTriggersSet:            p.AckTriggers != nil,
		AckAudienceOverride:       ackOverride,
	}
	if p.AckTriggers != nil {
		proto.AckTriggers = ackTriggerToProto(*p.AckTriggers)
	}
	if p.RetiredAt != nil {
		proto.RetiredAt = p.RetiredAt.UTC().Format(time.RFC3339)
	}
	return proto
}

func policyVersionToProto(pv domain.PolicyVersion) *corev1.PolicyVersion {
	proto := &corev1.PolicyVersion{
		Id:                pv.ID.String(),
		PolicyId:          pv.PolicyID.String(),
		VersionNo:         toInt32(pv.VersionNo),
		Status:            policyVersionStatusToProto(pv.Status),
		TemplateVersionId: nilableUUID(pv.TemplateVersionID),
		ContentJson:       pv.ContentJSON,
	}
	if !pv.CreatedAt.IsZero() {
		proto.CreatedAt = timestamppb.New(pv.CreatedAt)
	}
	return proto
}

// CreatePolicy handles PolicyService.CreatePolicy.
func (h *PolicyHandler) CreatePolicy(ctx context.Context, req *corev1.CreatePolicyRequest) (*corev1.CreatePolicyResponse, error) {
	homeGID, err := uuid.Parse(req.GetHomeCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid home_category_id: %v", err)
	}
	ownerUID, err := parseOptionalUUID(req.GetOwnerUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid owner_user_id: %v", err)
	}
	tplID, err := parseOptionalUUID(req.GetTemplateId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid template_id: %v", err)
	}

	g, err := h.categoryStore.Get(ctx, homeGID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "category not found")
		}
		logger.Ctx(ctx).Error(err, "get category failed", log.F("category_id", homeGID.String()))
		return nil, status.Error(codes.Internal, "get category failed")
	}

	p, err := domain.NewPolicy(req.GetTitle(), homeGID, sensitivityFromProto(req.GetSensitivity()), ownerUID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	p.TemplateID = tplID
	p.DocumentType = docTypeFromProto(req.GetDocumentType())

	p, err = h.store.CreatePolicy(ctx, p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create policy: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.created",
			ActorUserID: req.GetOwnerUserId(),
			Subject:     "policy:" + p.ID.String(),
			GroupID:     g.ID.String(),
		})
		// The store seeds the first draft with the policy; audit it the way SaveDraft does.
		if p.CurrentDraftVersionID != uuid.Nil {
			_ = h.auditor.Emit(ctx, audit.Event{
				Tier: audit.TierAudit, Action: "policy.draft_saved",
				ActorUserID: req.GetOwnerUserId(),
				Subject:     "policy_version:" + p.CurrentDraftVersionID.String(),
			})
		}
	}
	return &corev1.CreatePolicyResponse{Policy: policyToProto(p)}, nil
}

// GetPolicy handles PolicyService.GetPolicy.
func (h *PolicyHandler) GetPolicy(ctx context.Context, req *corev1.GetPolicyRequest) (*corev1.GetPolicyResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	p, err := h.store.GetPolicy(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, storeUnavailable(ctx, "get_policy", err)
	}
	proto := policyToProto(p)
	// Computed on this detail read only: in ListPolicies it would cost one query per row. A miss
	// leaves it false.
	proto.TemplateUpdateAvailable = h.templateUpdateAvailable(ctx, p)
	return &corev1.GetPolicyResponse{Policy: proto}, nil
}

// templateUpdateAvailable reports whether the effective template has a newer published version than
// the live version is pinned to. Any miss is false, so GetPolicy never fails on it.
func (h *PolicyHandler) templateUpdateAvailable(ctx context.Context, p domain.Policy) bool {
	if p.CurrentPublishedVersionID == uuid.Nil {
		return false
	}
	chain, err := h.categoryStore.AncestorChain(ctx, p.HomeCategoryID)
	if err != nil {
		return false
	}
	tplID := domain.EffectiveTemplateID(p.TemplateID, chain)
	if tplID == uuid.Nil {
		return false
	}
	avail, err := h.store.IsTemplateUpdateAvailable(ctx, p.CurrentPublishedVersionID, tplID)
	if err != nil {
		return false
	}
	return avail
}

// GetPolicyVersion handles PolicyService.GetPolicyVersion.
func (h *PolicyHandler) GetPolicyVersion(ctx context.Context, req *corev1.GetPolicyVersionRequest) (*corev1.GetPolicyVersionResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	cacheKey := "polver:" + id.String()
	if h.versionCache != nil {
		if b, ok := h.versionCache.GetBytes(ctx, cacheKey); ok {
			var v corev1.PolicyVersion
			if proto.Unmarshal(b, &v) == nil {
				return &corev1.GetPolicyVersionResponse{Version: &v}, nil
			}
		}
	}
	pv, err := h.store.GetPolicyVersion(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy version not found")
		}
		logger.Ctx(ctx).Error(err, "get policy version failed", log.F("version_id", id.String()))
		return nil, status.Error(codes.Internal, "get policy version failed")
	}
	out := policyVersionToProto(pv)
	// Only published versions are cached: their content never changes.
	if h.versionCache != nil && pv.Status == domain.PolicyVersionStatusPublished {
		if b, mErr := proto.Marshal(out); mErr == nil {
			h.versionCache.SetBytes(ctx, cacheKey, b)
		}
	}
	return &corev1.GetPolicyVersionResponse{Version: out}, nil
}

// ListPolicyVersions handles PolicyService.ListPolicyVersions.
func (h *PolicyHandler) ListPolicyVersions(ctx context.Context, req *corev1.ListPolicyVersionsRequest) (*corev1.ListPolicyVersionsResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	// The published list only changes on a publish or a status change, which drop this key; the TTL
	// is the backstop.
	cacheKey := "polvers:" + pid.String()
	if h.versionCache != nil {
		l := logger.Ctx(ctx)
		if b, ok := h.versionCache.GetBytes(ctx, cacheKey); ok {
			var resp corev1.ListPolicyVersionsResponse
			if proto.Unmarshal(b, &resp) == nil {
				l.Debug("policy-version list cache hit", log.F("cache_key", cacheKey))
				return &resp, nil
			}
		}
		l.Debug("policy-version list cache miss", log.F("cache_key", cacheKey))
	}
	vs, err := h.store.ListPublishedVersions(ctx, pid)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list versions: %v", err)
	}
	out := make([]*corev1.PolicyVersion, 0, len(vs))
	for _, v := range vs {
		out = append(out, policyVersionToProto(v))
	}
	resp := &corev1.ListPolicyVersionsResponse{Versions: out}
	if h.versionCache != nil {
		if b, mErr := proto.Marshal(resp); mErr == nil {
			h.versionCache.SetBytes(ctx, cacheKey, b)
		}
	}
	return resp, nil
}

// ListPolicies handles PolicyService.ListPolicies.
func (h *PolicyHandler) ListPolicies(ctx context.Context, req *corev1.ListPoliciesRequest) (*corev1.ListPoliciesResponse, error) {
	gid, err := uuid.Parse(req.GetCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid category_id: %v", err)
	}
	// An unset document type lists policies only.
	docType := docTypeFromProto(req.GetDocumentType())
	policies, err := h.store.ListPolicies(ctx, gid, req.GetIncludeDescendants(), docType)
	if err != nil {
		return nil, storeUnavailable(ctx, "list_policies", err)
	}
	out := make([]*corev1.Policy, 0, len(policies))
	for _, p := range policies {
		out = append(out, policyToProto(p))
	}
	return &corev1.ListPoliciesResponse{Policies: out}, nil
}

// SaveDraft handles PolicyService.SaveDraft.
func (h *PolicyHandler) SaveDraft(ctx context.Context, req *corev1.SaveDraftRequest) (*corev1.SaveDraftResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	// An empty template_version_id saves a freeform draft, with no template validation.
	tvID, err := parseOptionalUUID(req.GetTemplateVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid template_version_id: %v", err)
	}
	actor, err := parseOptionalUUID(req.GetActorUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}

	if tvID != uuid.Nil && h.validator != nil {
		// A failed lookup still validates against an empty template version, so the save goes
		// through.
		tv, terr := h.tplStore.GetTemplateVersion(ctx, tvID)
		if terr != nil {
			tv = domain.TemplateVersion{ID: tvID}
		}
		if err := h.validator.Validate(req.GetContentJson(), tv); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "content validation: %v", err)
		}
	}

	pv, err := domain.NewPolicyVersionDraft(pid, tvID, actor, req.GetContentJson())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	pv, res, err := h.store.UpsertDraft(ctx, pv)
	if err != nil {
		if errors.Is(err, store.ErrDraftAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "upsert draft: %v", err)
	}
	// A new draft starts with a copy of the published version's appendices.
	if res.Inserted && h.appendixCopier != nil {
		if pol, gerr := h.store.GetPolicy(ctx, pid); gerr == nil && pol.CurrentPublishedVersionID != uuid.Nil {
			if cerr := h.appendixCopier.CopyForward(ctx, pol.CurrentPublishedVersionID, pv.ID); cerr != nil {
				return nil, status.Errorf(codes.Internal, "copy appendices forward: %v", cerr)
			}
		}
	}
	// SaveDraft is the autosave endpoint: audit a real save (a new draft or changed content), not
	// an idle tick.
	if h.auditor != nil && res.ContentChanged {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.draft_saved",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy_version:" + pv.ID.String(),
		})
	}
	return &corev1.SaveDraftResponse{Version: policyVersionToProto(pv)}, nil
}

// PublishDraft handles PolicyService.PublishDraft.
func (h *PolicyHandler) PublishDraft(ctx context.Context, req *corev1.PublishDraftRequest) (*corev1.PublishDraftResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	actor, err := parseOptionalUUID(req.GetActorUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}

	// Read the staged rename before publishing, so a publish that applies it can be audited as a
	// rename. A miss only skips that event.
	var renamedFrom, renamedTo string
	if pol, gerr := h.store.GetPolicy(ctx, pid); gerr == nil {
		if pol.CurrentDraftVersionID != uuid.Nil {
			if draft, derr := h.store.GetPolicyVersion(ctx, pol.CurrentDraftVersionID); derr == nil && draft.ProposedTitle != nil {
				renamedFrom = pol.Title
				renamedTo = *draft.ProposedTitle
			}
		}
	}

	pv, err := h.store.PublishDraft(ctx, pid, actor)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "publish draft: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.published",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy_version:" + pv.ID.String(),
		})
		// The staged rename took effect: audit it as policy.renamed, like an in-place rename.
		if renamedTo != "" {
			_ = h.auditor.Emit(ctx, audit.Event{
				Tier: audit.TierAudit, Action: "policy.renamed",
				ActorUserID: req.GetActorUserId(),
				Subject:     "policy:" + pid.String(),
				Attributes: map[string]string{
					"from_title": renamedFrom,
					"to_title":   renamedTo,
				},
			})
		}
	}

	// A lifecycle failure never fails the RPC: the publish has already succeeded.
	h.emitPublishedLifecycle(ctx, pid, pv)

	// The published set changed: drop the cached version list.
	if h.versionCache != nil {
		h.versionCache.Del(ctx, "polvers:"+pid.String())
	}

	return &corev1.PublishDraftResponse{Version: policyVersionToProto(pv)}, nil
}

// DiscardDraft handles PolicyService.DiscardDraft. It deletes the current draft only, and returns
// NotFound when there is none.
func (h *PolicyHandler) DiscardDraft(ctx context.Context, req *corev1.DiscardDraftRequest) (*corev1.DiscardDraftResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	if err := h.store.DiscardDraft(ctx, pid); err != nil {
		if errors.Is(err, store.ErrNoDraft) {
			return nil, status.Errorf(codes.NotFound, "discard draft: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "discard draft: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.draft_discarded",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy:" + pid.String(),
		})
	}
	return &corev1.DiscardDraftResponse{}, nil
}

// DeletePolicy handles PolicyService.DeletePolicy. Only a never-published policy can be deleted;
// otherwise it's FailedPrecondition.
func (h *PolicyHandler) DeletePolicy(ctx context.Context, req *corev1.DeletePolicyRequest) (*corev1.DeletePolicyResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	if err := h.store.DeletePolicy(ctx, pid); err != nil {
		switch {
		case errors.Is(err, store.ErrHasPublishedVersion):
			return nil, status.Errorf(codes.FailedPrecondition, "delete policy: %v", err)
		case errors.Is(err, pgx.ErrNoRows):
			return nil, status.Errorf(codes.NotFound, "delete policy: %v", err)
		default:
			return nil, status.Errorf(codes.Internal, "delete policy: %v", err)
		}
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.deleted",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy:" + pid.String(),
		})
	}
	return &corev1.DeletePolicyResponse{Deleted: true}, nil
}

// RetirePolicy handles PolicyService.RetirePolicy. Retiring is idempotent and keeps completed
// acknowledgements.
func (h *PolicyHandler) RetirePolicy(ctx context.Context, req *corev1.RetirePolicyRequest) (*corev1.RetirePolicyResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	retired, err := h.store.RetirePolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Errorf(codes.NotFound, "retire policy: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "retire policy: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.retired",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy:" + pid.String(),
			GroupID:     retired.HomeCategoryID.String(),
		})
	}
	// Best effort: obligations tells the audience. Never fail the RPC on it.
	h.emitRetiredLifecycle(ctx, pid)
	return &corev1.RetirePolicyResponse{Policy: policyToProto(retired)}, nil
}

// SetPolicyOwner handles PolicyService.SetPolicyOwner.
func (h *PolicyHandler) SetPolicyOwner(ctx context.Context, req *corev1.SetPolicyOwnerRequest) (*corev1.SetPolicyOwnerResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	owner, err := uuid.Parse(req.GetOwnerUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid owner_user_id: %v", err)
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	cur, err := h.store.GetPolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "get policy: %v", err)
	}
	p, err := h.store.SetOwner(ctx, pid, owner)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "set owner: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.owner_changed",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy:" + p.ID.String(),
			GroupID:     p.HomeCategoryID.String(),
			Attributes: map[string]string{
				"from_owner": cur.OwnerUserID.String(),
				"to_owner":   owner.String(),
			},
		})
	}
	return &corev1.SetPolicyOwnerResponse{Policy: policyToProto(p)}, nil
}

// ListPoliciesByOwner handles PolicyService.ListPoliciesByOwner: the policies to hand over before a
// user is deleted.
func (h *PolicyHandler) ListPoliciesByOwner(ctx context.Context, req *corev1.ListPoliciesByOwnerRequest) (*corev1.ListPoliciesByOwnerResponse, error) {
	owner, err := uuid.Parse(req.GetOwnerUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid owner_user_id: %v", err)
	}
	policies, err := h.store.ListPoliciesByOwner(ctx, owner, req.GetIncludeRetired())
	if err != nil {
		return nil, storeUnavailable(ctx, "list_policies_by_owner", err)
	}
	out := make([]*corev1.Policy, 0, len(policies))
	for _, p := range policies {
		out = append(out, policyToProto(p))
	}
	return &corev1.ListPoliciesByOwnerResponse{Policies: out}, nil
}

// ReassignUserPolicies handles PolicyService.ReassignUserPolicies. It moves a user's policies and
// author grants in one transaction, so the user can be deleted without deleting a policy.
func (h *PolicyHandler) ReassignUserPolicies(ctx context.Context, req *corev1.ReassignUserPoliciesRequest) (*corev1.ReassignUserPoliciesResponse, error) {
	from, err := uuid.Parse(req.GetFromUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid from_user_id: %v", err)
	}
	to, err := uuid.Parse(req.GetToUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid to_user_id: %v", err)
	}
	if from == to {
		return nil, status.Error(codes.InvalidArgument, "from_user_id and to_user_id must differ")
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}

	ids, ownerCount, authorGrants, err := h.store.ReassignUserPolicies(ctx, from, to)
	if err != nil {
		return nil, storeUnavailable(ctx, "reassign_user_policies", err)
	}

	if h.auditor != nil {
		// One event per policy, so each transfer can be found on its own.
		for _, id := range ids {
			_ = h.auditor.Emit(ctx, audit.Event{
				Tier: audit.TierAudit, Action: "policy.owner_changed",
				ActorUserID: req.GetActorUserId(),
				Subject:     "policy:" + id.String(),
				Attributes: map[string]string{
					"from_owner": from.String(),
					"to_owner":   to.String(),
					"reason":     "user_reassign",
				},
			})
		}
		// One summary event for the category RACI author-grant sweep.
		if authorGrants > 0 {
			_ = h.auditor.Emit(ctx, audit.Event{
				Tier: audit.TierAudit, Action: "category.ruleset_changed",
				ActorUserID: req.GetActorUserId(),
				Subject:     "user:" + from.String(),
				Attributes: map[string]string{
					"from_user": from.String(),
					"to_user":   to.String(),
					"count":     strconv.Itoa(authorGrants),
					"reason":    "user_reassign",
				},
			})
		}
	}

	outIDs := make([]string, 0, len(ids))
	for _, id := range ids {
		outIDs = append(outIDs, id.String())
	}
	return &corev1.ReassignUserPoliciesResponse{
		ReassignedPolicyIds:    outIDs,
		ReassignedOwnerCount:   toInt32(ownerCount),
		ReassignedAuthorGrants: toInt32(authorGrants),
	}, nil
}

// SetPolicySensitivity handles PolicyService.SetPolicySensitivity. It changes the policy row only:
// no new version and no approval.
func (h *PolicyHandler) SetPolicySensitivity(ctx context.Context, req *corev1.SetPolicySensitivityRequest) (*corev1.SetPolicySensitivityResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	newSensitivity := sensitivityFromProto(req.GetSensitivity())
	cur, err := h.store.GetPolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "get policy: %v", err)
	}
	p, err := h.store.SetSensitivity(ctx, pid, newSensitivity)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "set sensitivity: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.sensitivity_changed",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy:" + p.ID.String(),
			GroupID:     p.HomeCategoryID.String(),
			Attributes: map[string]string{
				"from_sensitivity": string(cur.Sensitivity),
				"to_sensitivity":   string(newSensitivity),
			},
		})
	}
	return &corev1.SetPolicySensitivityResponse{Policy: policyToProto(p)}, nil
}

// RenamePolicy handles PolicyService.RenamePolicy. A never-published policy is renamed in place. A
// published policy's new title is staged on its draft (created from the live version if needed) and
// applied when that draft is published; the live title stays until then. A working version that is
// no longer an editable draft is refused with FailedPrecondition, so nothing is staged under its
// approvers.
func (h *PolicyHandler) RenamePolicy(ctx context.Context, req *corev1.RenamePolicyRequest) (*corev1.RenamePolicyResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	newTitle := req.GetNewTitle()
	if newTitle == "" {
		return nil, status.Error(codes.InvalidArgument, "new_title must not be empty")
	}
	actor, err := parseOptionalUUID(req.GetActorUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}

	p, err := h.store.GetPolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		logger.Ctx(ctx).Error(err, "get policy failed", log.F("policy_id", pid.String()))
		return nil, status.Error(codes.Internal, "get policy failed")
	}

	if p.CurrentPublishedVersionID == uuid.Nil {
		updated, err := h.store.SetTitle(ctx, pid, newTitle)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, status.Error(codes.NotFound, "policy not found")
			}
			return nil, status.Errorf(codes.Internal, "rename policy: %v", err)
		}
		if h.auditor != nil {
			_ = h.auditor.Emit(ctx, audit.Event{
				Tier: audit.TierAudit, Action: "policy.renamed",
				ActorUserID: req.GetActorUserId(),
				Subject:     "policy:" + updated.ID.String(),
				GroupID:     updated.HomeCategoryID.String(),
				Attributes: map[string]string{
					"from_title": p.Title,
					"to_title":   newTitle,
				},
			})
		}
		return &corev1.RenamePolicyResponse{Policy: policyToProto(updated), Staged: false}, nil
	}

	// Only an editable draft may carry a staged rename: staging on a version under review would
	// change the title under its approvers.
	if p.CurrentDraftVersionID != uuid.Nil {
		cur, gverr := h.store.GetPolicyVersion(ctx, p.CurrentDraftVersionID)
		if gverr != nil {
			l := logger.Ctx(ctx)
			l.Error(gverr, "load current draft for rename failed", log.F("policy_id", pid.String()), log.F("draft_version_id", p.CurrentDraftVersionID.String()))
			return nil, status.Errorf(codes.Internal, "load current draft: %v", gverr)
		}
		if cur.Status != domain.PolicyVersionStatusDraft {
			return nil, status.Error(codes.FailedPrecondition,
				"A change is pending approval. Resolve or withdraw that draft before renaming.")
		}
	}

	draftID, err := h.ensureDraft(ctx, p, actor)
	if err != nil {
		if errors.Is(err, store.ErrDraftAlreadyExists) {
			return nil, status.Errorf(codes.AlreadyExists, "%v", err)
		}
		logger.Ctx(ctx).Error(err, "ensure draft failed", log.F("policy_id", pid.String()))
		return nil, status.Errorf(codes.Internal, "ensure draft: %v", err)
	}
	if err := h.store.SetProposedTitle(ctx, draftID, &newTitle); err != nil {
		return nil, status.Errorf(codes.Internal, "stage rename: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.rename_staged",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy_version:" + draftID.String(),
			GroupID:     p.HomeCategoryID.String(),
			Attributes: map[string]string{
				"from_title": p.Title,
				"to_title":   newTitle,
			},
		})
	}
	// Re-read for the draft pointer; fall back to the snapshot on a miss.
	final := p
	if refreshed, gerr := h.store.GetPolicy(ctx, pid); gerr == nil {
		final = refreshed
	}
	return &corev1.RenamePolicyResponse{
		Policy:         policyToProto(final),
		Staged:         true,
		DraftVersionId: draftID.String(),
	}, nil
}

// ensureDraft returns the working draft's id, creating one from the published version the way
// SaveDraft does, appendices included. Call it only for a published policy.
func (h *PolicyHandler) ensureDraft(ctx context.Context, p domain.Policy, actor uuid.UUID) (uuid.UUID, error) {
	if p.CurrentDraftVersionID != uuid.Nil {
		return p.CurrentDraftVersionID, nil
	}
	pub, err := h.store.GetPolicyVersion(ctx, p.CurrentPublishedVersionID)
	if err != nil {
		return uuid.Nil, err
	}
	pv, err := domain.NewPolicyVersionDraft(p.ID, pub.TemplateVersionID, actor, pub.ContentJSON)
	if err != nil {
		return uuid.Nil, err
	}
	pv, res, err := h.store.UpsertDraft(ctx, pv)
	if err != nil {
		return uuid.Nil, err
	}
	if res.Inserted && h.appendixCopier != nil {
		if err := h.appendixCopier.CopyForward(ctx, p.CurrentPublishedVersionID, pv.ID); err != nil {
			return uuid.Nil, err
		}
	}
	return pv.ID, nil
}

// MovePolicy handles PolicyService.MovePolicy. It renumbers the policy from the new category's
// sequence; moving to the current category is a no-op.
func (h *PolicyHandler) MovePolicy(ctx context.Context, req *corev1.MovePolicyRequest) (*corev1.MovePolicyResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	dst, err := uuid.Parse(req.GetHomeCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid home_category_id: %v", err)
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	cur, err := h.store.GetPolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "get policy: %v", err)
	}
	if cur.HomeCategoryID == dst {
		return &corev1.MovePolicyResponse{Policy: policyToProto(cur)}, nil // no-op
	}
	// Existence only: the renumber reads the target's code inside the move transaction.
	if _, err := h.categoryStore.Get(ctx, dst); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "target category not found")
		}
		logger.Ctx(ctx).Error(err, "get target category failed", log.F("category_id", dst.String()))
		return nil, status.Error(codes.Internal, "get target category failed")
	}
	p, err := h.store.SetHomeCategory(ctx, pid, dst)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "move policy: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.moved",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy:" + p.ID.String(),
			GroupID:     p.HomeCategoryID.String(),
			Attributes: map[string]string{
				"from_group": cur.HomeCategoryID.String(),
				"to_group":   dst.String(),
				"old_number": cur.Number,
				"new_number": p.Number,
			},
		})
	}
	// New home category means new inherited governance, which can change this
	// policy's ack audience — purge acks whose user has left it.
	h.emitObligationChanged(ctx, p.ID.String())
	return &corev1.MovePolicyResponse{Policy: policyToProto(p)}, nil
}

// SetVersionStatus handles PolicyService.SetVersionStatus: workflow writes an approval outcome back
// through it. The approval states belong to workflow and are refused here.
func (h *PolicyHandler) SetVersionStatus(ctx context.Context, req *corev1.SetVersionStatusRequest) (*corev1.SetVersionStatusResponse, error) {
	pvID, err := uuid.Parse(req.GetPolicyVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_version_id: %v", err)
	}
	// actor_user_id is attribution, not a key. Workflow publishes as a system actor ("system" or
	// "system:*"), so only a value that is neither a UUID nor a system actor is refused.
	if !isSystemActor(req.GetActorUserId()) {
		if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
		}
	}
	target := domain.PolicyVersionStatus(req.GetStatus())
	if !domain.IsValidPolicyVersionStatus(target) {
		return nil, status.Errorf(codes.InvalidArgument,
			"status %q is not a stored policy-version status (want draft/published/superseded/archived)", req.GetStatus())
	}

	// Validate the transition before touching the store.
	cur, err := h.store.GetPolicyVersion(ctx, pvID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy version not found")
		}
		logger.Ctx(ctx).Error(err, "get policy version failed", log.F("version_id", pvID.String()))
		return nil, status.Error(codes.Internal, "get policy version failed")
	}
	if err := domain.ValidateVersionStatusTransition(cur.Status, target); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
	}

	pv, err := h.store.SetVersionStatus(ctx, pvID, target)
	if err != nil {
		// The transition was validated above, so a store-side transition error means the row
		// changed underneath us.
		if errors.Is(err, domain.ErrInvalidVersionStatusTransition) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "set version status: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.status_set",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy_version:" + pv.ID.String(),
			Attributes:  map[string]string{"status": string(target), "from": string(cur.Status)},
		})
	}
	// The status changed: drop the cached version and the version list.
	if h.versionCache != nil {
		h.versionCache.Del(ctx, "polver:"+pv.ID.String())
		h.versionCache.Del(ctx, "polvers:"+pv.PolicyID.String())
	}
	// A real move into published emits the lifecycle event PublishDraft does, or the AI index
	// misses versions published through approval. published to published is an allowed no-op and
	// emits nothing.
	if target == domain.PolicyVersionStatusPublished && cur.Status != domain.PolicyVersionStatusPublished {
		h.emitPublishedLifecycle(ctx, pv.PolicyID, pv)
	}
	return &corev1.SetVersionStatusResponse{Version: policyVersionToProto(pv)}, nil
}

// emitPublishedLifecycle publishes the version's content and access metadata on the jobs exchange.
// Best effort: a failure is logged and never fails the publish.
func (h *PolicyHandler) emitPublishedLifecycle(ctx context.Context, policyID uuid.UUID, pv domain.PolicyVersion) {
	if h.lifecyclePub == nil {
		return
	}
	content, err := h.buildVersionContent(ctx, policyID, pv)
	if err != nil {
		l := logger.Ctx(ctx)
		l.Warn("published lifecycle emit skipped: get policy failed", log.F("error", err.Error()), log.F("policy_id", policyID.String()))
		return
	}
	// A procedure has its own event, so it never reaches the acknowledgement pipeline.
	emit := h.lifecyclePub.EmitPublished
	if content.DocumentType == lifecycle.DocumentTypeProcedure {
		emit = h.lifecyclePub.EmitProcedurePublished
	}
	if err := emit(ctx, pv.PublishedAt, content); err != nil {
		l := logger.Ctx(ctx)
		l.Warn("published lifecycle emit failed", log.F("error", err.Error()), log.F("policy_id", pv.PolicyID.String()), log.F("version_id", pv.ID.String()), log.F("document_type", content.DocumentType))
	}
}

// emitRetiredLifecycle emits the retired event so obligations can tell the audience. Best effort: a
// failure is logged, never returned.
func (h *PolicyHandler) emitRetiredLifecycle(ctx context.Context, policyID uuid.UUID) {
	if h.lifecyclePub == nil {
		return
	}
	pol, err := h.store.GetPolicy(ctx, policyID)
	if err != nil {
		l := logger.Ctx(ctx)
		l.Warn("retired lifecycle emit skipped: get policy failed", log.F("error", err.Error()), log.F("policy_id", policyID.String()))
		return
	}
	retiredAt := time.Now().UTC()
	if pol.RetiredAt != nil {
		retiredAt = pol.RetiredAt.UTC()
	}
	// A procedure has its own retired event.
	emit := h.lifecyclePub.EmitRetired
	if pol.DocumentType == domain.DocumentTypeProcedure {
		emit = h.lifecyclePub.EmitProcedureRetired
	}
	if err := emit(ctx, retiredAt, policyID.String(), pol.Number, pol.Title); err != nil {
		l := logger.Ctx(ctx)
		l.Warn("retired lifecycle emit failed", log.F("error", err.Error()), log.F("policy_id", policyID.String()), log.F("document_type", string(pol.DocumentType)))
	}
}

// buildVersionContent builds the payload the AI indexer reads, for publish and reindex alike. Only
// a failed policy lookup is an error; unreadable content gives a metadata-only event.
func (h *PolicyHandler) buildVersionContent(ctx context.Context, policyID uuid.UUID, pv domain.PolicyVersion) (lifecycle.PolicyVersionContent, error) {
	pol, err := h.store.GetPolicy(ctx, policyID)
	if err != nil {
		return lifecycle.PolicyVersionContent{}, err
	}
	var categoryIDStr string
	if pol.HomeCategoryID != uuid.Nil {
		categoryIDStr = pol.HomeCategoryID.String()
	}

	var sections []lifecycle.SectionContent
	if pv.TemplateVersionID != uuid.Nil && h.tplStore != nil {
		if tv, err := h.tplStore.GetTemplateVersion(ctx, pv.TemplateVersionID); err == nil {
			for _, s := range domain.ExtractSections(pv.ContentJSON, tv.Sections) {
				sections = append(sections, lifecycle.SectionContent{Key: s.Key, Text: s.Text})
			}
		}
	}
	if sections == nil {
		// No template sections: extract from the content alone.
		for _, s := range domain.ExtractSections(pv.ContentJSON, nil) {
			sections = append(sections, lifecycle.SectionContent{Key: s.Key, Text: s.Text})
		}
	}

	return lifecycle.PolicyVersionContent{
		PolicyID:    pv.PolicyID.String(),
		VersionID:   pv.ID.String(),
		VersionNo:   pv.VersionNo,
		CategoryID:  categoryIDStr,
		Sensitivity: string(pol.Sensitivity),
		PolicyTitle: pol.Title,
		// The policy's own effective date. nil when it has none; obligations then shows the publish
		// time.
		EffectiveDate: pol.EffectiveDate,
		// Set for both kinds, so every consumer sees which kind it is.
		DocumentType: docTypeWire(pol.DocumentType),
		Sections:     sections,
	}, nil
}

// docTypeWire maps the document type to its lifecycle wire value; the zero value is a policy.
func docTypeWire(dt domain.DocumentType) string {
	if dt == domain.DocumentTypeProcedure {
		return lifecycle.DocumentTypeProcedure
	}
	return lifecycle.DocumentTypePolicy
}

// reindexPublishedVersion sends one published version to the AI indexer only, and removes the
// chunks of the policy's other versions. The indexer only upserts and search doesn't filter by
// version, so without the removal a superseded version would stay searchable. pv must already be
// published.
func (h *PolicyHandler) reindexPublishedVersion(ctx context.Context, policyID uuid.UUID, pv domain.PolicyVersion) (sections int, removedPrior int, err error) {
	content, err := h.buildVersionContent(ctx, policyID, pv)
	if err != nil {
		return 0, 0, status.Errorf(codes.Internal, "load policy for re-index: %v", err)
	}

	// Drafts are never indexed, so only the other published versions need removing.
	priors, lerr := h.store.ListPublishedVersions(ctx, policyID)
	if lerr != nil {
		return 0, 0, status.Errorf(codes.Internal, "list versions for re-index: %v", lerr)
	}

	// Straight to the AI queue for the kind through the default exchange, so no other jobs consumer
	// sees it.
	isProcedure := content.DocumentType == lifecycle.DocumentTypeProcedure
	emitRemove := h.lifecyclePub.EmitReindexRemove
	emitReindex := h.lifecyclePub.EmitReindex
	if isProcedure {
		emitRemove = h.lifecyclePub.EmitProcedureReindexRemove
		emitReindex = h.lifecyclePub.EmitProcedureReindex
	}

	for _, prior := range priors {
		if prior.ID == pv.ID {
			continue
		}
		if rerr := emitRemove(ctx, policyID.String(), prior.ID.String()); rerr != nil {
			return 0, removedPrior, status.Errorf(codes.Internal, "emit re-index remove: %v", rerr)
		}
		removedPrior++
	}

	if eerr := emitReindex(ctx, pv.PublishedAt, content); eerr != nil {
		return len(content.Sections), removedPrior, status.Errorf(codes.Internal, "emit re-index: %v", eerr)
	}
	return len(content.Sections), removedPrior, nil
}

// ReindexPolicy re-indexes the policy's current published version; FailedPrecondition when nothing
// is published.
func (h *PolicyHandler) ReindexPolicy(ctx context.Context, req *corev1.ReindexPolicyRequest) (*corev1.ReindexPolicyResponse, error) {
	if h.lifecyclePub == nil {
		return nil, status.Error(codes.Internal, "re-index publisher not configured")
	}
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	pol, err := h.store.GetPolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "get policy: %v", err)
	}
	if pol.CurrentPublishedVersionID == uuid.Nil {
		return nil, status.Error(codes.FailedPrecondition, "policy has no published version to re-index")
	}
	pv, err := h.store.GetPolicyVersion(ctx, pol.CurrentPublishedVersionID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get published version: %v", err)
	}
	sections, removedPrior, err := h.reindexPublishedVersion(ctx, pid, pv)
	if err != nil {
		return nil, err
	}
	h.emitReindexAudit(ctx, req.GetActorUserId(), pv.ID.String())
	return &corev1.ReindexPolicyResponse{
		VersionId:    pv.ID.String(),
		Sections:     toInt32(sections),
		RemovedPrior: toInt32(removedPrior),
	}, nil
}

// ReindexPolicyVersion re-indexes one version, which must be the published one.
func (h *PolicyHandler) ReindexPolicyVersion(ctx context.Context, req *corev1.ReindexPolicyVersionRequest) (*corev1.ReindexPolicyVersionResponse, error) {
	if h.lifecyclePub == nil {
		return nil, status.Error(codes.Internal, "re-index publisher not configured")
	}
	pvID, err := uuid.Parse(req.GetPolicyVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_version_id: %v", err)
	}
	pv, err := h.store.GetPolicyVersion(ctx, pvID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy version not found")
		}
		return nil, status.Errorf(codes.Internal, "get policy version: %v", err)
	}
	if pv.Status != domain.PolicyVersionStatusPublished {
		return nil, status.Errorf(codes.FailedPrecondition,
			"only a published version can be re-indexed (version is %q)", pv.Status)
	}
	sections, removedPrior, err := h.reindexPublishedVersion(ctx, pv.PolicyID, pv)
	if err != nil {
		return nil, err
	}
	h.emitReindexAudit(ctx, req.GetActorUserId(), pv.ID.String())
	return &corev1.ReindexPolicyVersionResponse{
		VersionId:    pv.ID.String(),
		Sections:     toInt32(sections),
		RemovedPrior: toInt32(removedPrior),
	}, nil
}

// ReindexAllPublished re-indexes every policy's published version, best effort per policy.
func (h *PolicyHandler) ReindexAllPublished(ctx context.Context, req *corev1.ReindexAllPublishedRequest) (*corev1.ReindexAllPublishedResponse, error) {
	if h.lifecyclePub == nil {
		return nil, status.Error(codes.Internal, "re-index publisher not configured")
	}
	policies, err := h.store.ListAllPolicies(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list policies: %v", err)
	}
	var reindexed, skipped, failures int32
	for _, pol := range policies {
		if pol.CurrentPublishedVersionID == uuid.Nil {
			skipped++
			continue
		}
		pv, gerr := h.store.GetPolicyVersion(ctx, pol.CurrentPublishedVersionID)
		if gerr != nil {
			failures++
			l := logger.Ctx(ctx)
			l.Warn("reindex-all: get published version failed", log.F("error", gerr.Error()), log.F("policy_id", pol.ID.String()))
			continue
		}
		if _, _, rerr := h.reindexPublishedVersion(ctx, pol.ID, pv); rerr != nil {
			failures++
			l := logger.Ctx(ctx)
			l.Warn("reindex-all: re-index failed", log.F("error", rerr.Error()), log.F("policy_id", pol.ID.String()))
			continue
		}
		reindexed++
	}
	h.emitReindexAudit(ctx, req.GetActorUserId(), "all")
	return &corev1.ReindexAllPublishedResponse{
		PoliciesReindexed: reindexed,
		Skipped:           skipped,
		Failures:          failures,
	}, nil
}

// emitReindexAudit audits a re-index; subject is the version id, or "all".
func (h *PolicyHandler) emitReindexAudit(ctx context.Context, actor, subject string) {
	if h.auditor == nil {
		return
	}
	_ = h.auditor.Emit(ctx, audit.Event{
		Tier:        audit.TierAudit,
		Action:      "policy.reindexed",
		ActorUserID: actor,
		Subject:     "policy_version:" + subject,
	})
}

// DiffVersions handles PolicyService.DiffVersions.
func (h *PolicyHandler) DiffVersions(ctx context.Context, req *corev1.DiffVersionsRequest) (*corev1.DiffVersionsResponse, error) {
	fromID, err := uuid.Parse(req.GetFromVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid from_version_id: %v", err)
	}
	toID, err := uuid.Parse(req.GetToVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid to_version_id: %v", err)
	}

	// A diff of two published versions never changes, so it's cached by version pair. One with a
	// draft side isn't.
	cacheKey := "poldiff:" + fromID.String() + ":" + toID.String()
	if h.versionCache != nil {
		l := logger.Ctx(ctx)
		if b, ok := h.versionCache.GetBytes(ctx, cacheKey); ok {
			var resp corev1.DiffVersionsResponse
			if proto.Unmarshal(b, &resp) == nil {
				l.Debug("version-diff cache hit", log.F("cache_key", cacheKey))
				return &resp, nil
			}
		}
		l.Debug("version-diff cache miss", log.F("cache_key", cacheKey))
	}

	fromPV, err := h.store.GetPolicyVersion(ctx, fromID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "from version not found")
		}
		logger.Ctx(ctx).Error(err, "get from version failed", log.F("version_id", fromID.String()))
		return nil, status.Error(codes.Internal, "get from version failed")
	}
	toPV, err := h.store.GetPolicyVersion(ctx, toID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "to version not found")
		}
		logger.Ctx(ctx).Error(err, "get to version failed", log.F("version_id", toID.String()))
		return nil, status.Error(codes.Internal, "get to version failed")
	}

	isTemplateMigration := fromPV.TemplateVersionID != toPV.TemplateVersionID

	// The pinned template version, not the template, so renamed sections still resolve.
	tv, err := h.tplStore.GetTemplateVersion(ctx, toPV.TemplateVersionID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetch template version: %v", err)
	}

	diffs, err := domain.DiffVersions(fromPV.ContentJSON, toPV.ContentJSON, tv.Sections, isTemplateMigration)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "diff: %v", err)
	}

	out := make([]*corev1.SectionDiff, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, &corev1.SectionDiff{
			SectionKey:    d.SectionKey,
			SectionTitle:  d.SectionTitle,
			ChangeType:    string(d.ChangeType),
			WordDiffHtml:  d.WordDiffHTML,
			IsBoilerplate: d.IsBoilerplate,
		})
	}
	resp := &corev1.DiffVersionsResponse{Diffs: out}
	if h.versionCache != nil &&
		fromPV.Status == domain.PolicyVersionStatusPublished &&
		toPV.Status == domain.PolicyVersionStatusPublished {
		if b, mErr := proto.Marshal(resp); mErr == nil {
			h.versionCache.SetBytes(ctx, cacheKey, b)
		}
	}
	return resp, nil
}

// UpdateDraftContent handles PolicyService.UpdateDraftContent, which collab calls once per
// debounced snapshot. The snapshot is validated against the pinned template version and written to
// the draft; any failure stops before the write and the audit event. An empty snapshot over a draft
// that still has text is refused with FailedPrecondition. A freeform draft sends an empty
// template_version_id, which must match its empty pin.
func (h *PolicyHandler) UpdateDraftContent(ctx context.Context, req *corev1.UpdateDraftContentRequest) (*corev1.UpdateDraftContentResponse, error) {
	// Internal, but it still needs an actor for audit.
	if req.GetActorUserId() == "" {
		return nil, status.Errorf(codes.Unauthenticated, "actor_user_id is required")
	}

	draftID, err := uuid.Parse(req.GetDraftId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid draft_id: %v", err)
	}
	// Empty is valid: a freeform draft has no template. A malformed value is not.
	reqTvID, err := parseOptionalUUID(req.GetTemplateVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid template_version_id: %v", err)
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	// policy_id is informational; refuse a malformed one rather than ignore it.
	if req.GetPolicyId() != "" {
		if _, err := uuid.Parse(req.GetPolicyId()); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
		}
	}

	pv, err := h.store.GetPolicyVersion(ctx, draftID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "draft not found")
		}
		logger.Ctx(ctx).Error(err, "get draft failed", log.F("draft_id", draftID.String()))
		return nil, status.Error(codes.Internal, "get draft failed")
	}
	if pv.Status != domain.PolicyVersionStatusDraft {
		return nil, status.Errorf(codes.FailedPrecondition,
			"policy version %s is not a draft (status=%s)", draftID, pv.Status)
	}
	// Freeform means no template, not any template: an empty pin matches only an empty claim, and a
	// real pin only itself.
	if pv.TemplateVersionID != reqTvID {
		return nil, status.Errorf(codes.InvalidArgument,
			"template_version_id %s does not match draft's pinned version %s",
			reqTvID, pv.TemplateVersionID)
	}

	// ValidateDraft, not the strict Validate: the editor produces headings and paragraphs, never
	// the strict template vocabulary, so the strict check would refuse every snapshot. Section-
	// shaped content still gets the strict check. A freeform draft has no template version to
	// fetch.
	var sections []validate.TemplateSection
	if reqTvID != uuid.Nil {
		tv, err := h.tplStore.GetTemplateVersion(ctx, reqTvID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "fetch template version: %v", err)
		}
		sections = templateSectionsForValidate(tv.Sections)
	}
	if err := validate.ValidateDraft(req.GetContentJson(), sections); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "content validation: %v", err)
	}

	// Never let a snapshot with no text replace a draft that has text: an editor that failed to
	// load its state would otherwise overwrite the draft. collab reports the refusal as
	// snapshot.rejected.
	if !validate.HasTextContent(req.GetContentJson()) && validate.HasTextContent(pv.ContentJSON) {
		return nil, status.Errorf(codes.FailedPrecondition,
			"content validation: refusing to checkpoint empty content over a draft that has text")
	}

	if err := h.store.UpdateDraftContent(ctx, draftID, req.GetContentJson()); err != nil {
		return nil, status.Errorf(codes.Internal, "update draft content: %v", err)
	}

	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.draft_content_updated",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy_version:" + draftID.String(),
		})
	}

	return &corev1.UpdateDraftContentResponse{
		DraftId:  draftID.String(),
		Accepted: true,
	}, nil
}

// templateSectionsForValidate maps template blocks to the validator's shape: an editable block's
// ContentJSON holds its region key, a boilerplate block's holds its exact content.
func templateSectionsForValidate(sections []domain.Section) []validate.TemplateSection {
	out := make([]validate.TemplateSection, len(sections))
	for i, s := range sections {
		blocks := make([]validate.TemplateBlock, len(s.Blocks))
		for j, b := range s.Blocks {
			vb := validate.TemplateBlock{Type: string(b.Type)}
			switch b.Type {
			case domain.BlockTypeBoilerplate:
				vb.Content = b.ContentJSON
			case domain.BlockTypeEditable:
				vb.RegionKey = b.ContentJSON
			}
			blocks[j] = vb
		}
		out[i] = validate.TemplateSection{Key: s.Key, Order: s.Order, Blocks: blocks}
	}
	return out
}

// resolveObligation works out one policy's acknowledgement obligation: the home category's trigger
// with any policy override, and the audience from the category subtree or the explicit override. A
// procedure, a retired policy, or one with no trigger or nothing published obligates no one.
// everyone is the inherited "Everyone" flag; when true the audience is every user.
func (h *PolicyHandler) resolveObligation(ctx context.Context, p domain.Policy) (requiresAck bool, onChange bool, audienceGroups []string, everyone bool, err error) {
	// Procedures never obligate anyone. Obligations are computed from this, so this one check keeps
	// them out of every acknowledgement path.
	if p.DocumentType == domain.DocumentTypeProcedure {
		return false, false, nil, false, nil
	}
	// A retired policy obligates no one; completed acknowledgements stay with obligations.
	if p.RetiredAt != nil {
		return false, false, nil, false, nil
	}
	owning, err := h.categoryStore.Get(ctx, p.HomeCategoryID)
	if err != nil {
		return false, false, nil, false, err
	}
	trig := owning.AckTriggers
	if p.AckTriggers != nil {
		trig = *p.AckTriggers
	}
	if trig == domain.AckNone || trig == "" {
		return false, false, nil, false, nil
	}
	if p.CurrentPublishedVersionID == uuid.Nil {
		return false, false, nil, false, nil
	}

	// Independent of ack_audience_override, which only narrows the explicit audience.
	chain, err := h.categoryStore.AncestorChain(ctx, p.HomeCategoryID)
	if err != nil {
		return false, false, nil, false, err
	}
	if ev := domain.EffectiveAckEveryone(chain); ev != nil {
		everyone = *ev
	}

	var audience []domain.Category
	if len(p.AckAudienceOverride) > 0 {
		for _, gid := range p.AckAudienceOverride {
			g, e := h.categoryStore.Get(ctx, gid)
			if e != nil {
				return false, false, nil, false, e
			}
			audience = append(audience, g)
		}
	} else {
		audience, err = h.categoryStore.Subtree(ctx, p.HomeCategoryID)
		if err != nil {
			return false, false, nil, false, err
		}
	}
	set := map[string]struct{}{}
	for _, g := range audience {
		if g.AudienceGroupIDs == nil {
			continue
		}
		for _, ad := range *g.AudienceGroupIDs {
			set[ad] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for ad := range set {
		out = append(out, ad)
	}
	return true, trig == domain.AckOnChange, out, everyone, nil
}

// ResolvePolicyObligation handles PolicyService.ResolvePolicyObligation.
func (h *PolicyHandler) ResolvePolicyObligation(ctx context.Context, req *corev1.ResolvePolicyObligationRequest) (*corev1.ResolvePolicyObligationResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	p, err := h.store.GetPolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		logger.Ctx(ctx).Error(err, "get policy failed", log.F("policy_id", pid.String()))
		return nil, status.Error(codes.Internal, "get policy failed")
	}
	requiresAck, onChange, audienceADs, everyone, err := h.resolveObligation(ctx, p)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "resolve obligation: %v", err)
	}
	return &corev1.ResolvePolicyObligationResponse{
		RequiresAck:        requiresAck,
		OnChange:           onChange,
		AudienceGroups:     audienceADs,
		PublishedVersionId: nilableUUID(p.CurrentPublishedVersionID),
		Everyone:           everyone,
	}, nil
}

// ListObligatingPolicies handles PolicyService.ListObligatingPolicies: the policies that need
// acknowledging.
func (h *PolicyHandler) ListObligatingPolicies(ctx context.Context, _ *corev1.ListObligatingPoliciesRequest) (*corev1.ListObligatingPoliciesResponse, error) {
	all, err := h.store.ListAllPolicies(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list all policies: %v", err)
	}
	var out []*corev1.ObligatingPolicy
	for _, p := range all {
		requiresAck, onChange, audienceADs, everyone, err := h.resolveObligation(ctx, p)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "resolve obligation for %s: %v", p.ID, err)
		}
		if !requiresAck {
			continue
		}
		var versionNo int32
		if p.CurrentPublishedVersionID != uuid.Nil {
			pv, err := h.store.GetPolicyVersion(ctx, p.CurrentPublishedVersionID)
			if err == nil {
				versionNo = toInt32(pv.VersionNo)
			}
		}
		out = append(out, &corev1.ObligatingPolicy{
			PolicyId:           p.ID.String(),
			Number:             p.Number,
			Title:              p.Title,
			PublishedVersionId: nilableUUID(p.CurrentPublishedVersionID),
			VersionNo:          versionNo,
			AudienceGroups:     audienceADs,
			OnChange:           onChange,
			Everyone:           everyone,
			// Always a policy (procedures are excluded above); carried so the response describes
			// itself.
			DocumentType: docTypeToProto(p.DocumentType),
		})
	}
	return &corev1.ListObligatingPoliciesResponse{Policies: out}, nil
}

// SetAck handles PolicyService.SetAck.
func (h *PolicyHandler) SetAck(ctx context.Context, req *corev1.SetAckRequest) (*corev1.SetAckResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}

	override := make([]uuid.UUID, 0, len(req.GetAckAudienceOverride()))
	for _, s := range req.GetAckAudienceOverride() {
		gid, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid ack_audience_override uuid %q: %v", s, err)
		}
		override = append(override, gid)
	}

	// ack_triggers_set=false clears the override, so the category's trigger applies.
	var ack *domain.AckTrigger
	if req.GetAckTriggersSet() {
		t := ackTriggerFromProto(req.GetAckTriggers())
		ack = &t
	}

	p, err := h.store.SetAck(ctx, pid, ack, override)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set ack: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier:    audit.TierAudit,
			Action:  "policy.ack_config_updated",
			Subject: "policy:" + p.ID.String(),
			GroupID: p.HomeCategoryID.String(),
		})
	}
	// A policy-level ack config change (trigger→none, or a narrowed audience
	// override) can shrink who must ack — purge acks whose user has left.
	h.emitObligationChanged(ctx, p.ID.String())
	return &corev1.SetAckResponse{Policy: policyToProto(p)}, nil
}

// emitObligationChanged publishes policy.obligation_changed. Best effort: obligations reconciles
// again on the next change.
func (h *PolicyHandler) emitObligationChanged(ctx context.Context, policyID string) {
	if h.lifecyclePub == nil {
		return
	}
	if err := h.lifecyclePub.EmitObligationChanged(ctx, policyID); err != nil {
		logger.Ctx(ctx).Warn("obligation-changed: emit", log.F("error", err.Error()), log.F("policy_id", policyID))
	}
}

// GetEffectiveTemplate handles PolicyService.GetEffectiveTemplate.
func (h *PolicyHandler) GetEffectiveTemplate(ctx context.Context, req *corev1.GetEffectiveTemplateRequest) (*corev1.GetEffectiveTemplateResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	p, err := h.store.GetPolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		logger.Ctx(ctx).Error(err, "get policy failed", log.F("policy_id", pid.String()))
		return nil, status.Error(codes.Internal, "get policy failed")
	}
	chain, err := h.categoryStore.AncestorChain(ctx, p.HomeCategoryID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ancestor chain: %v", err)
	}
	res, effectiveTplID := domain.EffectiveTemplate(p.TemplateID, p.TemplateNone, chain)
	switch res {
	case domain.TemplateExplicitNone:
		// Freeform is not an error: empty ids signal none.
		return &corev1.GetEffectiveTemplateResponse{None: true}, nil
	case domain.TemplateInheritNotFound:
		// Nothing pinned and not freeform is a misconfiguration.
		return nil, status.Errorf(codes.FailedPrecondition, "no effective template found for policy")
	}
	tv, err := h.tplStore.GetLatestPublishedVersion(ctx, effectiveTplID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "latest template version not found")
		}
		logger.Ctx(ctx).Error(err, "get latest template version failed", log.F("template_id", effectiveTplID.String()))
		return nil, status.Error(codes.Internal, "get latest template version failed")
	}
	return &corev1.GetEffectiveTemplateResponse{
		TemplateId:        effectiveTplID.String(),
		TemplateVersionId: tv.ID.String(),
	}, nil
}

// SetPolicyTemplate handles PolicyService.SetPolicyTemplate: none for freeform, a template_id for
// that template, or neither to inherit.
func (h *PolicyHandler) SetPolicyTemplate(ctx context.Context, req *corev1.SetPolicyTemplateRequest) (*corev1.SetPolicyTemplateResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	tplID, err := parseOptionalUUID(req.GetTemplateId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid template_id: %v", err)
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	p, err := h.store.SetTemplate(ctx, pid, tplID, req.GetNone())
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "set policy template: %v", err)
	}
	if h.auditor != nil {
		attrs := map[string]string{"none": strconv.FormatBool(req.GetNone())}
		if tplID != uuid.Nil {
			attrs["template_id"] = tplID.String()
		}
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.template_changed",
			ActorUserID: req.GetActorUserId(),
			Subject:     "policy:" + p.ID.String(),
			GroupID:     p.HomeCategoryID.String(),
			Attributes:  attrs,
		})
	}
	return &corev1.SetPolicyTemplateResponse{Policy: policyToProto(p)}, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"fmt"
	stewardauthz "github.com/Steward-GRC/steward-authz"
	"sort"
	"strconv"
	"strings"
	"time"

	log "github.com/Bugs5382/go-log"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/errcodes"
	"github.com/Steward-GRC/steward-core/internal/lifecycle"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// CategoryStorer is the store the CategoryHandler uses.
type CategoryStorer interface {
	Create(ctx context.Context, g domain.Category) (domain.Category, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Category, error)
	ListChildren(ctx context.Context, parentID uuid.UUID) ([]domain.Category, error)
	AncestorChain(ctx context.Context, categoryID uuid.UUID) ([]domain.Category, error)
	SetDefaults(ctx context.Context, id uuid.UUID, templateID, workflowID uuid.UUID, templateNone bool) (domain.Category, error)
	Rename(ctx context.Context, id uuid.UUID, name, slug string) (domain.Category, error)
	Delete(ctx context.Context, id uuid.UUID) error
	MoveCategory(ctx context.Context, categoryID uuid.UUID, newParentID *uuid.UUID) (domain.Category, int, error)
	SetGovernance(ctx context.Context, id uuid.UUID, owners []uuid.UUID, audienceGroupIDs *[]string, ack domain.AckTrigger, cadence domain.ReviewCadence, reviewDate *time.Time, exclusionGroupIDs *[]string, ackEveryone *bool) (domain.Category, error)
	Subtree(ctx context.Context, rootID uuid.UUID) ([]domain.Category, error)
	GetCategoryRuleset(ctx context.Context, categoryID uuid.UUID) ([]domain.CategoryRule, error)
	SetCategoryRuleset(ctx context.Context, categoryID uuid.UUID, rules []domain.CategoryRule) ([]domain.CategoryRule, error)
	// dryRun reads without writing.
	PurgeUserCategoryRules(ctx context.Context, userID uuid.UUID, dryRun bool) ([]store.PurgedCategoryRule, error)
}

// categoryPolicyLister lists the policies in a category or its subtree, for
// the obligation fan-out.
type categoryPolicyLister interface {
	ListPolicies(ctx context.Context, categoryID uuid.UUID, includeDescendants bool, docType domain.DocumentType) ([]domain.Policy, error)
}

// CategoryHandler implements corev1.CategoryServiceServer.
type CategoryHandler struct {
	corev1.UnimplementedCategoryServiceServer
	store   CategoryStorer
	auditor auditEmitter
	// Both nil unless WithObligationEmitter set them.
	lifecyclePub *lifecycle.Emitter
	policies     categoryPolicyLister
}

// NewCategoryHandler returns a handler over s. auditor may be nil.
func NewCategoryHandler(s CategoryStorer, auditor auditEmitter) *CategoryHandler {
	return &CategoryHandler{store: s, auditor: auditor}
}

// WithObligationEmitter makes governance and move changes emit one
// policy.obligation_changed per affected policy, so obligations can purge
// acknowledgements from users who left the audience.
func (h *CategoryHandler) WithObligationEmitter(em *lifecycle.Emitter, lister categoryPolicyLister) *CategoryHandler {
	h.lifecyclePub = em
	h.policies = lister
	return h
}

// emitObligationChangedForCategory notifies every policy in the subtree:
// governance inherits down the tree, so a change here can shrink any of their
// audiences. Failures are only logged; obligations' reconcile is idempotent
// and the next change triggers it again.
func (h *CategoryHandler) emitObligationChangedForCategory(ctx context.Context, categoryID uuid.UUID) {
	if h.lifecyclePub == nil || h.policies == nil {
		return
	}
	// Procedures carry no acknowledgement.
	pols, err := h.policies.ListPolicies(ctx, categoryID, true, domain.DocumentTypePolicy)
	if err != nil {
		logger.Ctx(ctx).Warn("obligation-changed: list policies", log.F("error", err.Error()), log.F("category_id", categoryID.String()))
		return
	}
	for _, p := range pols {
		if err := h.lifecyclePub.EmitObligationChanged(ctx, p.ID.String()); err != nil {
			logger.Ctx(ctx).Warn("obligation-changed: emit", log.F("error", err.Error()), log.F("policy_id", p.ID.String()))
		}
	}
}

func ackTriggerFromProto(t corev1.AckTrigger) domain.AckTrigger {
	switch t {
	case corev1.AckTrigger_ACK_TRIGGER_ON_PUBLISH:
		return domain.AckOnPublish
	case corev1.AckTrigger_ACK_TRIGGER_ON_CHANGE:
		return domain.AckOnChange
	default:
		return domain.AckNone
	}
}

func ackTriggerToProto(t domain.AckTrigger) corev1.AckTrigger {
	switch t {
	case domain.AckOnPublish:
		return corev1.AckTrigger_ACK_TRIGGER_ON_PUBLISH
	case domain.AckOnChange:
		return corev1.AckTrigger_ACK_TRIGGER_ON_CHANGE
	default:
		return corev1.AckTrigger_ACK_TRIGGER_NONE
	}
}

func reviewCadenceFromProto(c corev1.ReviewCadence) domain.ReviewCadence {
	switch c {
	case corev1.ReviewCadence_REVIEW_CADENCE_ANNUAL:
		return domain.CadenceAnnual
	case corev1.ReviewCadence_REVIEW_CADENCE_BIENNIAL:
		return domain.CadenceBiennial
	case corev1.ReviewCadence_REVIEW_CADENCE_ON_DATE:
		return domain.CadenceOnDate
	default:
		return domain.CadenceNone
	}
}

func reviewCadenceToProto(c domain.ReviewCadence) corev1.ReviewCadence {
	switch c {
	case domain.CadenceAnnual:
		return corev1.ReviewCadence_REVIEW_CADENCE_ANNUAL
	case domain.CadenceBiennial:
		return corev1.ReviewCadence_REVIEW_CADENCE_BIENNIAL
	case domain.CadenceOnDate:
		return corev1.ReviewCadence_REVIEW_CADENCE_ON_DATE
	default:
		return corev1.ReviewCadence_REVIEW_CADENCE_NONE
	}
}

// reviewDateToString formats a nullable time as "2006-01-02", or "" when nil.
func reviewDateToString(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format("2006-01-02")
}

// reviewDateFromString parses "2006-01-02" into a *time.Time; empty string
// returns nil (clear the column). A malformed string returns an error.
func reviewDateFromString(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func categoryToProto(g domain.Category) *corev1.Category {
	owners := make([]string, 0, len(g.Owners))
	for _, id := range g.Owners {
		owners = append(owners, id.String())
	}
	var audienceGroupIDs []string
	audienceGroupIDsSet := g.AudienceGroupIDs != nil
	if audienceGroupIDsSet {
		audienceGroupIDs = *g.AudienceGroupIDs
	}
	var exclusionGroupIDs []string
	exclusionGroupIDsSet := g.ExclusionGroupIDs != nil
	if exclusionGroupIDsSet {
		exclusionGroupIDs = *g.ExclusionGroupIDs
	}
	var ackEveryone bool
	ackEveryoneSet := g.AckEveryone != nil
	if ackEveryoneSet {
		ackEveryone = *g.AckEveryone
	}
	return &corev1.Category{
		Id:                   g.ID.String(),
		Name:                 g.Name,
		Slug:                 g.Slug,
		ParentId:             nilableUUID(g.ParentID),
		DefaultTemplateId:    nilableUUID(g.DefaultTemplateID),
		DefaultTemplateNone:  g.DefaultTemplateNone,
		DefaultWorkflowId:    nilableUUID(g.DefaultWorkflowID),
		Owners:               owners,
		AudienceGroupIds:     audienceGroupIDs,
		AckTriggers:          ackTriggerToProto(g.AckTriggers),
		ReviewCadence:        reviewCadenceToProto(g.ReviewCadence),
		ReviewDate:           reviewDateToString(g.ReviewDate),
		ExclusionGroupIds:    exclusionGroupIDs,
		AudienceGroupIdsSet:  audienceGroupIDsSet,
		ExclusionGroupIdsSet: exclusionGroupIDsSet,
		AckEveryone:          ackEveryone,
		AckEveryoneSet:       ackEveryoneSet,
	}
}

func nilableUUID(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// isSystemActor reports whether actor is "system" or "system:<component>",
// the attribution machine callers such as workflow use. Handlers that want a
// user UUID must still accept it.
func isSystemActor(s string) bool {
	return s == "system" || strings.HasPrefix(s, "system:")
}

// parseOptionalUUID returns uuid.Nil for an empty string.
func parseOptionalUUID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, nil
	}
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// CreateCategory handles CategoryService.CreateCategory.
func (h *CategoryHandler) CreateCategory(ctx context.Context, req *corev1.CreateCategoryRequest) (*corev1.CreateCategoryResponse, error) {
	parentID, err := parseOptionalUUID(req.GetParentId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent_id: %v", err)
	}
	g, err := domain.NewCategory(req.GetName(), req.GetSlug(), parentID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	g, err = h.store.Create(ctx, g)
	if err != nil {
		if errors.Is(err, store.ErrCategorySlugConflict) {
			return nil, status.Errorf(codes.FailedPrecondition, "create category: %v", err)
		}
		return nil, status.Errorf(codes.Internal, "create category: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "category.created",
			Subject:     "category:" + g.ID.String(),
			GroupID:     g.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.CreateCategoryResponse{Category: categoryToProto(g)}, nil
}

// GetCategory handles CategoryService.GetCategory.
func (h *CategoryHandler) GetCategory(ctx context.Context, req *corev1.GetCategoryRequest) (*corev1.GetCategoryResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	g, err := h.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "category not found")
		}
		logger.Ctx(ctx).Error(err, "get category failed", log.F("category_id", id.String()))
		return nil, status.Error(codes.Internal, "get category failed")
	}
	return &corev1.GetCategoryResponse{Category: categoryToProto(g)}, nil
}

// ListCategoryChildren handles CategoryService.ListCategoryChildren.
func (h *CategoryHandler) ListCategoryChildren(ctx context.Context, req *corev1.ListCategoryChildrenRequest) (*corev1.ListCategoryChildrenResponse, error) {
	parentID, err := parseOptionalUUID(req.GetParentId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid parent_id: %v", err)
	}
	categories, err := h.store.ListChildren(ctx, parentID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list children: %v", err)
	}
	out := make([]*corev1.Category, 0, len(categories))
	for _, g := range categories {
		out = append(out, categoryToProto(g))
	}
	return &corev1.ListCategoryChildrenResponse{Categories: out}, nil
}

// SetGovernance handles CategoryService.SetGovernance.
func (h *CategoryHandler) SetGovernance(ctx context.Context, req *corev1.SetGovernanceRequest) (*corev1.SetGovernanceResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	owners := make([]uuid.UUID, 0, len(req.GetOwners()))
	for _, s := range req.GetOwners() {
		uid, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid owner uuid %q: %v", s, err)
		}
		owners = append(owners, uid)
	}
	reviewDate, err := reviewDateFromString(req.GetReviewDate())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid review_date %q: %v", req.GetReviewDate(), err)
	}
	ack := ackTriggerFromProto(req.GetAckTriggers())
	cadence := reviewCadenceFromProto(req.GetReviewCadence())

	// Without the _provided flag a list is left nil, which keeps the stored
	// value: an absent field must not read as an explicit empty override.
	var audienceGroupIDsPtr *[]string
	if req.GetAudienceGroupIdsProvided() {
		ids := req.GetAudienceGroupIds()
		audienceGroupIDsPtr = &ids
	}
	var exclusionGroupIDsPtr *[]string
	if req.GetExclusionGroupIdsProvided() {
		ids := req.GetExclusionGroupIds()
		exclusionGroupIDsPtr = &ids
	}
	var ackEveryonePtr *bool
	if req.GetAckEveryoneProvided() {
		v := req.GetAckEveryone()
		ackEveryonePtr = &v
	}
	g, err := h.store.SetGovernance(ctx, id, owners, audienceGroupIDsPtr, ack, cadence, reviewDate, exclusionGroupIDsPtr, ackEveryonePtr)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set governance: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier:        audit.TierAudit,
			Action:      "category.governance_updated",
			Subject:     "category:" + g.ID.String(),
			GroupID:     g.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	h.emitObligationChangedForCategory(ctx, g.ID)
	return &corev1.SetGovernanceResponse{Category: categoryToProto(g)}, nil
}

// SetCategoryDefaults handles CategoryService.SetCategoryDefaults.
func (h *CategoryHandler) SetCategoryDefaults(ctx context.Context, req *corev1.SetCategoryDefaultsRequest) (*corev1.SetCategoryDefaultsResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	tplID, err := parseOptionalUUID(req.GetDefaultTemplateId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid default_template_id: %v", err)
	}
	wfID, err := parseOptionalUUID(req.GetDefaultWorkflowId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid default_workflow_id: %v", err)
	}
	g, err := h.store.SetDefaults(ctx, id, tplID, wfID, req.GetDefaultTemplateNone())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set defaults: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "category.defaults_set",
			Subject:     "category:" + g.ID.String(),
			GroupID:     g.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.SetCategoryDefaultsResponse{Category: categoryToProto(g)}, nil
}

// RenameCategory handles CategoryService.RenameCategory. The new slug
// re-derives the category code, which changes the derived number of every
// document in it.
func (h *CategoryHandler) RenameCategory(ctx context.Context, req *corev1.RenameCategoryRequest) (*corev1.RenameCategoryResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	name := strings.TrimSpace(req.GetName())
	slug := strings.TrimSpace(req.GetSlug())
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	if slug == "" {
		return nil, status.Error(codes.InvalidArgument, "slug is required")
	}

	prev, err := h.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "category not found")
		}
		logger.Ctx(ctx).Error(err, "get category failed", log.F("category_id", id.String()))
		return nil, status.Error(codes.Internal, "get category failed")
	}

	g, err := h.store.Rename(ctx, id, name, slug)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrCategoryNotFound):
			return nil, status.Errorf(codes.NotFound, "rename category: %v", err)
		case errors.Is(err, store.ErrCategorySlugConflict):
			return nil, status.Errorf(codes.FailedPrecondition, "rename category: %v", err)
		default:
			return nil, status.Errorf(codes.Internal, "rename category: %v", err)
		}
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "category.renamed",
			Subject: "category:" + g.ID.String(),
			GroupID: g.ID.String(),
			Attributes: map[string]string{
				"from_name": prev.Name,
				"to_name":   g.Name,
				"from_slug": prev.Slug,
				"to_slug":   g.Slug,
			},
		})
	}
	return &corev1.RenameCategoryResponse{Category: categoryToProto(g)}, nil
}

// DeleteCategory handles CategoryService.DeleteCategory. A subtree that holds
// documents is refused.
func (h *CategoryHandler) DeleteCategory(ctx context.Context, req *corev1.DeleteCategoryRequest) (*corev1.DeleteCategoryResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	if err := h.store.Delete(ctx, id); err != nil {
		switch {
		case errors.Is(err, store.ErrCategoryHasPolicies):
			// The message names the category, not its id, so it can be shown
			// as it is.
			name := id.String()
			if g, gerr := h.store.Get(ctx, id); gerr == nil && g.Name != "" {
				name = g.Name
			}
			return nil, errcodes.Error(ctx, errcodes.CategoryNotDeletable(name, err))
		case errors.Is(err, store.ErrCategoryNotFound):
			return nil, status.Errorf(codes.NotFound, "delete category: %v", err)
		default:
			return nil, status.Errorf(codes.Internal, "delete category: %v", err)
		}
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "category.deleted",
			Subject: "category:" + id.String(),
			GroupID: id.String(),
		})
	}
	return &corev1.DeleteCategoryResponse{Deleted: true}, nil
}

// MoveCategory handles CategoryService.MoveCategory. A cycle is
// InvalidArgument, too deep is FailedPrecondition, and nothing is applied on
// either.
func (h *CategoryHandler) MoveCategory(ctx context.Context, req *corev1.MoveCategoryRequest) (*corev1.MoveCategoryResponse, error) {
	id, err := uuid.Parse(req.GetCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid category_id: %v", err)
	}
	newParent, err := parseOptionalUUID(req.GetNewParentId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid new_parent_id: %v", err)
	}
	var newParentPtr *uuid.UUID
	if newParent != uuid.Nil {
		newParentPtr = &newParent
	}
	if newParentPtr != nil && *newParentPtr == id {
		return nil, status.Errorf(codes.InvalidArgument, "cannot move a category under itself")
	}

	g, affected, err := h.store.MoveCategory(ctx, id, newParentPtr)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrCategoryNotFound):
			return nil, status.Errorf(codes.NotFound, "move category: %v", err)
		case errors.Is(err, store.ErrCategoryMoveCycle):
			return nil, status.Errorf(codes.InvalidArgument, "move category: %v", err)
		case errors.Is(err, store.ErrCategoryMoveTooDeep):
			return nil, status.Errorf(codes.FailedPrecondition, "move category: %v", err)
		case errors.Is(err, store.ErrCategorySlugConflict):
			return nil, status.Errorf(codes.FailedPrecondition, "move category: %v", err)
		default:
			return nil, status.Errorf(codes.Internal, "move category: %v", err)
		}
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier:    audit.TierAudit,
			Action:  "category.moved",
			Subject: "category:" + g.ID.String(),
			GroupID: g.ID.String(),
			Attributes: map[string]string{
				"new_parent_id": nilableUUID(g.ParentID),
			},
		})
	}
	// The subtree now inherits from a new parent.
	h.emitObligationChangedForCategory(ctx, g.ID)
	return &corev1.MoveCategoryResponse{
		Category:      categoryToProto(g),
		AffectedCount: toUint32(affected),
	}, nil
}

// GetEffectiveGovernance handles CategoryService.GetEffectiveGovernance:
// the nearest category in the ancestor chain that sets a value wins.
func (h *CategoryHandler) GetEffectiveGovernance(ctx context.Context, req *corev1.GetEffectiveGovernanceRequest) (*corev1.GetEffectiveGovernanceResponse, error) {
	id, err := uuid.Parse(req.GetCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid category_id: %v", err)
	}
	chain, err := h.store.AncestorChain(ctx, id)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "ancestor chain: %v", err)
	}
	audience := domain.EffectiveAudienceGroups(chain)
	exclusion := domain.EffectiveExclusionGroups(chain)
	ackEveryone := domain.EffectiveAckEveryone(chain)
	resp := &corev1.GetEffectiveGovernanceResponse{
		AudienceSet:    audience != nil,
		ExclusionSet:   exclusion != nil,
		AckEveryoneSet: ackEveryone != nil,
	}
	if audience != nil {
		resp.AckAudienceGroups = audience
	}
	if exclusion != nil {
		resp.ExclusionGroups = exclusion
	}
	if ackEveryone != nil {
		resp.AckEveryone = *ackEveryone
	}
	return resp, nil
}

func grantFromProto(e corev1.GrantEffect) stewardauthz.Grant {
	switch e {
	case corev1.GrantEffect_GRANT_EFFECT_ALLOW:
		return stewardauthz.GrantAllow
	case corev1.GrantEffect_GRANT_EFFECT_DENY:
		return stewardauthz.GrantDeny
	default:
		return stewardauthz.GrantBlank
	}
}

func grantToProto(g stewardauthz.Grant) corev1.GrantEffect {
	switch g {
	case stewardauthz.GrantAllow:
		return corev1.GrantEffect_GRANT_EFFECT_ALLOW
	case stewardauthz.GrantDeny:
		return corev1.GrantEffect_GRANT_EFFECT_DENY
	default:
		return corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED
	}
}

func subjKindFromProto(k corev1.RuleSubjectKind) (stewardauthz.SubjectKind, error) {
	switch k {
	case corev1.RuleSubjectKind_RULE_SUBJECT_KIND_EVERYONE:
		return stewardauthz.SubjectEveryone, nil
	case corev1.RuleSubjectKind_RULE_SUBJECT_KIND_GROUP:
		return stewardauthz.SubjectGroup, nil
	case corev1.RuleSubjectKind_RULE_SUBJECT_KIND_USER:
		return stewardauthz.SubjectUser, nil
	default:
		return "", fmt.Errorf("invalid subject_kind %v", k)
	}
}

func subjKindToProto(k stewardauthz.SubjectKind) corev1.RuleSubjectKind {
	switch k {
	case stewardauthz.SubjectEveryone:
		return corev1.RuleSubjectKind_RULE_SUBJECT_KIND_EVERYONE
	case stewardauthz.SubjectGroup:
		return corev1.RuleSubjectKind_RULE_SUBJECT_KIND_GROUP
	case stewardauthz.SubjectUser:
		return corev1.RuleSubjectKind_RULE_SUBJECT_KIND_USER
	default:
		return corev1.RuleSubjectKind_RULE_SUBJECT_KIND_UNSPECIFIED
	}
}

func categoryRuleFromProto(p *corev1.CategoryRule) (domain.CategoryRule, error) {
	kind, err := subjKindFromProto(p.GetSubjectKind())
	if err != nil {
		return domain.CategoryRule{}, err
	}
	return domain.CategoryRule{
		SubjectKind: kind,
		SubjectRef:  p.GetSubjectRef(),
		Read:        grantFromProto(p.GetRead()),
		Ack:         grantFromProto(p.GetAck()),
		Approve:     grantFromProto(p.GetApprove()),
		Author:      grantFromProto(p.GetAuthor()),
	}, nil
}

func categoryRuleToProto(r domain.CategoryRule) *corev1.CategoryRule {
	return &corev1.CategoryRule{
		SubjectKind: subjKindToProto(r.SubjectKind),
		SubjectRef:  r.SubjectRef,
		Read:        grantToProto(r.Read),
		Ack:         grantToProto(r.Ack),
		Approve:     grantToProto(r.Approve),
		Author:      grantToProto(r.Author),
	}
}

// GetCategoryRuleset handles CategoryService.GetCategoryRuleset.
func (h *CategoryHandler) GetCategoryRuleset(ctx context.Context, req *corev1.GetCategoryRulesetRequest) (*corev1.GetCategoryRulesetResponse, error) {
	catID, err := uuid.Parse(req.GetCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid category_id: %v", err)
	}
	rules, err := h.store.GetCategoryRuleset(ctx, catID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get category ruleset: %v", err)
	}
	out := make([]*corev1.CategoryRule, 0, len(rules))
	for _, r := range rules {
		out = append(out, categoryRuleToProto(r))
	}
	return &corev1.GetCategoryRulesetResponse{Rules: out}, nil
}

// SetCategoryRuleset handles CategoryService.SetCategoryRuleset: it replaces
// the rules and emits one audit event per rule created, updated or deleted.
// The API only replaces whole rulesets, so the events come from diffing the
// rules before and after; if the before can't be read nothing is changed,
// rather than making an unaudited change.
func (h *CategoryHandler) SetCategoryRuleset(ctx context.Context, req *corev1.SetCategoryRulesetRequest) (*corev1.SetCategoryRulesetResponse, error) {
	catID, err := uuid.Parse(req.GetCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid category_id: %v", err)
	}
	domainRules := make([]domain.CategoryRule, 0, len(req.GetRules()))
	for _, p := range req.GetRules() {
		r, err := categoryRuleFromProto(p)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid rule: %v", err)
		}
		domainRules = append(domainRules, r)
	}
	name := catID.String()
	if g, gerr := h.store.Get(ctx, catID); gerr == nil && g.Name != "" {
		name = g.Name
	}
	if err := domain.ValidateCategoryRules(name, domainRules); err != nil {
		return nil, errcodes.Error(ctx, errcodes.InvalidCategoryRule(err))
	}

	var before []domain.CategoryRule
	if h.auditor != nil {
		before, err = h.store.GetCategoryRuleset(ctx, catID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "get category ruleset: %v", err)
		}
	}

	saved, err := h.store.SetCategoryRuleset(ctx, catID, domainRules)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set category ruleset: %v", err)
	}

	h.emitCategoryRuleAudit(ctx, catID, before, saved)

	out := make([]*corev1.CategoryRule, 0, len(saved))
	for _, r := range saved {
		out = append(out, categoryRuleToProto(r))
	}
	return &corev1.SetCategoryRulesetResponse{Rules: out}, nil
}

// ruleSubject is a rule's identity across a replace, which re-inserts every
// row and so changes every row id.
type ruleSubject struct {
	kind stewardauthz.SubjectKind
	ref  string
}

// grantAudit renders an empty grant as "unset", so an audit row tells "no
// grant" apart from "not recorded".
func grantAudit(g stewardauthz.Grant) string {
	s := string(g)
	if s == "" {
		return "unset"
	}
	return s
}

// ruleGrantsEqual compares grants and ordinal: order decides which rule
// wins, so a reorder changes access and is audited as an update.
func ruleGrantsEqual(a, b domain.CategoryRule) bool {
	return a.Read == b.Read &&
		a.Ack == b.Ack &&
		a.Approve == b.Approve &&
		a.Author == b.Author &&
		a.Ordinal == b.Ordinal
}

func categoryRuleAttrs(catID uuid.UUID, r domain.CategoryRule) map[string]string {
	return map[string]string{
		"category_id":  catID.String(),
		"subject_kind": string(r.SubjectKind),
		"subject_ref":  r.SubjectRef,
		"ordinal":      strconv.Itoa(r.Ordinal),
		"read":         grantAudit(r.Read),
		"ack":          grantAudit(r.Ack),
		"approve":      grantAudit(r.Approve),
		"author":       grantAudit(r.Author),
	}
}

// emitCategoryRuleAudit emits one audit event per rule a replace created,
// updated or deleted. Rules are matched by subject, and a subject listed more
// than once is matched pairwise in order. No change, no event. The request has
// no actor_user_id, so the actor is the forwarded one; during act-as the
// emitter credits the admin.
func (h *CategoryHandler) emitCategoryRuleAudit(ctx context.Context, catID uuid.UUID, before, after []domain.CategoryRule) {
	if h.auditor == nil {
		return
	}

	actor := actorFromContext(ctx)
	subject := "category:" + catID.String()

	category := func(rules []domain.CategoryRule) map[ruleSubject][]domain.CategoryRule {
		m := make(map[ruleSubject][]domain.CategoryRule, len(rules))
		for _, r := range rules {
			k := ruleSubject{kind: r.SubjectKind, ref: r.SubjectRef}
			m[k] = append(m[k], r)
		}
		return m
	}
	oldBy, newBy := category(before), category(after)

	emit := func(action string, attrs map[string]string) {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier:        audit.TierAudit,
			Action:      action,
			ActorUserID: actor,
			Subject:     subject,
			GroupID:     catID.String(),
			Attributes:  attrs,
		})
	}

	// A stable order for a multi-rule change.
	keys := make([]ruleSubject, 0, len(oldBy)+len(newBy))
	for k := range newBy {
		keys = append(keys, k)
	}
	for k := range oldBy {
		if _, ok := newBy[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].kind != keys[j].kind {
			return keys[i].kind < keys[j].kind
		}
		return keys[i].ref < keys[j].ref
	})

	for _, k := range keys {
		olds, news := oldBy[k], newBy[k]
		for i := 0; i < len(olds) || i < len(news); i++ {
			switch {
			case i >= len(olds):
				emit("category_rule.created", categoryRuleAttrs(catID, news[i]))
			case i >= len(news):
				emit("category_rule.deleted", categoryRuleAttrs(catID, olds[i]))
			case !ruleGrantsEqual(olds[i], news[i]):
				attrs := categoryRuleAttrs(catID, news[i])
				attrs["prev_ordinal"] = strconv.Itoa(olds[i].Ordinal)
				attrs["prev_read"] = grantAudit(olds[i].Read)
				attrs["prev_ack"] = grantAudit(olds[i].Ack)
				attrs["prev_approve"] = grantAudit(olds[i].Approve)
				attrs["prev_author"] = grantAudit(olds[i].Author)
				emit("category_rule.updated", attrs)
			}
		}
	}
}

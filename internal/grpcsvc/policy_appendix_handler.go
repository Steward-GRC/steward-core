// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"strconv"
	"strings"

	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// policyAppendixBackend is the part of *store.PolicyAppendixStore the handler uses.
type policyAppendixBackend interface {
	List(ctx context.Context, policyID uuid.UUID, includeRetired bool) ([]domain.PolicyAppendix, error)
	Get(ctx context.Context, id uuid.UUID) (domain.PolicyAppendix, error)
	Create(ctx context.Context, policyID, actor uuid.UUID, title, contentJSON string) (domain.PolicyAppendix, error)
	SaveDraft(ctx context.Context, id, actor uuid.UUID, title, contentJSON string) (domain.PolicyAppendix, bool, error)
	Publish(ctx context.Context, id, actor uuid.UUID) (domain.PolicyAppendix, error)
	Retire(ctx context.Context, id, actor uuid.UUID) (domain.PolicyAppendix, bool, error)
	Restore(ctx context.Context, id uuid.UUID) (domain.PolicyAppendix, bool, error)
	ReorderAppendices(ctx context.Context, policyID uuid.UUID, orderedIDs []uuid.UUID) ([]domain.PolicyAppendix, error)
}

// PolicyAppendixHandler implements corev1.PolicyAppendixServiceServer: the
// appendices a policy or procedure owns, each with its own version table.
// Core applies no role or reader rule here: it returns the published row and
// the draft side by side, and the gateway decides what a caller may see and do.
type PolicyAppendixHandler struct {
	corev1.UnimplementedPolicyAppendixServiceServer
	store   policyAppendixBackend
	auditor auditEmitter
}

// NewPolicyAppendixHandler returns a handler over the given backend. A nil
// auditor skips the audit events.
func NewPolicyAppendixHandler(s policyAppendixBackend, auditor auditEmitter) *PolicyAppendixHandler {
	return &PolicyAppendixHandler{store: s, auditor: auditor}
}

func appendixStateToProto(s domain.AppendixState) corev1.AppendixVersionState {
	switch s {
	case domain.AppendixDraft:
		return corev1.AppendixVersionState_APPENDIX_VERSION_STATE_DRAFT
	case domain.AppendixPublished:
		return corev1.AppendixVersionState_APPENDIX_VERSION_STATE_PUBLISHED
	default:
		return corev1.AppendixVersionState_APPENDIX_VERSION_STATE_UNSPECIFIED
	}
}

func idStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

func appendixVersionToProto(v *domain.PolicyAppendixVersion) *corev1.AppendixVersion {
	if v == nil {
		return nil
	}
	out := &corev1.AppendixVersion{
		Id: v.ID.String(), VersionNo: toInt32(v.VersionNo), State: appendixStateToProto(v.State),
		Title: v.Title, ContentJson: v.ContentJSON,
		CreatedAt: timestamppb.New(v.CreatedAt), CreatedBy: v.CreatedBy.String(),
		UpdatedAt: timestamppb.New(v.UpdatedAt), UpdatedBy: v.UpdatedBy.String(),
		ContributorIds: idStrings(v.Editors),
	}
	if v.PublishedAt != nil {
		out.PublishedAt = timestamppb.New(*v.PublishedAt)
	}
	if v.PublishedBy != nil {
		out.PublishedBy = v.PublishedBy.String()
	}
	return out
}

func policyAppendixToProto(a domain.PolicyAppendix) *corev1.PolicyAppendix {
	out := &corev1.PolicyAppendix{
		Id: a.ID.String(), PolicyId: a.PolicyID.String(), Letter: a.Letter, OrderIndex: toInt32(a.OrderIndex),
		Published: appendixVersionToProto(a.Published()), Draft: appendixVersionToProto(a.Draft()),
		ContributorIds: idStrings(a.ContributorIDs()),
	}
	if a.RetiredAt != nil {
		out.RetiredAt = timestamppb.New(*a.RetiredAt)
	}
	for _, v := range a.Versions {
		out.Versions = append(out.Versions, &corev1.AppendixVersionInfo{
			VersionNo: toInt32(v.VersionNo), State: appendixStateToProto(v.State), ContributorIds: idStrings(v.Editors),
		})
	}
	for _, v := range a.PublishedVersions() {
		out.PublishedVersions = append(out.PublishedVersions, appendixVersionToProto(&v))
	}
	return out
}

// policyAppendixErr turns a store error into the gRPC error. Store faults are
// coded and only logged, never sent.
func policyAppendixErr(ctx context.Context, op string, err error) error {
	l := logger.Ctx(ctx)
	switch {
	case errors.Is(err, store.ErrPolicyAppendixNotFound), errors.Is(err, store.ErrAppendixPolicyNotFound):
		l.Warn("policy appendix op refused: not found", log.F("op", op), log.F("reason", err.Error()))
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, store.ErrAppendixReorderMismatch):
		l.Warn("policy appendix op refused: bad order list", log.F("op", op), log.F("reason", err.Error()))
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, store.ErrAppendixNoDraft), errors.Is(err, store.ErrAppendixRetired):
		l.Warn("policy appendix op refused: wrong state", log.F("op", op), log.F("reason", err.Error()))
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		l.Error(err, "policy appendix store fault", log.F("op", op))
		return storeUnavailable(ctx, op, err)
	}
}

func parseAppendixActor(s string) (uuid.UUID, error) {
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	return id, nil
}

// audited records one appendix write in the audit tier: ids only, never a
// title or text. A failed emit is logged and does not undo the write.
func (h *PolicyAppendixHandler) audited(ctx context.Context, action, actor string, a domain.PolicyAppendix) {
	if h.auditor == nil {
		return
	}
	attrs := map[string]string{"policy_id": a.PolicyID.String(), "appendix_id": a.ID.String()}
	if p := a.Published(); p != nil {
		attrs["published_version_no"] = strconv.Itoa(p.VersionNo)
	}
	if d := a.Draft(); d != nil {
		attrs["draft_version_no"] = strconv.Itoa(d.VersionNo)
	}
	err := h.auditor.Emit(ctx, audit.Event{
		Tier: audit.TierAudit, Action: action, ActorUserID: actor, Subject: "policy_appendix:" + a.ID.String(),
		Attributes: attrs,
	})
	if err != nil {
		logger.Ctx(ctx).Error(err, "policy appendix audit failed", log.F("action", action), log.F("appendix_id", a.ID.String()))
	}
}

// ListPolicyAppendices handles PolicyAppendixService.ListPolicyAppendices.
func (h *PolicyAppendixHandler) ListPolicyAppendices(ctx context.Context, req *corev1.ListPolicyAppendicesRequest) (*corev1.ListPolicyAppendicesResponse, error) {
	l := logger.Ctx(ctx)
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		l.Debug("list policy appendices refused: bad policy_id")
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	l.Debug("list policy appendices", log.F("policy_id", pid.String()), log.F("include_retired", req.GetIncludeRetired()))
	list, err := h.store.List(ctx, pid, req.GetIncludeRetired())
	if err != nil {
		return nil, policyAppendixErr(ctx, "list_policy_appendices", err)
	}
	out := make([]*corev1.PolicyAppendix, len(list))
	for i, a := range list {
		out[i] = policyAppendixToProto(a)
	}
	l.Debug("list policy appendices done", log.F("policy_id", pid.String()), log.F("count", len(out)))
	return &corev1.ListPolicyAppendicesResponse{Appendices: out}, nil
}

// CreatePolicyAppendix handles PolicyAppendixService.CreatePolicyAppendix.
func (h *PolicyAppendixHandler) CreatePolicyAppendix(ctx context.Context, req *corev1.CreatePolicyAppendixRequest) (*corev1.CreatePolicyAppendixResponse, error) {
	l := logger.Ctx(ctx)
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	actor, err := parseAppendixActor(req.GetActorUserId())
	if err != nil {
		return nil, err
	}
	if err := domain.ValidateAppendixContent(req.GetContentJson()); err != nil {
		l.Debug("create policy appendix refused: bad content", log.F("policy_id", pid.String()))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	l.Debug("create policy appendix", log.F("policy_id", pid.String()), log.F("actor", actor.String()),
		log.F("title_given", strings.TrimSpace(req.GetTitle()) != ""))
	a, err := h.store.Create(ctx, pid, actor, req.GetTitle(), req.GetContentJson())
	if err != nil {
		return nil, policyAppendixErr(ctx, "create_policy_appendix", err)
	}
	l.Info("policy appendix created", log.F("policy_id", pid.String()), log.F("appendix_id", a.ID.String()), log.F("letter", a.Letter))
	h.audited(ctx, "appendix.created", req.GetActorUserId(), a)
	return &corev1.CreatePolicyAppendixResponse{Appendix: policyAppendixToProto(a)}, nil
}

// SaveAppendixDraft handles PolicyAppendixService.SaveAppendixDraft. A save
// that changes nothing writes no audit event.
func (h *PolicyAppendixHandler) SaveAppendixDraft(ctx context.Context, req *corev1.SaveAppendixDraftRequest) (*corev1.SaveAppendixDraftResponse, error) {
	l := logger.Ctx(ctx)
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	actor, err := parseAppendixActor(req.GetActorUserId())
	if err != nil {
		return nil, err
	}
	title := strings.TrimSpace(req.GetTitle())
	if title == "" {
		l.Debug("save appendix draft refused: blank title", log.F("appendix_id", id.String()))
		return nil, status.Error(codes.InvalidArgument, "an appendix needs a title")
	}
	if err := domain.ValidateAppendixContent(req.GetContentJson()); err != nil {
		l.Debug("save appendix draft refused: bad content", log.F("appendix_id", id.String()))
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	l.Debug("save appendix draft", log.F("appendix_id", id.String()), log.F("actor", actor.String()))
	a, changed, err := h.store.SaveDraft(ctx, id, actor, title, req.GetContentJson())
	if err != nil {
		return nil, policyAppendixErr(ctx, "save_appendix_draft", err)
	}
	if !changed {
		l.Debug("save appendix draft: nothing changed, no draft made", log.F("appendix_id", id.String()))
		return &corev1.SaveAppendixDraftResponse{Appendix: policyAppendixToProto(a)}, nil
	}
	l.Debug("appendix draft saved", log.F("appendix_id", id.String()), log.F("draft_version_no", a.Draft().VersionNo))
	h.audited(ctx, "appendix.saved", req.GetActorUserId(), a)
	return &corev1.SaveAppendixDraftResponse{Appendix: policyAppendixToProto(a)}, nil
}

// PublishAppendix handles PolicyAppendixService.PublishAppendix.
func (h *PolicyAppendixHandler) PublishAppendix(ctx context.Context, req *corev1.PublishAppendixRequest) (*corev1.PublishAppendixResponse, error) {
	l := logger.Ctx(ctx)
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	actor, err := parseAppendixActor(req.GetActorUserId())
	if err != nil {
		return nil, err
	}
	l.Debug("publish appendix", log.F("appendix_id", id.String()), log.F("actor", actor.String()))
	a, err := h.store.Publish(ctx, id, actor)
	if err != nil {
		return nil, policyAppendixErr(ctx, "publish_appendix", err)
	}
	l.Info("appendix published", log.F("appendix_id", id.String()), log.F("version_no", a.Published().VersionNo))
	h.audited(ctx, "appendix.published", req.GetActorUserId(), a)
	return &corev1.PublishAppendixResponse{Appendix: policyAppendixToProto(a)}, nil
}

// RetireAppendix handles PolicyAppendixService.RetireAppendix.
func (h *PolicyAppendixHandler) RetireAppendix(ctx context.Context, req *corev1.RetireAppendixRequest) (*corev1.RetireAppendixResponse, error) {
	l := logger.Ctx(ctx)
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	actor, err := parseAppendixActor(req.GetActorUserId())
	if err != nil {
		return nil, err
	}
	l.Debug("retire appendix", log.F("appendix_id", id.String()), log.F("actor", actor.String()))
	a, changed, err := h.store.Retire(ctx, id, actor)
	if err != nil {
		return nil, policyAppendixErr(ctx, "retire_appendix", err)
	}
	if changed {
		l.Info("appendix retired", log.F("appendix_id", id.String()))
		h.audited(ctx, "appendix.retired", req.GetActorUserId(), a)
	} else {
		l.Debug("retire appendix: already retired", log.F("appendix_id", id.String()))
	}
	return &corev1.RetireAppendixResponse{Appendix: policyAppendixToProto(a)}, nil
}

// RestoreAppendix handles PolicyAppendixService.RestoreAppendix.
func (h *PolicyAppendixHandler) RestoreAppendix(ctx context.Context, req *corev1.RestoreAppendixRequest) (*corev1.RestoreAppendixResponse, error) {
	l := logger.Ctx(ctx)
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	actor, err := parseAppendixActor(req.GetActorUserId())
	if err != nil {
		return nil, err
	}
	l.Debug("restore appendix", log.F("appendix_id", id.String()), log.F("actor", actor.String()))
	a, changed, err := h.store.Restore(ctx, id)
	if err != nil {
		return nil, policyAppendixErr(ctx, "restore_appendix", err)
	}
	if changed {
		l.Info("appendix restored", log.F("appendix_id", id.String()), log.F("letter", a.Letter))
		h.audited(ctx, "appendix.restored", req.GetActorUserId(), a)
	} else {
		l.Debug("restore appendix: already live", log.F("appendix_id", id.String()))
	}
	return &corev1.RestoreAppendixResponse{Appendix: policyAppendixToProto(a)}, nil
}

// ReorderPolicyAppendices handles PolicyAppendixService.ReorderPolicyAppendices.
// A list that is not every live appendix of the policy exactly once is
// InvalidArgument and nothing changes. It makes no draft and touches no
// version row.
func (h *PolicyAppendixHandler) ReorderPolicyAppendices(ctx context.Context, req *corev1.ReorderPolicyAppendicesRequest) (*corev1.ReorderPolicyAppendicesResponse, error) {
	l := logger.Ctx(ctx)
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		l.Debug("reorder policy appendices refused: bad policy_id")
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	actor, err := parseAppendixActor(req.GetActorUserId())
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(req.GetOrderedIds()))
	for i, s := range req.GetOrderedIds() {
		id, err := uuid.Parse(s)
		if err != nil {
			l.Debug("reorder policy appendices refused: bad ordered id", log.F("policy_id", pid.String()), log.F("index", i))
			return nil, status.Errorf(codes.InvalidArgument, "ordered_ids[%d]: %v", i, err)
		}
		ids[i] = id
	}
	l.Debug("reorder policy appendices", log.F("policy_id", pid.String()), log.F("actor", actor.String()), log.F("count", len(ids)))
	list, err := h.store.ReorderAppendices(ctx, pid, ids)
	if err != nil {
		return nil, policyAppendixErr(ctx, "reorder_policy_appendices", err)
	}
	l.Info("policy appendices reordered", log.F("policy_id", pid.String()), log.F("count", len(list)))
	if h.auditor != nil {
		err := h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "appendix.reordered", ActorUserID: req.GetActorUserId(),
			Subject:    "policy:" + pid.String(),
			Attributes: map[string]string{"policy_id": pid.String(), "appendix_count": strconv.Itoa(len(list))},
		})
		if err != nil {
			l.Error(err, "policy appendix audit failed", log.F("action", "appendix.reordered"), log.F("policy_id", pid.String()))
		}
	}
	out := make([]*corev1.PolicyAppendix, len(list))
	for i, a := range list {
		out[i] = policyAppendixToProto(a)
	}
	return &corev1.ReorderPolicyAppendicesResponse{Appendices: out}, nil
}

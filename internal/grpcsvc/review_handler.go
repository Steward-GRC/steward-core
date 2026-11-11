// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"

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

// reviewBackend is the part of *store.ReviewStore the handler uses.
type reviewBackend interface {
	CreateThread(ctx context.Context, t domain.ReviewThread) (domain.ReviewThread, error)
	GetThread(ctx context.Context, id uuid.UUID) (domain.ReviewThread, error)
	ListThreads(ctx context.Context, versionID, approver uuid.UUID) ([]domain.ReviewThread, error)
	AddMessage(ctx context.Context, threadID uuid.UUID, m domain.ReviewMessage) (domain.ReviewThread, error)
	SetResolved(ctx context.Context, id uuid.UUID, resolved bool) (domain.ReviewThread, error)
	DeleteThread(ctx context.Context, id uuid.UUID) error
}

// ReviewHandler implements corev1.ReviewServiceServer.
type ReviewHandler struct {
	corev1.UnimplementedReviewServiceServer
	store   reviewBackend
	auditor auditEmitter
	limits  domain.ReviewLimits
}

// NewReviewHandler returns a handler over the given backend.
func NewReviewHandler(s reviewBackend, auditor auditEmitter, limits domain.ReviewLimits) *ReviewHandler {
	return &ReviewHandler{store: s, auditor: auditor, limits: limits}
}

func reviewMessageToProto(m domain.ReviewMessage) *corev1.ReviewMessage {
	return &corev1.ReviewMessage{
		Id: m.ID.String(), AuthorUserId: m.AuthorUserID.String(), Body: m.Body, CreatedAt: timestamppb.New(m.CreatedAt),
	}
}

func reviewThreadToProto(t domain.ReviewThread) *corev1.ReviewThread {
	msgs := make([]*corev1.ReviewMessage, len(t.Messages))
	for i, m := range t.Messages {
		msgs[i] = reviewMessageToProto(m)
	}
	return &corev1.ReviewThread{
		Id: t.ID.String(), PolicyVersionId: t.PolicyVersionID.String(), Quote: t.Quote, Occurrence: int32(t.Occurrence),
		Resolved: t.Resolved, CreatedAt: timestamppb.New(t.CreatedAt), ApproverUserId: t.ApproverUserID.String(),
		CreatedBy: t.CreatedBy.String(), Messages: msgs,
	}
}

func reviewSideFromProto(s corev1.ReviewSide) domain.ReviewSide {
	switch s {
	case corev1.ReviewSide_REVIEW_SIDE_AUTHOR:
		return domain.ReviewSideAuthor
	case corev1.ReviewSide_REVIEW_SIDE_REVIEWER:
		return domain.ReviewSideReviewer
	default:
		return domain.ReviewSideUnspecified
	}
}

// reviewErr turns a rule or store error into the gRPC error. The text of a
// comment is never in an error message; store faults are coded and only logged.
func reviewErr(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(err, domain.ErrReviewBlankBody), errors.Is(err, domain.ErrReviewBodyTooLong),
		errors.Is(err, domain.ErrReviewBlankQuote), errors.Is(err, domain.ErrReviewQuoteTooLong),
		errors.Is(err, domain.ErrReviewBadOccurrence), errors.Is(err, domain.ErrReviewNoApprover),
		errors.Is(err, domain.ErrReviewBadSide):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, domain.ErrReviewNotApprover), errors.Is(err, domain.ErrReviewNotStarter):
		return status.Error(codes.PermissionDenied, err.Error())
	case errors.Is(err, store.ErrReviewThreadNotFound), errors.Is(err, store.ErrReviewVersionNotFound):
		return status.Error(codes.NotFound, err.Error())
	default:
		return storeUnavailable(ctx, op, err)
	}
}

// audited records one thread event in the activity tier: ids and the outcome
// only, never a comment or the quoted text. A failed emit is logged and does
// not undo the write.
func (h *ReviewHandler) audited(ctx context.Context, action, actor string, side corev1.ReviewSide, t domain.ReviewThread) {
	if h.auditor == nil {
		return
	}
	err := h.auditor.Emit(ctx, audit.Event{
		Tier: audit.TierActivity, Action: action, ActorUserID: actor, Subject: "review_thread:" + t.ID.String(),
		Attributes: map[string]string{
			"thread_id": t.ID.String(), "policy_version_id": t.PolicyVersionID.String(),
			"approver_user_id": t.ApproverUserID.String(), "actor_side": side.String(), "outcome": "ok",
		},
	})
	if err != nil {
		logger.Ctx(ctx).Error(err, "review audit failed", log.F("action", action), log.F("thread_id", t.ID.String()))
	}
}

// actorAndSide reads the gateway-supplied actor and side.
func actorAndSide(actorID string, side corev1.ReviewSide) (uuid.UUID, domain.ReviewSide, error) {
	actor, err := uuid.Parse(actorID)
	if err != nil {
		return uuid.Nil, 0, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	ds := reviewSideFromProto(side)
	if ds == domain.ReviewSideUnspecified {
		return uuid.Nil, 0, status.Error(codes.InvalidArgument, domain.ErrReviewBadSide.Error())
	}
	return actor, ds, nil
}

// AddReviewThread handles ReviewService.AddReviewThread.
func (h *ReviewHandler) AddReviewThread(ctx context.Context, req *corev1.AddReviewThreadRequest) (*corev1.AddReviewThreadResponse, error) {
	l := logger.Ctx(ctx)
	actor, side, err := actorAndSide(req.GetActorUserId(), req.GetActorSide())
	if err != nil {
		l.Debug("add review thread refused: bad actor or side", log.F("side", req.GetActorSide().String()))
		return nil, err
	}
	versionID, err := uuid.Parse(req.GetPolicyVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_version_id: %v", err)
	}
	approver, err := parseOptionalUUID(req.GetApproverUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid approver_user_id: %v", err)
	}
	l.Debug("add review thread", log.F("policy_version_id", versionID.String()), log.F("actor", actor.String()), log.F("side", side),
		log.F("approver", approver.String()), log.F("occurrence", req.GetOccurrence()))
	t, err := domain.NewReviewThread(h.limits, side, actor, versionID, approver, req.GetQuote(), int(req.GetOccurrence()), req.GetBody())
	if err != nil {
		l.Debug("add review thread refused", log.F("policy_version_id", versionID.String()), log.F("reason", err.Error()))
		return nil, reviewErr(ctx, "add_review_thread", err)
	}
	saved, err := h.store.CreateThread(ctx, t)
	if err != nil {
		l.Error(err, "add review thread failed", log.F("policy_version_id", versionID.String()))
		return nil, reviewErr(ctx, "add_review_thread", err)
	}
	l.Info("review thread created", log.F("thread_id", saved.ID.String()), log.F("policy_version_id", versionID.String()),
		log.F("approver", approver.String()), log.F("side", side))
	h.audited(ctx, "review.thread.created", req.GetActorUserId(), req.GetActorSide(), saved)
	return &corev1.AddReviewThreadResponse{Thread: reviewThreadToProto(saved)}, nil
}

// ListReviewThreads handles ReviewService.ListReviewThreads. An empty
// approver_user_id lists every thread of the version.
func (h *ReviewHandler) ListReviewThreads(ctx context.Context, req *corev1.ListReviewThreadsRequest) (*corev1.ListReviewThreadsResponse, error) {
	l := logger.Ctx(ctx)
	versionID, err := uuid.Parse(req.GetPolicyVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_version_id: %v", err)
	}
	approver, err := parseOptionalUUID(req.GetApproverUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid approver_user_id: %v", err)
	}
	l.Debug("list review threads", log.F("policy_version_id", versionID.String()), log.F("approver", approver.String()))
	list, err := h.store.ListThreads(ctx, versionID, approver)
	if err != nil {
		l.Error(err, "list review threads failed", log.F("policy_version_id", versionID.String()))
		return nil, reviewErr(ctx, "list_review_threads", err)
	}
	out := make([]*corev1.ReviewThread, len(list))
	for i, t := range list {
		out[i] = reviewThreadToProto(t)
	}
	l.Debug("review threads listed", log.F("policy_version_id", versionID.String()), log.F("count", len(out)),
		log.F("filtered", approver != uuid.Nil))
	return &corev1.ListReviewThreadsResponse{Threads: out}, nil
}

// loadForAct reads a thread and checks that the actor may act on it.
func (h *ReviewHandler) loadForAct(ctx context.Context, op, threadID, actorID string, side corev1.ReviewSide, check func(domain.ReviewThread, domain.ReviewSide, uuid.UUID) error) (domain.ReviewThread, uuid.UUID, domain.ReviewSide, error) {
	l := logger.Ctx(ctx)
	actor, ds, err := actorAndSide(actorID, side)
	if err != nil {
		return domain.ReviewThread{}, uuid.Nil, 0, err
	}
	id, err := uuid.Parse(threadID)
	if err != nil {
		return domain.ReviewThread{}, uuid.Nil, 0, status.Errorf(codes.InvalidArgument, "invalid thread_id: %v", err)
	}
	t, err := h.store.GetThread(ctx, id)
	if err != nil {
		l.Debug(op+": thread not read", log.F("thread_id", id.String()), log.F("reason", err.Error()))
		return domain.ReviewThread{}, uuid.Nil, 0, reviewErr(ctx, op, err)
	}
	if err := check(t, ds, actor); err != nil {
		l.Warn(op+" refused", log.F("thread_id", id.String()), log.F("actor", actor.String()), log.F("side", ds), log.F("reason", err.Error()))
		return domain.ReviewThread{}, uuid.Nil, 0, reviewErr(ctx, op, err)
	}
	return t, actor, ds, nil
}

func canAct(t domain.ReviewThread, s domain.ReviewSide, a uuid.UUID) error { return t.CheckAct(s, a) }

func canDelete(t domain.ReviewThread, s domain.ReviewSide, a uuid.UUID) error {
	return t.CheckDelete(s, a)
}

// ReplyReviewThread handles ReviewService.ReplyReviewThread.
func (h *ReviewHandler) ReplyReviewThread(ctx context.Context, req *corev1.ReplyReviewThreadRequest) (*corev1.ReplyReviewThreadResponse, error) {
	l := logger.Ctx(ctx)
	t, actor, _, err := h.loadForAct(ctx, "reply_review_thread", req.GetThreadId(), req.GetActorUserId(), req.GetActorSide(), canAct)
	if err != nil {
		return nil, err
	}
	m, err := domain.NewReviewMessage(h.limits, actor, req.GetBody())
	if err != nil {
		l.Debug("reply refused", log.F("thread_id", t.ID.String()), log.F("reason", err.Error()))
		return nil, reviewErr(ctx, "reply_review_thread", err)
	}
	saved, err := h.store.AddMessage(ctx, t.ID, m)
	if err != nil {
		l.Error(err, "reply failed", log.F("thread_id", t.ID.String()))
		return nil, reviewErr(ctx, "reply_review_thread", err)
	}
	l.Info("review thread replied", log.F("thread_id", t.ID.String()), log.F("messages", len(saved.Messages)))
	h.audited(ctx, "review.thread.replied", req.GetActorUserId(), req.GetActorSide(), saved)
	return &corev1.ReplyReviewThreadResponse{Thread: reviewThreadToProto(saved)}, nil
}

// SetReviewThreadResolved handles ReviewService.SetReviewThreadResolved.
func (h *ReviewHandler) SetReviewThreadResolved(ctx context.Context, req *corev1.SetReviewThreadResolvedRequest) (*corev1.SetReviewThreadResolvedResponse, error) {
	l := logger.Ctx(ctx)
	t, _, _, err := h.loadForAct(ctx, "set_review_thread_resolved", req.GetThreadId(), req.GetActorUserId(), req.GetActorSide(), canAct)
	if err != nil {
		return nil, err
	}
	saved, err := h.store.SetResolved(ctx, t.ID, req.GetResolved())
	if err != nil {
		l.Error(err, "set review thread resolved failed", log.F("thread_id", t.ID.String()))
		return nil, reviewErr(ctx, "set_review_thread_resolved", err)
	}
	action := "review.thread.reopened"
	if req.GetResolved() {
		action = "review.thread.resolved"
	}
	l.Info("review thread state changed", log.F("thread_id", t.ID.String()), log.F("resolved", saved.Resolved), log.F("was_resolved", t.Resolved))
	h.audited(ctx, action, req.GetActorUserId(), req.GetActorSide(), saved)
	return &corev1.SetReviewThreadResolvedResponse{Thread: reviewThreadToProto(saved)}, nil
}

// DeleteReviewThread handles ReviewService.DeleteReviewThread. Only the user
// who started the thread may delete it.
func (h *ReviewHandler) DeleteReviewThread(ctx context.Context, req *corev1.DeleteReviewThreadRequest) (*corev1.DeleteReviewThreadResponse, error) {
	l := logger.Ctx(ctx)
	t, _, _, err := h.loadForAct(ctx, "delete_review_thread", req.GetThreadId(), req.GetActorUserId(), req.GetActorSide(), canDelete)
	if err != nil {
		return nil, err
	}
	if err := h.store.DeleteThread(ctx, t.ID); err != nil {
		l.Error(err, "delete review thread failed", log.F("thread_id", t.ID.String()))
		return nil, reviewErr(ctx, "delete_review_thread", err)
	}
	l.Info("review thread deleted", log.F("thread_id", t.ID.String()), log.F("policy_version_id", t.PolicyVersionID.String()))
	h.audited(ctx, "review.thread.deleted", req.GetActorUserId(), req.GetActorSide(), t)
	return &corev1.DeleteReviewThreadResponse{}, nil
}

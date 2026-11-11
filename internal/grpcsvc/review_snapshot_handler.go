// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"

	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
)

// reviewSnapshotBackend is the part of *store.ReviewSnapshotStore the handler uses.
type reviewSnapshotBackend interface {
	GetOrCreate(ctx context.Context, versionID, userID uuid.UUID) (domain.ReviewSnapshotView, error)
	Mark(ctx context.Context, versionID, userID uuid.UUID) error
}

// snapshotTemplates is the part of the template store the handler uses to read
// the sections of a version's pinned template version.
type snapshotTemplates interface {
	GetTemplateVersion(ctx context.Context, id uuid.UUID) (domain.TemplateVersion, error)
}

// ReviewSnapshotHandler implements corev1.ReviewSnapshotServiceServer. It takes
// the approver's user id as given: whether the caller holds a seat is the
// gateway's check, and the gateway calls MarkVersionReviewed after each act.
type ReviewSnapshotHandler struct {
	corev1.UnimplementedReviewSnapshotServiceServer
	store     reviewSnapshotBackend
	templates snapshotTemplates
	auditor   auditEmitter
}

// NewReviewSnapshotHandler returns a handler over the given backends. auditor
// may be nil.
func NewReviewSnapshotHandler(s reviewSnapshotBackend, t snapshotTemplates, auditor auditEmitter) *ReviewSnapshotHandler {
	return &ReviewSnapshotHandler{store: s, templates: t, auditor: auditor}
}

// snapshotIDs parses the version and approver ids of a request.
func snapshotIDs(versionID, userID string) (uuid.UUID, uuid.UUID, error) {
	v, err := uuid.Parse(versionID)
	if err != nil {
		return uuid.Nil, uuid.Nil, status.Errorf(codes.InvalidArgument, "policy_version_id: %v", err)
	}
	if userID == "" {
		return uuid.Nil, uuid.Nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	u, err := uuid.Parse(userID)
	if err != nil {
		return uuid.Nil, uuid.Nil, status.Errorf(codes.InvalidArgument, "user_id: %v", err)
	}
	return v, u, nil
}

// emitSnapshot records a snapshot event in the activity tier: the ids only,
// never the text.
func (h *ReviewSnapshotHandler) emitSnapshot(ctx context.Context, action string, versionID, userID uuid.UUID) {
	if h.auditor == nil {
		return
	}
	if err := h.auditor.Emit(ctx, audit.Event{
		Tier: audit.TierActivity, Action: action,
		ActorUserID: userID.String(),
		Subject:     "policy_version:" + versionID.String(),
	}); err != nil {
		logger.Ctx(ctx).Warn("review snapshot audit failed", log.F("action", action),
			log.F("policy_version_id", versionID.String()), log.F("error", err.Error()))
	}
}

// GetChangesSinceReview handles ReviewSnapshotService.GetChangesSinceReview.
func (h *ReviewSnapshotHandler) GetChangesSinceReview(ctx context.Context, req *corev1.GetChangesSinceReviewRequest) (*corev1.GetChangesSinceReviewResponse, error) {
	l := logger.Ctx(ctx)
	versionID, userID, err := snapshotIDs(req.GetPolicyVersionId(), req.GetUserId())
	if err != nil {
		l.Debug("changes since review refused", log.F("reason", "invalid id"))
		return nil, err
	}
	l.Debug("changes since review", log.F("policy_version_id", versionID.String()), log.F("user_id", userID.String()))

	snap, err := h.store.GetOrCreate(ctx, versionID, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			l.Debug("changes since review: version not found", log.F("policy_version_id", versionID.String()))
			return nil, status.Error(codes.NotFound, "policy version not found")
		}
		l.Error(err, "get review snapshot failed", log.F("policy_version_id", versionID.String()))
		return nil, status.Error(codes.Internal, "get review snapshot failed")
	}
	if snap.Created {
		l.Info("review snapshot taken", log.F("policy_version_id", versionID.String()), log.F("user_id", userID.String()))
		h.emitSnapshot(ctx, "review.snapshot.taken", versionID, userID)
	}

	// The pinned template version gives the sections; a version with none is
	// compared by the section keys its texts have.
	var defs []domain.Section
	if snap.TemplateVersionID != uuid.Nil {
		tv, err := h.templates.GetTemplateVersion(ctx, snap.TemplateVersionID)
		if err != nil {
			l.Error(err, "get template version failed", log.F("template_version_id", snap.TemplateVersionID.String()))
			return nil, status.Error(codes.Internal, "get template version failed")
		}
		defs = tv.Sections
		l.Debug("sections from the pinned template", log.F("sections", len(defs)))
	} else {
		defs, err = domain.FreeformSections(snap.Before, snap.Current)
		if err != nil {
			l.Error(err, "read freeform sections failed", log.F("policy_version_id", versionID.String()))
			return nil, status.Error(codes.Internal, "read sections failed")
		}
		l.Debug("sections from the keys present (freeform)", log.F("sections", len(defs)))
	}

	diffs, err := domain.DiffVersions(snap.Before, snap.Current, defs, false)
	if err != nil {
		l.Error(err, "diff against review snapshot failed", log.F("policy_version_id", versionID.String()))
		return nil, status.Error(codes.Internal, "diff failed")
	}
	out := make([]*corev1.SectionDiff, 0, len(diffs))
	changed := 0
	for _, d := range diffs {
		if d.ChangeType != domain.DiffChangeUnchanged {
			changed++
		}
		out = append(out, &corev1.SectionDiff{
			SectionKey:    d.SectionKey,
			SectionTitle:  d.SectionTitle,
			ChangeType:    string(d.ChangeType),
			WordDiffHtml:  d.WordDiffHTML,
			IsBoilerplate: d.IsBoilerplate,
		})
	}
	l.Debug("changes since review computed", log.F("policy_version_id", versionID.String()),
		log.F("sections", len(out)), log.F("changed", changed))
	return &corev1.GetChangesSinceReviewResponse{Diffs: out}, nil
}

// MarkVersionReviewed handles ReviewSnapshotService.MarkVersionReviewed.
func (h *ReviewSnapshotHandler) MarkVersionReviewed(ctx context.Context, req *corev1.MarkVersionReviewedRequest) (*corev1.MarkVersionReviewedResponse, error) {
	l := logger.Ctx(ctx)
	versionID, userID, err := snapshotIDs(req.GetPolicyVersionId(), req.GetUserId())
	if err != nil {
		l.Debug("mark version reviewed refused", log.F("reason", "invalid id"))
		return nil, err
	}
	if err := h.store.Mark(ctx, versionID, userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			l.Debug("mark version reviewed: version not found", log.F("policy_version_id", versionID.String()))
			return nil, status.Error(codes.NotFound, "policy version not found")
		}
		l.Error(err, "mark version reviewed failed", log.F("policy_version_id", versionID.String()))
		return nil, status.Error(codes.Internal, "mark version reviewed failed")
	}
	l.Info("review snapshot moved", log.F("policy_version_id", versionID.String()), log.F("user_id", userID.String()))
	h.emitSnapshot(ctx, "review.snapshot.moved", versionID, userID)
	return &corev1.MarkVersionReviewedResponse{}, nil
}

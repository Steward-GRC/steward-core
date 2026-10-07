// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"time"

	grpcactor "github.com/Bugs5382/go-grpc-actor"
	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/lifecycle"
)

// RecordBreakGlassRead handles PolicyService.RecordBreakGlassRead. The gateway
// has already found an active break-glass grant for this one document; core
// records the read in the audit tier and publishes the notice for the owner
// and the compliance admins. Both must succeed: the read is refused rather
// than served unrecorded, so any failure is Unavailable and the gateway serves
// nothing.
func (h *PolicyHandler) RecordBreakGlassRead(ctx context.Context, req *corev1.RecordBreakGlassReadRequest) (*corev1.RecordBreakGlassReadResponse, error) {
	l := logger.Ctx(ctx)
	a, ok := grpcactor.FromContext(ctx)
	if !ok || a.Subject == "" {
		return nil, status.Error(codes.Unauthenticated, "break-glass read needs the reader")
	}
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid policy_id: %v", err)
	}
	versionID := req.GetPolicyVersionId()
	if versionID != "" {
		vid, verr := uuid.Parse(versionID)
		if verr != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid policy_version_id: %v", verr)
		}
		pv, verr := h.store.GetPolicyVersion(ctx, vid)
		if verr != nil || pv.PolicyID != pid {
			return nil, status.Error(codes.InvalidArgument, "policy_version_id is not a version of policy_id")
		}
	}
	p, err := h.store.GetPolicy(ctx, pid)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		return nil, status.Errorf(codes.Internal, "get policy: %v", err)
	}
	if h.auditor == nil || h.lifecyclePub == nil {
		l.Error(errors.New("break-glass read refused"), "no audit or jobs publisher configured", log.F("policy_id", pid.String()))
		return nil, status.Error(codes.Unavailable, "break-glass reads can't be recorded")
	}

	attrs := map[string]string{"policy_number": p.Number}
	if versionID != "" {
		attrs["policy_version_id"] = versionID
	}
	if err := h.auditor.Emit(ctx, audit.Event{
		Tier: audit.TierAudit, Action: "policy.break_glass_read",
		ActorUserID: a.Subject,
		Subject:     "policy:" + pid.String(),
		GroupID:     p.HomeCategoryID.String(),
		Attributes:  attrs,
	}); err != nil {
		l.Error(err, "break-glass read audit failed; read refused", log.F("policy_id", pid.String()))
		return nil, status.Error(codes.Unavailable, "break-glass read could not be audited")
	}
	evt := lifecycle.BreakGlassReadEvent{
		EventID: uuid.NewString(), ReadAt: time.Now(),
		PolicyID: pid.String(), PolicyVersionID: versionID,
		Number: p.Number, Title: p.Title, DocumentType: string(p.DocumentType),
		OwnerUserID: p.OwnerUserID.String(), ReaderUserID: a.Subject,
	}
	if a.Impersonated() {
		evt.ActAsAdminUserID = a.Impersonator
	}
	if err := h.lifecyclePub.EmitBreakGlassRead(ctx, evt); err != nil {
		l.Error(err, "break-glass read notice failed; read refused", log.F("policy_id", pid.String()))
		return nil, status.Error(codes.Unavailable, "break-glass read notice could not be sent")
	}
	l.Info("break-glass read recorded", log.F("policy_id", pid.String()), log.F("event_id", evt.EventID),
		log.F("act_as", a.Impersonated()))
	return &corev1.RecordBreakGlassReadResponse{}, nil
}

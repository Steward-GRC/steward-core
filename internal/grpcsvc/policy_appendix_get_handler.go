// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"

	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
)

// GetPolicyAppendix handles PolicyAppendixService.GetPolicyAppendix. It is a
// read: no write and no audit event.
func (h *PolicyAppendixHandler) GetPolicyAppendix(ctx context.Context, req *corev1.GetPolicyAppendixRequest) (*corev1.GetPolicyAppendixResponse, error) {
	l := logger.Ctx(ctx)
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		l.Debug("get policy appendix refused: bad id")
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	l.Debug("get policy appendix", log.F("appendix_id", id.String()))
	a, err := h.store.Get(ctx, id)
	if err != nil {
		return nil, policyAppendixErr(ctx, "get_policy_appendix", err)
	}
	l.Debug("get policy appendix done", log.F("appendix_id", id.String()), log.F("retired", !a.Live()))
	return &corev1.GetPolicyAppendixResponse{Appendix: policyAppendixToProto(a)}, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
)

// relationBackend is the part of *store.RelationStore the handler uses.
type relationBackend interface {
	ListByPolicy(ctx context.Context, policyID uuid.UUID) ([]domain.RelatedPolicy, error)
	SetForPolicy(ctx context.Context, policyID uuid.UUID, relatedIDs []uuid.UUID) ([]domain.RelatedPolicy, error)
}

// RelationHandler implements corev1.RelationServiceServer.
type RelationHandler struct {
	corev1.UnimplementedRelationServiceServer
	store relationBackend
}

// NewRelationHandler returns a handler over the given backend.
func NewRelationHandler(s relationBackend) *RelationHandler { return &RelationHandler{store: s} }

func relatedToProto(r domain.RelatedPolicy) *corev1.RelatedPolicy {
	return &corev1.RelatedPolicy{
		PolicyId: r.PolicyID.String(),
		Number:   r.Number,
		Title:    r.Title,
	}
}

func (h *RelationHandler) ListRelatedPolicies(ctx context.Context, req *corev1.ListRelatedPoliciesRequest) (*corev1.ListRelatedPoliciesResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	list, err := h.store.ListByPolicy(ctx, pid)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list related policies: %v", err)
	}
	out := make([]*corev1.RelatedPolicy, len(list))
	for i, r := range list {
		out[i] = relatedToProto(r)
	}
	return &corev1.ListRelatedPoliciesResponse{Related: out}, nil
}

func (h *RelationHandler) SetRelatedPolicies(ctx context.Context, req *corev1.SetRelatedPoliciesRequest) (*corev1.SetRelatedPoliciesResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(req.GetRelatedPolicyIds()))
	for i, s := range req.GetRelatedPolicyIds() {
		rid, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "related_policy_ids[%d]: %v", i, err)
		}
		ids = append(ids, rid)
	}
	saved, err := h.store.SetForPolicy(ctx, pid, ids)
	if err != nil {
		// An unknown related id fails the foreign key: the caller sent it.
		return nil, status.Errorf(codes.InvalidArgument, "set related policies: %v", err)
	}
	out := make([]*corev1.RelatedPolicy, len(saved))
	for i, r := range saved {
		out[i] = relatedToProto(r)
	}
	return &corev1.SetRelatedPoliciesResponse{Related: out}, nil
}

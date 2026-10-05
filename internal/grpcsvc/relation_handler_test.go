// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeRelationBackend struct {
	byPolicy map[uuid.UUID][]domain.RelatedPolicy
}

func newFakeRelationBackend() *fakeRelationBackend {
	return &fakeRelationBackend{byPolicy: map[uuid.UUID][]domain.RelatedPolicy{}}
}

func (f *fakeRelationBackend) ListByPolicy(_ context.Context, policyID uuid.UUID) ([]domain.RelatedPolicy, error) {
	return f.byPolicy[policyID], nil
}

func (f *fakeRelationBackend) SetForPolicy(_ context.Context, policyID uuid.UUID, relatedIDs []uuid.UUID) ([]domain.RelatedPolicy, error) {
	out := make([]domain.RelatedPolicy, 0, len(relatedIDs))
	for _, rid := range relatedIDs {
		if rid == policyID {
			continue // drop self-reference (mirrors store)
		}
		out = append(out, domain.RelatedPolicy{PolicyID: rid, Number: "POL-1", Title: "Related"})
	}
	f.byPolicy[policyID] = out
	return out, nil
}

func TestRelationHandler_SetThenList(t *testing.T) {
	h := grpcsvc.NewRelationHandler(newFakeRelationBackend())
	pid := uuid.New().String()
	rid := uuid.New().String()

	setResp, err := h.SetRelatedPolicies(context.Background(), &corev1.SetRelatedPoliciesRequest{
		PolicyId: pid, RelatedPolicyIds: []string{rid},
	})
	if err != nil {
		t.Fatalf("SetRelatedPolicies: %v", err)
	}
	if len(setResp.GetRelated()) != 1 || setResp.GetRelated()[0].GetPolicyId() != rid {
		t.Fatalf("unexpected set result: %+v", setResp.GetRelated())
	}

	listResp, err := h.ListRelatedPolicies(context.Background(), &corev1.ListRelatedPoliciesRequest{PolicyId: pid})
	if err != nil {
		t.Fatalf("ListRelatedPolicies: %v", err)
	}
	if len(listResp.GetRelated()) != 1 {
		t.Fatalf("list: want 1, got %d", len(listResp.GetRelated()))
	}
}

func TestRelationHandler_RejectsBadID(t *testing.T) {
	h := grpcsvc.NewRelationHandler(newFakeRelationBackend())
	_, err := h.SetRelatedPolicies(context.Background(), &corev1.SetRelatedPoliciesRequest{
		PolicyId: uuid.New().String(), RelatedPolicyIds: []string{"not-a-uuid"},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("want InvalidArgument for bad related id, got %v", err)
	}
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
)

// Issue #51: in-review and rejected statuses through the handler.

func statusHandler(t *testing.T, st domain.PolicyVersionStatus, content string) (*grpcsvc.PolicyHandler, *fakePolicyStore, uuid.UUID) {
	t.Helper()
	fakePS := newFakePolicyStore()
	policyID, verID := uuid.New(), uuid.New()
	fakePS.policies[policyID] = domain.Policy{ID: policyID}
	fakePS.versions[verID] = domain.PolicyVersion{ID: verID, PolicyID: policyID, Status: st, ContentJSON: content}
	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil)
	return h, fakePS, verID
}

func setStatusReq(id uuid.UUID, to string) *corev1.SetVersionStatusRequest {
	return &corev1.SetVersionStatusRequest{PolicyVersionId: id.String(), Status: to, ActorUserId: "system"}
}

func textDoc(text string) string {
	return `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"` + text + `"}]}]}}`
}

// Mock: freeform-and-empty "is refused when the document holds no text, spaces alone included".
func TestSetVersionStatusInReviewRefusesEmptyContent(t *testing.T) {
	for name, content := range map[string]string{
		"initial {}":  `{}`,
		"no children": `{"root":{"type":"root","children":[]}}`,
		"spaces only": textDoc("   "),
	} {
		t.Run(name, func(t *testing.T) {
			h, fakePS, verID := statusHandler(t, domain.PolicyVersionStatusDraft, content)
			_, err := h.SetVersionStatus(context.Background(), setStatusReq(verID, "in_review"))
			if got := status.Code(err); got != codes.FailedPrecondition {
				t.Fatalf("code = %v (%v), want FailedPrecondition", got, err)
			}
			if !strings.Contains(err.Error(), "no text") {
				t.Fatalf("refusal should name the empty document, got: %v", err)
			}
			if fakePS.versions[verID].Status != domain.PolicyVersionStatusDraft {
				t.Fatalf("status changed to %q despite the refusal", fakePS.versions[verID].Status)
			}
		})
	}
}

// Mock: freeform-and-empty "goes through with one character of text".
func TestSetVersionStatusInReviewAcceptsText(t *testing.T) {
	h, fakePS, verID := statusHandler(t, domain.PolicyVersionStatusDraft, textDoc("x"))
	resp, err := h.SetVersionStatus(context.Background(), setStatusReq(verID, "in_review"))
	if err != nil {
		t.Fatalf("SetVersionStatus(in_review): %v", err)
	}
	if resp.GetVersion().GetStatus() != corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_IN_REVIEW {
		t.Fatalf("response status = %v, want IN_REVIEW", resp.GetVersion().GetStatus())
	}
	if fakePS.versions[verID].Status != domain.PolicyVersionStatusInReview {
		t.Fatalf("stored status = %q", fakePS.versions[verID].Status)
	}
}

// Wire values: rejected goes out as REJECTED, and workflow's string API takes it.
func TestPolicyVersionStatusToProtoReviewStates(t *testing.T) {
	h, _, verID := statusHandler(t, domain.PolicyVersionStatusInReview, textDoc("x"))
	resp, err := h.SetVersionStatus(context.Background(), setStatusReq(verID, "rejected"))
	if err != nil {
		t.Fatalf("SetVersionStatus(rejected): %v", err)
	}
	if resp.GetVersion().GetStatus() != corev1.PolicyVersionStatus_POLICY_VERSION_STATUS_REJECTED {
		t.Fatalf("response status = %v, want REJECTED", resp.GetVersion().GetStatus())
	}
	// A rejected version cannot be revived.
	_, err = h.SetVersionStatus(context.Background(), setStatusReq(verID, "published"))
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("rejected -> published: code = %v, want FailedPrecondition", got)
	}
}

// Mock: links.ts EDITABLE: a draft and one in review are editable, a rejected one is final.
func TestUpdateDraftContentRefusesRejectedAllowsInReview(t *testing.T) {
	for st, want := range map[domain.PolicyVersionStatus]codes.Code{
		domain.PolicyVersionStatusInReview: codes.OK,
		domain.PolicyVersionStatusRejected: codes.FailedPrecondition,
	} {
		t.Run(string(st), func(t *testing.T) {
			h, fakePS, verID := statusHandler(t, st, textDoc("old"))
			pv := fakePS.versions[verID]
			_, err := h.UpdateDraftContent(context.Background(), &corev1.UpdateDraftContentRequest{
				PolicyId: pv.PolicyID.String(), DraftId: verID.String(), ContentJson: textDoc("new"),
				ActorUserId: uuid.New().String(),
			})
			if got := status.Code(err); got != want {
				t.Fatalf("code = %v (%v), want %v", got, err, want)
			}
		})
	}
}

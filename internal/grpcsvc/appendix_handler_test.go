// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"
	"time"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeAppendixBackend struct {
	appendices []domain.Appendix
	addErr     error
	updateErr  error
	deleteErr  error
	reorderErr error
}

func (f *fakeAppendixBackend) ListByVersion(_ context.Context, versionID uuid.UUID) ([]domain.Appendix, error) {
	var out []domain.Appendix
	for _, a := range f.appendices {
		if a.PolicyVersionID == versionID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeAppendixBackend) Add(_ context.Context, a domain.Appendix) (domain.Appendix, error) {
	if f.addErr != nil {
		return domain.Appendix{}, f.addErr
	}
	a.OrderIndex = 0
	a.CreatedAt = time.Now()
	a.UpdatedAt = time.Now()
	f.appendices = append(f.appendices, a)
	return a, nil
}

func (f *fakeAppendixBackend) Update(_ context.Context, id uuid.UUID, title, contentJSON string) (domain.Appendix, error) {
	if f.updateErr != nil {
		return domain.Appendix{}, f.updateErr
	}
	for i, a := range f.appendices {
		if a.ID == id {
			f.appendices[i].Title = title
			f.appendices[i].ContentJSON = contentJSON
			return f.appendices[i], nil
		}
	}
	return domain.Appendix{}, store.ErrVersionNotDraft
}

func (f *fakeAppendixBackend) Delete(_ context.Context, id uuid.UUID) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	for i, a := range f.appendices {
		if a.ID == id {
			f.appendices = append(f.appendices[:i], f.appendices[i+1:]...)
			return nil
		}
	}
	return nil
}

func (f *fakeAppendixBackend) Reorder(_ context.Context, versionID uuid.UUID, orderedIDs []uuid.UUID) ([]domain.Appendix, error) {
	if f.reorderErr != nil {
		return nil, f.reorderErr
	}
	idxMap := make(map[uuid.UUID]int, len(orderedIDs))
	for i, id := range orderedIDs {
		idxMap[id] = i
	}
	var out []domain.Appendix
	for i, a := range f.appendices {
		if a.PolicyVersionID == versionID {
			if newIdx, ok := idxMap[a.ID]; ok {
				f.appendices[i].OrderIndex = newIdx
			}
			out = append(out, f.appendices[i])
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].OrderIndex < out[i].OrderIndex {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out, nil
}

func TestAppendixHandler_AddValidationAndDraftGuard(t *testing.T) {
	ctx := context.Background()
	vid := uuid.New().String()

	t.Run("empty title -> InvalidArgument", func(t *testing.T) {
		fake := &fakeAppendixBackend{}
		h := grpcsvc.NewAppendixHandler(fake)

		_, err := h.AddAppendix(ctx, &corev1.AddAppendixRequest{
			PolicyVersionId: vid,
			Title:           "",
			ContentJson:     `{"root":{}}`,
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if got := status.Code(err); got != codes.InvalidArgument {
			t.Fatalf("status code: got %v want InvalidArgument", got)
		}
		if len(fake.appendices) != 0 {
			t.Fatalf("expected no appendices stored, got %d", len(fake.appendices))
		}
	})

	t.Run("backend ErrVersionNotDraft -> FailedPrecondition", func(t *testing.T) {
		fake := &fakeAppendixBackend{addErr: store.ErrVersionNotDraft}
		h := grpcsvc.NewAppendixHandler(fake)

		_, err := h.AddAppendix(ctx, &corev1.AddAppendixRequest{
			PolicyVersionId: vid,
			Title:           "Appendix A",
			ContentJson:     `{"root":{}}`,
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if got := status.Code(err); got != codes.FailedPrecondition {
			t.Fatalf("status code: got %v want FailedPrecondition", got)
		}
	})
}

func TestAppendixHandler_CRUDMapping(t *testing.T) {
	ctx := context.Background()
	vid := uuid.New()

	fake := &fakeAppendixBackend{}
	h := grpcsvc.NewAppendixHandler(fake)

	addResp, err := h.AddAppendix(ctx, &corev1.AddAppendixRequest{
		PolicyVersionId: vid.String(),
		Title:           "Appendix A",
		ContentJson:     `{"root":{}}`,
	})
	if err != nil {
		t.Fatalf("AddAppendix: %v", err)
	}
	if addResp.GetAppendix() == nil {
		t.Fatal("expected non-nil appendix in response")
	}
	if addResp.GetAppendix().GetTitle() != "Appendix A" {
		t.Fatalf("title: got %q want %q", addResp.GetAppendix().GetTitle(), "Appendix A")
	}
	if addResp.GetAppendix().GetOrderIndex() != 0 {
		t.Fatalf("order_index: got %d want 0", addResp.GetAppendix().GetOrderIndex())
	}
	if addResp.GetAppendix().GetPolicyVersionId() != vid.String() {
		t.Fatalf("policy_version_id: got %q want %q", addResp.GetAppendix().GetPolicyVersionId(), vid.String())
	}
	firstID := addResp.GetAppendix().GetId()

	addResp2, err := h.AddAppendix(ctx, &corev1.AddAppendixRequest{
		PolicyVersionId: vid.String(),
		Title:           "Appendix B",
		ContentJson:     `{"root":{}}`,
	})
	if err != nil {
		t.Fatalf("AddAppendix second: %v", err)
	}
	secondID := addResp2.GetAppendix().GetId()

	listResp, err := h.ListAppendices(ctx, &corev1.ListAppendicesRequest{
		PolicyVersionId: vid.String(),
	})
	if err != nil {
		t.Fatalf("ListAppendices: %v", err)
	}
	if len(listResp.GetAppendices()) != 2 {
		t.Fatalf("expected 2 appendices, got %d", len(listResp.GetAppendices()))
	}

	reorderResp, err := h.ReorderAppendices(ctx, &corev1.ReorderAppendicesRequest{
		PolicyVersionId: vid.String(),
		OrderedIds:      []string{secondID, firstID},
	})
	if err != nil {
		t.Fatalf("ReorderAppendices: %v", err)
	}
	if len(reorderResp.GetAppendices()) != 2 {
		t.Fatalf("expected 2 appendices after reorder, got %d", len(reorderResp.GetAppendices()))
	}
	if reorderResp.GetAppendices()[0].GetId() != secondID {
		t.Fatalf("reorder[0]: got %q want %q", reorderResp.GetAppendices()[0].GetId(), secondID)
	}

	delResp, err := h.DeleteAppendix(ctx, &corev1.DeleteAppendixRequest{Id: firstID})
	if err != nil {
		t.Fatalf("DeleteAppendix: %v", err)
	}
	if delResp == nil {
		t.Fatal("expected non-nil delete response")
	}

	listResp2, err := h.ListAppendices(ctx, &corev1.ListAppendicesRequest{
		PolicyVersionId: vid.String(),
	})
	if err != nil {
		t.Fatalf("ListAppendices after delete: %v", err)
	}
	if len(listResp2.GetAppendices()) != 1 {
		t.Fatalf("expected 1 appendix after delete, got %d", len(listResp2.GetAppendices()))
	}
	if listResp2.GetAppendices()[0].GetId() != secondID {
		t.Fatalf("remaining appendix: got %q want %q", listResp2.GetAppendices()[0].GetId(), secondID)
	}
}

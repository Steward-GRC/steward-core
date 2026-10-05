// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeReferenceBackend struct {
	refs        map[uuid.UUID]domain.Reference
	attached    map[uuid.UUID][]uuid.UUID
	deleteInUse bool
}

func newFakeReferenceBackend() *fakeReferenceBackend {
	return &fakeReferenceBackend{refs: map[uuid.UUID]domain.Reference{}, attached: map[uuid.UUID][]uuid.UUID{}}
}

func (f *fakeReferenceBackend) ListReferences(_ context.Context, includeArchived bool) ([]domain.Reference, error) {
	out := []domain.Reference{}
	for _, r := range f.refs {
		if !includeArchived && r.Archived {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeReferenceBackend) CreateReference(_ context.Context, r domain.Reference) (domain.Reference, error) {
	r.ID = uuid.New()
	f.refs[r.ID] = r
	return r, nil
}

func (f *fakeReferenceBackend) UpdateReference(_ context.Context, id uuid.UUID, r domain.Reference) (domain.Reference, error) {
	if _, ok := f.refs[id]; !ok {
		return domain.Reference{}, store.ErrReferenceNotFound
	}
	r.ID = id
	f.refs[id] = r
	return r, nil
}

func (f *fakeReferenceBackend) DeleteReference(_ context.Context, id uuid.UUID) error {
	if _, ok := f.refs[id]; !ok {
		return store.ErrReferenceNotFound
	}
	if f.deleteInUse {
		return store.ErrReferenceInUse
	}
	delete(f.refs, id)
	return nil
}

func (f *fakeReferenceBackend) SetReferenceArchived(_ context.Context, id uuid.UUID, archived bool) (domain.Reference, error) {
	r, ok := f.refs[id]
	if !ok {
		return domain.Reference{}, store.ErrReferenceNotFound
	}
	r.Archived = archived
	f.refs[id] = r
	return r, nil
}

func (f *fakeReferenceBackend) ListPolicyReferences(_ context.Context, policyID uuid.UUID) ([]domain.Reference, error) {
	out := []domain.Reference{}
	for _, id := range f.attached[policyID] {
		out = append(out, f.refs[id])
	}
	return out, nil
}

func (f *fakeReferenceBackend) SetPolicyReferences(_ context.Context, policyID uuid.UUID, ids []uuid.UUID) ([]domain.Reference, error) {
	f.attached[policyID] = ids
	return f.ListPolicyReferences(context.Background(), policyID)
}

func TestReferenceHandlerCreateValidatesKindAndAudits(t *testing.T) {
	cap := &capturePublisher{}
	h := grpcsvc.NewReferenceHandler(newFakeReferenceBackend(), audit.New(cap))
	actor := uuid.New().String()

	_, err := h.CreateReference(context.Background(), &corev1.CreateReferenceRequest{
		Input:       &corev1.ReferenceInput{Label: "note", Kind: corev1.ReferenceKind_REFERENCE_KIND_TEXT},
		ActorUserId: actor,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("TEXT without body: want InvalidArgument, got %v", err)
	}

	_, err = h.CreateReference(context.Background(), &corev1.CreateReferenceRequest{
		Input:       &corev1.ReferenceInput{Label: "link", Kind: corev1.ReferenceKind_REFERENCE_KIND_LINK},
		ActorUserId: actor,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("LINK without url: want InvalidArgument, got %v", err)
	}

	_, err = h.CreateReference(context.Background(), &corev1.CreateReferenceRequest{
		Input: &corev1.ReferenceInput{Kind: corev1.ReferenceKind_REFERENCE_KIND_STANDARD},
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty label: want InvalidArgument, got %v", err)
	}

	resp, err := h.CreateReference(context.Background(), &corev1.CreateReferenceRequest{
		Input:       &corev1.ReferenceInput{Label: "ISO 27001", Kind: corev1.ReferenceKind_REFERENCE_KIND_STANDARD, Clause: "A.5"},
		ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("CreateReference: %v", err)
	}
	if resp.GetReference().GetCreatedByUserId() != actor {
		t.Fatalf("created_by not set on response: %q", resp.GetReference().GetCreatedByUserId())
	}
	if len(cap.calls) != 1 || cap.calls[0].event.Action != "reference.created" {
		t.Fatalf("want reference.created audit, got %+v", cap.calls)
	}
}

func TestReferenceHandlerDeleteInUseMapsFailedPrecondition(t *testing.T) {
	be := newFakeReferenceBackend()
	be.deleteInUse = true
	id := uuid.New()
	be.refs[id] = domain.Reference{ID: id, Label: "x", Kind: domain.ReferenceKindStandard}
	h := grpcsvc.NewReferenceHandler(be, audit.New(&capturePublisher{}))

	_, err := h.DeleteReference(context.Background(), &corev1.DeleteReferenceRequest{Id: id.String()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete-in-use: want FailedPrecondition, got %v", err)
	}
}

func TestReferenceHandlerSetPolicyReferencesAudits(t *testing.T) {
	cap := &capturePublisher{}
	be := newFakeReferenceBackend()
	h := grpcsvc.NewReferenceHandler(be, audit.New(cap))

	rid := uuid.New()
	be.refs[rid] = domain.Reference{ID: rid, Label: "std", Kind: domain.ReferenceKindStandard}
	pid := uuid.New()

	if _, err := h.SetPolicyReferences(context.Background(), &corev1.SetPolicyReferencesRequest{
		PolicyId: pid.String(), ReferenceIds: []string{rid.String()}, ActorUserId: uuid.New().String(),
	}); err != nil {
		t.Fatalf("SetPolicyReferences: %v", err)
	}
	if len(cap.calls) != 1 || cap.calls[0].event.Action != "policy.references_changed" {
		t.Fatalf("want policy.references_changed audit, got %+v", cap.calls)
	}
}

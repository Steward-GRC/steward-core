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

type fakeDefinitionLibraryBackend struct {
	defs        map[uuid.UUID]domain.DefinitionEntry
	attached    map[uuid.UUID][]uuid.UUID
	deleteInUse bool
}

func newFakeDefinitionLibraryBackend() *fakeDefinitionLibraryBackend {
	return &fakeDefinitionLibraryBackend{defs: map[uuid.UUID]domain.DefinitionEntry{}, attached: map[uuid.UUID][]uuid.UUID{}}
}

func (f *fakeDefinitionLibraryBackend) ListDefinitionEntries(_ context.Context, categoryID *uuid.UUID, includeArchived bool) ([]domain.DefinitionEntry, error) {
	out := []domain.DefinitionEntry{}
	for _, d := range f.defs {
		if !includeArchived && d.Archived {
			continue
		}
		if categoryID != nil && d.CategoryID != *categoryID {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeDefinitionLibraryBackend) ListPolicyDefinitionCandidates(_ context.Context, _ uuid.UUID, _ bool) ([]domain.DefinitionEntry, error) {
	out := []domain.DefinitionEntry{}
	for _, d := range f.defs {
		out = append(out, d)
	}
	return out, nil
}

func (f *fakeDefinitionLibraryBackend) CreateDefinitionEntry(_ context.Context, d domain.DefinitionEntry) (domain.DefinitionEntry, error) {
	d.ID = uuid.New()
	f.defs[d.ID] = d
	return d, nil
}

func (f *fakeDefinitionLibraryBackend) UpdateDefinitionEntry(_ context.Context, id uuid.UUID, d domain.DefinitionEntry) (domain.DefinitionEntry, error) {
	cur, ok := f.defs[id]
	if !ok {
		return domain.DefinitionEntry{}, store.ErrDefinitionEntryNotFound
	}
	cur.Term = d.Term
	cur.Definition = d.Definition
	f.defs[id] = cur
	return cur, nil
}

func (f *fakeDefinitionLibraryBackend) DeleteDefinitionEntry(_ context.Context, id uuid.UUID) error {
	if _, ok := f.defs[id]; !ok {
		return store.ErrDefinitionEntryNotFound
	}
	if f.deleteInUse {
		return store.ErrDefinitionEntryInUse
	}
	delete(f.defs, id)
	return nil
}

func (f *fakeDefinitionLibraryBackend) SetDefinitionEntryArchived(_ context.Context, id uuid.UUID, archived bool) (domain.DefinitionEntry, error) {
	d, ok := f.defs[id]
	if !ok {
		return domain.DefinitionEntry{}, store.ErrDefinitionEntryNotFound
	}
	d.Archived = archived
	f.defs[id] = d
	return d, nil
}

func (f *fakeDefinitionLibraryBackend) ListPolicyDefinitionEntries(_ context.Context, policyID uuid.UUID) ([]domain.DefinitionEntry, error) {
	out := []domain.DefinitionEntry{}
	for _, id := range f.attached[policyID] {
		out = append(out, f.defs[id])
	}
	return out, nil
}

func (f *fakeDefinitionLibraryBackend) SetPolicyDefinitionEntries(_ context.Context, policyID uuid.UUID, ids []uuid.UUID) ([]domain.DefinitionEntry, error) {
	f.attached[policyID] = ids
	return f.ListPolicyDefinitionEntries(context.Background(), policyID)
}

func TestDefinitionLibraryHandlerCreateValidatesAndAudits(t *testing.T) {
	cap := &capturePublisher{}
	h := grpcsvc.NewDefinitionLibraryHandler(newFakeDefinitionLibraryBackend(), audit.New(cap))
	actor := uuid.New().String()
	cat := uuid.New().String()

	_, err := h.CreateDefinitionEntry(context.Background(), &corev1.CreateDefinitionEntryRequest{
		Input:       &corev1.DefinitionEntryInput{CategoryId: cat, Definition: "d"},
		ActorUserId: actor,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty term: want InvalidArgument, got %v", err)
	}

	_, err = h.CreateDefinitionEntry(context.Background(), &corev1.CreateDefinitionEntryRequest{
		Input:       &corev1.DefinitionEntryInput{CategoryId: cat, Term: "T"},
		ActorUserId: actor,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty definition: want InvalidArgument, got %v", err)
	}

	_, err = h.CreateDefinitionEntry(context.Background(), &corev1.CreateDefinitionEntryRequest{
		Input:       &corev1.DefinitionEntryInput{CategoryId: "not-a-uuid", Term: "T", Definition: "d"},
		ActorUserId: actor,
	})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad category: want InvalidArgument, got %v", err)
	}

	resp, err := h.CreateDefinitionEntry(context.Background(), &corev1.CreateDefinitionEntryRequest{
		Input:       &corev1.DefinitionEntryInput{CategoryId: cat, Term: "Term", Definition: "def"},
		ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("CreateDefinitionEntry: %v", err)
	}
	if resp.GetDefinition().GetCreatedByUserId() != actor || resp.GetDefinition().GetCategoryId() != cat {
		t.Fatalf("actor/category not persisted: %+v", resp.GetDefinition())
	}
	if len(cap.calls) != 1 || cap.calls[0].event.Action != "definition.created" {
		t.Fatalf("want definition.created audit, got %+v", cap.calls)
	}
}

func TestDefinitionLibraryHandlerDeleteInUseMapsFailedPrecondition(t *testing.T) {
	be := newFakeDefinitionLibraryBackend()
	be.deleteInUse = true
	id := uuid.New()
	be.defs[id] = domain.DefinitionEntry{ID: id, CategoryID: uuid.New(), Term: "x", Definition: "y"}
	h := grpcsvc.NewDefinitionLibraryHandler(be, audit.New(&capturePublisher{}))

	_, err := h.DeleteDefinitionEntry(context.Background(), &corev1.DeleteDefinitionEntryRequest{Id: id.String()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("delete-in-use: want FailedPrecondition, got %v", err)
	}
}

func TestDefinitionLibraryHandlerSetPolicyDefinitionsAudits(t *testing.T) {
	cap := &capturePublisher{}
	be := newFakeDefinitionLibraryBackend()
	h := grpcsvc.NewDefinitionLibraryHandler(be, audit.New(cap))

	did := uuid.New()
	be.defs[did] = domain.DefinitionEntry{ID: did, CategoryID: uuid.New(), Term: "t", Definition: "d"}
	pid := uuid.New()

	if _, err := h.SetPolicyDefinitionEntries(context.Background(), &corev1.SetPolicyDefinitionEntriesRequest{
		PolicyId: pid.String(), DefinitionIds: []string{did.String()}, ActorUserId: uuid.New().String(),
	}); err != nil {
		t.Fatalf("SetPolicyDefinitionEntries: %v", err)
	}
	if len(cap.calls) != 1 || cap.calls[0].event.Action != "policy.definitions_changed" {
		t.Fatalf("want policy.definitions_changed audit, got %+v", cap.calls)
	}
}

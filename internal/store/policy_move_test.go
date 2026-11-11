// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// moveFixture makes a source category holding two policies and a procedure, and a target
// category that already holds one policy (so its sequence does not start at 1).
type moveFixture struct {
	ps          *store.PolicyStore
	src, dst    domain.Category
	srcDocs     []domain.Policy // policy, policy, procedure
	dstExisting domain.Policy
}

func newMoveFixture(t *testing.T) moveFixture {
	t.Helper()
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	src, _ := setupCategoryAndTemplate(t, gs, ts)
	dst, err := gs.Create(ctx, mustMoveCategory(t, "Finance", "fin-"+uuid.New().String()[:8]))
	if err != nil {
		t.Fatalf("create target category: %v", err)
	}
	make := func(title string, cat uuid.UUID, dt domain.DocumentType) domain.Policy {
		p, err := domain.NewPolicy(title, cat, domain.SensitivityStandard, uuid.New())
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		p.DocumentType = dt
		out, err := ps.CreatePolicy(ctx, p)
		if err != nil {
			t.Fatalf("CreatePolicy %q: %v", title, err)
		}
		return out
	}
	return moveFixture{
		ps: ps, src: src, dst: dst,
		srcDocs: []domain.Policy{
			make("Expenses", src.ID, domain.DocumentTypePolicy),
			make("Travel", src.ID, domain.DocumentTypePolicy),
			make("Claims how-to", src.ID, domain.DocumentTypeProcedure),
		},
		dstExisting: make("Budget", dst.ID, domain.DocumentTypePolicy),
	}
}

func mustMoveCategory(t *testing.T, name, slug string) domain.Category {
	t.Helper()
	c, err := domain.NewCategory(name, slug, uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	return c
}

func ids(ps []domain.Policy) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.ID)
	}
	return out
}

// Mock: "previewMovePolicies says what each document would get and changes nothing".
func TestPreviewMoveNumbersReadsTheTargetSequenceAndChangesNothing(t *testing.T) {
	f := newMoveFixture(t)
	ctx := context.Background()
	types := []domain.DocumentType{domain.DocumentTypePolicy, domain.DocumentTypePolicy, domain.DocumentTypeProcedure}
	code := store.PolicyNumberCode(f.dst.Slug)

	got, err := f.ps.PreviewMoveNumbers(ctx, f.dst.ID, types)
	if err != nil {
		t.Fatalf("PreviewMoveNumbers: %v", err)
	}
	want := []string{"POL-" + code + "-000002", "POL-" + code + "-000003", "PRC-" + code + "-000001"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("number %d: got %q want %q", i, got[i], want[i])
		}
	}
	// A second preview gives the same answer: nothing was consumed.
	again, _ := f.ps.PreviewMoveNumbers(ctx, f.dst.ID, types)
	if again[0] != want[0] {
		t.Fatalf("preview consumed a number: %v", again)
	}
	for _, d := range f.srcDocs {
		cur, err := f.ps.GetPolicy(ctx, d.ID)
		if err != nil || cur.HomeCategoryID != f.src.ID || cur.Number != d.Number {
			t.Fatalf("preview changed %s: %+v %v", d.ID, cur, err)
		}
	}
}

func TestPreviewMoveNumbersUnknownTarget(t *testing.T) {
	f := newMoveFixture(t)
	_, err := f.ps.PreviewMoveNumbers(context.Background(), uuid.New(), []domain.DocumentType{domain.DocumentTypePolicy})
	if err == nil {
		t.Fatal("want an error for an unknown target")
	}
}

// Mock: "movePolicies moves them all, renumbering in the target's sequence".
func TestMovePoliciesRenumbersInTheTargetSequence(t *testing.T) {
	f := newMoveFixture(t)
	ctx := context.Background()
	code := store.PolicyNumberCode(f.dst.Slug)

	moved, err := f.ps.MovePolicies(ctx, f.dst.ID, ids(f.srcDocs))
	if err != nil {
		t.Fatalf("MovePolicies: %v", err)
	}
	want := []string{"POL-" + code + "-000002", "POL-" + code + "-000003", "PRC-" + code + "-000001"}
	for i, m := range moved {
		if m.HomeCategoryID != f.dst.ID || m.Number != want[i] {
			t.Fatalf("moved[%d] = %q in %s, want %q in %s", i, m.Number, m.HomeCategoryID, want[i], f.dst.ID)
		}
	}
	for _, d := range f.srcDocs {
		cur, err := f.ps.GetPolicy(ctx, d.ID)
		if err != nil || cur.HomeCategoryID != f.dst.ID {
			t.Fatalf("%s is still in the source (err %v)", d.ID, err)
		}
	}
}

// Mock: "refuses the whole batch when one document would be refused, and moves nothing": a
// retired document that slipped past the plan rolls the whole move back, sequence included.
func TestMovePoliciesIsAllOrNothing(t *testing.T) {
	f := newMoveFixture(t)
	ctx := context.Background()
	if _, err := f.ps.RetirePolicy(ctx, f.srcDocs[2].ID); err != nil {
		t.Fatalf("RetirePolicy: %v", err)
	}
	code := store.PolicyNumberCode(f.dst.Slug)

	_, err := f.ps.MovePolicies(ctx, f.dst.ID, ids(f.srcDocs))
	if !errors.Is(err, store.ErrPolicyRetired) {
		t.Fatalf("want ErrPolicyRetired, got %v", err)
	}
	for _, d := range f.srcDocs {
		cur, err := f.ps.GetPolicy(ctx, d.ID)
		if err != nil || cur.HomeCategoryID != f.src.ID || cur.Number != d.Number {
			t.Fatalf("partial move of %s: %+v %v", d.ID, cur, err)
		}
	}
	// The rolled-back batch consumed no target numbers.
	next, _ := f.ps.PreviewMoveNumbers(ctx, f.dst.ID, []domain.DocumentType{domain.DocumentTypePolicy})
	if next[0] != "POL-"+code+"-000002" {
		t.Fatalf("sequence advanced by a refused batch: %v", next)
	}
}

// A document already in the target keeps its number and is not renumbered; an unknown id fails
// the batch.
func TestMovePoliciesSkipsDocumentsAlreadyThereAndRejectsUnknown(t *testing.T) {
	f := newMoveFixture(t)
	ctx := context.Background()
	moved, err := f.ps.MovePolicies(ctx, f.dst.ID, []uuid.UUID{f.dstExisting.ID, f.srcDocs[0].ID})
	if err != nil {
		t.Fatalf("MovePolicies: %v", err)
	}
	if moved[0].Number != f.dstExisting.Number {
		t.Fatalf("document already in the target was renumbered: %q -> %q", f.dstExisting.Number, moved[0].Number)
	}
	if _, err := f.ps.MovePolicies(ctx, f.dst.ID, []uuid.UUID{uuid.New()}); err == nil {
		t.Fatal("want an error for an unknown policy")
	}
}

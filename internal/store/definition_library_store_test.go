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

func TestDefinitionLibraryCategoryScopeAndCandidates(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	gs := store.NewCategoryStore(pool)
	ps := store.NewPolicyStore(pool)
	ds := store.NewDefinitionLibraryStore(pool)
	actor := uuid.New()

	// Category chain: root R -> child C. Policy P is homed in C.
	root, err := domain.NewCategory("Root", "root-"+uuid.New().String()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory root: %v", err)
	}
	root, err = gs.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := domain.NewCategory("Child", "child-"+uuid.New().String()[:8], root.ID)
	if err != nil {
		t.Fatalf("NewCategory child: %v", err)
	}
	child, err = gs.Create(ctx, child)
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	// An unrelated category, NOT in P's chain.
	other, err := domain.NewCategory("Other", "other-"+uuid.New().String()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory other: %v", err)
	}
	other, err = gs.Create(ctx, other)
	if err != nil {
		t.Fatalf("Create other: %v", err)
	}

	pol, err := domain.NewPolicy("Chain Policy "+uuid.New().String()[:8], child.ID, domain.SensitivityStandard, actor)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	pol, err = ps.CreatePolicy(ctx, pol)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// One definition in each of the three categories.
	dChild, err := ds.CreateDefinitionEntry(ctx, domain.DefinitionEntry{CategoryID: child.ID, Term: "Bravo", Definition: "child term", CreatedByUserID: actor.String()})
	if err != nil {
		t.Fatalf("Create dChild: %v", err)
	}
	dRoot, err := ds.CreateDefinitionEntry(ctx, domain.DefinitionEntry{CategoryID: root.ID, Term: "Alpha", Definition: "root term", CreatedByUserID: actor.String()})
	if err != nil {
		t.Fatalf("Create dRoot: %v", err)
	}
	if _, err := ds.CreateDefinitionEntry(ctx, domain.DefinitionEntry{CategoryID: other.ID, Term: "Zulu", Definition: "other term", CreatedByUserID: actor.String()}); err != nil {
		t.Fatalf("Create dOther: %v", err)
	}

	// created_by + created_at stamped.
	if dChild.CreatedByUserID != actor.String() || dChild.CreatedAt.IsZero() {
		t.Fatalf("created_by/created_at not stamped: %+v", dChild)
	}

	// ListDefinitionEntries scoped to child returns ONLY the child definition.
	scoped, err := ds.ListDefinitionEntries(ctx, &child.ID, false)
	if err != nil || len(scoped) != 1 || scoped[0].ID != dChild.ID {
		t.Fatalf("category-scoped list wrong: %v %+v", err, scoped)
	}
	// Unscoped list returns all three, ordered by term.
	all, err := ds.ListDefinitionEntries(ctx, nil, false)
	if err != nil || len(all) != 3 {
		t.Fatalf("unscoped list: %v len=%d", err, len(all))
	}
	if all[0].Term != "Alpha" || all[1].Term != "Bravo" || all[2].Term != "Zulu" {
		t.Fatalf("not ordered by term: %+v", all)
	}

	// Candidates for P = definitions whose category is in P's chain (child + root),
	// ordered by term; the unrelated "other" definition is excluded.
	cands, err := ds.ListPolicyDefinitionCandidates(ctx, pol.ID, false)
	if err != nil || len(cands) != 2 {
		t.Fatalf("candidates: %v len=%d %+v", err, len(cands), cands)
	}
	if cands[0].ID != dRoot.ID || cands[1].ID != dChild.ID {
		t.Fatalf("candidate order/content wrong: %+v", cands)
	}
}

func TestDefinitionLibraryAttachLiveAndUsedBy(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	gs := store.NewCategoryStore(pool)
	ps := store.NewPolicyStore(pool)
	ds := store.NewDefinitionLibraryStore(pool)

	g, err := domain.NewCategory("Cat", "cat-"+uuid.New().String()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	g, err = gs.Create(ctx, g)
	if err != nil {
		t.Fatalf("Create category: %v", err)
	}
	pol, err := domain.NewPolicy("Attach Policy "+uuid.New().String()[:8], g.ID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	pol, err = ps.CreatePolicy(ctx, pol)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	a, err := ds.CreateDefinitionEntry(ctx, domain.DefinitionEntry{CategoryID: g.ID, Term: "Aardvark", Definition: "one"})
	if err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, err := ds.CreateDefinitionEntry(ctx, domain.DefinitionEntry{CategoryID: g.ID, Term: "Zebra", Definition: "two"})
	if err != nil {
		t.Fatalf("Create b: %v", err)
	}

	// Attach both (order preserved, duplicate dropped).
	attached, err := ds.SetPolicyDefinitionEntries(ctx, pol.ID, []uuid.UUID{b.ID, a.ID, b.ID})
	if err != nil {
		t.Fatalf("SetPolicyDefinitionEntries: %v", err)
	}
	if len(attached) != 2 || attached[0].ID != b.ID || attached[1].ID != a.ID {
		t.Fatalf("attach order wrong: %+v", attached)
	}

	// Used-by reflects the attachment.
	lib, err := ds.ListDefinitionEntries(ctx, nil, false)
	if err != nil {
		t.Fatalf("ListDefinitionEntries: %v", err)
	}
	for _, d := range lib {
		if d.UsedByCount != 1 {
			t.Fatalf("used-by want 1 for %s, got %d", d.Term, d.UsedByCount)
		}
	}

	// Editing a library entry propagates LIVE to the attached view.
	if _, err := ds.UpdateDefinitionEntry(ctx, a.ID, domain.DefinitionEntry{Term: "Aardvark", Definition: "ONE-EDITED"}); err != nil {
		t.Fatalf("UpdateDefinitionEntry: %v", err)
	}
	live, err := ds.ListPolicyDefinitionEntries(ctx, pol.ID)
	if err != nil || len(live) != 2 {
		t.Fatalf("ListPolicyDefinitionEntries: %v len=%d", err, len(live))
	}
	if live[1].Definition != "ONE-EDITED" {
		t.Fatalf("live update did not propagate: %+v", live[1])
	}

	// Update of an unknown id -> ErrDefinitionEntryNotFound.
	if _, err := ds.UpdateDefinitionEntry(ctx, uuid.New(), domain.DefinitionEntry{Term: "x", Definition: "y"}); !errors.Is(err, store.ErrDefinitionEntryNotFound) {
		t.Fatalf("want ErrDefinitionEntryNotFound, got %v", err)
	}
}

func TestDefinitionLibraryDeleteRefusedWhenInUseAndArchive(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	gs := store.NewCategoryStore(pool)
	ps := store.NewPolicyStore(pool)
	ds := store.NewDefinitionLibraryStore(pool)

	g, err := domain.NewCategory("Cat", "cat-"+uuid.New().String()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	g, err = gs.Create(ctx, g)
	if err != nil {
		t.Fatalf("Create category: %v", err)
	}
	pol, err := domain.NewPolicy("Del Policy "+uuid.New().String()[:8], g.ID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	pol, err = ps.CreatePolicy(ctx, pol)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	d, err := ds.CreateDefinitionEntry(ctx, domain.DefinitionEntry{CategoryID: g.ID, Term: "Term", Definition: "def"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Attached -> delete REFUSED.
	if _, err := ds.SetPolicyDefinitionEntries(ctx, pol.ID, []uuid.UUID{d.ID}); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := ds.DeleteDefinitionEntry(ctx, d.ID); !errors.Is(err, store.ErrDefinitionEntryInUse) {
		t.Fatalf("want ErrDefinitionEntryInUse, got %v", err)
	}

	// Archive while attached: excluded from active list, still resolves live.
	if _, err := ds.SetDefinitionEntryArchived(ctx, d.ID, true); err != nil {
		t.Fatalf("archive: %v", err)
	}
	active, err := ds.ListDefinitionEntries(ctx, nil, false)
	if err != nil || len(active) != 0 {
		t.Fatalf("archived must drop from active list: %v %+v", err, active)
	}
	live, err := ds.ListPolicyDefinitionEntries(ctx, pol.ID)
	if err != nil || len(live) != 1 || !live[0].Archived {
		t.Fatalf("archived-but-attached must still resolve: %v %+v", err, live)
	}

	// Detach -> delete succeeds.
	if _, err := ds.SetPolicyDefinitionEntries(ctx, pol.ID, nil); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := ds.DeleteDefinitionEntry(ctx, d.ID); err != nil {
		t.Fatalf("DeleteDefinitionEntry after detach: %v", err)
	}
	// Unknown id -> ErrDefinitionEntryNotFound.
	if err := ds.DeleteDefinitionEntry(ctx, uuid.New()); !errors.Is(err, store.ErrDefinitionEntryNotFound) {
		t.Fatalf("want ErrDefinitionEntryNotFound, got %v", err)
	}
}

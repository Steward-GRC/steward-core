// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func TestCategoryStoreCreateAndGet(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	g, err := domain.NewCategory("HR", "hr", uuid.Nil)
	if err != nil {
		t.Fatalf("domain: %v", err)
	}
	created, err := s.Create(ctx, g)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == uuid.Nil {
		t.Fatal("expected non-nil ID after insert")
	}

	got, err := s.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "HR" || got.Slug != "hr" {
		t.Fatalf("got %+v", got)
	}
	if got.ParentID != uuid.Nil {
		t.Fatalf("expected root category (Nil parent), got %v", got.ParentID)
	}
}

func TestCategoryStoreAncestorChain(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	root, err := domain.NewCategory("Root", "root", uuid.Nil)
	if err != nil {
		t.Fatalf("domain root: %v", err)
	}
	root, err = s.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := domain.NewCategory("Child", "child", root.ID)
	if err != nil {
		t.Fatalf("domain child: %v", err)
	}
	child, err = s.Create(ctx, child)
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	grand, err := domain.NewCategory("Grand", "grand", child.ID)
	if err != nil {
		t.Fatalf("domain grand: %v", err)
	}
	grand, err = s.Create(ctx, grand)
	if err != nil {
		t.Fatalf("Create grand: %v", err)
	}

	chain, err := s.AncestorChain(ctx, grand.ID) // leaf -> root
	if err != nil {
		t.Fatalf("AncestorChain: %v", err)
	}
	if len(chain) != 3 {
		t.Fatalf("expected 3 categories in chain, got %d", len(chain))
	}
	if chain[0].ID != grand.ID {
		t.Fatalf("expected grand first, got %v", chain[0].ID)
	}
	if chain[2].ID != root.ID {
		t.Fatalf("expected root last, got %v", chain[2].ID)
	}
}

func TestCategoryStoreListChildren(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	root, _ := domain.NewCategory("Root", "root", uuid.Nil)
	root, err := s.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	for _, slug := range []string{"hr", "safety", "ops"} {
		child, _ := domain.NewCategory(slug, slug, root.ID)
		if _, err := s.Create(ctx, child); err != nil {
			t.Fatalf("Create child %s: %v", slug, err)
		}
	}

	children, err := s.ListChildren(ctx, root.ID)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	if len(children) != 3 {
		t.Fatalf("expected 3 children, got %d", len(children))
	}

	roots, err := s.ListChildren(ctx, uuid.Nil)
	if err != nil {
		t.Fatalf("ListChildren(root): %v", err)
	}
	if len(roots) != 1 || roots[0].ID != root.ID {
		t.Fatalf("expected single root category, got %+v", roots)
	}
}

func TestCategoryStoreSetDefaults(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ctx := context.Background()

	g, _ := domain.NewCategory("HR", "hr", uuid.Nil)
	g, err := gs.Create(ctx, g)
	if err != nil {
		t.Fatalf("Create category: %v", err)
	}

	tpl, _ := domain.NewTemplate("Standard", uuid.Nil)
	tpl, err = ts.CreateTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	workflowID := uuid.New()
	updated, err := gs.SetDefaults(ctx, g.ID, tpl.ID, workflowID, false)
	if err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}
	if updated.DefaultTemplateID != tpl.ID {
		t.Fatalf("expected DefaultTemplateID=%v, got %v", tpl.ID, updated.DefaultTemplateID)
	}
	if updated.DefaultWorkflowID != workflowID {
		t.Fatalf("expected DefaultWorkflowID=%v, got %v", workflowID, updated.DefaultWorkflowID)
	}
}

func TestCategoryStoreSetGovernancePersistsAndRoundTrips(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	g, _ := domain.NewCategory("Finance", "finance", uuid.Nil)
	g, err := gs.Create(ctx, g)
	if err != nil {
		t.Fatalf("Create category: %v", err)
	}

	owner1, owner2 := uuid.New(), uuid.New()
	owners := []uuid.UUID{owner1, owner2}
	audienceGroups := []string{"cn=finance,dc=corp", "cn=all-staff,dc=corp"}
	ack := domain.AckOnPublish
	cadence := domain.CadenceAnnual
	reviewDate := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)

	updated, err := gs.SetGovernance(ctx, g.ID, owners, &audienceGroups, ack, cadence, &reviewDate, nil, nil)
	if err != nil {
		t.Fatalf("SetGovernance: %v", err)
	}

	if len(updated.Owners) != 2 {
		t.Fatalf("expected 2 owners, got %d", len(updated.Owners))
	}
	if updated.Owners[0] != owner1 || updated.Owners[1] != owner2 {
		t.Fatalf("owner mismatch: got %v", updated.Owners)
	}
	if updated.AudienceGroupIDs == nil {
		t.Fatal("expected non-nil AudienceGroupIDs after setting array")
	}
	if len(*updated.AudienceGroupIDs) != 2 {
		t.Fatalf("expected 2 audience_group_ids, got %d", len(*updated.AudienceGroupIDs))
	}
	if (*updated.AudienceGroupIDs)[0] != audienceGroups[0] || (*updated.AudienceGroupIDs)[1] != audienceGroups[1] {
		t.Fatalf("audience_group_ids mismatch: got %v", *updated.AudienceGroupIDs)
	}
	if updated.AckTriggers != ack {
		t.Fatalf("expected ack_triggers=%q, got %q", ack, updated.AckTriggers)
	}
	if updated.ReviewCadence != cadence {
		t.Fatalf("expected review_cadence=%q, got %q", cadence, updated.ReviewCadence)
	}
	if updated.ReviewDate == nil || !updated.ReviewDate.Equal(reviewDate) {
		t.Fatalf("expected review_date=%v, got %v", reviewDate, updated.ReviewDate)
	}

	// Round-trip via Get.
	fetched, err := gs.Get(ctx, g.ID)
	if err != nil {
		t.Fatalf("Get after SetGovernance: %v", err)
	}
	if len(fetched.Owners) != 2 {
		t.Fatalf("Get: expected 2 owners, got %d", len(fetched.Owners))
	}
	if fetched.AckTriggers != ack {
		t.Fatalf("Get: expected ack_triggers=%q, got %q", ack, fetched.AckTriggers)
	}

	// Clearing owners + nil review_date should also persist cleanly.
	cleared, err := gs.SetGovernance(ctx, g.ID, nil, nil, domain.AckNone, domain.CadenceNone, nil, nil, nil)
	if err != nil {
		t.Fatalf("SetGovernance clear: %v", err)
	}
	if len(cleared.Owners) != 0 {
		t.Fatalf("expected empty owners after clear, got %v", cleared.Owners)
	}
	if cleared.ReviewDate != nil {
		t.Fatalf("expected nil review_date after clear, got %v", cleared.ReviewDate)
	}
}

func TestCategoryStoreSubtreeReturnsRootAndDescendants(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	root, _ := domain.NewCategory("Corp", "corp", uuid.Nil)
	root, err := gs.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child1, _ := domain.NewCategory("Finance", "finance", root.ID)
	child1, err = gs.Create(ctx, child1)
	if err != nil {
		t.Fatalf("Create child1: %v", err)
	}
	child2, _ := domain.NewCategory("HR", "hr", root.ID)
	child2, err = gs.Create(ctx, child2)
	if err != nil {
		t.Fatalf("Create child2: %v", err)
	}
	grand, _ := domain.NewCategory("Payroll", "payroll", child1.ID)
	grand, err = gs.Create(ctx, grand)
	if err != nil {
		t.Fatalf("Create grand: %v", err)
	}

	// Set governance on root so we can verify hydration in the subtree result.
	ownerID := uuid.New()
	rootADs := []string{"cn=corp"}
	_, err = gs.SetGovernance(ctx, root.ID, []uuid.UUID{ownerID}, &rootADs, domain.AckOnChange, domain.CadenceBiennial, nil, nil, nil)
	if err != nil {
		t.Fatalf("SetGovernance: %v", err)
	}

	subtree, err := gs.Subtree(ctx, root.ID)
	if err != nil {
		t.Fatalf("Subtree: %v", err)
	}

	if len(subtree) != 4 {
		t.Fatalf("expected 4 categories (root+3 descendants), got %d", len(subtree))
	}

	// Build a map for easy assertions.
	byID := make(map[uuid.UUID]domain.Category, len(subtree))
	for _, grp := range subtree {
		byID[grp.ID] = grp
	}

	for _, id := range []uuid.UUID{root.ID, child1.ID, child2.ID, grand.ID} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("Subtree missing category %v", id)
		}
	}

	// Governance fields should be hydrated.
	rootResult := byID[root.ID]
	if len(rootResult.Owners) != 1 || rootResult.Owners[0] != ownerID {
		t.Fatalf("Subtree: root owners mismatch, got %v", rootResult.Owners)
	}
	if rootResult.AckTriggers != domain.AckOnChange {
		t.Fatalf("Subtree: root ack_triggers mismatch, got %q", rootResult.AckTriggers)
	}
}

// TestCategoryStoreSetGovernanceExclusionGroupIDs verifies that:
//   - nil exclusionGroupIDs stores SQL NULL (inherit).
//   - a non-nil pointer to a non-empty slice stores the array.
//   - a non-nil pointer to an empty slice stores '{}' (explicit no-exclusion override).
//
// Also confirms that AncestorChain correctly hydrates ExclusionGroupIDs so
// EffectiveExclusionGroups can walk it.
func TestCategoryStoreSetGovernanceExclusionGroupIDs(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	root, _ := domain.NewCategory("Root", "root2", uuid.Nil)
	root, err := gs.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}

	// 1. Nil exclusion → stored as NULL → ExclusionGroupIDs is nil.
	updated, err := gs.SetGovernance(ctx, root.ID, nil, nil, domain.AckNone, domain.CadenceNone, nil, nil, nil)
	if err != nil {
		t.Fatalf("SetGovernance(nil exclusion): %v", err)
	}
	if updated.ExclusionGroupIDs != nil {
		t.Fatalf("expected nil ExclusionGroupIDs for NULL, got %v", *updated.ExclusionGroupIDs)
	}

	// 2. Non-nil pointer to non-empty slice → stored as array → pointer is non-nil.
	excl := []string{"excl-category-a", "excl-category-b"}
	updated, err = gs.SetGovernance(ctx, root.ID, nil, nil, domain.AckNone, domain.CadenceNone, nil, &excl, nil)
	if err != nil {
		t.Fatalf("SetGovernance(non-empty exclusion): %v", err)
	}
	if updated.ExclusionGroupIDs == nil {
		t.Fatal("expected non-nil ExclusionGroupIDs after setting array")
	}
	if len(*updated.ExclusionGroupIDs) != 2 {
		t.Fatalf("expected 2 exclusion categories, got %d: %v", len(*updated.ExclusionGroupIDs), *updated.ExclusionGroupIDs)
	}

	// 3. Non-nil pointer to empty slice → stored as '{}' → pointer is non-nil, len=0.
	emptyExcl := []string{}
	updated, err = gs.SetGovernance(ctx, root.ID, nil, nil, domain.AckNone, domain.CadenceNone, nil, &emptyExcl, nil)
	if err != nil {
		t.Fatalf("SetGovernance(empty exclusion override): %v", err)
	}
	if updated.ExclusionGroupIDs == nil {
		t.Fatal("expected non-nil ExclusionGroupIDs pointer for explicit empty override")
	}
	if len(*updated.ExclusionGroupIDs) != 0 {
		t.Fatalf("expected empty exclusion category list, got %v", *updated.ExclusionGroupIDs)
	}

	// 4. AncestorChain correctly propagates ExclusionGroupIDs.
	child, _ := domain.NewCategory("Child", "child2", root.ID)
	child, err = gs.Create(ctx, child)
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	// Root has non-nil exclusion (from step 2 then overridden to empty in step 3,
	// then restore to non-empty).
	rootExcl := []string{"root-excl"}
	_, err = gs.SetGovernance(ctx, root.ID, nil, nil, domain.AckNone, domain.CadenceNone, nil, &rootExcl, nil)
	if err != nil {
		t.Fatalf("SetGovernance root excl restore: %v", err)
	}
	// Child has nil exclusion → should inherit root's.
	chain, err := gs.AncestorChain(ctx, child.ID)
	if err != nil {
		t.Fatalf("AncestorChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected chain of 2, got %d", len(chain))
	}
	// chain[0] = child (nil exclusion), chain[1] = root (non-nil exclusion)
	if chain[0].ExclusionGroupIDs != nil {
		t.Fatalf("child should have nil ExclusionGroupIDs (inherits), got %v", chain[0].ExclusionGroupIDs)
	}
	if chain[1].ExclusionGroupIDs == nil {
		t.Fatal("root should have non-nil ExclusionGroupIDs")
	}
	got := domain.EffectiveExclusionGroups(chain)
	if len(got) != 1 || got[0] != "root-excl" {
		t.Fatalf("EffectiveExclusionGroups via chain: expected [root-excl], got %v", got)
	}
}

// TestCategoryStoreSetGovernanceAudienceGroupIDs verifies that:
//   - nil audienceGroupIDs stores SQL NULL (inherit).
//   - a non-nil pointer to a non-empty slice stores the array.
//   - a non-nil pointer to an empty slice stores '{}' (explicit no-audience override).
//
// Also confirms that AncestorChain correctly hydrates AudienceGroupIDs so
// EffectiveAudienceGroups can walk it.
func TestCategoryStoreSetGovernanceAudienceGroupIDs(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	root, _ := domain.NewCategory("Root", "root3", uuid.Nil)
	root, err := gs.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}

	// 1. Nil audienceGroupIDs → stored as NULL → AudienceGroupIDs is nil.
	updated, err := gs.SetGovernance(ctx, root.ID, nil, nil, domain.AckNone, domain.CadenceNone, nil, nil, nil)
	if err != nil {
		t.Fatalf("SetGovernance(nil audienceGroupIDs): %v", err)
	}
	if updated.AudienceGroupIDs != nil {
		t.Fatalf("expected nil AudienceGroupIDs for NULL, got %v", *updated.AudienceGroupIDs)
	}

	// 2. Non-nil pointer to non-empty slice → stored as array → pointer is non-nil.
	ads := []string{"ad-category-a", "ad-category-b"}
	updated, err = gs.SetGovernance(ctx, root.ID, nil, &ads, domain.AckNone, domain.CadenceNone, nil, nil, nil)
	if err != nil {
		t.Fatalf("SetGovernance(non-empty audienceGroupIDs): %v", err)
	}
	if updated.AudienceGroupIDs == nil {
		t.Fatal("expected non-nil AudienceGroupIDs after setting array")
	}
	if len(*updated.AudienceGroupIDs) != 2 {
		t.Fatalf("expected 2 ad categories, got %d: %v", len(*updated.AudienceGroupIDs), *updated.AudienceGroupIDs)
	}

	// 3. Non-nil pointer to empty slice → stored as '{}' → pointer is non-nil, len=0.
	emptyADs := []string{}
	updated, err = gs.SetGovernance(ctx, root.ID, nil, &emptyADs, domain.AckNone, domain.CadenceNone, nil, nil, nil)
	if err != nil {
		t.Fatalf("SetGovernance(empty audienceGroupIDs override): %v", err)
	}
	if updated.AudienceGroupIDs == nil {
		t.Fatal("expected non-nil AudienceGroupIDs pointer for explicit empty override")
	}
	if len(*updated.AudienceGroupIDs) != 0 {
		t.Fatalf("expected empty ad category list, got %v", *updated.AudienceGroupIDs)
	}

	// 4. AncestorChain correctly propagates AudienceGroupIDs for EffectiveAudienceGroups.
	child, _ := domain.NewCategory("Child", "child3", root.ID)
	child, err = gs.Create(ctx, child)
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	// Restore root to a non-empty set.
	rootADs := []string{"root-ad"}
	_, err = gs.SetGovernance(ctx, root.ID, nil, &rootADs, domain.AckNone, domain.CadenceNone, nil, nil, nil)
	if err != nil {
		t.Fatalf("SetGovernance root ad restore: %v", err)
	}
	// Child has nil audienceGroupIDs → should inherit root's via EffectiveAudienceGroups.
	chain, err := gs.AncestorChain(ctx, child.ID)
	if err != nil {
		t.Fatalf("AncestorChain: %v", err)
	}
	if len(chain) != 2 {
		t.Fatalf("expected chain of 2, got %d", len(chain))
	}
	// chain[0] = child (nil AudienceGroupIDs), chain[1] = root (non-nil AudienceGroupIDs)
	if chain[0].AudienceGroupIDs != nil {
		t.Fatalf("child should have nil AudienceGroupIDs (inherits), got %v", chain[0].AudienceGroupIDs)
	}
	if chain[1].AudienceGroupIDs == nil {
		t.Fatal("root should have non-nil AudienceGroupIDs")
	}
	effective := domain.EffectiveAudienceGroups(chain)
	if len(effective) != 1 || effective[0] != "root-ad" {
		t.Fatalf("EffectiveAudienceGroups via chain: expected [root-ad], got %v", effective)
	}
}

func TestCategoryStoreRename(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	g, _ := domain.NewCategory("Old Name", "old-slug", uuid.Nil)
	g, err := s.Create(ctx, g)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	renamed, err := s.Rename(ctx, g.ID, "New Name", "new-slug")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Name != "New Name" || renamed.Slug != "new-slug" {
		t.Fatalf("got %+v", renamed)
	}

	// Persisted.
	got, err := s.Get(ctx, g.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "New Name" || got.Slug != "new-slug" {
		t.Fatalf("persisted %+v", got)
	}

	// Unknown id → ErrCategoryNotFound.
	if _, err := s.Rename(ctx, uuid.New(), "x", "x"); !errors.Is(err, store.ErrCategoryNotFound) {
		t.Fatalf("expected ErrCategoryNotFound, got %v", err)
	}

	// Sibling slug collision → ErrCategorySlugConflict. The uniqueness constraint is
	// (parent_id, slug); NULL parents are distinct in Postgres, so use two
	// children under a shared parent to exercise the collision.
	parent, _ := domain.NewCategory("Parent", "parent", uuid.Nil)
	parent, err = s.Create(ctx, parent)
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	a, _ := domain.NewCategory("A", "a", parent.ID)
	if _, err := s.Create(ctx, a); err != nil {
		t.Fatalf("Create sibling a: %v", err)
	}
	b, _ := domain.NewCategory("B", "b", parent.ID)
	b, err = s.Create(ctx, b)
	if err != nil {
		t.Fatalf("Create sibling b: %v", err)
	}
	if _, err := s.Rename(ctx, b.ID, "B", "a"); !errors.Is(err, store.ErrCategorySlugConflict) {
		t.Fatalf("expected ErrCategorySlugConflict, got %v", err)
	}
}

// TestCategoryStoreDeleteGuard exercises the delete guard: a category whose CHILD owns
// a policy is refused, while an empty category (with an empty child) deletes and its
// rows are gone.
func TestCategoryStoreDeleteGuard(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	// Reuse the shared helper only for a published template version.
	_, tv := setupCategoryAndTemplate(t, gs, ts)

	// --- Case 1: root -> child, policy lives in the child -> blocked. -------
	root, _ := domain.NewCategory("Blocked Root", "blocked-root", uuid.Nil)
	root, err := gs.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, _ := domain.NewCategory("Blocked Child", "blocked-child", root.ID)
	child, err = gs.Create(ctx, child)
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	p, _ := domain.NewPolicy("Child Policy", child.ID, domain.SensitivityStandard, uuid.New())
	if _, err := ps.CreatePolicy(ctx, p); err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// Deleting the ROOT must be refused because a descendant owns a policy.
	if err := gs.Delete(ctx, root.ID); !errors.Is(err, store.ErrCategoryHasPolicies) {
		t.Fatalf("expected ErrCategoryHasPolicies deleting root, got %v", err)
	}
	// Deleting the child directly is also refused (it owns the policy).
	if err := gs.Delete(ctx, child.ID); !errors.Is(err, store.ErrCategoryHasPolicies) {
		t.Fatalf("expected ErrCategoryHasPolicies deleting child, got %v", err)
	}
	// Both rows still present.
	if _, err := gs.Get(ctx, root.ID); err != nil {
		t.Fatalf("root should still exist: %v", err)
	}
	if _, err := gs.Get(ctx, child.ID); err != nil {
		t.Fatalf("child should still exist: %v", err)
	}

	_ = tv // template version only needed to satisfy policy creation FK chain

	// --- Case 2: empty root + empty child -> deletes whole subtree. ---------
	eRoot, _ := domain.NewCategory("Empty Root", "empty-root", uuid.Nil)
	eRoot, err = gs.Create(ctx, eRoot)
	if err != nil {
		t.Fatalf("Create empty root: %v", err)
	}
	eChild, _ := domain.NewCategory("Empty Child", "empty-child", eRoot.ID)
	eChild, err = gs.Create(ctx, eChild)
	if err != nil {
		t.Fatalf("Create empty child: %v", err)
	}

	if err := gs.Delete(ctx, eRoot.ID); err != nil {
		t.Fatalf("Delete empty subtree: %v", err)
	}
	if _, err := gs.Get(ctx, eRoot.ID); err == nil {
		t.Fatal("empty root should be gone")
	}
	if _, err := gs.Get(ctx, eChild.ID); err == nil {
		t.Fatal("empty child should be gone (cascade)")
	}

	// Unknown id → ErrCategoryNotFound.
	if err := gs.Delete(ctx, uuid.New()); !errors.Is(err, store.ErrCategoryNotFound) {
		t.Fatalf("expected ErrCategoryNotFound, got %v", err)
	}
}

// TestCategoryStoreMoveCategory exercises MoveCategory end-to-end against Postgres:
// a valid re-parent updates parent_id, move-to-root clears it, a cycle is
// rejected, the depth-3 boundary is honored (depth 3 OK, depth 4 fails), and a
// failed move applies nothing.
func TestCategoryStoreMoveCategory(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	mk := func(name, slug string, parent uuid.UUID) domain.Category {
		g, err := domain.NewCategory(name, slug, parent)
		if err != nil {
			t.Fatalf("NewCategory %s: %v", name, err)
		}
		g, err = gs.Create(ctx, g)
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return g
	}

	// --- Case 1: valid re-parent (top-level under another top-level). --------
	a := mk("A", "a", uuid.Nil)
	b := mk("B", "b", uuid.Nil)
	got, affected, err := gs.MoveCategory(ctx, b.ID, &a.ID)
	if err != nil {
		t.Fatalf("MoveCategory b->a: %v", err)
	}
	if got.ParentID != a.ID {
		t.Fatalf("parent: got %v want %v", got.ParentID, a.ID)
	}
	if affected != 1 {
		t.Fatalf("affected: got %d want 1", affected)
	}

	// --- Case 2: move to root (promote child to top-level). ------------------
	got, _, err = gs.MoveCategory(ctx, b.ID, nil)
	if err != nil {
		t.Fatalf("MoveCategory b->root: %v", err)
	}
	if got.ParentID != uuid.Nil {
		t.Fatalf("expected root (Nil parent), got %v", got.ParentID)
	}

	// --- Case 3: cycle — move a under its own descendant. --------------------
	// Build a -> c so c is a descendant of a; moving a under c is a cycle.
	c := mk("C", "c", a.ID)
	if _, _, err := gs.MoveCategory(ctx, a.ID, &c.ID); !errors.Is(err, store.ErrCategoryMoveCycle) {
		t.Fatalf("expected ErrCategoryMoveCycle, got %v", err)
	}
	// Nothing applied: a is still a root.
	if aa, err := gs.Get(ctx, a.ID); err != nil || aa.ParentID != uuid.Nil {
		t.Fatalf("cycle move must not change a; parent=%v err=%v", aa.ParentID, err)
	}

	// --- Case 4: depth boundary. --------------------------------------------
	// Fresh tree: mid -> leaf (mid's subtree height = 2). Moving mid under a
	// depth-1 root lands leaf at depth 3 -> OK.
	top := mk("Top", "top", uuid.Nil)
	mid := mk("Mid", "mid", uuid.Nil)
	_ = mk("Leaf", "leaf", mid.ID)
	if _, _, err := gs.MoveCategory(ctx, mid.ID, &top.ID); err != nil {
		t.Fatalf("depth-3 move should succeed: %v", err)
	}
	// Now top(1)->mid(2)->leaf(3): top's subtree height = 3. Moving top under
	// another root would land leaf at depth 4 -> FAIL, nothing applied.
	other := mk("Other", "other", uuid.Nil)
	if _, _, err := gs.MoveCategory(ctx, top.ID, &other.ID); !errors.Is(err, store.ErrCategoryMoveTooDeep) {
		t.Fatalf("expected ErrCategoryMoveTooDeep, got %v", err)
	}
	if tt, err := gs.Get(ctx, top.ID); err != nil || tt.ParentID != uuid.Nil {
		t.Fatalf("too-deep move must not change top; parent=%v err=%v", tt.ParentID, err)
	}

	// --- Case 5: unknown category -> NotFound. ---------------------------------
	if _, _, err := gs.MoveCategory(ctx, uuid.New(), &a.ID); !errors.Is(err, store.ErrCategoryNotFound) {
		t.Fatalf("expected ErrCategoryNotFound, got %v", err)
	}
}

func TestCategoryStore_CategoryRuleset_RoundTrip(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	// A category to attach rules to.
	cat, err := domain.NewCategory("RACI-Test", "raci-test", uuid.Nil)
	if err != nil {
		t.Fatalf("domain.NewCategory: %v", err)
	}
	cat, err = s.Create(ctx, cat)
	if err != nil {
		t.Fatalf("Create category: %v", err)
	}

	// Initial get: empty ruleset.
	rules, err := s.GetCategoryRuleset(ctx, cat.ID)
	if err != nil {
		t.Fatalf("GetCategoryRuleset (empty): %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected empty ruleset, got %d rules", len(rules))
	}

	// Set a 2-rule ruleset.
	input := []domain.CategoryRule{
		{SubjectKind: "everyone", SubjectRef: "", Read: "allow", Ack: "allow", Approve: "", Author: ""},
		{SubjectKind: "group", SubjectRef: "finance-approvers", Read: "allow", Ack: "allow", Approve: "allow", Author: "allow"},
	}
	got, err := s.SetCategoryRuleset(ctx, cat.ID, input)
	if err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rules back, got %d", len(got))
	}
	if got[0].Ordinal != 1 || got[1].Ordinal != 2 {
		t.Fatalf("ordinals: got %d,%d want 1,2", got[0].Ordinal, got[1].Ordinal)
	}
	if got[0].SubjectKind != "everyone" || got[0].Read != "allow" {
		t.Fatalf("rule 0 mismatch: %+v", got[0])
	}
	if got[1].SubjectKind != "group" || got[1].SubjectRef != "finance-approvers" || got[1].Approve != "allow" {
		t.Fatalf("rule 1 mismatch: %+v", got[1])
	}

	// Full replace: set a different single-rule ruleset and assert old rows gone.
	replacement := []domain.CategoryRule{
		{SubjectKind: "user", SubjectRef: "u-1234", Read: "deny", Ack: "", Approve: "", Author: ""},
	}
	got2, err := s.SetCategoryRuleset(ctx, cat.ID, replacement)
	if err != nil {
		t.Fatalf("SetCategoryRuleset (replace): %v", err)
	}
	if len(got2) != 1 {
		t.Fatalf("expected 1 rule after replace, got %d", len(got2))
	}
	if got2[0].SubjectKind != "user" || got2[0].SubjectRef != "u-1234" || got2[0].Read != "deny" {
		t.Fatalf("replacement rule mismatch: %+v", got2[0])
	}

	// Re-read via Get to confirm persistence.
	persisted, err := s.GetCategoryRuleset(ctx, cat.ID)
	if err != nil {
		t.Fatalf("GetCategoryRuleset after replace: %v", err)
	}
	if len(persisted) != 1 || persisted[0].SubjectKind != "user" {
		t.Fatalf("persisted mismatch: %+v", persisted)
	}
}

// TestCategoryStore_PurgeUserCategoryRules checks that a deleted account's
// user-subject rules go with it and nothing else moves. The fixture seeds a
// user-subject deny rule beside every neighbouring rule kind the purge must
// leave alone.
func TestCategoryStore_PurgeUserCategoryRules(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	victim := uuid.New()
	bystander := uuid.New()

	mkCat := func(name, slug string) domain.Category {
		g, err := domain.NewCategory(name, slug+"-"+uuid.New().String()[:8], uuid.Nil)
		if err != nil {
			t.Fatalf("domain.NewCategory(%s): %v", name, err)
		}
		g, err = gs.Create(ctx, g)
		if err != nil {
			t.Fatalf("Create(%s): %v", name, err)
		}
		return g
	}
	catA, catB, catC := mkCat("Alpha", "alpha"), mkCat("Bravo", "bravo"), mkCat("Charlie", "charlie")

	// catA: the victim's rule sits in the MIDDLE of the ruleset, so the purge
	// also has to prove it leaves the surviving rules in their original
	// precedence order (an allow/deny firewall is order-sensitive).
	if _, err := gs.SetCategoryRuleset(ctx, catA.ID, []domain.CategoryRule{
		{SubjectKind: "everyone", Read: "allow"},
		{SubjectKind: "user", SubjectRef: victim.String(), Ack: "deny"}, // the rule the purge must remove
		{SubjectKind: "group", SubjectRef: "finance-approvers", Approve: "allow"},
		{SubjectKind: "user", SubjectRef: bystander.String(), Read: "allow"},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset(catA): %v", err)
	}
	// catB: the victim holds TWO rules in one category — both must go, and the
	// category must be reported once, not twice.
	if _, err := gs.SetCategoryRuleset(ctx, catB.ID, []domain.CategoryRule{
		{SubjectKind: "user", SubjectRef: victim.String(), Author: "allow"},
		{SubjectKind: "user", SubjectRef: victim.String(), Read: "allow"},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset(catB): %v", err)
	}
	// catC: a CATEGORY-subject rule whose subject_ref is byte-identical to the
	// victim's user id. Scoping on subject_ref alone would delete it.
	if _, err := gs.SetCategoryRuleset(ctx, catC.ID, []domain.CategoryRule{
		{SubjectKind: "group", SubjectRef: victim.String(), Read: "allow"},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset(catC): %v", err)
	}

	// The victim is also a category owner and a policy owner. Ownership moves
	// only with a target user (ReassignUserPolicies), and authorship keeps
	// naming whoever acted.
	if _, err := gs.SetGovernance(ctx, catA.ID, []uuid.UUID{victim}, nil,
		domain.AckNone, domain.CadenceNone, nil, nil, nil); err != nil {
		t.Fatalf("SetGovernance(catA): %v", err)
	}
	g, _ := setupCategoryAndTemplate(t, gs, ts)
	pol, err := domain.NewPolicy("Owned By Victim", g.ID, domain.SensitivityStandard, victim)
	if err != nil {
		t.Fatalf("domain.NewPolicy: %v", err)
	}
	pol, err = ps.CreatePolicy(ctx, pol)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// --- Case 1: dry run reports the same three rows and mutates nothing. ----
	preview, err := gs.PurgeUserCategoryRules(ctx, victim, true)
	if err != nil {
		t.Fatalf("PurgeUserCategoryRules(dry run): %v", err)
	}
	if len(preview) != 3 {
		t.Fatalf("dry run: got %d rules, want 3 (%+v)", len(preview), preview)
	}
	for _, p := range preview {
		if p.Rule.SubjectKind != "user" || p.Rule.SubjectRef != victim.String() {
			t.Fatalf("dry run returned a rule outside scope: %+v", p)
		}
	}
	if rules, err := gs.GetCategoryRuleset(ctx, catA.ID); err != nil || len(rules) != 4 {
		t.Fatalf("dry run mutated catA: %d rules (err=%v)", len(rules), err)
	}
	if rules, err := gs.GetCategoryRuleset(ctx, catB.ID); err != nil || len(rules) != 2 {
		t.Fatalf("dry run mutated catB: %d rules (err=%v)", len(rules), err)
	}

	// --- Case 2: the real purge removes exactly the user-subject rules. ------
	removed, err := gs.PurgeUserCategoryRules(ctx, victim, false)
	if err != nil {
		t.Fatalf("PurgeUserCategoryRules: %v", err)
	}
	if len(removed) != 3 {
		t.Fatalf("purge: got %d rules removed, want 3 (%+v)", len(removed), removed)
	}
	affected := map[uuid.UUID]int{}
	for _, r := range removed {
		affected[r.CategoryID]++
		if r.Rule.SubjectKind != "user" || r.Rule.SubjectRef != victim.String() {
			t.Fatalf("purge removed a rule outside scope: %+v", r)
		}
	}
	if affected[catA.ID] != 1 || affected[catB.ID] != 2 || len(affected) != 2 {
		t.Fatalf("affected categories: %v (want catA x1, catB x2, catC absent)", affected)
	}
	// The removed rows must carry their grants so the audit trail records WHAT
	// was revoked, not merely that something was.
	var sawAckDeny bool
	for _, r := range removed {
		if r.CategoryID == catA.ID && r.Rule.Ack == "deny" {
			sawAckDeny = true
		}
	}
	if !sawAckDeny {
		t.Fatalf("removed rows lost their grants — cannot audit what was revoked: %+v", removed)
	}

	// --- Case 3: what survived, and in what order. --------------------------
	type wantRule struct {
		kind, ref string
	}
	for _, tc := range []struct {
		name string
		cat  uuid.UUID
		want []wantRule
	}{
		{
			name: "catA keeps everyone/category/bystander in precedence order",
			cat:  catA.ID,
			want: []wantRule{
				{"everyone", ""},
				{"group", "finance-approvers"},
				{"user", bystander.String()},
			},
		},
		{
			name: "catB is emptied (both rules were the victim's)",
			cat:  catB.ID,
			want: nil,
		},
		{
			name: "catC category-subject rule survives despite matching subject_ref",
			cat:  catC.ID,
			want: []wantRule{{"group", victim.String()}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gs.GetCategoryRuleset(ctx, tc.cat)
			if err != nil {
				t.Fatalf("GetCategoryRuleset: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rules, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if string(got[i].SubjectKind) != w.kind || got[i].SubjectRef != w.ref {
					t.Fatalf("rule %d: got %s/%s, want %s/%s", i,
						got[i].SubjectKind, got[i].SubjectRef, w.kind, w.ref)
				}
				// Ordinals are re-closed to 1..n so the next full-ruleset
				// replace does not read as a reorder of every surviving rule.
				if got[i].Ordinal != i+1 {
					t.Fatalf("rule %d ordinal: got %d, want %d", i, got[i].Ordinal, i+1)
				}
			}
		})
	}

	// --- Case 4: nothing outside category_rules moved. ----------------------
	ownerCategory, err := gs.Get(ctx, catA.ID)
	if err != nil {
		t.Fatalf("Get(catA): %v", err)
	}
	if len(ownerCategory.Owners) != 1 || ownerCategory.Owners[0] != victim {
		t.Fatalf("category owners must be untouched (they need a target user to move to): %v", ownerCategory.Owners)
	}
	gotPol, err := ps.GetPolicy(ctx, pol.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if gotPol.OwnerUserID != victim {
		t.Fatalf("policy owner must be untouched: got %v want %v", gotPol.OwnerUserID, victim)
	}
	var createdBy uuid.UUID
	if err := pool.Pool().QueryRow(ctx,
		`SELECT created_by FROM policy_versions WHERE policy_id = $1 ORDER BY version_no LIMIT 1`,
		pol.ID).Scan(&createdBy); err != nil {
		t.Fatalf("read policy_versions.created_by: %v", err)
	}
	if createdBy != victim {
		t.Fatalf("historical authorship must keep naming who acted: got %v want %v", createdBy, victim)
	}

	// --- Case 5: idempotent — a second purge removes nothing, errors nothing. -
	again, err := gs.PurgeUserCategoryRules(ctx, victim, false)
	if err != nil {
		t.Fatalf("PurgeUserCategoryRules (second call): %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second purge was not a no-op: %+v", again)
	}
	if rules, err := gs.GetCategoryRuleset(ctx, catA.ID); err != nil || len(rules) != 3 {
		t.Fatalf("second purge changed catA: %d rules (err=%v)", len(rules), err)
	}

	// --- Case 6: a user with no rules at all is a clean no-op. --------------
	if got, err := gs.PurgeUserCategoryRules(ctx, uuid.New(), false); err != nil || len(got) != 0 {
		t.Fatalf("purge for an unknown user: got %+v err=%v", got, err)
	}
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// TestCategoryCreateAssignsCollisionSafeCode checks that two categories whose
// slugs derive the same base code (both "Information *" give INFORM) get
// distinct stored codes, and that a policy in each carries its own code.
func TestCategoryCreateAssignsCollisionSafeCode(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	sec, err := domain.NewCategory("Information Security", "information-security", uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory sec: %v", err)
	}
	if sec, err = gs.Create(ctx, sec); err != nil {
		t.Fatalf("Create sec: %v", err)
	}
	tech, err := domain.NewCategory("Information Technology", "information-technology", uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory tech: %v", err)
	}
	if tech, err = gs.Create(ctx, tech); err != nil {
		t.Fatalf("Create tech: %v", err)
	}

	p1, _ := domain.NewPolicy("Sec Policy", sec.ID, domain.SensitivityStandard, uuid.New())
	p1, err = ps.CreatePolicy(ctx, p1)
	if err != nil {
		t.Fatalf("CreatePolicy sec: %v", err)
	}
	p2, _ := domain.NewPolicy("Tech Policy", tech.ID, domain.SensitivityStandard, uuid.New())
	p2, err = ps.CreatePolicy(ctx, p2)
	if err != nil {
		t.Fatalf("CreatePolicy tech: %v", err)
	}

	if p1.Number == p2.Number {
		t.Fatalf("collision: both policies numbered %q", p1.Number)
	}
	if p1.Number != "POL-INFORM-000001" {
		t.Fatalf("sec policy number: got %q, want POL-INFORM-000001", p1.Number)
	}
	if p2.Number != "POL-INFORM2-000001" {
		t.Fatalf("tech policy number: got %q, want POL-INFORM2-000001", p2.Number)
	}
}

// TestRenameCascadesPolicyNumber checks that renaming a category re-derives
// its code and every policy number follows it, each keeping its sequence.
func TestRenameCascadesPolicyNumber(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	g, err := domain.NewCategory("Information Technology", "information-technology", uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	if g, err = gs.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}

	p, _ := domain.NewPolicy("AI Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err = ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if p.Number != "POL-INFORM-000001" || p.Sequence != 1 {
		t.Fatalf("create: got number=%q seq=%d, want POL-INFORM-000001 / 1", p.Number, p.Sequence)
	}

	// Rename the category slug so its derived code becomes IT2.
	renamed, err := gs.Rename(ctx, g.ID, "IT2", "it2")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if renamed.Slug != "it2" {
		t.Fatalf("slug not updated: got %q", renamed.Slug)
	}
	// categories.code was re-derived from the new slug.
	var code string
	if err := pool.Pool().QueryRow(ctx, `SELECT code FROM categories WHERE id = $1`, g.ID).Scan(&code); err != nil {
		t.Fatalf("read code: %v", err)
	}
	if code != "IT2" {
		t.Fatalf("code not re-derived: got %q, want IT2", code)
	}

	// The existing policy must now render from the new code, sequence unchanged.
	got, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Number != "POL-IT2-000001" {
		t.Fatalf("cascade: got %q, want POL-IT2-000001", got.Number)
	}
	if got.Sequence != 1 {
		t.Fatalf("sequence changed: got %d, want 1", got.Sequence)
	}
}

// TestRenameDisambiguatesCollidingCode verifies that when a rename's new slug
// derives to a code already held by ANOTHER category, the renamed category gets
// a disambiguated suffix (the same rule Create uses) rather than colliding.
func TestRenameDisambiguatesCollidingCode(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	occupier, err := domain.NewCategory("Finance", "finance", uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory finance: %v", err)
	}
	if occupier, err = gs.Create(ctx, occupier); err != nil {
		t.Fatalf("Create finance: %v", err)
	}
	// occupier now holds code FINANC (PolicyNumberCode("finance")).

	other, err := domain.NewCategory("Facilities", "facilities", uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory facilities: %v", err)
	}
	if other, err = gs.Create(ctx, other); err != nil {
		t.Fatalf("Create facilities: %v", err)
	}

	// Rename "other" to a slug whose code collides with the occupier's FINANC.
	renamed, err := gs.Rename(ctx, other.ID, "Finance Ops", "financ")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	var otherCode, occCode string
	if err := pool.Pool().QueryRow(ctx, `SELECT code FROM categories WHERE id = $1`, renamed.ID).Scan(&otherCode); err != nil {
		t.Fatalf("read other code: %v", err)
	}
	if err := pool.Pool().QueryRow(ctx, `SELECT code FROM categories WHERE id = $1`, occupier.ID).Scan(&occCode); err != nil {
		t.Fatalf("read occupier code: %v", err)
	}
	if otherCode == occCode {
		t.Fatalf("codes collided: both %q", otherCode)
	}
	if occCode != "FINANC" || otherCode != "FINANC2" {
		t.Fatalf("disambiguation: occupier=%q renamed=%q, want FINANC / FINANC2", occCode, otherCode)
	}
}

// TestRenameKeepsCodeWhenBaseUnchanged verifies a name-only rename (or one whose
// slug still derives to the same base code) is idempotent on the code: the
// category's own code is freed from the taken set, so it is not needlessly bumped
// to a suffixed variant.
func TestRenameKeepsCodeWhenBaseUnchanged(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	g, err := domain.NewCategory("Human Resources", "human-resources", uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	if g, err = gs.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	var before string
	if err := pool.Pool().QueryRow(ctx, `SELECT code FROM categories WHERE id = $1`, g.ID).Scan(&before); err != nil {
		t.Fatalf("read code: %v", err)
	}

	// Change only the display name; slug (and thus the derived base code) is unchanged.
	if _, err := gs.Rename(ctx, g.ID, "People & Culture", "human-resources"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	var after string
	if err := pool.Pool().QueryRow(ctx, `SELECT code FROM categories WHERE id = $1`, g.ID).Scan(&after); err != nil {
		t.Fatalf("read code: %v", err)
	}
	if after != before {
		t.Fatalf("code changed on base-unchanged rename: %q -> %q", before, after)
	}
}

// TestCategoryCreateRejectsDuplicateSlug checks slug uniqueness among roots:
// the index keys on COALESCE(parent_id, <nil sentinel>), so all roots share
// one bucket, while children under different parents may share a segment
// (see TestChildSegmentSharedAcrossParents).
func TestCategoryCreateRejectsDuplicateSlug(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	a, _ := domain.NewCategory("Alpha", "dup-slug", uuid.Nil)
	if _, err := gs.Create(ctx, a); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, _ := domain.NewCategory("Beta", "dup-slug", uuid.Nil)
	_, err := gs.Create(ctx, b)
	if !errors.Is(err, store.ErrCategorySlugConflict) {
		t.Fatalf("expected ErrCategorySlugConflict, got %v", err)
	}
}

// TestChildInheritsSlugPathCode checks that a child's code comes from its
// effective slug path (parent segment plus its own), the root's code is
// unchanged, and a policy in the child renders from the child's code.
func TestChildInheritsSlugPathCode(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	root, err := domain.NewCategory("IT", "it", uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory root: %v", err)
	}
	if root, err = gs.Create(ctx, root); err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, err := domain.NewCategory("Security", "security", root.ID)
	if err != nil {
		t.Fatalf("NewCategory child: %v", err)
	}
	if child, err = gs.Create(ctx, child); err != nil {
		t.Fatalf("Create child: %v", err)
	}

	rootCode := readCategoryCode(t, pool, root.ID)
	childCode := readCategoryCode(t, pool, child.ID)
	if rootCode != "IT" {
		t.Fatalf("root code: got %q, want IT (root numbering must be unchanged)", rootCode)
	}
	// PolicyNumberCode("it-security") strips the dash and caps at 6 -> ITSECU.
	if childCode != "ITSECU" {
		t.Fatalf("child code: got %q, want ITSECU (derived from effective path it-security)", childCode)
	}

	p, _ := domain.NewPolicy("Child Policy", child.ID, domain.SensitivityStandard, uuid.New())
	if p, err = ps.CreatePolicy(ctx, p); err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if p.Number != "POL-ITSECU-000001" {
		t.Fatalf("child policy number: got %q, want POL-ITSECU-000001", p.Number)
	}
}

// TestChildSegmentSharedAcrossParents verifies the new capability: two children
// under DIFFERENT parents may share the same local slug segment (previously
// blocked by the global slug constraint), and still receive DISTINCT, collision-
// safe codes because the code derives from the full effective path.
func TestChildSegmentSharedAcrossParents(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	it, _ := domain.NewCategory("IT", "it", uuid.Nil)
	it, err := gs.Create(ctx, it)
	if err != nil {
		t.Fatalf("Create it: %v", err)
	}
	hr, _ := domain.NewCategory("HR", "hr", uuid.Nil)
	hr, err = gs.Create(ctx, hr)
	if err != nil {
		t.Fatalf("Create hr: %v", err)
	}

	itSec, _ := domain.NewCategory("Security", "security", it.ID)
	if itSec, err = gs.Create(ctx, itSec); err != nil {
		t.Fatalf("Create it/security: %v", err)
	}
	hrSec, _ := domain.NewCategory("Security", "security", hr.ID)
	if hrSec, err = gs.Create(ctx, hrSec); err != nil {
		t.Fatalf("Create hr/security (shared segment must be allowed): %v", err)
	}

	itSecCode := readCategoryCode(t, pool, itSec.ID)
	hrSecCode := readCategoryCode(t, pool, hrSec.ID)
	if itSecCode == hrSecCode {
		t.Fatalf("codes collided: both %q", itSecCode)
	}
	if itSecCode != "ITSECU" || hrSecCode != "HRSECU" {
		t.Fatalf("hierarchy-aware codes: it/security=%q hr/security=%q, want ITSECU / HRSECU", itSecCode, hrSecCode)
	}
}

// TestSameParentDuplicateSegmentRejected verifies siblings under the SAME parent
// still cannot share a slug segment.
func TestSameParentDuplicateSegmentRejected(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	root, _ := domain.NewCategory("IT", "it", uuid.Nil)
	root, err := gs.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	a, _ := domain.NewCategory("Security A", "security", root.ID)
	if _, err := gs.Create(ctx, a); err != nil {
		t.Fatalf("Create a: %v", err)
	}
	b, _ := domain.NewCategory("Security B", "security", root.ID)
	if _, err := gs.Create(ctx, b); !errors.Is(err, store.ErrCategorySlugConflict) {
		t.Fatalf("expected ErrCategorySlugConflict for duplicate sibling segment, got %v", err)
	}
}

// TestRenameParentCascadesDescendantCodes verifies that renaming a parent's slug
// segment cascades hierarchy-aware code re-derivation to its descendants: the
// child's code (and its policies' numbers) track the parent's new segment.
func TestRenameParentCascadesDescendantCodes(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	root, _ := domain.NewCategory("IT", "it", uuid.Nil)
	root, err := gs.Create(ctx, root)
	if err != nil {
		t.Fatalf("Create root: %v", err)
	}
	child, _ := domain.NewCategory("Security", "security", root.ID)
	if child, err = gs.Create(ctx, child); err != nil {
		t.Fatalf("Create child: %v", err)
	}
	p, _ := domain.NewPolicy("Child Policy", child.ID, domain.SensitivityStandard, uuid.New())
	if p, err = ps.CreatePolicy(ctx, p); err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if p.Number != "POL-ITSECU-000001" {
		t.Fatalf("pre-rename child policy number: got %q, want POL-ITSECU-000001", p.Number)
	}

	// Rename the ROOT's segment it -> tech. The child's effective path becomes
	// tech-security -> code TECHSE, cascading to its policy's number.
	if _, err := gs.Rename(ctx, root.ID, "Technology", "tech"); err != nil {
		t.Fatalf("Rename root: %v", err)
	}
	if got := readCategoryCode(t, pool, root.ID); got != "TECH" {
		t.Fatalf("root code after rename: got %q, want TECH", got)
	}
	if got := readCategoryCode(t, pool, child.ID); got != "TECHSE" {
		t.Fatalf("child code after parent rename: got %q, want TECHSE", got)
	}
	got, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Number != "POL-TECHSE-000001" {
		t.Fatalf("cascade to descendant policy: got %q, want POL-TECHSE-000001", got.Number)
	}
}

// TestMoveReDerivesSubtreeCode verifies that moving a category to a new parent
// re-derives its (and its subtree's) hierarchy-aware code from the new effective
// path.
func TestMoveReDerivesSubtreeCode(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ctx := context.Background()

	it, _ := domain.NewCategory("IT", "it", uuid.Nil)
	it, err := gs.Create(ctx, it)
	if err != nil {
		t.Fatalf("Create it: %v", err)
	}
	hr, _ := domain.NewCategory("HR", "hr", uuid.Nil)
	hr, err = gs.Create(ctx, hr)
	if err != nil {
		t.Fatalf("Create hr: %v", err)
	}
	sec, _ := domain.NewCategory("Security", "security", it.ID)
	if sec, err = gs.Create(ctx, sec); err != nil {
		t.Fatalf("Create it/security: %v", err)
	}
	if got := readCategoryCode(t, pool, sec.ID); got != "ITSECU" {
		t.Fatalf("pre-move code: got %q, want ITSECU", got)
	}

	// Move security under HR: effective path hr-security -> code HRSECU.
	hrID := hr.ID
	if _, _, err := gs.MoveCategory(ctx, sec.ID, &hrID); err != nil {
		t.Fatalf("MoveCategory: %v", err)
	}
	if got := readCategoryCode(t, pool, sec.ID); got != "HRSECU" {
		t.Fatalf("post-move code: got %q, want HRSECU", got)
	}
}

// readCategoryCode is a small test helper that reads categories.code for a category.
func readCategoryCode(t *testing.T, pool *postgres.DB, id uuid.UUID) string {
	t.Helper()
	var code string
	if err := pool.Pool().QueryRow(context.Background(), `SELECT code FROM categories WHERE id = $1`, id).Scan(&code); err != nil {
		t.Fatalf("read code for %s: %v", id, err)
	}
	return code
}

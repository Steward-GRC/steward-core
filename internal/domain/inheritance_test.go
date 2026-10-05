// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// buildChain returns [root, child, grandchild] with default template IDs set on root only.
func buildChain(rootTemplateID uuid.UUID) []domain.Category {
	root := domain.Category{ID: uuid.New(), Slug: "root", DefaultTemplateID: rootTemplateID}
	child := domain.Category{ID: uuid.New(), ParentID: root.ID, Slug: "child"}
	grand := domain.Category{ID: uuid.New(), ParentID: child.ID, Slug: "grand"}
	return []domain.Category{root, child, grand}
}

func TestEffectiveTemplateUsesCategoryAncestor(t *testing.T) {
	tid := uuid.New()
	chain := buildChain(tid)
	// No policy-level override
	got := domain.EffectiveTemplateID(uuid.Nil, chain)
	if got != tid {
		t.Fatalf("expected root template %v, got %v", tid, got)
	}
}

func TestEffectiveTemplatePolicyOverrideWins(t *testing.T) {
	tid := uuid.New()
	override := uuid.New()
	chain := buildChain(tid)
	got := domain.EffectiveTemplateID(override, chain)
	if got != override {
		t.Fatalf("expected policy override %v, got %v", override, got)
	}
}

func TestEffectiveTemplateNilWhenNoneSet(t *testing.T) {
	chain := buildChain(uuid.Nil) // root has no default
	got := domain.EffectiveTemplateID(uuid.Nil, chain)
	if got != uuid.Nil {
		t.Fatalf("expected uuid.Nil, got %v", got)
	}
}

func TestEffectiveTemplateNearestAncestorWins(t *testing.T) {
	rootTID := uuid.New()
	childTID := uuid.New()
	root := domain.Category{ID: uuid.New(), Slug: "root", DefaultTemplateID: rootTID}
	child := domain.Category{ID: uuid.New(), ParentID: root.ID, Slug: "child", DefaultTemplateID: childTID}
	grand := domain.Category{ID: uuid.New(), ParentID: child.ID, Slug: "grand"}
	// chain ordered leaf→root
	chain := []domain.Category{grand, child, root}
	got := domain.EffectiveTemplateID(uuid.Nil, chain)
	if got != childTID {
		t.Fatalf("expected child template %v (nearest ancestor), got %v", childTID, got)
	}
}

// ---------------------------------------------------------------------------
// EffectiveTemplate (tri-state) tests
// ---------------------------------------------------------------------------

func TestEffectiveTemplate_PolicyExplicitNoneWins(t *testing.T) {
	rootTID := uuid.New()
	chain := buildChain(rootTID) // root has a specific default
	res, id := domain.EffectiveTemplate(uuid.Nil, true, chain)
	if res != domain.TemplateExplicitNone || id != uuid.Nil {
		t.Fatalf("policy explicit-none must win over category chain; got res=%v id=%v", res, id)
	}
}

func TestEffectiveTemplate_PolicySpecificWins(t *testing.T) {
	rootTID := uuid.New()
	override := uuid.New()
	chain := buildChain(rootTID)
	res, id := domain.EffectiveTemplate(override, false, chain)
	if res != domain.TemplateResolved || id != override {
		t.Fatalf("policy specific override must win; got res=%v id=%v", res, id)
	}
}

func TestEffectiveTemplate_CategoryExplicitNoneLeafWins(t *testing.T) {
	rootTID := uuid.New()
	// leaf is explicit-none, root has a specific default; leaf-wins → none.
	leaf := domain.Category{ID: uuid.New(), Slug: "leaf", DefaultTemplateNone: true}
	root := domain.Category{ID: uuid.New(), Slug: "root", DefaultTemplateID: rootTID}
	chain := []domain.Category{leaf, root}
	res, id := domain.EffectiveTemplate(uuid.Nil, false, chain)
	if res != domain.TemplateExplicitNone || id != uuid.Nil {
		t.Fatalf("nearest category explicit-none must win; got res=%v id=%v", res, id)
	}
}

func TestEffectiveTemplate_CategorySpecificResolves(t *testing.T) {
	childTID := uuid.New()
	leaf := domain.Category{ID: uuid.New(), Slug: "leaf"}
	child := domain.Category{ID: uuid.New(), Slug: "child", DefaultTemplateID: childTID}
	chain := []domain.Category{leaf, child}
	res, id := domain.EffectiveTemplate(uuid.Nil, false, chain)
	if res != domain.TemplateResolved || id != childTID {
		t.Fatalf("nearest category specific default must resolve; got res=%v id=%v", res, id)
	}
}

func TestEffectiveTemplate_InheritNotFound(t *testing.T) {
	chain := buildChain(uuid.Nil) // nothing set anywhere
	res, id := domain.EffectiveTemplate(uuid.Nil, false, chain)
	if res != domain.TemplateInheritNotFound || id != uuid.Nil {
		t.Fatalf("all-unset must be inherit-not-found; got res=%v id=%v", res, id)
	}
}

// ---------------------------------------------------------------------------
// EffectiveAudienceGroups tests
// ---------------------------------------------------------------------------

func strSlicePtr(s ...string) *[]string { return &s }

// TestEffectiveAudienceGroups_LeafWins: leaf sets AudienceGroupIDs; leaf wins over root.
func TestEffectiveAudienceGroups_LeafWins(t *testing.T) {
	leaf := domain.Category{ID: uuid.New(), Slug: "leaf", AudienceGroupIDs: strSlicePtr("leaf-category")}
	root := domain.Category{ID: uuid.New(), Slug: "root", AudienceGroupIDs: strSlicePtr("root-category")}
	chain := []domain.Category{leaf, root}
	got := domain.EffectiveAudienceGroups(chain)
	if len(got) != 1 || got[0] != "leaf-category" {
		t.Fatalf("expected [leaf-category], got %v", got)
	}
}

// TestEffectiveAudienceGroups_Inherit: leaf has nil AudienceGroupIDs → inherit from root.
func TestEffectiveAudienceGroups_Inherit(t *testing.T) {
	leaf := domain.Category{ID: uuid.New(), Slug: "leaf", AudienceGroupIDs: nil}
	root := domain.Category{ID: uuid.New(), Slug: "root", AudienceGroupIDs: strSlicePtr("root-category")}
	chain := []domain.Category{leaf, root}
	got := domain.EffectiveAudienceGroups(chain)
	if len(got) != 1 || got[0] != "root-category" {
		t.Fatalf("expected [root-category] from ancestor, got %v", got)
	}
}

// TestEffectiveAudienceGroups_ExplicitEmptyOverride: leaf sets AudienceGroupIDs to
// empty (non-nil pointer to empty slice); this is an explicit "no audience"
// that blocks root's value.
func TestEffectiveAudienceGroups_ExplicitEmptyOverride(t *testing.T) {
	emptyADs := []string{}
	leaf := domain.Category{ID: uuid.New(), Slug: "leaf", AudienceGroupIDs: &emptyADs}
	root := domain.Category{ID: uuid.New(), Slug: "root", AudienceGroupIDs: strSlicePtr("root-category")}
	chain := []domain.Category{leaf, root}
	got := domain.EffectiveAudienceGroups(chain)
	if got == nil || len(got) != 0 {
		t.Fatalf("expected explicit empty override (empty non-nil), got %v", got)
	}
}

// TestEffectiveAudienceGroups_NoneSet: all nil → returns nil.
func TestEffectiveAudienceGroups_NoneSet(t *testing.T) {
	chain := []domain.Category{
		{ID: uuid.New(), Slug: "leaf", AudienceGroupIDs: nil},
		{ID: uuid.New(), Slug: "root", AudienceGroupIDs: nil},
	}
	got := domain.EffectiveAudienceGroups(chain)
	if got != nil {
		t.Fatalf("expected nil when no category sets AudienceGroupIDs, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// EffectiveExclusionGroups tests
// ---------------------------------------------------------------------------

// TestEffectiveExclusionGroups_LeafWins: leaf sets ExclusionGroupIDs → wins.
func TestEffectiveExclusionGroups_LeafWins(t *testing.T) {
	excl := []string{"excl-category"}
	leaf := domain.Category{ID: uuid.New(), Slug: "leaf", ExclusionGroupIDs: &excl}
	rootExcl := []string{"root-excl"}
	root := domain.Category{ID: uuid.New(), Slug: "root", ExclusionGroupIDs: &rootExcl}
	chain := []domain.Category{leaf, root}
	got := domain.EffectiveExclusionGroups(chain)
	if len(got) != 1 || got[0] != "excl-category" {
		t.Fatalf("expected [excl-category], got %v", got)
	}
}

// TestEffectiveExclusionGroups_Inherit: leaf nil → inherit root's exclusions.
func TestEffectiveExclusionGroups_Inherit(t *testing.T) {
	rootExcl := []string{"root-excl"}
	leaf := domain.Category{ID: uuid.New(), Slug: "leaf", ExclusionGroupIDs: nil}
	root := domain.Category{ID: uuid.New(), Slug: "root", ExclusionGroupIDs: &rootExcl}
	chain := []domain.Category{leaf, root}
	got := domain.EffectiveExclusionGroups(chain)
	if len(got) != 1 || got[0] != "root-excl" {
		t.Fatalf("expected [root-excl] from ancestor, got %v", got)
	}
}

// TestEffectiveExclusionGroups_ExplicitEmptyOverride: non-nil empty pointer
// overrides ancestor exclusions with "no exclusions".
func TestEffectiveExclusionGroups_ExplicitEmptyOverride(t *testing.T) {
	emptyExcl := []string{}
	leaf := domain.Category{ID: uuid.New(), Slug: "leaf", ExclusionGroupIDs: &emptyExcl}
	rootExcl := []string{"root-excl"}
	root := domain.Category{ID: uuid.New(), Slug: "root", ExclusionGroupIDs: &rootExcl}
	chain := []domain.Category{leaf, root}
	got := domain.EffectiveExclusionGroups(chain)
	if got == nil || len(got) != 0 {
		t.Fatalf("expected explicit empty exclusion override, got %v", got)
	}
}

// TestEffectiveExclusionGroups_NoneSet: no category sets exclusions → nil.
func TestEffectiveExclusionGroups_NoneSet(t *testing.T) {
	chain := []domain.Category{
		{ID: uuid.New(), Slug: "leaf", ExclusionGroupIDs: nil},
		{ID: uuid.New(), Slug: "root", ExclusionGroupIDs: nil},
	}
	got := domain.EffectiveExclusionGroups(chain)
	if got != nil {
		t.Fatalf("expected nil when no category sets ExclusionGroupIDs, got %v", got)
	}
}

func TestEffectiveSlugPath(t *testing.T) {
	// Chains are leaf-first (home category at index 0, root at index len-1), as
	// returned by CategoryStore.AncestorChain.
	root := domain.Category{ID: uuid.New(), Slug: "it"}
	child := domain.Category{ID: uuid.New(), ParentID: root.ID, Slug: "security"}
	grand := domain.Category{ID: uuid.New(), ParentID: child.ID, Slug: "phishing"}

	if got := domain.EffectiveSlugPath([]domain.Category{root}); got != "it" {
		t.Errorf("root effective slug = %q, want %q", got, "it")
	}
	if got := domain.EffectiveSlugPath([]domain.Category{child, root}); got != "it-security" {
		t.Errorf("child effective slug = %q, want %q", got, "it-security")
	}
	if got := domain.EffectiveSlugPath([]domain.Category{grand, child, root}); got != "it-security-phishing" {
		t.Errorf("grandchild effective slug = %q, want %q", got, "it-security-phishing")
	}
	if got := domain.EffectiveSlugPath(nil); got != "" {
		t.Errorf("empty chain effective slug = %q, want empty", got)
	}
}

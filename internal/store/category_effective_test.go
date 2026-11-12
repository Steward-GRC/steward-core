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

func effCategory(t *testing.T, s *store.CategoryStore, name, slug string, parent uuid.UUID) domain.Category {
	t.Helper()
	c, err := domain.NewCategory(name, slug, parent)
	if err != nil {
		t.Fatalf("NewCategory %s: %v", name, err)
	}
	c, err = s.Create(context.Background(), c)
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	return c
}

func TestCategoryStore_GetEffectiveRulesetChainChildFirst(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	top := effCategory(t, s, "Top", "top", uuid.Nil)
	mid := effCategory(t, s, "Mid", "mid", top.ID)
	leaf := effCategory(t, s, "Leaf", "leaf", mid.ID)

	ownerTop, ownerLeaf := uuid.New(), uuid.New()
	for _, g := range []struct {
		id     uuid.UUID
		owners []uuid.UUID
	}{{top.ID, []uuid.UUID{ownerTop}}, {leaf.ID, []uuid.UUID{ownerLeaf, ownerTop}}} {
		if _, err := s.SetGovernance(ctx, g.id, g.owners, nil, domain.AckNone, domain.CadenceNone, nil, nil, nil); err != nil {
			t.Fatalf("SetGovernance: %v", err)
		}
	}
	rulesets := map[uuid.UUID][]domain.CategoryRule{
		top.ID:  {{SubjectKind: "everyone", Read: "allow"}, {SubjectKind: "group", SubjectRef: "managers", Manage: "allow"}},
		mid.ID:  {{SubjectKind: "user", SubjectRef: "u-1", Author: "deny"}},
		leaf.ID: {{SubjectKind: "group", SubjectRef: "g-1", Read: "allow", Ack: "allow", Approve: "deny", Author: "allow", Manage: "deny"}, {SubjectKind: "everyone", Read: "deny"}},
	}
	for id, rules := range rulesets {
		if _, err := s.SetCategoryRuleset(ctx, id, rules); err != nil {
			t.Fatalf("SetCategoryRuleset: %v", err)
		}
	}

	got, err := s.GetEffectiveRuleset(ctx, leaf.ID)
	if err != nil {
		t.Fatalf("GetEffectiveRuleset: %v", err)
	}
	if len(got) != 3 || got[0].CategoryID != leaf.ID || got[1].CategoryID != mid.ID || got[2].CategoryID != top.ID {
		t.Fatalf("want leaf, mid, top; got %+v", got)
	}
	if got[0].Name != "Leaf" || got[2].Name != "Top" {
		t.Fatalf("names: %+v", got)
	}
	if len(got[0].Owners) != 2 || got[0].Owners[0] != ownerLeaf || got[0].Owners[1] != ownerTop {
		t.Fatalf("leaf owners (as stored): %+v", got[0].Owners)
	}
	if len(got[1].Owners) != 0 || len(got[2].Owners) != 1 || got[2].Owners[0] != ownerTop {
		t.Fatalf("mid/top owners: %+v / %+v", got[1].Owners, got[2].Owners)
	}
	for _, lv := range got {
		want := rulesets[lv.CategoryID]
		if len(lv.Rules) != len(want) {
			t.Fatalf("%s: want %d rules, got %+v", lv.Name, len(want), lv.Rules)
		}
		for i, r := range lv.Rules {
			w := want[i]
			if r.Ordinal != i+1 || r.SubjectKind != w.SubjectKind || r.SubjectRef != w.SubjectRef ||
				r.Read != w.Read || r.Ack != w.Ack || r.Approve != w.Approve || r.Author != w.Author || r.Manage != w.Manage {
				t.Fatalf("%s rule %d: want %+v got %+v", lv.Name, i, w, r)
			}
		}
	}
}

func TestCategoryStore_GetEffectiveRulesetTopLevelIsOneLevel(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	top := effCategory(t, s, "Solo", "solo", uuid.Nil)
	got, err := s.GetEffectiveRuleset(ctx, top.ID)
	if err != nil {
		t.Fatalf("GetEffectiveRuleset: %v", err)
	}
	if len(got) != 1 || got[0].CategoryID != top.ID {
		t.Fatalf("want one level, got %+v", got)
	}
	if got[0].Rules == nil || got[0].Owners == nil {
		t.Fatalf("rules and owners must be non-nil: %+v", got[0])
	}
}

func TestCategoryStore_GetEffectiveRulesetLevelWithoutRules(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	top := effCategory(t, s, "Top", "top", uuid.Nil)
	child := effCategory(t, s, "Child", "child", top.ID)
	if _, err := s.SetCategoryRuleset(ctx, child.ID, nil); err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}
	got, err := s.GetEffectiveRuleset(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetEffectiveRuleset: %v", err)
	}
	if len(got) != 2 || len(got[0].Rules) != 0 || got[0].Rules == nil || len(got[1].Rules) == 0 {
		t.Fatalf("a level with no rules stays in the chain with empty rules: %+v", got)
	}
}

func TestCategoryStore_GetEffectiveRulesetUnknownIsNotFound(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	if _, err := s.GetEffectiveRuleset(context.Background(), uuid.New()); !errors.Is(err, store.ErrCategoryNotFound) {
		t.Fatalf("want ErrCategoryNotFound, got %v", err)
	}
}

// A cyclic or over-deep parent chain (not reachable through the API) must end:
// the walk is capped by the category depth limit and never revisits a category.
func TestCategoryStore_GetEffectiveRulesetCyclicChainTerminates(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	a := effCategory(t, s, "A", "a", uuid.Nil)
	b := effCategory(t, s, "B", "b", a.ID)
	if _, err := pool.Querier().Exec(ctx, `UPDATE categories SET parent_id = $1 WHERE id = $2`, b.ID, a.ID); err != nil {
		t.Fatalf("make cycle: %v", err)
	}
	got, err := s.GetEffectiveRuleset(ctx, b.ID)
	if err != nil {
		t.Fatalf("GetEffectiveRuleset on a cycle: %v", err)
	}
	if len(got) != 2 || got[0].CategoryID != b.ID || got[1].CategoryID != a.ID {
		t.Fatalf("a cycle returns each category once: %+v", got)
	}
}

func TestCategoryStore_GetEffectiveRulesetDepthIsCapped(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	c1 := effCategory(t, s, "C1", "c1", uuid.Nil)
	c2 := effCategory(t, s, "C2", "c2", c1.ID)
	c3 := effCategory(t, s, "C3", "c3", c2.ID)
	c4 := effCategory(t, s, "C4", "c4", uuid.Nil)
	if _, err := pool.Querier().Exec(ctx, `UPDATE categories SET parent_id = $1 WHERE id = $2`, c3.ID, c4.ID); err != nil {
		t.Fatalf("make a fourth level: %v", err)
	}
	got, err := s.GetEffectiveRuleset(ctx, c4.ID)
	if err != nil {
		t.Fatalf("GetEffectiveRuleset: %v", err)
	}
	if len(got) != 3 || got[0].CategoryID != c4.ID {
		t.Fatalf("the chain is capped at the category depth limit (3): %+v", got)
	}
}

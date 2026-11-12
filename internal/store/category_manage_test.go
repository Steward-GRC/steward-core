// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func TestCategoryStore_ManageCellRoundTrips(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	cat, err := domain.NewCategory("Manage-Test", "manage-test", uuid.Nil)
	if err != nil {
		t.Fatalf("domain.NewCategory: %v", err)
	}
	cat, err = s.Create(ctx, cat)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The default rule has a blank Manage cell.
	def, err := s.GetCategoryRuleset(ctx, cat.ID)
	if err != nil {
		t.Fatalf("GetCategoryRuleset (default): %v", err)
	}
	if len(def) != 1 || def[0].Manage != "" {
		t.Fatalf("default rule must have a blank manage cell, got %+v", def)
	}

	in := []domain.CategoryRule{
		{SubjectKind: "everyone", Read: "allow"},
		{SubjectKind: "group", SubjectRef: "doc-managers", Manage: "allow"},
		{SubjectKind: "user", SubjectRef: "u-1", Read: "allow", Manage: "deny"},
	}
	saved, err := s.SetCategoryRuleset(ctx, cat.ID, in)
	if err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}
	got, err := s.GetCategoryRuleset(ctx, cat.ID)
	if err != nil {
		t.Fatalf("GetCategoryRuleset: %v", err)
	}
	for _, rules := range [][]domain.CategoryRule{saved, got} {
		if len(rules) != 3 || rules[0].Manage != "" || rules[1].Manage != "allow" || rules[2].Manage != "deny" {
			t.Fatalf("manage cells did not round-trip: %+v", rules)
		}
		if rules[1].Read != "" || rules[2].Read != "allow" {
			t.Fatalf("manage disturbed the other cells: %+v", rules)
		}
	}
}

func TestCategoryStore_PurgeKeepsTheManageCell(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	cat, err := domain.NewCategory("Purge-Manage", "purge-manage", uuid.Nil)
	if err != nil {
		t.Fatalf("domain.NewCategory: %v", err)
	}
	cat, err = s.Create(ctx, cat)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	victim := uuid.New()
	if _, err := s.SetCategoryRuleset(ctx, cat.ID, []domain.CategoryRule{
		{SubjectKind: "group", SubjectRef: "managers", Manage: "allow"},
		{SubjectKind: "user", SubjectRef: victim.String(), Manage: "deny"},
		{SubjectKind: "group", SubjectRef: "others", Read: "allow", Manage: "deny"},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}

	preview, err := s.PurgeUserCategoryRules(ctx, victim, true)
	if err != nil || len(preview) != 1 || preview[0].Rule.Manage != "deny" {
		t.Fatalf("the preview must carry the manage cell: %+v (err=%v)", preview, err)
	}
	removed, err := s.PurgeUserCategoryRules(ctx, victim, false)
	if err != nil || len(removed) != 1 || removed[0].Rule.Manage != "deny" {
		t.Fatalf("the purge must report the manage cell it removed: %+v (err=%v)", removed, err)
	}

	got, err := s.GetCategoryRuleset(ctx, cat.ID)
	if err != nil {
		t.Fatalf("GetCategoryRuleset: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 survivors, got %+v", got)
	}
	if got[0].Ordinal != 1 || got[0].SubjectRef != "managers" || got[0].Manage != "allow" {
		t.Fatalf("survivor 1 lost its manage cell or place: %+v", got[0])
	}
	if got[1].Ordinal != 2 || got[1].SubjectRef != "others" || got[1].Read != "allow" || got[1].Manage != "deny" {
		t.Fatalf("survivor 2 lost its manage cell or place: %+v", got[1])
	}
}

// The data fix inside the manage migration: a category with no rules gets
// Everyone: Read once, a category that has rules is left alone, and running
// the migration again changes nothing.
func TestMigrationManageColumn_BackfillsAnEmptyCategoryOnce(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewCategoryStore(pool)
	ctx := context.Background()

	mk := func(name, slug string) domain.Category {
		c, err := domain.NewCategory(name, slug, uuid.Nil)
		if err != nil {
			t.Fatalf("domain.NewCategory: %v", err)
		}
		c, err = s.Create(ctx, c)
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return c
	}
	empty := mk("Empty", "empty")
	ruled := mk("Ruled", "ruled")

	// An older category: it exists with no rules at all.
	if _, err := s.SetCategoryRuleset(ctx, empty.ID, nil); err != nil {
		t.Fatalf("empty the ruleset: %v", err)
	}
	custom := []domain.CategoryRule{{SubjectKind: "group", SubjectRef: "finance", Approve: "allow"}}
	if _, err := s.SetCategoryRuleset(ctx, ruled.ID, custom); err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}

	up, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0034_category_manage.up.sql"))
	if err != nil {
		t.Fatalf("read the migration: %v", err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := pool.Querier().Exec(ctx, string(up)); err != nil {
			t.Fatalf("run %d of the up migration: %v", run, err)
		}
		got, err := s.GetCategoryRuleset(ctx, empty.ID)
		if err != nil {
			t.Fatalf("GetCategoryRuleset(empty): %v", err)
		}
		if len(got) != 1 || got[0].Ordinal != 1 || got[0].SubjectKind != "everyone" || got[0].SubjectRef != "" ||
			got[0].Read != "allow" || got[0].Ack != "" || got[0].Approve != "" || got[0].Author != "" || got[0].Manage != "" {
			t.Fatalf("run %d: want exactly Everyone: Read, got %+v", run, got)
		}
		kept, err := s.GetCategoryRuleset(ctx, ruled.ID)
		if err != nil {
			t.Fatalf("GetCategoryRuleset(ruled): %v", err)
		}
		if len(kept) != 1 || kept[0].SubjectRef != "finance" || kept[0].Approve != "allow" || kept[0].Read != "" {
			t.Fatalf("run %d: a category with rules must be left alone, got %+v", run, kept)
		}
	}
}

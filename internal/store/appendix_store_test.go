// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
)

// makeVersion creates a policy + a policy_versions row with the given status
// ("draft" or "published") and returns (policyID, versionID).
// It reuses setupCategoryAndTemplate + the existing PolicyStore methods.
func makeVersion(t *testing.T, ps *store.PolicyStore, pool *postgres.DB, status string) (policyID, versionID uuid.UUID) {
	t.Helper()
	ctx := context.Background()

	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, err := domain.NewPolicy("Test Policy "+uuid.New().String()[:8], g.ID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("makeVersion NewPolicy: %v", err)
	}
	p, err = ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("makeVersion CreatePolicy: %v", err)
	}

	draft, err := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if err != nil {
		t.Fatalf("makeVersion NewPolicyVersionDraft: %v", err)
	}
	saved, _, err := ps.UpsertDraft(ctx, draft)
	if err != nil {
		t.Fatalf("makeVersion UpsertDraft: %v", err)
	}

	if status == "published" {
		pub, err := ps.PublishDraft(ctx, p.ID, uuid.New())
		if err != nil {
			t.Fatalf("makeVersion PublishDraft: %v", err)
		}
		return p.ID, pub.ID
	}

	return p.ID, saved.ID
}

// mustAppendix calls domain.NewAppendix and fatals on error.
func mustAppendix(t *testing.T, versionID uuid.UUID, title, contentJSON string) domain.Appendix {
	t.Helper()
	a, err := domain.NewAppendix(versionID, title, contentJSON)
	if err != nil {
		t.Fatalf("mustAppendix: %v", err)
	}
	return a
}

// publishVersion marks a policy_version as published via raw SQL.
func publishVersion(t *testing.T, pool *postgres.DB, versionID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Pool().Exec(ctx,
		`UPDATE policy_versions SET status='published', version_no=1, published_at=now() WHERE id=$1`,
		versionID); err != nil {
		t.Fatalf("publishVersion: %v", err)
	}
}

func TestAppendixStore_DeleteMiddleRenumbers(t *testing.T) {
	pool := newTestDB(t)
	ps := store.NewPolicyStore(pool)
	as := store.NewAppendixStore(pool)
	ctx := context.Background()

	_, draftID := makeVersion(t, ps, pool, "draft")

	// Add three appendices → order_index 0,1,2.
	a0, err := as.Add(ctx, mustAppendix(t, draftID, "First", `{"i":0}`))
	if err != nil {
		t.Fatalf("Add a0: %v", err)
	}
	a1, err := as.Add(ctx, mustAppendix(t, draftID, "Middle", `{"i":1}`))
	if err != nil {
		t.Fatalf("Add a1: %v", err)
	}
	a2, err := as.Add(ctx, mustAppendix(t, draftID, "Last", `{"i":2}`))
	if err != nil {
		t.Fatalf("Add a2: %v", err)
	}
	if a0.OrderIndex != 0 || a1.OrderIndex != 1 || a2.OrderIndex != 2 {
		t.Fatalf("initial order: %d %d %d", a0.OrderIndex, a1.OrderIndex, a2.OrderIndex)
	}

	// Delete the MIDDLE appendix.
	if err := as.Delete(ctx, a1.ID); err != nil {
		t.Fatalf("Delete middle: %v", err)
	}

	// Survivors must be contiguous 0,1 — no gap.
	list, err := as.ListByVersion(ctx, draftID)
	if err != nil {
		t.Fatalf("ListByVersion: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 survivors, got %d: %+v", len(list), list)
	}
	if list[0].ID != a0.ID || list[0].OrderIndex != 0 {
		t.Fatalf("survivor[0]: want id=%s order=0, got id=%s order=%d", a0.ID, list[0].ID, list[0].OrderIndex)
	}
	if list[1].ID != a2.ID || list[1].OrderIndex != 1 {
		t.Fatalf("survivor[1]: want id=%s order=1, got id=%s order=%d", a2.ID, list[1].ID, list[1].OrderIndex)
	}
}

func TestAppendixStore_DraftCRUDReorderGuardCopy(t *testing.T) {
	pool := newTestDB(t)
	ps := store.NewPolicyStore(pool)
	as := store.NewAppendixStore(pool)
	ctx := context.Background()

	_, draftID := makeVersion(t, ps, pool, "draft")

	// Add two appendices → order_index 0,1.
	a0, err := as.Add(ctx, mustAppendix(t, draftID, "Intake Form", `{"a":0}`))
	if err != nil {
		t.Fatalf("Add a0: %v", err)
	}
	a1, err := as.Add(ctx, mustAppendix(t, draftID, "Escalation Matrix", `{"a":1}`))
	if err != nil {
		t.Fatalf("Add a1: %v", err)
	}
	if a0.OrderIndex != 0 || a1.OrderIndex != 1 {
		t.Fatalf("order: %d %d", a0.OrderIndex, a1.OrderIndex)
	}

	// List ordered.
	list, err := as.ListByVersion(ctx, draftID)
	if err != nil || len(list) != 2 || list[0].ID != a0.ID || list[1].ID != a1.ID {
		t.Fatalf("list: %+v err=%v", list, err)
	}

	// Update.
	up, err := as.Update(ctx, a0.ID, "Intake Form v2", `{"a":0,"v":2}`)
	if err != nil || up.Title != "Intake Form v2" {
		t.Fatalf("update: %+v err=%v", up, err)
	}

	// Reorder → swap.
	re, err := as.Reorder(ctx, draftID, []uuid.UUID{a1.ID, a0.ID})
	if err != nil || re[0].ID != a1.ID || re[0].OrderIndex != 0 || re[1].ID != a0.ID || re[1].OrderIndex != 1 {
		t.Fatalf("reorder: %+v err=%v", re, err)
	}

	// Delete.
	if err := as.Delete(ctx, a0.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	list2, _ := as.ListByVersion(ctx, draftID)
	if len(list2) != 1 || list2[0].ID != a1.ID {
		t.Fatalf("after delete: %+v", list2)
	}

	// Draft-guard: publish the version, then writes must fail with ErrVersionNotDraft.
	publishVersion(t, pool, draftID)
	if _, err := as.Add(ctx, mustAppendix(t, draftID, "Late", `{"x":1}`)); err != store.ErrVersionNotDraft {
		t.Fatalf("add to published: want ErrVersionNotDraft, got %v", err)
	}
	if _, err := as.Update(ctx, a1.ID, "x", `{"y":1}`); err != store.ErrVersionNotDraft {
		t.Fatalf("update on published: want ErrVersionNotDraft, got %v", err)
	}
	if err := as.Delete(ctx, a1.ID); err != store.ErrVersionNotDraft {
		t.Fatalf("delete on published: want ErrVersionNotDraft, got %v", err)
	}

	// Copy-forward: from the now-published version into a fresh draft.
	_, newDraftID := makeVersion(t, ps, pool, "draft")
	if err := as.CopyForward(ctx, draftID, newDraftID); err != nil {
		t.Fatalf("copy-forward: %v", err)
	}
	copied, _ := as.ListByVersion(ctx, newDraftID)
	if len(copied) != 1 || copied[0].Title != "Escalation Matrix" || copied[0].OrderIndex != 0 {
		t.Fatalf("copied: %+v", copied)
	}
	// Copied rows are independent (new ids).
	if copied[0].ID == a1.ID {
		t.Fatalf("copy-forward must assign new ids")
	}
}

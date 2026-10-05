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

func TestReferenceStoreLibraryAndAttachLive(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	ps := store.NewPolicyStore(pool)
	policyID, _ := makeVersion(t, ps, pool, "draft")

	rs := store.NewReferenceStore(pool)
	actor := uuid.New().String()

	// Library CRUD; created_by + created_at are stamped on create.
	a, err := rs.CreateReference(ctx, domain.Reference{
		Label: "Records Retention Standard", Kind: domain.ReferenceKindStandard, Clause: "4.2", CreatedByUserID: actor,
	})
	if err != nil {
		t.Fatalf("CreateReference: %v", err)
	}
	if a.CreatedByUserID != actor {
		t.Fatalf("created_by not persisted: %q", a.CreatedByUserID)
	}
	if a.CreatedAt.IsZero() {
		t.Fatalf("created_at not stamped")
	}
	b, err := rs.CreateReference(ctx, domain.Reference{
		Label: "Acceptable Use", Kind: domain.ReferenceKindText, Body: "Users must...", CreatedByUserID: actor,
	})
	if err != nil {
		t.Fatalf("CreateReference b: %v", err)
	}

	// Library ordered by label; both active with used-by 0.
	lib, err := rs.ListReferences(ctx, false)
	if err != nil || len(lib) != 2 {
		t.Fatalf("ListReferences: %v len=%d", err, len(lib))
	}
	if lib[0].Label != "Acceptable Use" || lib[1].Label != "Records Retention Standard" {
		t.Fatalf("not ordered by label: %+v", lib)
	}
	if lib[0].UsedByCount != 0 || lib[1].UsedByCount != 0 {
		t.Fatalf("used-by should be 0, got %+v", lib)
	}

	// Attach both to the policy; order preserved, dup dropped.
	attached, err := rs.SetPolicyReferences(ctx, policyID, []uuid.UUID{b.ID, a.ID, b.ID})
	if err != nil {
		t.Fatalf("SetPolicyReferences: %v", err)
	}
	if len(attached) != 2 || attached[0].ID != b.ID || attached[1].ID != a.ID {
		t.Fatalf("attach order wrong: %+v", attached)
	}

	// Used-by now reflects the attachment.
	lib, err = rs.ListReferences(ctx, false)
	if err != nil {
		t.Fatalf("ListReferences after attach: %v", err)
	}
	for _, r := range lib {
		if r.UsedByCount != 1 {
			t.Fatalf("used-by want 1 for %s, got %d", r.Label, r.UsedByCount)
		}
	}

	// Editing the library entry propagates LIVE to the policy's view.
	if _, err := rs.UpdateReference(ctx, a.ID, domain.Reference{
		Label: "Records Retention Standard 2026", Kind: domain.ReferenceKindStandard, Clause: "4.2", URL: "https://standards.example.org",
	}); err != nil {
		t.Fatalf("UpdateReference: %v", err)
	}
	live, err := rs.ListPolicyReferences(ctx, policyID)
	if err != nil || len(live) != 2 {
		t.Fatalf("ListPolicyReferences: %v len=%d", err, len(live))
	}
	if live[1].Label != "Records Retention Standard 2026" || live[1].URL != "https://standards.example.org" {
		t.Fatalf("live update did not propagate: %+v", live[1])
	}

	// Update of an unknown id -> ErrReferenceNotFound.
	if _, err := rs.UpdateReference(ctx, uuid.New(), domain.Reference{Label: "x", Kind: domain.ReferenceKindText, Body: "y"}); !errors.Is(err, store.ErrReferenceNotFound) {
		t.Fatalf("want ErrReferenceNotFound, got %v", err)
	}
}

func TestReferenceStoreDeleteRefusedWhenInUse(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	ps := store.NewPolicyStore(pool)
	policyID, _ := makeVersion(t, ps, pool, "draft")
	rs := store.NewReferenceStore(pool)

	r, err := rs.CreateReference(ctx, domain.Reference{Label: "Records Handling Guide", Kind: domain.ReferenceKindLink, URL: "https://guides.example.org"})
	if err != nil {
		t.Fatalf("CreateReference: %v", err)
	}

	// Attach it, then a delete must be REFUSED.
	if _, err := rs.SetPolicyReferences(ctx, policyID, []uuid.UUID{r.ID}); err != nil {
		t.Fatalf("SetPolicyReferences: %v", err)
	}
	if err := rs.DeleteReference(ctx, r.ID); !errors.Is(err, store.ErrReferenceInUse) {
		t.Fatalf("want ErrReferenceInUse, got %v", err)
	}

	// Detach, then the delete succeeds.
	if _, err := rs.SetPolicyReferences(ctx, policyID, nil); err != nil {
		t.Fatalf("detach: %v", err)
	}
	if err := rs.DeleteReference(ctx, r.ID); err != nil {
		t.Fatalf("DeleteReference after detach: %v", err)
	}

	// Unknown id on delete -> ErrReferenceNotFound.
	if err := rs.DeleteReference(ctx, uuid.New()); !errors.Is(err, store.ErrReferenceNotFound) {
		t.Fatalf("want ErrReferenceNotFound, got %v", err)
	}
}

func TestReferenceStoreArchiveExcludedButResolvesWhenAttached(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	ps := store.NewPolicyStore(pool)
	policyID, _ := makeVersion(t, ps, pool, "draft")
	rs := store.NewReferenceStore(pool)

	active, err := rs.CreateReference(ctx, domain.Reference{Label: "Active Std", Kind: domain.ReferenceKindStandard})
	if err != nil {
		t.Fatalf("CreateReference active: %v", err)
	}
	arch, err := rs.CreateReference(ctx, domain.Reference{Label: "Old Std", Kind: domain.ReferenceKindStandard})
	if err != nil {
		t.Fatalf("CreateReference arch: %v", err)
	}

	// Attach the soon-to-be-archived entry to the policy, then archive it.
	if _, err := rs.SetPolicyReferences(ctx, policyID, []uuid.UUID{arch.ID}); err != nil {
		t.Fatalf("SetPolicyReferences: %v", err)
	}
	archived, err := rs.SetReferenceArchived(ctx, arch.ID, true)
	if err != nil || !archived.Archived {
		t.Fatalf("SetReferenceArchived: %v archived=%v", err, archived.Archived)
	}

	// Active list (default) EXCLUDES the archived entry.
	activeList, err := rs.ListReferences(ctx, false)
	if err != nil {
		t.Fatalf("ListReferences(active): %v", err)
	}
	if len(activeList) != 1 || activeList[0].ID != active.ID {
		t.Fatalf("archived entry must be excluded from the active list; got %+v", activeList)
	}

	// includeArchived=true returns both, and reports the archived entry's usage.
	all, err := rs.ListReferences(ctx, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListReferences(all): %v len=%d", err, len(all))
	}
	var archUsed int
	for _, r := range all {
		if r.ID == arch.ID {
			archUsed = r.UsedByCount
		}
	}
	if archUsed != 1 {
		t.Fatalf("archived entry usedByCount want 1, got %d", archUsed)
	}

	// The archived-but-attached entry STILL resolves live on the policy.
	live, err := rs.ListPolicyReferences(ctx, policyID)
	if err != nil || len(live) != 1 || live[0].ID != arch.ID || !live[0].Archived {
		t.Fatalf("archived-but-attached must still resolve; got %v %+v", err, live)
	}

	// Restore flips it back into the active list.
	if _, err := rs.SetReferenceArchived(ctx, arch.ID, false); err != nil {
		t.Fatalf("SetReferenceArchived(restore): %v", err)
	}
	restored, err := rs.ListReferences(ctx, false)
	if err != nil || len(restored) != 2 {
		t.Fatalf("restore should return the entry to the active list; got %v len=%d", err, len(restored))
	}

	// SetReferenceArchived on an unknown id -> ErrReferenceNotFound.
	if _, err := rs.SetReferenceArchived(ctx, uuid.New(), true); !errors.Is(err, store.ErrReferenceNotFound) {
		t.Fatalf("want ErrReferenceNotFound, got %v", err)
	}
}

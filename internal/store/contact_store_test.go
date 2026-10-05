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

func TestContactStoreLibraryAndAttachLive(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	ps := store.NewPolicyStore(pool)
	policyID, _ := makeVersion(t, ps, pool, "draft")

	cs := store.NewContactStore(pool)

	// Library CRUD.
	a, err := cs.CreateBlock(ctx, domain.ContactBlock{Label: "IT Service Desk", Email: "it@example.org"})
	if err != nil {
		t.Fatalf("CreateBlock: %v", err)
	}
	b, err := cs.CreateBlock(ctx, domain.ContactBlock{Label: "Security"})
	if err != nil {
		t.Fatalf("CreateBlock b: %v", err)
	}
	lib, err := cs.ListBlocks(ctx, false)
	if err != nil || len(lib) != 2 {
		t.Fatalf("ListBlocks: %v len=%d", err, len(lib))
	}

	// Attach both to the policy; resolved live.
	attached, err := cs.SetForPolicy(ctx, policyID, []uuid.UUID{a.ID, b.ID, a.ID}) // dup dropped
	if err != nil {
		t.Fatalf("SetForPolicy: %v", err)
	}
	if len(attached) != 2 || attached[0].Label != "IT Service Desk" {
		t.Fatalf("unexpected attachments: %+v", attached)
	}

	// Editing the library block propagates LIVE to the policy's view.
	if _, err := cs.UpdateBlock(ctx, a.ID, domain.ContactBlock{Label: "IT Service Desk", Email: "helpdesk@example.org"}); err != nil {
		t.Fatalf("UpdateBlock: %v", err)
	}
	live, err := cs.ListByPolicy(ctx, policyID)
	if err != nil || len(live) != 2 {
		t.Fatalf("ListByPolicy: %v len=%d", err, len(live))
	}
	if live[0].Email != "helpdesk@example.org" {
		t.Fatalf("live update did not propagate: %+v", live[0])
	}

	// Deleting a library block cascade-detaches it from the policy.
	if err := cs.DeleteBlock(ctx, a.ID); err != nil {
		t.Fatalf("DeleteBlock: %v", err)
	}
	after, err := cs.ListByPolicy(ctx, policyID)
	if err != nil || len(after) != 1 || after[0].ID != b.ID {
		t.Fatalf("cascade-detach failed: %v %+v", err, after)
	}

	// Unknown id on delete -> ErrContactBlockNotFound.
	if err := cs.DeleteBlock(ctx, uuid.New()); !errors.Is(err, store.ErrContactBlockNotFound) {
		t.Fatalf("want ErrContactBlockNotFound, got %v", err)
	}
}

func TestContactStoreArchiveExcludedButResolvesWhenAttached(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	ps := store.NewPolicyStore(pool)
	policyID, _ := makeVersion(t, ps, pool, "draft")
	cs := store.NewContactStore(pool)

	active, err := cs.CreateBlock(ctx, domain.ContactBlock{Label: "Active Desk", Email: "a@example.org"})
	if err != nil {
		t.Fatalf("CreateBlock active: %v", err)
	}
	arch, err := cs.CreateBlock(ctx, domain.ContactBlock{Label: "Old Desk", Email: "old@example.org"})
	if err != nil {
		t.Fatalf("CreateBlock arch: %v", err)
	}

	// Attach the soon-to-be-archived block to the policy, then archive it.
	if _, err := cs.SetForPolicy(ctx, policyID, []uuid.UUID{arch.ID}); err != nil {
		t.Fatalf("SetForPolicy: %v", err)
	}
	archived, err := cs.SetArchived(ctx, arch.ID, true)
	if err != nil || !archived.Archived {
		t.Fatalf("SetArchived: %v archived=%v", err, archived.Archived)
	}

	// Active list (default) EXCLUDES the archived block; usedByCount is populated.
	activeList, err := cs.ListBlocks(ctx, false)
	if err != nil {
		t.Fatalf("ListBlocks(active): %v", err)
	}
	if len(activeList) != 1 || activeList[0].ID != active.ID {
		t.Fatalf("archived block must be excluded from the active list; got %+v", activeList)
	}
	if activeList[0].UsedByCount != 0 {
		t.Fatalf("active block usedByCount want 0, got %d", activeList[0].UsedByCount)
	}

	// includeArchived=true returns both, and reports the archived block's usage.
	all, err := cs.ListBlocks(ctx, true)
	if err != nil || len(all) != 2 {
		t.Fatalf("ListBlocks(all): %v len=%d", err, len(all))
	}
	var archUsed int
	for _, b := range all {
		if b.ID == arch.ID {
			archUsed = b.UsedByCount
		}
	}
	if archUsed != 1 {
		t.Fatalf("archived block usedByCount want 1, got %d", archUsed)
	}

	// The archived-but-attached block STILL resolves live on the policy.
	live, err := cs.ListByPolicy(ctx, policyID)
	if err != nil || len(live) != 1 || live[0].ID != arch.ID || !live[0].Archived {
		t.Fatalf("archived-but-attached must still resolve; got %v %+v", err, live)
	}

	// Restore flips it back into the active list.
	if _, err := cs.SetArchived(ctx, arch.ID, false); err != nil {
		t.Fatalf("SetArchived(restore): %v", err)
	}
	restored, err := cs.ListBlocks(ctx, false)
	if err != nil || len(restored) != 2 {
		t.Fatalf("restore should return the block to the active list; got %v len=%d", err, len(restored))
	}
}

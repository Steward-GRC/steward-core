// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/store"
)

func TestRelationStoreSetListRoundTripAndFullReplace(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	ps := store.NewPolicyStore(pool)
	// Three policies: A (the subject) and B, C (link targets).
	a, _ := makeVersion(t, ps, pool, "draft")
	b, _ := makeVersion(t, ps, pool, "draft")
	c, _ := makeVersion(t, ps, pool, "draft")

	rs := store.NewRelationStore(pool)

	got, err := rs.ListByPolicy(ctx, a)
	if err != nil {
		t.Fatalf("ListByPolicy(empty): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 relations initially, got %d", len(got))
	}

	// Link A -> B, C. A self-reference and a duplicate are dropped.
	saved, err := rs.SetForPolicy(ctx, a, []uuid.UUID{b, a, c, b})
	if err != nil {
		t.Fatalf("SetForPolicy: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("want 2 relations (self + dup dropped), got %d: %+v", len(saved), saved)
	}
	// Resolved live: number+title come from the related policy row.
	if saved[0].PolicyID != b || saved[0].Number == "" || saved[0].Title == "" {
		t.Fatalf("first relation not resolved: %+v", saved[0])
	}

	// Full replace: a new set wipes the old links.
	replaced, err := rs.SetForPolicy(ctx, a, []uuid.UUID{c})
	if err != nil {
		t.Fatalf("SetForPolicy(replace): %v", err)
	}
	if len(replaced) != 1 || replaced[0].PolicyID != c {
		t.Fatalf("full replace failed: %+v", replaced)
	}

	// Unknown related id violates the FK -> error.
	if _, err := rs.SetForPolicy(ctx, a, []uuid.UUID{uuid.New()}); err == nil {
		t.Fatal("want error for unknown related policy id, got nil")
	}
}

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

func TestAssetStoreCreateAndGet(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	as := store.NewAssetStore(pool)

	id := uuid.New()
	in := domain.Asset{
		ID:              id,
		StorageKey:      domain.StorageKeyFor(id),
		ContentType:     "image/png",
		SizeBytes:       2048,
		Filename:        "diagram.png",
		CreatedByUserID: "user-42",
	}

	saved, err := as.Create(ctx, in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if saved.ID != id {
		t.Errorf("saved id = %s, want %s", saved.ID, id)
	}
	if saved.CreatedAt.IsZero() {
		t.Errorf("created_at not populated by default")
	}

	got, err := as.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.StorageKey != in.StorageKey || got.ContentType != in.ContentType ||
		got.SizeBytes != in.SizeBytes || got.Filename != in.Filename ||
		got.CreatedByUserID != in.CreatedByUserID {
		t.Errorf("round-trip mismatch: got %+v, want %+v", got, in)
	}
}

func TestAssetStoreGetMissing(t *testing.T) {
	pool := newTestDB(t)
	_, err := store.NewAssetStore(pool).Get(context.Background(), uuid.New())
	if !errors.Is(err, store.ErrAssetNotFound) {
		t.Fatalf("err = %v, want ErrAssetNotFound", err)
	}
}

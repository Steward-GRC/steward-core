// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func TestAssetSettingsStoreDefaultsToEveryKindOn(t *testing.T) {
	ss := store.NewAssetSettingsStore(newTestDB(t))
	got, err := ss.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got.AllowedKinds, domain.AllAssetKinds()) {
		t.Errorf("default kinds = %v, want every kind %v", got.AllowedKinds, domain.AllAssetKinds())
	}
	if got.UpdatedBy != "" {
		t.Errorf("updated_by = %q before any change, want empty", got.UpdatedBy)
	}
}

func TestAssetSettingsStoreSetRoundTripAndTurnEverythingOff(t *testing.T) {
	ctx := context.Background()
	ss := store.NewAssetSettingsStore(newTestDB(t))

	saved, err := ss.Set(ctx, []domain.AssetKind{domain.KindPDF, domain.KindImage}, "admin-1")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	want := []domain.AssetKind{domain.KindPDF, domain.KindImage}
	if !reflect.DeepEqual(saved.AllowedKinds, want) || saved.UpdatedBy != "admin-1" || saved.UpdatedAt.IsZero() {
		t.Errorf("saved = %+v, want kinds %v by admin-1 with a time", saved, want)
	}
	got, err := ss.Get(ctx)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !reflect.DeepEqual(got.AllowedKinds, want) || got.UpdatedBy != "admin-1" {
		t.Errorf("read back %+v, want kinds %v", got, want)
	}

	// Turning every kind off is allowed: an empty set, not the default.
	off, err := ss.Set(ctx, nil, "admin-2")
	if err != nil {
		t.Fatalf("Set empty: %v", err)
	}
	if len(off.AllowedKinds) != 0 || off.UpdatedBy != "admin-2" {
		t.Errorf("after all off: %+v", off)
	}
	again, _ := ss.Get(ctx)
	if len(again.AllowedKinds) != 0 {
		t.Errorf("all-off did not persist: %v", again.AllowedKinds)
	}
}

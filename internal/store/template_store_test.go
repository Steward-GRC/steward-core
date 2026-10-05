// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func TestTemplateStoreCreateAndVersion(t *testing.T) {
	pool := newTestDB(t)
	ts := store.NewTemplateStore(pool)
	ctx := context.Background()

	tpl, err := domain.NewTemplate("Standard Policy", uuid.Nil)
	if err != nil {
		t.Fatalf("domain: %v", err)
	}
	tpl, err = ts.CreateTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	sections := []domain.Section{{
		Key:    "purpose",
		Title:  "Purpose",
		Order:  1,
		Blocks: []domain.Block{{Type: domain.BlockTypeEditable}},
	}}
	tv, err := domain.NewTemplateVersion(tpl.ID.String(), sections)
	if err != nil {
		t.Fatalf("domain version: %v", err)
	}
	tv, err = ts.CreateTemplateVersion(ctx, tv)
	if err != nil {
		t.Fatalf("CreateTemplateVersion: %v", err)
	}
	if tv.VersionNo != 1 {
		t.Fatalf("expected version_no=1, got %d", tv.VersionNo)
	}

	published, err := ts.PublishTemplateVersion(ctx, tv.ID)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.Status != domain.TemplateVersionStatusPublished {
		t.Fatalf("expected published, got %q", published.Status)
	}

	latest, err := ts.GetLatestPublishedVersion(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("GetLatest: %v", err)
	}
	if latest.ID != published.ID {
		t.Fatalf("latest mismatch: got %v want %v", latest.ID, published.ID)
	}
	if len(latest.Sections) != 1 || latest.Sections[0].Key != "purpose" {
		t.Fatalf("sections did not round-trip: %+v", latest.Sections)
	}
}

func TestTemplateStoreVersionNoIncrements(t *testing.T) {
	pool := newTestDB(t)
	ts := store.NewTemplateStore(pool)
	ctx := context.Background()

	tpl, _ := domain.NewTemplate("Standard", uuid.Nil)
	tpl, err := ts.CreateTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	sections := []domain.Section{{
		Key:    "purpose",
		Title:  "Purpose",
		Order:  1,
		Blocks: []domain.Block{{Type: domain.BlockTypeEditable}},
	}}

	for i := 1; i <= 3; i++ {
		tv, err := domain.NewTemplateVersion(tpl.ID.String(), sections)
		if err != nil {
			t.Fatalf("domain version %d: %v", i, err)
		}
		tv, err = ts.CreateTemplateVersion(ctx, tv)
		if err != nil {
			t.Fatalf("CreateTemplateVersion %d: %v", i, err)
		}
		if tv.VersionNo != i {
			t.Fatalf("expected version_no=%d, got %d", i, tv.VersionNo)
		}
	}
}

func TestTemplateStoreGetLatestPublishedSkipsDrafts(t *testing.T) {
	pool := newTestDB(t)
	ts := store.NewTemplateStore(pool)
	ctx := context.Background()

	tpl, _ := domain.NewTemplate("Standard", uuid.Nil)
	tpl, err := ts.CreateTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	sections := []domain.Section{{
		Key:    "purpose",
		Title:  "Purpose",
		Order:  1,
		Blocks: []domain.Block{{Type: domain.BlockTypeEditable}},
	}}

	v1, _ := domain.NewTemplateVersion(tpl.ID.String(), sections)
	v1, err = ts.CreateTemplateVersion(ctx, v1)
	if err != nil {
		t.Fatalf("create v1: %v", err)
	}
	if _, err := ts.PublishTemplateVersion(ctx, v1.ID); err != nil {
		t.Fatalf("publish v1: %v", err)
	}

	// v2 stays as draft.
	v2, _ := domain.NewTemplateVersion(tpl.ID.String(), sections)
	if _, err := ts.CreateTemplateVersion(ctx, v2); err != nil {
		t.Fatalf("create v2: %v", err)
	}

	latest, err := ts.GetLatestPublishedVersion(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("GetLatestPublishedVersion: %v", err)
	}
	if latest.ID != v1.ID {
		t.Fatalf("expected latest=v1 (only published), got %v", latest.ID)
	}
}

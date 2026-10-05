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

func TestTemplateStoreRenameTemplate(t *testing.T) {
	pool := newTestDB(t)
	ts := store.NewTemplateStore(pool)
	ctx := context.Background()

	tpl, err := domain.NewTemplate("Original Name", uuid.Nil)
	if err != nil {
		t.Fatalf("domain: %v", err)
	}
	tpl, err = ts.CreateTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	// Rename changes the name and leaves the version list untouched.
	before, err := ts.ListTemplateVersions(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("ListTemplateVersions before: %v", err)
	}
	renamed, err := ts.RenameTemplate(ctx, tpl.ID, "New Name")
	if err != nil {
		t.Fatalf("RenameTemplate: %v", err)
	}
	if renamed.Name != "New Name" {
		t.Fatalf("name not updated: got %q", renamed.Name)
	}
	if renamed.ID != tpl.ID || renamed.Code != tpl.Code {
		t.Fatalf("id/code changed on rename: %+v", renamed)
	}
	after, err := ts.ListTemplateVersions(ctx, tpl.ID)
	if err != nil {
		t.Fatalf("ListTemplateVersions after: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("rename created a version: before=%d after=%d", len(before), len(after))
	}

	// Unknown id -> ErrTemplateNotFound.
	if _, err := ts.RenameTemplate(ctx, uuid.New(), "x"); !errors.Is(err, store.ErrTemplateNotFound) {
		t.Fatalf("want ErrTemplateNotFound, got %v", err)
	}
}

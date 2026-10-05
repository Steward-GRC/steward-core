// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/google/uuid"
)

func TestTemplateVersionAllowsEmptyDraft(t *testing.T) {
	// A draft may start with no sections; the "at least one section" invariant
	// is enforced at publish time, not at creation.
	tv, err := domain.NewTemplateVersion(uuid.New().String(), nil)
	if err != nil {
		t.Fatalf("unexpected err for empty draft: %v", err)
	}
	if tv.Status != domain.TemplateVersionStatusDraft {
		t.Fatalf("expected draft status, got %q", tv.Status)
	}
	if len(tv.Sections) != 0 {
		t.Fatalf("expected no sections, got %d", len(tv.Sections))
	}
}

func TestTemplateVersionSectionOrderEnforced(t *testing.T) {
	sections := []domain.Section{
		{Key: "scope", Title: "Scope", Order: 2, Blocks: []domain.Block{{Type: domain.BlockTypeEditable}}},
		{Key: "purpose", Title: "Purpose", Order: 1, Blocks: []domain.Block{{Type: domain.BlockTypeEditable}}},
	}
	tv, err := domain.NewTemplateVersion(uuid.New().String(), sections)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if tv.Sections[0].Key != "purpose" {
		t.Fatalf("expected sections sorted by order; first key = %q", tv.Sections[0].Key)
	}
}

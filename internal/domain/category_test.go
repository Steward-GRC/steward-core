// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/google/uuid"
)

func TestCategorySlugMustBeNonEmpty(t *testing.T) {
	_, err := domain.NewCategory("HR", "", uuid.Nil)
	if err == nil {
		t.Fatal("expected error for empty slug")
	}
}

func TestCategoryNameMustBeNonEmpty(t *testing.T) {
	_, err := domain.NewCategory("", "hr", uuid.Nil)
	if err == nil {
		t.Fatal("expected error for empty name")
	}
}

func TestCategoryCreatedWithoutParent(t *testing.T) {
	g, err := domain.NewCategory("HR", "hr", uuid.Nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if g.ID == uuid.Nil {
		t.Fatal("expected non-nil ID")
	}
	if g.ParentID != uuid.Nil {
		t.Fatalf("expected nil parent, got %v", g.ParentID)
	}
}

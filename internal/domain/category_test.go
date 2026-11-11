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

// TestCategoryDocumentTypesAccepts maps the mock's accepts() rule: both takes either type, policies
// and procedures take their own, none takes neither, and the zero value reads as both.
func TestCategoryDocumentTypesAccepts(t *testing.T) {
	cases := []struct {
		name      string
		types     domain.CategoryDocumentTypes
		policy    bool
		procedure bool
	}{
		{"zero value reads as both", "", true, true},
		{"both", domain.CategoryDocumentTypesBoth, true, true},
		{"policies only", domain.CategoryDocumentTypesPolicies, true, false},
		{"procedures only", domain.CategoryDocumentTypesProcedures, false, true},
		{"none is a container", domain.CategoryDocumentTypesNone, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.types.Accepts(domain.DocumentTypePolicy); got != c.policy {
				t.Errorf("Accepts(policy) = %v, want %v", got, c.policy)
			}
			if got := c.types.Accepts(domain.DocumentTypeProcedure); got != c.procedure {
				t.Errorf("Accepts(procedure) = %v, want %v", got, c.procedure)
			}
			// An unset document type reads as a policy, like the rest of the domain.
			if got := c.types.Accepts(""); got != c.policy {
				t.Errorf("Accepts(unset) = %v, want %v", got, c.policy)
			}
		})
	}
}

func TestCategoryDocumentTypesValid(t *testing.T) {
	for _, ok := range []domain.CategoryDocumentTypes{"", "both", "policies", "procedures", "none"} {
		if !ok.Valid() {
			t.Errorf("%q should be valid", ok)
		}
	}
	if domain.CategoryDocumentTypes("everything").Valid() {
		t.Error("an unknown value must not be valid")
	}
}

// A new category holds both types, whatever its parent holds: the setting is the category's own and
// is not inherited (the mock creates every category as BOTH).
func TestNewCategoryHoldsBoth(t *testing.T) {
	c, err := domain.NewCategory("HR", "hr", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	if c.DocumentTypes != domain.CategoryDocumentTypesBoth {
		t.Fatalf("DocumentTypes = %q, want both", c.DocumentTypes)
	}
}

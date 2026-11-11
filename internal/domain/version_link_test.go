// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// Mock: "offers a fixed list, each type with its inverse".
func TestRelationshipTypes_InversesRoundTrip(t *testing.T) {
	types := domain.RelationshipTypes()
	if len(types) != 10 {
		t.Fatalf("len = %d, want 10 (five pairs)", len(types))
	}
	related, ok := domain.LookupRelationshipType("RELATED_TO")
	if !ok || related.Label != "is related to" || related.InverseID != "RELATED_BY" || related.InverseLabel != "is related by" {
		t.Fatalf("RELATED_TO = %+v", related)
	}
	for _, typ := range types {
		inverse, ok := domain.LookupRelationshipType(typ.InverseID)
		if !ok {
			t.Fatalf("%s has no inverse", typ.ID)
		}
		if inverse.InverseID != typ.ID {
			t.Errorf("%s round trip: inverse of inverse = %s", typ.ID, inverse.InverseID)
		}
		if inverse.Label != typ.InverseLabel {
			t.Errorf("%s: inverse label %q, want %q", typ.ID, inverse.Label, typ.InverseLabel)
		}
	}
}

// Mock: "refuses an unknown type, a link to itself and a repeated link".
func TestValidateRelatedLinks(t *testing.T) {
	own, other := uuid.New(), uuid.New()
	cases := []struct {
		name  string
		links []domain.RelatedLinkInput
		want  error
	}{
		{"fine", []domain.RelatedLinkInput{{PolicyID: other, Type: "SUPERSEDES"}, {PolicyID: other, Type: "RELATED_TO"}}, nil},
		{"empty", nil, nil},
		{"unknown type", []domain.RelatedLinkInput{{PolicyID: other, Type: "LOVES"}}, domain.ErrUnknownRelationshipType},
		{"itself", []domain.RelatedLinkInput{{PolicyID: own, Type: "RELATED_TO"}}, domain.ErrRelatedToItself},
		{"repeated", []domain.RelatedLinkInput{{PolicyID: other, Type: "RELATED_TO"}, {PolicyID: other, Type: "RELATED_TO"}}, domain.ErrRepeatedLink},
	}
	for _, c := range cases {
		if err := domain.ValidateRelatedLinks(own, c.links); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
}

// Mock: "names each reference only once".
func TestValidateLinkIDs_RefusesRepeats(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	if err := domain.ValidateLinkIDs([]uuid.UUID{a, b}); err != nil {
		t.Fatalf("distinct ids: %v", err)
	}
	if err := domain.ValidateLinkIDs([]uuid.UUID{a, b, a}); !errors.Is(err, domain.ErrRepeatedLink) {
		t.Fatalf("repeat: err = %v, want ErrRepeatedLink", err)
	}
}

// Mock: "are unique by title" / "refuse an empty title": the title is trimmed.
func TestNormalizeDefinitionTerm(t *testing.T) {
	got, err := domain.NormalizeDefinitionTerm("  Rto ")
	if err != nil || got != "Rto" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := domain.NormalizeDefinitionTerm("  \t"); !errors.Is(err, domain.ErrEmptyDefinitionTerm) {
		t.Fatalf("empty: err = %v", err)
	}
}

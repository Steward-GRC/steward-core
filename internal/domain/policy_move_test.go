// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

func TestRefuseMove(t *testing.T) {
	now := time.Now()
	target := domain.Category{ID: uuid.New(), Name: "Conduct"}
	live := domain.Policy{ID: uuid.New(), DocumentType: domain.DocumentTypePolicy}
	retired := domain.Policy{ID: uuid.New(), RetiredAt: &now}
	acceptsAll := func(domain.Category, domain.DocumentType) bool { return true }
	acceptsNone := func(domain.Category, domain.DocumentType) bool { return false }

	if r := domain.RefuseMove(live, target, acceptsAll); r != nil {
		t.Fatalf("a live document the target holds must move, got %+v", r)
	}
	if r := domain.RefuseMove(live, target, nil); r != nil {
		t.Fatalf("no type gate means every type is held, got %+v", r)
	}
	r := domain.RefuseMove(retired, target, acceptsAll)
	if r == nil || r.Code != domain.MoveRefusalRetired {
		t.Fatalf("retired: got %+v", r)
	}
	r = domain.RefuseMove(live, target, acceptsNone)
	if r == nil || r.Code != domain.MoveRefusalCategoryRefusesType || !strings.Contains(r.Message, "CATEGORY_REFUSES_TYPE") ||
		!strings.Contains(r.Message, "Conduct") {
		t.Fatalf("type refusal: got %+v", r)
	}
	// Retired is checked first, as the mock does.
	if r := domain.RefuseMove(retired, target, acceptsNone); r == nil || r.Code != domain.MoveRefusalRetired {
		t.Fatalf("retired must win: %+v", r)
	}
}

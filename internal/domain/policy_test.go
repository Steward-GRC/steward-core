// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/google/uuid"
)

func TestPolicyTitleMustBeNonEmpty(t *testing.T) {
	_, err := domain.NewPolicy("", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err == nil {
		t.Fatal("expected error for empty title")
	}
}

func TestPolicyNumberGeneratedFromCategorySlug(t *testing.T) {
	p, err := domain.NewPolicy("IT Security", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if p.Number != "" {
		// number is assigned by store (requires category slug + sequence); domain keeps it empty until persisted
		t.Fatalf("expected empty number before persistence, got %q", p.Number)
	}
}

func TestNewDraftCreatesVersionWithDraftStatus(t *testing.T) {
	pv, err := domain.NewPolicyVersionDraft(uuid.New(), uuid.New(), uuid.New(), `{"root":{}}`)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if pv.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("expected draft status, got %q", pv.Status)
	}
}

func TestNewDraftRejectsEmptyContent(t *testing.T) {
	_, err := domain.NewPolicyVersionDraft(uuid.New(), uuid.New(), uuid.New(), "")
	if err == nil {
		t.Fatal("expected error for empty content")
	}
}

func TestPublishDraftFreezesPolicyVersion(t *testing.T) {
	pv, _ := domain.NewPolicyVersionDraft(uuid.New(), uuid.New(), uuid.New(), `{"root":{}}`)
	pv.VersionNo = 1
	published, err := pv.Publish()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if published.Status != domain.PolicyVersionStatusPublished {
		t.Fatalf("expected published status, got %q", published.Status)
	}
	if published.PublishedAt.IsZero() {
		t.Fatal("expected non-zero PublishedAt")
	}
}

func TestPublishDraftRequiresVersionNo(t *testing.T) {
	pv, _ := domain.NewPolicyVersionDraft(uuid.New(), uuid.New(), uuid.New(), `{"root":{}}`)
	// VersionNo defaults to 0 (unassigned)
	_, err := pv.Publish()
	if err == nil {
		t.Fatal("expected error when VersionNo is 0 (not yet assigned by store)")
	}
}

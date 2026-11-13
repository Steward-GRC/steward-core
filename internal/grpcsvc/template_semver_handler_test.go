// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"testing"
	"time"

	"github.com/google/uuid"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
)

func TestTemplateVersionToProtoCarriesNumberTypeAndDates(t *testing.T) {
	published := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	created := published.Add(-time.Hour)
	tv := domain.TemplateVersion{
		ID: uuid.New(), TemplateID: uuid.New(), VersionNo: 4, Status: domain.TemplateVersionStatusPublished,
		CreatedAt: created, PublishedAt: &published,
		Semver: domain.SemVer{Major: 2, Minor: 1}, ChangeType: domain.ChangeTypeMinor,
	}
	got := templateVersionToProto(tv)
	if got.GetSemanticVersion().GetText() != "2.1.0" || got.GetChangeType() != corev1.ChangeType_CHANGE_TYPE_MINOR {
		t.Fatalf("number and type = %v / %v", got.GetSemanticVersion(), got.GetChangeType())
	}
	if !got.GetPublishedAt().AsTime().Equal(published) || !got.GetCreatedAt().AsTime().Equal(created) {
		t.Fatalf("dates = %v / %v", got.GetCreatedAt(), got.GetPublishedAt())
	}
	tv.PublishedAt = nil
	if templateVersionToProto(tv).GetPublishedAt() != nil {
		t.Fatal("a draft has no published_at")
	}
}

func TestTemplateToProtoCarriesUpdatedAt(t *testing.T) {
	at := time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)
	got := templateToProto(domain.Template{ID: uuid.New(), Name: "T", UpdatedAt: at})
	if !got.GetUpdatedAt().AsTime().Equal(at) {
		t.Fatalf("updated_at = %v", got.GetUpdatedAt())
	}
	if templateToProto(domain.Template{ID: uuid.New()}).GetUpdatedAt() != nil {
		t.Fatal("no updated_at when the store gave none")
	}
}

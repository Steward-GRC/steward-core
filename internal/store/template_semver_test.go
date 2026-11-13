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

func newTemplateWithDraft(t *testing.T, ts *store.TemplateStore, secs []domain.Section) domain.TemplateVersion {
	t.Helper()
	ctx := context.Background()
	tpl, _ := domain.NewTemplate("T-"+uuid.NewString()[:6], uuid.Nil)
	tpl, err := ts.CreateTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}
	tv, _ := domain.NewTemplateVersion(tpl.ID.String(), secs)
	tv, err = ts.CreateTemplateVersion(ctx, tv)
	if err != nil {
		t.Fatalf("CreateTemplateVersion: %v", err)
	}
	return tv
}

func nextDraft(t *testing.T, ts *store.TemplateStore, templateID uuid.UUID, secs []domain.Section) domain.TemplateVersion {
	t.Helper()
	tv, _ := domain.NewTemplateVersion(templateID.String(), secs)
	tv, err := ts.CreateTemplateVersion(context.Background(), tv)
	if err != nil {
		t.Fatalf("CreateTemplateVersion: %v", err)
	}
	return tv
}

func TestTemplateSemverFirstPublishIsOneZeroZero(t *testing.T) {
	ts := store.NewTemplateStore(newTestDB(t))
	ctx := context.Background()
	a := domain.Section{Key: "a", Title: "A", Order: 1, Required: true}
	d := newTemplateWithDraft(t, ts, []domain.Section{a})
	if !d.Projected || d.Semver.String() != "1.0.0" || d.ChangeType != domain.ChangeTypeMajor {
		t.Fatalf("first draft projection = %s / %q / %v", d.Semver, d.ChangeType, d.Projected)
	}
	pub, err := ts.PublishTemplateVersion(ctx, d.ID)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Semver.String() != "1.0.0" || pub.ChangeType != domain.ChangeTypeMajor || pub.PublishedAt == nil || pub.Projected {
		t.Fatalf("published = %s / %q / %v / %v", pub.Semver, pub.ChangeType, pub.PublishedAt, pub.Projected)
	}
}

func TestTemplateSemverFollowsTheSectionChange(t *testing.T) {
	ts := store.NewTemplateStore(newTestDB(t))
	ctx := context.Background()
	a := domain.Section{Key: "a", Title: "A", Order: 1, Required: true}
	b := domain.Section{Key: "b", Title: "B", Order: 2}
	c := domain.Section{Key: "c", Title: "C", Order: 3, Required: true}

	v1 := newTemplateWithDraft(t, ts, []domain.Section{a})
	if _, err := ts.PublishTemplateVersion(ctx, v1.ID); err != nil {
		t.Fatal(err)
	}

	steps := []struct {
		name string
		secs []domain.Section
		want string
		ct   domain.ChangeType
	}{
		{"an optional section is a minor", []domain.Section{a, b}, "1.1.0", domain.ChangeTypeMinor},
		{"a retitle is a fix", []domain.Section{a, {Key: "b", Title: "Bee", Order: 2}}, "1.1.1", domain.ChangeTypeFix},
		{"a required section is a major", []domain.Section{a, {Key: "b", Title: "Bee", Order: 2}, c}, "2.0.0", domain.ChangeTypeMajor},
	}
	for _, s := range steps {
		d := nextDraft(t, ts, v1.TemplateID, s.secs)
		if !d.Projected || d.Semver.String() != s.want || d.ChangeType != s.ct {
			t.Fatalf("%s: draft projection = %s / %q", s.name, d.Semver, d.ChangeType)
		}
		pub, err := ts.PublishTemplateVersion(ctx, d.ID)
		if err != nil {
			t.Fatalf("%s: publish: %v", s.name, err)
		}
		if pub.Semver.String() != s.want || pub.ChangeType != s.ct {
			t.Fatalf("%s: published = %s / %q, want %s / %q", s.name, pub.Semver, pub.ChangeType, s.want, s.ct)
		}
		got, err := ts.GetTemplateVersion(ctx, pub.ID)
		if err != nil || got.Semver != pub.Semver {
			t.Fatalf("%s: stored = %s, %v", s.name, got.Semver, err)
		}
	}

	list, err := ts.ListTemplateVersions(ctx, v1.TemplateID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 || list[0].Semver.String() != "2.0.0" || list[3].Semver.String() != "1.0.0" || list[0].PublishedAt == nil {
		t.Fatalf("list = %+v", list)
	}
}

func TestTemplateSemverDraftEditReprojects(t *testing.T) {
	ts := store.NewTemplateStore(newTestDB(t))
	ctx := context.Background()
	a := domain.Section{Key: "a", Title: "A", Order: 1, Required: true}
	v1 := newTemplateWithDraft(t, ts, []domain.Section{a})
	if _, err := ts.PublishTemplateVersion(ctx, v1.ID); err != nil {
		t.Fatal(err)
	}
	d := nextDraft(t, ts, v1.TemplateID, []domain.Section{a})
	if d.Semver.String() != "1.0.1" {
		t.Fatalf("unchanged draft = %s", d.Semver)
	}
	got, err := ts.UpdateTemplateVersionSections(ctx, d.ID, []domain.Section{a, {Key: "z", Title: "Z", Order: 2, Required: true}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Semver.String() != "2.0.0" || got.ChangeType != domain.ChangeTypeMajor {
		t.Fatalf("after the edit = %s / %q", got.Semver, got.ChangeType)
	}
}

func TestTemplateUpdatedAtFollowsVersionActivity(t *testing.T) {
	ts := store.NewTemplateStore(newTestDB(t))
	ctx := context.Background()
	a := domain.Section{Key: "a", Title: "A", Order: 1, Required: true}
	v := newTemplateWithDraft(t, ts, []domain.Section{a})

	before, err := ts.GetTemplate(ctx, v.TemplateID)
	if err != nil {
		t.Fatal(err)
	}
	if before.UpdatedAt.Before(before.CreatedAt) {
		t.Fatalf("updated %v is before created %v", before.UpdatedAt, before.CreatedAt)
	}
	pub, err := ts.PublishTemplateVersion(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ts.GetTemplate(ctx, v.TemplateID)
	if err != nil {
		t.Fatal(err)
	}
	if pub.PublishedAt == nil || after.UpdatedAt.Before(*pub.PublishedAt) {
		t.Fatalf("a publish must move updated_at: updated %v, published %v", after.UpdatedAt, pub.PublishedAt)
	}
	list, err := ts.ListTemplates(ctx, nil)
	if err != nil || len(list) != 1 || !list[0].UpdatedAt.Equal(after.UpdatedAt) {
		t.Fatalf("list = %+v, %v", list, err)
	}
	renamed, err := ts.RenameTemplate(ctx, v.TemplateID, "New name")
	if err != nil || renamed.UpdatedAt.Before(after.UpdatedAt) {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
}

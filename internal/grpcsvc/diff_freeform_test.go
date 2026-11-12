// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"strings"
	"testing"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/google/uuid"
)

const (
	freeformFrom = `{"root":{"type":"root","children":[
		{"type":"paragraph","children":[{"type":"text","text":"Intro."}]},
		{"type":"heading","children":[{"type":"text","text":"Purpose"}]},
		{"type":"paragraph","children":[{"type":"text","text":"Why we exist."}]}]}}`
	freeformTo = `{"root":{"type":"root","children":[
		{"type":"paragraph","children":[{"type":"text","text":"Intro."}]},
		{"type":"heading","children":[{"type":"text","text":"Purpose"}]},
		{"type":"paragraph","children":[{"type":"text","text":"Why we really exist."}]},
		{"type":"heading","children":[{"type":"text","text":"Scope"}]},
		{"type":"paragraph","children":[{"type":"text","text":"Everyone."}]}]}}`
)

func diffKinds(resp *corev1.DiffVersionsResponse) string {
	var parts []string
	for _, d := range resp.GetDiffs() {
		parts = append(parts, d.GetSectionKey()+"|"+d.GetSectionTitle()+"|"+d.GetChangeType())
	}
	return strings.Join(parts, ",")
}

func TestDiffVersionsFreeformPair(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	c := newFakeCache()
	policyID, fromID, toID := uuid.New(), uuid.New(), uuid.New()
	fakePS.versions[fromID] = domain.PolicyVersion{ID: fromID, PolicyID: policyID,
		Status: domain.PolicyVersionStatusPublished, ContentJSON: freeformFrom}
	fakePS.versions[toID] = domain.PolicyVersion{ID: toID, PolicyID: policyID,
		Status: domain.PolicyVersionStatusPublished, ContentJSON: freeformTo}

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), fakeTS, domain.NoopValidator{}, nil).WithVersionCache(c)
	resp, err := h.DiffVersions(context.Background(),
		&corev1.DiffVersionsRequest{FromVersionId: fromID.String(), ToVersionId: toID.String()})
	if err != nil {
		t.Fatalf("DiffVersions: %v", err)
	}
	want := "body|Introduction|unchanged,purpose|Purpose|changed,scope|Scope|added"
	if got := diffKinds(resp); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	if !strings.Contains(resp.GetDiffs()[1].GetWordDiffHtml(), "<ins>really</ins>") {
		t.Fatalf("changed html %q", resp.GetDiffs()[1].GetWordDiffHtml())
	}
	if len(fakeTS.getVersionCalls) != 0 {
		t.Fatalf("a template-less pair must not look up a template version: %v", fakeTS.getVersionCalls)
	}
	// Both published, so the pair is cached like any other.
	if c.sets != 1 {
		t.Fatalf("want the published pair cached, sets=%d", c.sets)
	}
}

func TestDiffVersionsMixedPairDoesNotError(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	tvID := uuid.New()
	fakeTS.versions[tvID] = domain.TemplateVersion{ID: tvID, Status: domain.TemplateVersionStatusPublished,
		Sections: []domain.Section{{Key: "purpose", Title: "Purpose", Order: 1}}}
	policyID, fromID, toID := uuid.New(), uuid.New(), uuid.New()
	fakePS.versions[fromID] = domain.PolicyVersion{ID: fromID, PolicyID: policyID,
		Status: domain.PolicyVersionStatusPublished, TemplateVersionID: tvID, ContentJSON: freeformFrom}
	fakePS.versions[toID] = domain.PolicyVersion{ID: toID, PolicyID: policyID,
		Status: domain.PolicyVersionStatusPublished, ContentJSON: freeformTo}

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.DiffVersions(context.Background(),
		&corev1.DiffVersionsRequest{FromVersionId: fromID.String(), ToVersionId: toID.String()})
	if err != nil {
		t.Fatalf("DiffVersions: %v", err)
	}
	want := "body|Introduction|unchanged,purpose|Purpose|changed,scope|Scope|added"
	if got := diffKinds(resp); got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	for _, d := range resp.GetDiffs() {
		if d.GetIsBoilerplate() {
			t.Fatalf("a block diff is never boilerplate: %+v", d)
		}
	}
}

func TestDiffVersionsTemplatePairStillUsesTemplateSections(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	fromID, toID, tvID := seedPublishedPair(fakePS, fakeTS)
	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), fakeTS, domain.NoopValidator{}, nil)
	resp, err := h.DiffVersions(context.Background(),
		&corev1.DiffVersionsRequest{FromVersionId: fromID.String(), ToVersionId: toID.String()})
	if err != nil {
		t.Fatalf("DiffVersions: %v", err)
	}
	if got := diffKinds(resp); got != "body|Body|changed" {
		t.Fatalf("got %s", got)
	}
	if len(fakeTS.getVersionCalls) != 1 || fakeTS.getVersionCalls[0] != tvID {
		t.Fatalf("template pair must load its template version: %v", fakeTS.getVersionCalls)
	}
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// pinnedTemplates answers "latest published version" from a fixed map, so a test can say which
// version a draft must end up pinned to. A template not in the map has no published version.
type pinnedTemplates struct {
	*fakeTemplateStore
	latest map[uuid.UUID]uuid.UUID
}

func (p pinnedTemplates) GetLatestPublishedVersion(_ context.Context, templateID uuid.UUID) (domain.TemplateVersion, error) {
	if id, ok := p.latest[templateID]; ok {
		return domain.TemplateVersion{ID: id, TemplateID: templateID, Status: domain.TemplateVersionStatusPublished}, nil
	}
	return domain.TemplateVersion{}, pgx.ErrNoRows
}

type freeformEnv struct {
	h        *grpcsvc.PolicyHandler
	ps       *fakePolicyStore
	category domain.Category
	tplA     uuid.UUID // the category default
	tplB     uuid.UUID // another template
	tvA, tvB uuid.UUID // their latest published versions
}

func newFreeformEnv(t *testing.T) *freeformEnv {
	t.Helper()
	e := &freeformEnv{
		ps:   newFakePolicyStore(),
		tplA: uuid.New(), tplB: uuid.New(), tvA: uuid.New(), tvB: uuid.New(),
	}
	cs := newFakeCategoryStore()
	e.category = domain.Category{ID: uuid.New(), Slug: "finance", DefaultTemplateID: e.tplA}
	cs.categories[e.category.ID] = e.category
	ts := pinnedTemplates{
		fakeTemplateStore: newFakeTemplateStore(),
		latest:            map[uuid.UUID]uuid.UUID{e.tplA: e.tvA, e.tplB: e.tvB},
	}
	e.h = grpcsvc.NewPolicyHandler(e.ps, cs, ts, domain.NoopValidator{}, nil)
	return e
}

func (e *freeformEnv) create(t *testing.T, templateID string) *corev1.Policy {
	t.Helper()
	resp, err := e.h.CreatePolicy(context.Background(), &corev1.CreatePolicyRequest{
		HomeCategoryId: e.category.ID.String(), Title: "Hello", TemplateId: templateID,
		OwnerUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	return resp.GetPolicy()
}

func (e *freeformEnv) draftOf(t *testing.T, p *corev1.Policy) domain.PolicyVersion {
	t.Helper()
	id, err := uuid.Parse(p.GetCurrentDraftVersionId())
	if err != nil {
		t.Fatalf("policy has no draft id: %v", err)
	}
	return e.ps.versions[id]
}

func (e *freeformEnv) setTemplate(p *corev1.Policy, none bool, templateID string) (*corev1.SetPolicyTemplateResponse, error) {
	return e.h.SetPolicyTemplate(context.Background(), &corev1.SetPolicyTemplateRequest{
		PolicyId: p.GetId(), None: none, TemplateId: templateID, ActorUserId: uuid.New().String(),
	})
}

// Mock: freeform-and-empty "is the category's default when none is chosen".
func TestCreatePolicyStartsFromCategoryDefaultTemplate(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, "")
	if got := e.draftOf(t, p).TemplateVersionID; got != e.tvA {
		t.Fatalf("draft pinned to %s, want the category default's latest version %s", got, e.tvA)
	}
}

func TestCreatePolicyChosenTemplateWinsOverCategoryDefault(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, e.tplB.String())
	if got := e.draftOf(t, p).TemplateVersionID; got != e.tvB {
		t.Fatalf("draft pinned to %s, want the chosen template's version %s", got, e.tvB)
	}
}

func TestCreatePolicyWithNoDefaultStaysUnpinned(t *testing.T) {
	e := newFreeformEnv(t)
	cat := e.category
	cat.DefaultTemplateID = uuid.Nil
	e.h = grpcsvc.NewPolicyHandler(e.ps, func() *fakeCategoryStore {
		cs := newFakeCategoryStore()
		cs.categories[cat.ID] = cat
		return cs
	}(), newFakeTemplateStore(), domain.NoopValidator{}, nil)
	p := e.create(t, "")
	if got := e.draftOf(t, p).TemplateVersionID; got != uuid.Nil {
		t.Fatalf("draft pinned to %s, want none", got)
	}
}

// Mock: "blanks the untouched starting draft, drops its template and marks the policy freeform".
func TestSetPolicyTemplateNoneBlanksUntouchedDraft(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, "")
	resp, err := e.setTemplate(p, true, "")
	if err != nil {
		t.Fatalf("SetPolicyTemplate: %v", err)
	}
	if !resp.GetPolicy().GetTemplateNone() || resp.GetPolicy().GetTemplateId() != "" {
		t.Fatalf("policy = none %v template %q, want freeform", resp.GetPolicy().GetTemplateNone(), resp.GetPolicy().GetTemplateId())
	}
	d := e.draftOf(t, p)
	if d.TemplateVersionID != uuid.Nil {
		t.Fatalf("draft still pinned to %s", d.TemplateVersionID)
	}
	if d.ContentJSON != "{}" {
		t.Fatalf("draft content = %q, want blank", d.ContentJSON)
	}
}

// A draft holding only a seeded skeleton (no author text) is still untouched, and is blanked.
func TestSetPolicyTemplateNoneBlanksSkeletonDraft(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, "")
	d := e.draftOf(t, p)
	d.ContentJSON = `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"   "}]}]}}`
	e.ps.versions[d.ID] = d
	if _, err := e.setTemplate(p, true, ""); err != nil {
		t.Fatalf("SetPolicyTemplate: %v", err)
	}
	if got := e.draftOf(t, p).ContentJSON; got != "{}" {
		t.Fatalf("draft content = %q, want blank", got)
	}
}

// Mock: "leaves a draft the author has already written alone".
func TestSetPolicyTemplateNoneLeavesWrittenDraftAlone(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, "")
	d := e.draftOf(t, p)
	d.ContentJSON = textDoc("My own words.")
	e.ps.versions[d.ID] = d
	if _, err := e.setTemplate(p, true, ""); err != nil {
		t.Fatalf("SetPolicyTemplate: %v", err)
	}
	got := e.draftOf(t, p)
	if got.ContentJSON != textDoc("My own words.") {
		t.Fatalf("written draft was changed: %q", got.ContentJSON)
	}
	if got.TemplateVersionID != e.tvA {
		t.Fatalf("written draft was re-pinned to %s", got.TemplateVersionID)
	}
}

// A draft already in review is not the starting draft and is never touched.
func TestSetPolicyTemplateLeavesInReviewVersionAlone(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, "")
	d := e.draftOf(t, p)
	d.Status = domain.PolicyVersionStatusInReview
	e.ps.versions[d.ID] = d
	if _, err := e.setTemplate(p, true, ""); err != nil {
		t.Fatalf("SetPolicyTemplate: %v", err)
	}
	if got := e.draftOf(t, p).TemplateVersionID; got != e.tvA {
		t.Fatalf("in-review version was re-pinned to %s", got)
	}
}

// Mock: "starts the untouched draft from another template when one is chosen".
func TestSetPolicyTemplateStartsUntouchedDraftFromChosenTemplate(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, "")
	resp, err := e.setTemplate(p, false, e.tplB.String())
	if err != nil {
		t.Fatalf("SetPolicyTemplate: %v", err)
	}
	if resp.GetPolicy().GetTemplateId() != e.tplB.String() {
		t.Fatalf("policy template = %q, want %s", resp.GetPolicy().GetTemplateId(), e.tplB)
	}
	if got := e.draftOf(t, p).TemplateVersionID; got != e.tvB {
		t.Fatalf("draft pinned to %s, want %s", got, e.tvB)
	}
}

// Inheriting again (neither none nor an id) puts the untouched draft back on the category default.
func TestSetPolicyTemplateInheritRepinsToCategoryDefault(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, e.tplB.String())
	if _, err := e.setTemplate(p, false, ""); err != nil {
		t.Fatalf("SetPolicyTemplate: %v", err)
	}
	if got := e.draftOf(t, p).TemplateVersionID; got != e.tvA {
		t.Fatalf("draft pinned to %s, want category default %s", got, e.tvA)
	}
}

// A chosen template with no published version is refused before anything is written.
func TestSetPolicyTemplateRefusesTemplateWithoutPublishedVersion(t *testing.T) {
	e := newFreeformEnv(t)
	p := e.create(t, "")
	_, err := e.setTemplate(p, false, uuid.New().String())
	if got := status.Code(err); got != codes.FailedPrecondition {
		t.Fatalf("code = %v (%v), want FailedPrecondition", got, err)
	}
	pid, _ := uuid.Parse(p.GetId())
	if e.ps.policies[pid].TemplateID != uuid.Nil {
		t.Fatal("policy template changed despite the refusal")
	}
}

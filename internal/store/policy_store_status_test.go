// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// Tests for the in-review and rejected version statuses (issue #51). The
// behaviour mirrors the mock server's author loop: a submitted version keeps
// the draft slot and stays editable, a rejection is final and frees the slot.

const reviewedText = `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"Body"}]}]}}`

type statusFixture struct {
	t   *testing.T
	db  *postgres.DB
	ps  *store.PolicyStore
	p   domain.Policy
	tv  domain.TemplateVersion
	ctx context.Context
}

func newStatusFixture(t *testing.T) *statusFixture {
	t.Helper()
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)
	p, _ := domain.NewPolicy("Status Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	return &statusFixture{t: t, db: pool, ps: ps, p: p, tv: tv, ctx: ctx}
}

// save upserts the policy's draft with the given content.
func (f *statusFixture) save(content string) (domain.PolicyVersion, store.UpsertDraftResult) {
	f.t.Helper()
	d, _ := domain.NewPolicyVersionDraft(f.p.ID, f.tv.ID, uuid.New(), content)
	saved, res, err := f.ps.UpsertDraft(f.ctx, d)
	if err != nil {
		f.t.Fatalf("UpsertDraft: %v", err)
	}
	return saved, res
}

func (f *statusFixture) setStatus(id uuid.UUID, to domain.PolicyVersionStatus) domain.PolicyVersion {
	f.t.Helper()
	pv, err := f.ps.SetVersionStatus(f.ctx, id, to)
	if err != nil {
		f.t.Fatalf("SetVersionStatus(%s): %v", to, err)
	}
	return pv
}

func (f *statusFixture) policy() domain.Policy {
	f.t.Helper()
	p, err := f.ps.GetPolicy(f.ctx, f.p.ID)
	if err != nil {
		f.t.Fatalf("GetPolicy: %v", err)
	}
	return p
}

// Mock: author-loop "makes a rejection final: the author's next save starts a
// new draft, the rejected version stays".
func TestPolicyStoreRejectedVersionStaysAndNextSaveIsNewDraft(t *testing.T) {
	f := newStatusFixture(t)
	d1, _ := f.save(reviewedText)
	f.setStatus(d1.ID, domain.PolicyVersionStatusInReview)
	f.setStatus(d1.ID, domain.PolicyVersionStatusRejected)

	d2, res := f.save(`{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"Second"}]}]}}`)
	if d2.ID == d1.ID {
		t.Fatalf("next save reused the rejected version %s", d1.ID)
	}
	if !res.Inserted {
		t.Fatal("expected the next save to insert a new draft row")
	}
	if d2.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("new draft status = %q, want draft", d2.Status)
	}
	kept, err := f.ps.GetPolicyVersion(f.ctx, d1.ID)
	if err != nil {
		t.Fatalf("GetPolicyVersion rejected: %v", err)
	}
	if kept.Status != domain.PolicyVersionStatusRejected {
		t.Fatalf("rejected version status = %q, want rejected", kept.Status)
	}
	if kept.ContentJSON == "" || kept.ContentJSON == d2.ContentJSON {
		t.Fatalf("rejected version content changed: %q", kept.ContentJSON)
	}

	// A second rejection must not collide with the first on (policy_id, version_no).
	f.setStatus(d2.ID, domain.PolicyVersionStatusInReview)
	f.setStatus(d2.ID, domain.PolicyVersionStatusRejected)
	d3, _ := f.save(reviewedText)
	if d3.ID == d1.ID || d3.ID == d2.ID {
		t.Fatalf("third draft reused a rejected id")
	}

	// A rejection takes no version number: the next publish is version 1.
	pub := f.setStatus(d3.ID, domain.PolicyVersionStatusPublished)
	if pub.VersionNo != 1 {
		t.Fatalf("first published version_no = %d, want 1", pub.VersionNo)
	}
}

// Mock: author-loop "ends the run as soon as the approvals still possible
// cannot reach the quorum": currentVersionStatus REJECTED, nothing published.
func TestPolicyStoreRejectedIsCurrentVersionWhenNothingPublished(t *testing.T) {
	f := newStatusFixture(t)
	d1, _ := f.save(reviewedText)
	f.setStatus(d1.ID, domain.PolicyVersionStatusInReview)
	f.setStatus(d1.ID, domain.PolicyVersionStatusRejected)

	p := f.policy()
	if p.CurrentVersionStatus != domain.PolicyVersionStatusRejected {
		t.Fatalf("current status = %q, want rejected", p.CurrentVersionStatus)
	}
	if p.CurrentPublishedVersionID != uuid.Nil {
		t.Fatalf("published pointer = %s, want none", p.CurrentPublishedVersionID)
	}
	if p.CurrentDraftVersionID != uuid.Nil {
		t.Fatalf("draft pointer = %s, want none (the rejected version is not the draft)", p.CurrentDraftVersionID)
	}

	listed, err := f.ps.ListPolicies(f.ctx, f.p.HomeCategoryID, false, domain.DocumentTypePolicy)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if len(listed) != 1 || listed[0].CurrentVersionStatus != domain.PolicyVersionStatusRejected {
		t.Fatalf("ListPolicies current status = %+v, want rejected", listed)
	}

	// The next save makes the draft the current version again.
	d2, _ := f.save(reviewedText)
	p = f.policy()
	if p.CurrentVersionStatus != domain.PolicyVersionStatusDraft || p.CurrentDraftVersionID != d2.ID {
		t.Fatalf("after next save: status %q draft %s, want draft %s", p.CurrentVersionStatus, p.CurrentDraftVersionID, d2.ID)
	}
}

// Mock: author-loop "starts a run on the home category's workflow..." (the
// version reads IN_REVIEW) and saveDraft "one in review stays in review".
func TestPolicyStoreSubmitMovesDraftToInReview(t *testing.T) {
	f := newStatusFixture(t)
	d1, _ := f.save(reviewedText)
	got := f.setStatus(d1.ID, domain.PolicyVersionStatusInReview)
	if got.Status != domain.PolicyVersionStatusInReview {
		t.Fatalf("status = %q, want in_review", got.Status)
	}
	p := f.policy()
	if p.CurrentVersionStatus != domain.PolicyVersionStatusInReview || p.CurrentDraftVersionID != d1.ID {
		t.Fatalf("policy view: status %q draft %s, want in_review %s", p.CurrentVersionStatus, p.CurrentDraftVersionID, d1.ID)
	}
	// It keeps the draft slot: a raw second draft is still refused.
	err := f.ps.InsertDraftRaw(f.ctx, uuid.New(), f.p.ID, f.tv.ID, reviewedText, uuid.New())
	if !errors.Is(err, store.ErrDraftAlreadyExists) {
		t.Fatalf("second draft beside an in-review version: err = %v, want ErrDraftAlreadyExists", err)
	}
	// Approval publishes it with the next version number.
	pub := f.setStatus(d1.ID, domain.PolicyVersionStatusPublished)
	if pub.VersionNo != 1 || pub.Status != domain.PolicyVersionStatusPublished {
		t.Fatalf("published from review: no %d status %q", pub.VersionNo, pub.Status)
	}
}

func TestPolicyStoreSaveWhileInReviewStaysInReview(t *testing.T) {
	f := newStatusFixture(t)
	d1, _ := f.save(reviewedText)
	f.setStatus(d1.ID, domain.PolicyVersionStatusInReview)

	edited := `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"Edited"}]}]}}`
	d2, res := f.save(edited)
	if d2.ID != d1.ID || res.Inserted {
		t.Fatalf("save in review: id %s inserted %v, want in-place on %s", d2.ID, res.Inserted, d1.ID)
	}
	if d2.Status != domain.PolicyVersionStatusInReview {
		t.Fatalf("status after save = %q, want in_review", d2.Status)
	}
	if err := f.ps.UpdateDraftContent(f.ctx, d1.ID, reviewedText); err != nil {
		t.Fatalf("UpdateDraftContent in review: %v", err)
	}
}

// Mock: author-loop "returns a withdrawn submission to Draft".
func TestPolicyStoreWithdrawInReviewReturnsToDraft(t *testing.T) {
	f := newStatusFixture(t)
	d1, _ := f.save(reviewedText)
	f.setStatus(d1.ID, domain.PolicyVersionStatusInReview)
	got := f.setStatus(d1.ID, domain.PolicyVersionStatusDraft)
	if got.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("status = %q, want draft", got.Status)
	}
	if p := f.policy(); p.CurrentVersionStatus != domain.PolicyVersionStatusDraft || p.CurrentDraftVersionID != d1.ID {
		t.Fatalf("policy view after withdraw: %q %s", p.CurrentVersionStatus, p.CurrentDraftVersionID)
	}
}

// Mock: links.ts EDITABLE leaves REJECTED out; the rejected version is final.
func TestPolicyStoreRejectedVersionRefusesInPlaceEdits(t *testing.T) {
	f := newStatusFixture(t)
	d1, _ := f.save(reviewedText)
	f.setStatus(d1.ID, domain.PolicyVersionStatusInReview)
	f.setStatus(d1.ID, domain.PolicyVersionStatusRejected)

	if err := f.ps.UpdateDraftContent(f.ctx, d1.ID, `{"root":{"type":"root","children":[]}}`); err == nil {
		t.Fatal("UpdateDraftContent on a rejected version: want an error")
	}
	title := "New title"
	if err := f.ps.SetProposedTitle(f.ctx, d1.ID, &title); err == nil {
		t.Fatal("SetProposedTitle on a rejected version: want an error")
	}
	if err := f.ps.DiscardDraft(f.ctx, f.p.ID); !errors.Is(err, store.ErrNoDraft) {
		t.Fatalf("DiscardDraft with only a rejected version: err = %v, want ErrNoDraft", err)
	}
	if _, err := f.ps.GetPolicyVersion(f.ctx, d1.ID); err != nil {
		t.Fatalf("rejected version gone after discard: %v", err)
	}
	// Rejected is terminal except for archiving.
	if _, err := f.ps.SetVersionStatus(f.ctx, d1.ID, domain.PolicyVersionStatusPublished); !errors.Is(err, domain.ErrInvalidVersionStatusTransition) {
		t.Fatalf("rejected -> published: err = %v, want ErrInvalidVersionStatusTransition", err)
	}
}

// Mock: policyVersions resolver lists versions with a publishedAt only.
func TestPolicyStoreListPublishedVersionsSkipsReviewAndRejected(t *testing.T) {
	f := newStatusFixture(t)
	d1, _ := f.save(reviewedText)
	f.setStatus(d1.ID, domain.PolicyVersionStatusPublished)

	d2, _ := f.save(reviewedText)
	f.setStatus(d2.ID, domain.PolicyVersionStatusInReview)
	f.setStatus(d2.ID, domain.PolicyVersionStatusRejected)
	d3, _ := f.save(reviewedText)
	f.setStatus(d3.ID, domain.PolicyVersionStatusInReview)

	vs, err := f.ps.ListPublishedVersions(f.ctx, f.p.ID)
	if err != nil {
		t.Fatalf("ListPublishedVersions: %v", err)
	}
	if len(vs) != 1 || vs[0].ID != d1.ID {
		t.Fatalf("listed %d versions, want only the published one", len(vs))
	}
	// A rejection beside a published version leaves the published one current.
	f.setStatus(d3.ID, domain.PolicyVersionStatusRejected)
	if p := f.policy(); p.CurrentVersionStatus != domain.PolicyVersionStatusPublished || p.CurrentPublishedVersionID != d1.ID {
		t.Fatalf("current after rejection beside published: %q %s", p.CurrentVersionStatus, p.CurrentPublishedVersionID)
	}
}

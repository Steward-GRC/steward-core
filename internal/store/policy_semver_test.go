// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

type semverEnv struct {
	t   *testing.T
	ps  *store.PolicyStore
	ctx context.Context
	pol domain.Policy
	tv  domain.TemplateVersion
}

func newSemverEnv(t *testing.T) *semverEnv {
	t.Helper()
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)
	p, _ := domain.NewPolicy("Semver Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	return &semverEnv{t: t, ps: ps, ctx: ctx, pol: p, tv: tv}
}

func (e *semverEnv) save(text string) domain.PolicyVersion {
	e.t.Helper()
	d, _ := domain.NewPolicyVersionDraft(e.pol.ID, e.tv.ID, uuid.New(), `{"sections":{"purpose":"`+text+`"}}`)
	v, _, err := e.ps.UpsertDraft(e.ctx, d)
	if err != nil {
		e.t.Fatalf("UpsertDraft: %v", err)
	}
	return v
}

func (e *semverEnv) publish(v domain.PolicyVersion) domain.PolicyVersion {
	e.t.Helper()
	out, err := e.ps.SetVersionStatus(e.ctx, v.ID, domain.PolicyVersionStatusPublished)
	if err != nil {
		e.t.Fatalf("publish: %v", err)
	}
	return out
}

func (e *semverEnv) get(id uuid.UUID) domain.PolicyVersion {
	e.t.Helper()
	v, err := e.ps.GetPolicyVersion(e.ctx, id)
	if err != nil {
		e.t.Fatalf("GetPolicyVersion: %v", err)
	}
	return v
}

func TestSemverFirstPublishIsOneZeroZero(t *testing.T) {
	e := newSemverEnv(t)
	v := e.publish(e.save("Review access."))
	if v.Semver.String() != "1.0.0" || v.VersionNo != 1 || v.Change.Type != domain.ChangeTypeMajor {
		t.Fatalf("first publish = %s / no %d / %q", v.Semver, v.VersionNo, v.Change.Type)
	}
}

func TestSemverPublishFollowsTheClassifiedType(t *testing.T) {
	e := newSemverEnv(t)
	e.publish(e.save("Keep a log of all access reviews done by the team lead."))

	// A fix: one word swapped.
	fix := e.publish(e.save("Keep a log of all access reviews done by the group lead."))
	if fix.Semver.String() != "1.0.1" || fix.Change.Type != domain.ChangeTypeFix || fix.VersionNo != 2 {
		t.Fatalf("fix = %s / %q / no %d", fix.Semver, fix.Change.Type, fix.VersionNo)
	}
	// A minor: more than ten words change.
	minor := e.publish(e.save("Keep a log of all access reviews done by the group lead. Share the log with every manager and keep each review on record for later."))
	if minor.Semver.String() != "1.1.0" || minor.Change.Type != domain.ChangeTypeMinor {
		t.Fatalf("minor = %s / %q", minor.Semver, minor.Change.Type)
	}
	// A major: a new obligation.
	major := e.publish(e.save("Keep a log of all access reviews done by the group lead. Staff must share the log within 30 days."))
	if major.Semver.String() != "2.0.0" || major.Change.Type != domain.ChangeTypeMajor {
		t.Fatalf("major = %s / %q", major.Semver, major.Change.Type)
	}
	if got := e.get(major.ID); got.Semver != major.Semver {
		t.Fatalf("stored semver = %s, want %s", got.Semver, major.Semver)
	}
}

func TestSemverSubmitStoresTheSuggestionAndProjectsTheNumber(t *testing.T) {
	e := newSemverEnv(t)
	e.publish(e.save("Keep a log of all access reviews."))
	d := e.save("Keep a log of all access reviews. Staff must report within 5 days.")

	// A draft projects what it would publish as, before any submit.
	proj := e.get(d.ID)
	if !proj.Projected || proj.Semver.String() != "2.0.0" || proj.Change.Type != domain.ChangeTypeMajor || proj.Change.Suggested != domain.ChangeTypeMajor {
		t.Fatalf("draft projection = %+v", proj)
	}

	sub, err := e.ps.SetVersionStatus(e.ctx, d.ID, domain.PolicyVersionStatusInReview)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if sub.Change.Suggested != domain.ChangeTypeMajor || sub.Change.Type != domain.ChangeTypeMajor || sub.VersionNo != 0 {
		t.Fatalf("submitted = %+v", sub)
	}
	got := e.get(d.ID)
	if got.Change.Suggested != domain.ChangeTypeMajor || got.Semver.String() != "2.0.0" || !got.Projected {
		t.Fatalf("stored suggestion = %+v", got)
	}
}

func TestSemverAuthorRaisesAtSubmit(t *testing.T) {
	e := newSemverEnv(t)
	e.publish(e.save("Keep a log of all access reviews done by the team lead."))
	d := e.save("Keep a log of all access reviews done by the group lead.")
	author := uuid.New()
	if _, err := e.ps.SetVersionStatus(e.ctx, d.ID, domain.PolicyVersionStatusInReview,
		store.WithChangePick(domain.ChangeTypeMinor, "", author)); err != nil {
		t.Fatalf("submit: %v", err)
	}
	pub := e.publish(d)
	if pub.Semver.String() != "1.1.0" || pub.Change.Type != domain.ChangeTypeMinor || pub.Change.SetBy != author {
		t.Fatalf("published = %s / %+v", pub.Semver, pub.Change)
	}
}

func TestSemverLoweringWaitsForAgreement(t *testing.T) {
	e := newSemverEnv(t)
	e.publish(e.save("Keep a log of all access reviews."))
	d := e.save("Keep a log of all access reviews. Staff must report within 5 days.")
	author, approver := uuid.New(), uuid.New()

	_, err := e.ps.SetVersionStatus(e.ctx, d.ID, domain.PolicyVersionStatusInReview,
		store.WithChangePick(domain.ChangeTypeMinor, " ", author))
	if !errors.Is(err, domain.ErrChangeReasonRequired) {
		t.Fatalf("lowering without a reason: err = %v", err)
	}
	if got := e.get(d.ID); got.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("a refused submit must leave the draft a draft, got %q", got.Status)
	}

	sub, err := e.ps.SetVersionStatus(e.ctx, d.ID, domain.PolicyVersionStatusInReview,
		store.WithChangePick(domain.ChangeTypeMinor, "wording only", author))
	if err != nil {
		t.Fatalf("submit with a lowering: %v", err)
	}
	if sub.Change.Type != domain.ChangeTypeMajor || sub.Change.Requested != domain.ChangeTypeMinor || sub.Change.Reason != "wording only" {
		t.Fatalf("lowering applied early: %+v", sub.Change)
	}

	// Nothing but the agreement applies it: a publish now is still Major.
	pending := e.get(d.ID)
	if pending.Semver.String() != "2.0.0" {
		t.Fatalf("projection must follow the effective type, got %s", pending.Semver)
	}

	agreed, err := e.ps.RecordChangeTypeAgreement(e.ctx, d.ID, []uuid.UUID{approver})
	if err != nil {
		t.Fatalf("agreement: %v", err)
	}
	if agreed.Change.Type != domain.ChangeTypeMinor || agreed.Change.Requested != "" || len(agreed.Change.AgreedBy) != 1 || agreed.Change.AgreedBy[0] != approver {
		t.Fatalf("after agreement = %+v", agreed.Change)
	}
	pub := e.publish(d)
	if pub.Semver.String() != "1.1.0" || pub.Change.Type != domain.ChangeTypeMinor {
		t.Fatalf("published = %s / %+v", pub.Semver, pub.Change)
	}
	if got := e.get(pub.ID); len(got.Change.AgreedBy) != 1 {
		t.Fatalf("who agreed must be kept: %+v", got.Change)
	}
}

func TestSemverAgreementNeedsAPendingLowering(t *testing.T) {
	e := newSemverEnv(t)
	e.publish(e.save("Keep a log."))
	d := e.save("Keep a log of everything.")
	if _, err := e.ps.RecordChangeTypeAgreement(e.ctx, d.ID, []uuid.UUID{uuid.New()}); !errors.Is(err, domain.ErrNoPendingLowering) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.ps.RecordChangeTypeAgreement(e.ctx, uuid.New(), []uuid.UUID{uuid.New()}); err == nil {
		t.Fatal("an unknown version must fail")
	}
}

func TestSemverSetChangeTypeOnADraftAndRefusedOnPublished(t *testing.T) {
	e := newSemverEnv(t)
	first := e.publish(e.save("Keep a log."))
	author := uuid.New()
	if _, err := e.ps.SetVersionChangeType(e.ctx, first.ID, domain.ChangeTypeMajor, "", author); !errors.Is(err, store.ErrChangeTypeLocked) {
		t.Fatalf("a published version is not editable: %v", err)
	}
	d := e.save("Keep a log of every review.")
	got, err := e.ps.SetVersionChangeType(e.ctx, d.ID, domain.ChangeTypeMajor, "", author)
	if err != nil {
		t.Fatalf("SetVersionChangeType: %v", err)
	}
	if got.Change.Type != domain.ChangeTypeMajor || got.Change.SetBy != author {
		t.Fatalf("change = %+v", got.Change)
	}
	if e.publish(d).Semver.String() != "2.0.0" {
		t.Fatal("a raised pick must publish as chosen")
	}
}

func TestSemverFirstVersionRefusesAPick(t *testing.T) {
	e := newSemverEnv(t)
	d := e.save("Keep a log.")
	if _, err := e.ps.SetVersionChangeType(e.ctx, d.ID, domain.ChangeTypeFix, "r", uuid.New()); !errors.Is(err, store.ErrFirstVersion) {
		t.Fatalf("err = %v, want ErrFirstVersion", err)
	}
}

func TestSemverEditAfterSubmitCannotSlipPastTheSuggestion(t *testing.T) {
	e := newSemverEnv(t)
	e.publish(e.save("Keep a log of all access reviews done by the team lead."))
	d := e.save("Keep a log of all access reviews done by the group lead.")
	if _, err := e.ps.SetVersionStatus(e.ctx, d.ID, domain.PolicyVersionStatusInReview,
		store.WithChangePick(domain.ChangeTypeFix, "", uuid.New())); err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The in-review version stays editable; the new text adds an obligation.
	e.save("Keep a log of all access reviews done by the group lead. Staff must share it within 30 days.")
	pub := e.publish(d)
	if pub.Semver.String() != "2.0.0" || pub.Change.Type != domain.ChangeTypeMajor {
		t.Fatalf("published = %s / %q, want 2.0.0 major", pub.Semver, pub.Change.Type)
	}
}

func TestSemverWithdrawAndRepublishDoNotCollide(t *testing.T) {
	e := newSemverEnv(t)
	e.publish(e.save("Keep a log of all access reviews done by the team lead."))
	two := e.publish(e.save("Keep a log of all access reviews done by the group lead."))
	if two.Semver.String() != "1.0.1" {
		t.Fatalf("second = %s", two.Semver)
	}
	// Withdraw the published version back to a draft, then publish it again.
	if _, err := e.ps.SetVersionStatus(e.ctx, two.ID, domain.PolicyVersionStatusDraft); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	again := e.publish(two)
	if again.VersionNo != 2 || again.Semver.String() != "1.0.1" {
		t.Fatalf("republished = no %d / %s", again.VersionNo, again.Semver)
	}
	next := e.publish(e.save("Keep a log of all access reviews done by the group lead. Staff must share it every month."))
	if next.VersionNo != 3 || next.Semver.String() != "2.0.0" {
		t.Fatalf("next = no %d / %s", next.VersionNo, next.Semver)
	}
}

func TestSemverPublishDraftShortcutNumbersToo(t *testing.T) {
	e := newSemverEnv(t)
	e.save("Keep a log.")
	v, err := e.ps.PublishDraft(e.ctx, e.pol.ID, uuid.Nil)
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if v.Semver.String() != "1.0.0" {
		t.Fatalf("PublishDraft = %s", v.Semver)
	}
	e.save("Keep a log of all work.")
	v2, err := e.ps.PublishDraft(e.ctx, e.pol.ID, uuid.Nil)
	if err != nil {
		t.Fatalf("PublishDraft 2: %v", err)
	}
	if v2.VersionNo != 2 || v2.Semver.IsZero() {
		t.Fatalf("PublishDraft 2 = no %d / %s", v2.VersionNo, v2.Semver)
	}
}

func TestSemverListAndPolicyReadCarryTheNumber(t *testing.T) {
	e := newSemverEnv(t)
	e.publish(e.save("Keep a log of all access reviews done by the team lead."))
	two := e.publish(e.save("Keep a log of all access reviews done by the group lead."))
	vs, err := e.ps.ListPublishedVersions(e.ctx, e.pol.ID)
	if err != nil {
		t.Fatalf("ListPublishedVersions: %v", err)
	}
	if len(vs) != 2 || vs[0].Semver.String() != "1.0.0" || vs[1].Semver.String() != "1.0.1" {
		t.Fatalf("list = %+v", vs)
	}
	pol, err := e.ps.GetPolicy(e.ctx, e.pol.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol.CurrentSemver != two.Semver {
		t.Fatalf("policy current semver = %s, want %s", pol.CurrentSemver, two.Semver)
	}
	list, err := e.ps.ListPolicies(e.ctx, e.pol.HomeCategoryID, false, domain.DocumentTypePolicy)
	if err != nil || len(list) != 1 || list[0].CurrentSemver != two.Semver {
		t.Fatalf("ListPolicies = %+v, %v", list, err)
	}
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// Tests for the per-approver review snapshots (issue #55). They mirror the
// mock server's review-snapshots tests: the snapshot is taken on first read,
// moves only for the approver who acted, and a later edit shows again.

const (
	snapBefore = `{"sections":{"steps":"Book through the portal 14 days ahead."}}`
	snapEdit1  = `{"sections":{"steps":"Book through the portal 21 days ahead."}}`
	snapEdit2  = `{"sections":{"steps":"Book through the portal 30 days ahead."}}`
)

type snapFixture struct {
	t       *testing.T
	ps      *store.PolicyStore
	ss      *store.ReviewSnapshotStore
	ctx     context.Context
	policy  domain.Policy
	tv      domain.TemplateVersion
	version uuid.UUID
	charlie uuid.UUID
	alice   uuid.UUID
}

// newSnapFixture submits a draft with snapBefore, so the version is in review.
func newSnapFixture(t *testing.T) *snapFixture {
	t.Helper()
	pool := newTestDB(t)
	ctx := context.Background()
	ps := store.NewPolicyStore(pool)
	g, tv := setupCategoryAndTemplate(t, store.NewCategoryStore(pool), store.NewTemplateStore(pool))
	p, _ := domain.NewPolicy("Snapshot Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	f := &snapFixture{
		t: t, ps: ps, ss: store.NewReviewSnapshotStore(pool), ctx: ctx, policy: p, tv: tv,
		charlie: uuid.New(), alice: uuid.New(),
	}
	d, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), snapBefore)
	saved, _, err := ps.UpsertDraft(ctx, d)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	if _, err := ps.SetVersionStatus(ctx, saved.ID, domain.PolicyVersionStatusInReview); err != nil {
		t.Fatalf("SetVersionStatus(in_review): %v", err)
	}
	f.version = saved.ID
	return f
}

// authorEdits edits the version in place, as the author does while it is in review.
func (f *snapFixture) authorEdits(content string) {
	f.t.Helper()
	if err := f.ps.UpdateDraftContent(f.ctx, f.version, content); err != nil {
		f.t.Fatalf("UpdateDraftContent: %v", err)
	}
}

func (f *snapFixture) read(user uuid.UUID) domain.ReviewSnapshotView {
	f.t.Helper()
	v, err := f.ss.GetOrCreate(f.ctx, f.version, user)
	if err != nil {
		f.t.Fatalf("GetOrCreate: %v", err)
	}
	return v
}

func (f *snapFixture) mark(user uuid.UUID) {
	f.t.Helper()
	if err := f.ss.Mark(f.ctx, f.version, user); err != nil {
		f.t.Fatalf("Mark: %v", err)
	}
}

// moved reports whether the approver's snapshot text differs from the current text.
func moved(v domain.ReviewSnapshotView) bool { return v.Before != v.Current }

func TestSnapshotFirstReadShowsNothingAndCreatesIt(t *testing.T) {
	f := newSnapFixture(t)
	v := f.read(f.charlie)
	if !v.Created {
		t.Fatal("the first read should create the snapshot")
	}
	if moved(v) {
		t.Fatalf("a first read shows nothing: before %s current %s", v.Before, v.Current)
	}
	if v.TemplateVersionID != f.tv.ID {
		t.Fatalf("template version: got %s want %s", v.TemplateVersionID, f.tv.ID)
	}
	if again := f.read(f.charlie); again.Created {
		t.Fatal("a second read must not create it again")
	}
}

func TestSnapshotShowsEachApproverTheEditMadeInReview(t *testing.T) {
	f := newSnapFixture(t)
	if moved(f.read(f.charlie)) || moved(f.read(f.alice)) {
		t.Fatal("nothing before the author edits")
	}
	f.authorEdits(snapEdit1)
	for name, u := range map[string]uuid.UUID{"charlie": f.charlie, "alice": f.alice} {
		v := f.read(u)
		if !moved(v) {
			t.Fatalf("%s should see the edit", name)
		}
		if v.Created {
			t.Fatalf("%s: the snapshot was already taken", name)
		}
	}
}

func TestSnapshotStoresACopyNotAReference(t *testing.T) {
	f := newSnapFixture(t)
	f.read(f.charlie)
	f.authorEdits(snapEdit1)
	v := f.read(f.charlie)
	if v.Before == v.Current {
		t.Fatal("the snapshot must keep the text as it was when first read")
	}
	var got, want map[string]map[string]string
	mustJSON(t, v.Before, &got)
	mustJSON(t, snapBefore, &want)
	if got["sections"]["steps"] != want["sections"]["steps"] {
		t.Fatalf("snapshot text changed: %s", v.Before)
	}
}

func TestMarkMovesOnlyThatApprover(t *testing.T) {
	f := newSnapFixture(t)
	f.read(f.charlie)
	f.read(f.alice)
	f.authorEdits(snapEdit1)
	f.mark(f.charlie)
	if moved(f.read(f.charlie)) {
		t.Fatal("charlie acted, so nothing should show for charlie")
	}
	if !moved(f.read(f.alice)) {
		t.Fatal("alice did not act, so she should still see the edit")
	}
}

func TestMarkBeforeAnyReadTakesTheSnapshotAtTheCurrentText(t *testing.T) {
	f := newSnapFixture(t)
	f.authorEdits(snapEdit1)
	f.mark(f.charlie)
	v := f.read(f.charlie)
	if v.Created || moved(v) {
		t.Fatalf("marking creates the snapshot at the current text: %+v", v)
	}
}

func TestSnapshotShowsALaterEditAgainAfterTheApproverMovedOn(t *testing.T) {
	f := newSnapFixture(t)
	f.read(f.charlie)
	f.authorEdits(snapEdit1)
	f.mark(f.charlie)
	if moved(f.read(f.charlie)) {
		t.Fatal("moved on, nothing to show")
	}
	f.authorEdits(snapEdit2)
	if !moved(f.read(f.charlie)) {
		t.Fatal("a later edit should show again")
	}
}

func TestSnapshotIsKeptAcrossWithdrawAndResubmit(t *testing.T) {
	f := newSnapFixture(t)
	f.read(f.charlie)
	if _, err := f.ps.SetVersionStatus(f.ctx, f.version, domain.PolicyVersionStatusDraft); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	f.authorEdits(snapEdit1)
	if _, err := f.ps.SetVersionStatus(f.ctx, f.version, domain.PolicyVersionStatusInReview); err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	v := f.read(f.charlie)
	if v.Created || !moved(v) {
		t.Fatalf("the snapshot should survive withdraw and resubmit: %+v", v)
	}
}

func TestSnapshotsGoWithTheVersion(t *testing.T) {
	f := newSnapFixture(t)
	f.read(f.charlie)
	if err := f.ps.DiscardDraft(f.ctx, f.policy.ID); err != nil {
		t.Fatalf("DiscardDraft: %v", err)
	}
	if _, err := f.ss.GetOrCreate(f.ctx, f.version, f.charlie); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("want ErrNoRows for a deleted version, got %v", err)
	}
	// Nothing is left behind by the cascade.
	n, err := f.ss.Count(f.ctx, f.version)
	if err != nil || n != 0 {
		t.Fatalf("snapshots left after delete: %d, %v", n, err)
	}
}

func TestSnapshotUnknownVersion(t *testing.T) {
	f := newSnapFixture(t)
	if _, err := f.ss.GetOrCreate(f.ctx, uuid.New(), f.charlie); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("GetOrCreate: want ErrNoRows, got %v", err)
	}
	if err := f.ss.Mark(f.ctx, uuid.New(), f.charlie); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("Mark: want ErrNoRows, got %v", err)
	}
}

func TestSnapshotFreeformVersionHasNilTemplate(t *testing.T) {
	ctx := context.Background()
	// A freeform policy: no template version.
	pool := newTestDB(t)
	ps := store.NewPolicyStore(pool)
	g, _ := setupCategoryAndTemplate(t, store.NewCategoryStore(pool), store.NewTemplateStore(pool))
	p, _ := domain.NewPolicy("Freeform", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	d, _ := domain.NewPolicyVersionDraft(p.ID, uuid.Nil, uuid.New(), snapBefore)
	saved, _, err := ps.UpsertDraft(ctx, d)
	if err != nil {
		t.Fatalf("UpsertDraft freeform: %v", err)
	}
	v, err := store.NewReviewSnapshotStore(pool).GetOrCreate(ctx, saved.ID, uuid.New())
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if v.TemplateVersionID != uuid.Nil {
		t.Fatalf("freeform has no template version, got %s", v.TemplateVersionID)
	}
}

func mustJSON(t *testing.T, in string, out any) {
	t.Helper()
	if err := json.Unmarshal([]byte(in), out); err != nil {
		t.Fatalf("unmarshal %q: %v", in, err)
	}
}

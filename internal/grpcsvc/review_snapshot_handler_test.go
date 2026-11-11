// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
)

// The handler side of the per-approver review snapshots (issue #55). The
// scenarios follow the mock's review-snapshots tests; the store's own tests
// cover the SQL.

const (
	hsBefore = `{"sections":{"steps":"Book through the portal 14 days ahead.","afterwards":"Ask."}}`
	hsEdit1  = `{"sections":{"steps":"Book through the portal 21 days ahead.","afterwards":"Ask."}}`
	hsEdit2  = `{"sections":{"steps":"Book through the portal 30 days ahead.","afterwards":"Ask."}}`
)

// fakeSnapshots keeps snapshots in memory the way the store does: lazily on
// first read, replaced on mark, one per (version, user).
type fakeSnapshots struct {
	templateVersion uuid.UUID
	current         map[uuid.UUID]string
	snaps           map[string]string
	err             error
}

func newFakeSnapshots(version uuid.UUID, tv uuid.UUID, content string) *fakeSnapshots {
	return &fakeSnapshots{templateVersion: tv, current: map[uuid.UUID]string{version: content}, snaps: map[string]string{}}
}

func (f *fakeSnapshots) key(v, u uuid.UUID) string { return v.String() + "/" + u.String() }

func (f *fakeSnapshots) GetOrCreate(_ context.Context, v, u uuid.UUID) (domain.ReviewSnapshotView, error) {
	if f.err != nil {
		return domain.ReviewSnapshotView{}, f.err
	}
	cur, ok := f.current[v]
	if !ok {
		return domain.ReviewSnapshotView{}, fmt.Errorf("get: %w", pgx.ErrNoRows)
	}
	created := false
	if _, ok := f.snaps[f.key(v, u)]; !ok {
		f.snaps[f.key(v, u)] = cur
		created = true
	}
	return domain.ReviewSnapshotView{Before: f.snaps[f.key(v, u)], Current: cur, TemplateVersionID: f.templateVersion, Created: created}, nil
}

func (f *fakeSnapshots) Mark(_ context.Context, v, u uuid.UUID) error {
	if f.err != nil {
		return f.err
	}
	cur, ok := f.current[v]
	if !ok {
		return fmt.Errorf("mark: %w", pgx.ErrNoRows)
	}
	f.snaps[f.key(v, u)] = cur
	return nil
}

type fakeSnapshotTemplates struct{ sections []domain.Section }

func (f fakeSnapshotTemplates) GetTemplateVersion(_ context.Context, id uuid.UUID) (domain.TemplateVersion, error) {
	return domain.TemplateVersion{ID: id, Sections: f.sections}, nil
}

type snapHarness struct {
	h       *grpcsvc.ReviewSnapshotHandler
	fake    *fakeSnapshots
	pub     *capturePublisher
	version uuid.UUID
	charlie string
	alice   string
}

func newSnapHarness(t *testing.T, templated bool) *snapHarness {
	t.Helper()
	version, tv := uuid.New(), uuid.New()
	if !templated {
		tv = uuid.Nil
	}
	fake := newFakeSnapshots(version, tv, hsBefore)
	tpl := fakeSnapshotTemplates{sections: []domain.Section{
		{Key: "steps", Title: "Steps", Order: 1}, {Key: "afterwards", Title: "Afterwards", Order: 2},
	}}
	pub := &capturePublisher{}
	return &snapHarness{
		h: grpcsvc.NewReviewSnapshotHandler(fake, tpl, audit.New(pub)), fake: fake, pub: pub, version: version,
		charlie: uuid.NewString(), alice: uuid.NewString(),
	}
}

func (s *snapHarness) since(t *testing.T, user string) []*corev1.SectionDiff {
	t.Helper()
	resp, err := s.h.GetChangesSinceReview(context.Background(),
		&corev1.GetChangesSinceReviewRequest{PolicyVersionId: s.version.String(), UserId: user})
	if err != nil {
		t.Fatalf("GetChangesSinceReview: %v", err)
	}
	return resp.GetDiffs()
}

func (s *snapHarness) mark(t *testing.T, user string) {
	t.Helper()
	if _, err := s.h.MarkVersionReviewed(context.Background(),
		&corev1.MarkVersionReviewedRequest{PolicyVersionId: s.version.String(), UserId: user}); err != nil {
		t.Fatalf("MarkVersionReviewed: %v", err)
	}
}

func (s *snapHarness) edit(content string) { s.fake.current[s.version] = content }

func changedOnly(diffs []*corev1.SectionDiff) []string {
	var out []string
	for _, d := range diffs {
		if d.GetChangeType() != "unchanged" {
			out = append(out, d.GetSectionKey())
		}
	}
	return out
}

func TestChangesSinceReviewShowsNothingBeforeTheAuthorEdits(t *testing.T) {
	for _, templated := range []bool{true, false} {
		s := newSnapHarness(t, templated)
		diffs := s.since(t, s.charlie)
		if got := changedOnly(diffs); len(got) != 0 {
			t.Fatalf("templated=%v: want nothing, got %v", templated, got)
		}
		if len(diffs) == 0 {
			t.Fatalf("templated=%v: the unchanged sections are still listed", templated)
		}
	}
}

func TestChangesSinceReviewShowsEachApproverTheEdit(t *testing.T) {
	s := newSnapHarness(t, true)
	s.since(t, s.charlie)
	s.since(t, s.alice)
	s.edit(hsEdit1)
	for _, u := range []string{s.charlie, s.alice} {
		got := changedOnly(s.since(t, u))
		if len(got) != 1 || got[0] != "steps" {
			t.Fatalf("want steps changed, got %v", got)
		}
	}
}

func TestChangesSinceReviewFreeformUsesTheKeysPresent(t *testing.T) {
	s := newSnapHarness(t, false)
	s.since(t, s.charlie)
	s.edit(hsEdit1)
	diffs := s.since(t, s.charlie)
	got := changedOnly(diffs)
	if len(got) != 1 || got[0] != "steps" {
		t.Fatalf("want steps changed, got %v", got)
	}
	for _, d := range diffs {
		if d.GetSectionKey() == "steps" && d.GetSectionTitle() != "Steps" {
			t.Fatalf("freeform title: %q", d.GetSectionTitle())
		}
	}
}

func TestMarkVersionReviewedMovesOnlyThatApprover(t *testing.T) {
	s := newSnapHarness(t, true)
	s.since(t, s.charlie)
	s.since(t, s.alice)
	s.edit(hsEdit1)
	s.mark(t, s.charlie)
	if got := changedOnly(s.since(t, s.charlie)); len(got) != 0 {
		t.Fatalf("charlie moved on: %v", got)
	}
	if got := changedOnly(s.since(t, s.alice)); len(got) == 0 {
		t.Fatal("alice did not act: she still sees the edit")
	}
}

func TestChangesSinceReviewShowsALaterEditAgain(t *testing.T) {
	s := newSnapHarness(t, true)
	s.since(t, s.charlie)
	s.edit(hsEdit1)
	s.mark(t, s.charlie)
	if got := changedOnly(s.since(t, s.charlie)); len(got) != 0 {
		t.Fatalf("moved on: %v", got)
	}
	s.edit(hsEdit2)
	if got := changedOnly(s.since(t, s.charlie)); len(got) == 0 {
		t.Fatal("a later edit shows again")
	}
}

func TestReviewSnapshotRefusals(t *testing.T) {
	s := newSnapHarness(t, true)
	ctx := context.Background()
	cases := map[string]struct {
		version, user string
		want          codes.Code
	}{
		"bad version id": {"nope", s.charlie, codes.InvalidArgument},
		"blank user":     {s.version.String(), "", codes.InvalidArgument},
		"bad user id":    {s.version.String(), "nope", codes.InvalidArgument},
		"unknown":        {uuid.NewString(), s.charlie, codes.NotFound},
	}
	for name, c := range cases {
		_, err := s.h.GetChangesSinceReview(ctx, &corev1.GetChangesSinceReviewRequest{PolicyVersionId: c.version, UserId: c.user})
		if status.Code(err) != c.want {
			t.Fatalf("%s (get): got %v want %v", name, status.Code(err), c.want)
		}
		_, err = s.h.MarkVersionReviewed(ctx, &corev1.MarkVersionReviewedRequest{PolicyVersionId: c.version, UserId: c.user})
		if status.Code(err) != c.want {
			t.Fatalf("%s (mark): got %v want %v", name, status.Code(err), c.want)
		}
	}
	s.fake.err = fmt.Errorf("db down")
	if _, err := s.h.GetChangesSinceReview(ctx, &corev1.GetChangesSinceReviewRequest{PolicyVersionId: s.version.String(), UserId: s.charlie}); status.Code(err) != codes.Internal {
		t.Fatalf("store failure: got %v", status.Code(err))
	}
}

func TestReviewSnapshotAuditCarriesIDsOnly(t *testing.T) {
	s := newSnapHarness(t, true)
	s.since(t, s.charlie)
	s.since(t, s.charlie) // the second read creates nothing and emits nothing
	s.edit(hsEdit1)
	s.mark(t, s.charlie)

	var actions []string
	for _, c := range s.pub.calls {
		ev := c.event
		actions = append(actions, ev.Action)
		if ev.Tier != audit.TierActivity {
			t.Fatalf("%s: tier %s, want activity", ev.Action, ev.Tier)
		}
		if ev.ActorUserID != s.charlie || ev.Subject != "policy_version:"+s.version.String() {
			t.Fatalf("%s: actor %q subject %q", ev.Action, ev.ActorUserID, ev.Subject)
		}
		blob := fmt.Sprint(ev)
		for _, text := range []string{"portal", "Book through", "Ask."} {
			if strings.Contains(blob, text) {
				t.Fatalf("%s: audit event carries content: %s", ev.Action, blob)
			}
		}
	}
	want := []string{"review.snapshot.taken", "review.snapshot.moved"}
	if fmt.Sprint(actions) != fmt.Sprint(want) {
		t.Fatalf("actions: got %v want %v", actions, want)
	}
}

func TestReviewSnapshotHandlerWithoutAuditor(t *testing.T) {
	version := uuid.New()
	h := grpcsvc.NewReviewSnapshotHandler(newFakeSnapshots(version, uuid.Nil, hsBefore), fakeSnapshotTemplates{}, nil)
	if _, err := h.MarkVersionReviewed(context.Background(),
		&corev1.MarkVersionReviewedRequest{PolicyVersionId: version.String(), UserId: uuid.NewString()}); err != nil {
		t.Fatalf("MarkVersionReviewed: %v", err)
	}
}

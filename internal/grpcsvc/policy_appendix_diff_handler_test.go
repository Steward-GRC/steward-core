// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

const (
	diffPublished = `{"sections":{"rates":"Per-diem rates by country, reviewed annually by Finance."}}`
	diffDraft     = `{"sections":{"rates":"Per-diem rates by country, reviewed every quarter.","notes":"New note."}}`
	diffTyped     = `{"sections":{"rates":"Typed, not saved."}}`
)

// writeSpy counts every write that reaches the backend, so a read-only call
// can show it made none.
type writeSpy struct {
	*fakePolicyAppendixBackend
	writes int
}

func (w *writeSpy) Create(ctx context.Context, p, a uuid.UUID, title, c string) (domain.PolicyAppendix, error) {
	w.writes++
	return w.fakePolicyAppendixBackend.Create(ctx, p, a, title, c)
}

func (w *writeSpy) SaveDraft(ctx context.Context, id, a uuid.UUID, title, c string) (domain.PolicyAppendix, bool, error) {
	w.writes++
	return w.fakePolicyAppendixBackend.SaveDraft(ctx, id, a, title, c)
}

func (w *writeSpy) Publish(ctx context.Context, id, a uuid.UUID) (domain.PolicyAppendix, error) {
	w.writes++
	return w.fakePolicyAppendixBackend.Publish(ctx, id, a)
}

func (w *writeSpy) Retire(ctx context.Context, id, a uuid.UUID) (domain.PolicyAppendix, bool, error) {
	w.writes++
	return w.fakePolicyAppendixBackend.Retire(ctx, id, a)
}

func (w *writeSpy) Restore(ctx context.Context, id uuid.UUID) (domain.PolicyAppendix, bool, error) {
	w.writes++
	return w.fakePolicyAppendixBackend.Restore(ctx, id)
}

func (w *writeSpy) ReorderAppendices(ctx context.Context, p uuid.UUID, ids []uuid.UUID) ([]domain.PolicyAppendix, error) {
	w.writes++
	return w.fakePolicyAppendixBackend.ReorderAppendices(ctx, p, ids)
}

type diffEnv struct {
	*appendixFixture
	w *writeSpy
}

// diffFixture holds an appendix with published v1 and (optionally) a draft v2.
func diffFixture(published, draft string) *diffEnv {
	f := newAppendixFixture()
	w := &writeSpy{fakePolicyAppendixBackend: f.b}
	f.h = grpcsvc.NewPolicyAppendixHandler(w, f.audit)
	a := f.appendix()
	a.Versions = nil
	if published != "" {
		v := f.row(1, domain.AppendixPublished, "Glossary")
		v.ContentJSON = published
		a.Versions = append(a.Versions, v)
	}
	if draft != "" {
		v := f.row(2, domain.AppendixDraft, "Glossary")
		v.ContentJSON = draft
		a.Versions = append(a.Versions, v)
	}
	f.b.result = a
	return &diffEnv{appendixFixture: f, w: w}
}

func changeTypes(r *corev1.DiffPolicyAppendixResponse) map[string]string {
	out := map[string]string{}
	for _, d := range r.GetDiffs() {
		out[d.GetSectionKey()] = d.GetChangeType()
	}
	return out
}

// appendices.test.ts "the version diff" (is shown to an author, a site admin
// and compliance): the diff of a draft change against the published row.
func TestDiffPolicyAppendix_DraftAgainstPublished(t *testing.T) {
	f := diffFixture(diffPublished, diffDraft)
	resp, err := f.h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{Id: f.id.String()})
	require.NoError(t, err)
	require.Equal(t, f.id, f.b.gotID)
	require.Equal(t, map[string]string{"notes": "added", "rates": "changed"}, changeTypes(resp))
	require.Equal(t, "notes", resp.GetDiffs()[0].GetSectionKey(), "sections come in key order")
	require.Contains(t, resp.GetDiffs()[1].GetWordDiffHtml(), "<ins>")
	require.Empty(t, f.events(), "a diff writes no audit event")
}

// appendix-titles-contributors.test.ts "diffs text that is not saved against
// what is published".
func TestDiffPolicyAppendix_LiveTextAgainstPublished(t *testing.T) {
	f := diffFixture(diffPublished, "")
	live := `{"sections":{"rates":"Per-diem rates by country, now reviewed every quarter."}}`
	resp, err := f.h.DiffPolicyAppendix(context.Background(),
		&corev1.DiffPolicyAppendixRequest{Id: f.id.String(), ContentJson: proto.String(live)})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"rates": "changed"}, changeTypes(resp))
}

// The supplied text stands in for the stored draft.
func TestDiffPolicyAppendix_LiveTextWinsOverTheDraft(t *testing.T) {
	f := diffFixture(diffPublished, diffDraft)
	resp, err := f.h.DiffPolicyAppendix(context.Background(),
		&corev1.DiffPolicyAppendixRequest{Id: f.id.String(), ContentJson: proto.String(diffTyped)})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"rates": "changed"}, changeTypes(resp), "the draft's notes section is not in the diff")
}

// appendix-titles-contributors.test.ts "saves nothing: no draft appears and
// the stored draft is left alone".
func TestDiffPolicyAppendix_SavesNothing(t *testing.T) {
	f := diffFixture(diffPublished, "")
	before := f.b.result
	_, err := f.h.DiffPolicyAppendix(context.Background(),
		&corev1.DiffPolicyAppendixRequest{Id: f.id.String(), ContentJson: proto.String(diffTyped)})
	require.NoError(t, err)
	require.Equal(t, 1, f.b.calls, "one read and no write")
	require.Zero(t, f.w.writes, "no store write method was called")
	require.Nil(t, f.b.result.Draft(), "no draft appears")
	require.Equal(t, before, f.b.result)
	require.Empty(t, f.events())

	f = diffFixture(diffPublished, diffDraft)
	_, err = f.h.DiffPolicyAppendix(context.Background(),
		&corev1.DiffPolicyAppendixRequest{Id: f.id.String(), ContentJson: proto.String(diffTyped)})
	require.NoError(t, err)
	require.Zero(t, f.w.writes)
	require.Equal(t, diffDraft, f.b.result.Draft().ContentJSON, "the stored draft is left alone")
}

// appendix-titles-contributors.test.ts "is unchanged against itself".
func TestDiffPolicyAppendix_UnchangedAgainstItself(t *testing.T) {
	f := diffFixture(diffPublished, "")
	for _, req := range []*corev1.DiffPolicyAppendixRequest{
		{Id: f.id.String()},
		{Id: f.id.String(), ContentJson: proto.String(diffPublished)},
	} {
		resp, err := f.h.DiffPolicyAppendix(context.Background(), req)
		require.NoError(t, err)
		require.NotEmpty(t, resp.GetDiffs())
		for _, d := range resp.GetDiffs() {
			require.Equal(t, "unchanged", d.GetChangeType())
			require.Empty(t, d.GetWordDiffHtml())
		}
	}
}

// With nothing published the whole text reads as added.
func TestDiffPolicyAppendix_NothingPublished(t *testing.T) {
	f := diffFixture("", diffDraft)
	resp, err := f.h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{Id: f.id.String()})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"notes": "added", "rates": "added"}, changeTypes(resp))
}

// A retired appendix can still be diffed.
func TestDiffPolicyAppendix_RetiredStillDiffs(t *testing.T) {
	f := diffFixture(diffPublished, diffDraft)
	gone := f.at
	f.b.result.RetiredAt = &gone
	resp, err := f.h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{Id: f.id.String()})
	require.NoError(t, err)
	require.Len(t, resp.GetDiffs(), 2)
}

// appendix-titles-contributors.test.ts "the live diff": text that does not
// pass the save checks is refused and nothing is read for it.
func TestDiffPolicyAppendix_BadContentIsInvalid(t *testing.T) {
	f := diffFixture(diffPublished, "")
	_, err := f.h.DiffPolicyAppendix(context.Background(),
		&corev1.DiffPolicyAppendixRequest{Id: f.id.String(), ContentJson: proto.String("{not json")})
	requireCode(t, err, codes.InvalidArgument)
	require.Zero(t, f.w.writes)
}

func TestDiffPolicyAppendix_BadIDIsInvalid(t *testing.T) {
	f := diffFixture(diffPublished, "")
	_, err := f.h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{Id: "nope"})
	requireCode(t, err, codes.InvalidArgument)
	require.Zero(t, f.b.calls)
}

func TestDiffPolicyAppendix_UnknownIDIsNotFound(t *testing.T) {
	f := diffFixture(diffPublished, "")
	f.b.err = store.ErrPolicyAppendixNotFound
	_, err := f.h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{Id: f.id.String()})
	requireCode(t, err, codes.NotFound)
}

// A stored row that cannot be parsed is a fault, not the caller's mistake.
func TestDiffPolicyAppendix_UnparseableStoredRowIsInternal(t *testing.T) {
	f := diffFixture("{broken", "")
	_, err := f.h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{Id: f.id.String()})
	requireCode(t, err, codes.Internal)
}

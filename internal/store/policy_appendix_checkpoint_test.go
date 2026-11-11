// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Steward-GRC/steward-core/internal/store"
)

// The Go counterparts of the "an appendix checkpoint" cases of the mock
// server's appendix-collab.test.ts and the co-edit case of
// appendix-titles-contributors.test.ts (core#87).

// appendix-collab.test.ts "becomes the appendix's draft and leaves the published row alone".
func TestPolicyAppendix_CheckpointStartsTheNextDraftWithThePublishedTitle(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Per-diem rates")
	pub, err := e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	before := *pub.Published()

	got, changed, err := e.st.CheckpointDraft(e.ctx, a.ID, e.policy, e.bob, content("Written in the room"))
	require.NoError(t, err)
	require.True(t, changed)
	d := got.Draft()
	require.NotNil(t, d)
	require.Equal(t, 2, d.VersionNo)
	require.Equal(t, "Per-diem rates", d.Title)
	require.JSONEq(t, content("Written in the room"), d.ContentJSON)
	require.Equal(t, before, *got.Published(), "the published row is left alone")
}

// appendix-collab.test.ts "updates the draft that is already there, rather than adding another".
func TestPolicyAppendix_CheckpointUpdatesTheOpenDraftAndKeepsItsTitle(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Per-diem rates")
	_, _, err := e.st.SaveDraft(e.ctx, a.ID, e.bob, "Per-diem rates (revised)", content("Typed."))
	require.NoError(t, err)
	for _, words := range []string{"First", "Second"} {
		_, changed, err := e.st.CheckpointDraft(e.ctx, a.ID, e.policy, e.bob, content(words))
		require.NoError(t, err)
		require.True(t, changed)
	}
	got := e.list(t, false)[0]
	require.Len(t, got.Versions, 1, "one draft, not another beside it")
	require.Equal(t, "Per-diem rates (revised)", got.Draft().Title, "a checkpoint keeps the draft's title")
	require.JSONEq(t, content("Second"), got.Draft().ContentJSON)
}

// appendix-collab.test.ts "makes no draft when the checkpoint is what is already published (bug log 246)".
func TestPolicyAppendix_CheckpointEqualToPublishedMakesNoDraft(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Per-diem rates")
	pub, err := e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	// The same JSON laid out differently still counts as the published text.
	got, changed, err := e.st.CheckpointDraft(e.ctx, a.ID, e.policy, e.alice, "  "+pub.Published().ContentJSON+" ")
	require.NoError(t, err)
	require.False(t, changed)
	require.Nil(t, got.Draft())
	require.Nil(t, e.list(t, false)[0].Draft())
	require.Equal(t, []uuid.UUID{e.bob}, e.list(t, false)[0].ContributorIDs(), "a no-op checkpoint adds no contributor")
}

// appendix-titles-contributors.test.ts "adds whoever co-edits in the appendix room and checkpoints".
func TestPolicyAppendix_CheckpointRecordsTheActorOnceInFirstSaveOrder(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Per-diem rates") // bob
	_, _, err := e.st.CheckpointDraft(e.ctx, a.ID, e.policy, e.alice, content("One"))
	require.NoError(t, err)
	_, _, err = e.st.CheckpointDraft(e.ctx, a.ID, e.policy, e.alice, content("Two"))
	require.NoError(t, err)
	got, _, err := e.st.CheckpointDraft(e.ctx, a.ID, e.policy, e.bob, content("Three"))
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{e.bob, e.alice}, got.Draft().Editors)
	require.Equal(t, []uuid.UUID{e.bob, e.alice}, got.ContributorIDs())
}

// Core rule: a retired appendix takes no checkpoint, as it takes no save.
func TestPolicyAppendix_CheckpointRefusesARetiredAppendix(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Per-diem rates")
	_, _, err := e.st.Retire(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	_, _, err = e.st.CheckpointDraft(e.ctx, a.ID, e.policy, e.bob, content("Late"))
	require.ErrorIs(t, err, store.ErrAppendixRetired)
	require.JSONEq(t, content("Terms."), e.list(t, true)[0].Draft().ContentJSON, "nothing stored")
}

// Core rule: an unknown appendix is not found, and a policy that is not the appendix's is refused.
func TestPolicyAppendix_CheckpointUnknownIDAndWrongPolicy(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Per-diem rates")
	_, _, err := e.st.CheckpointDraft(e.ctx, uuid.New(), e.policy, e.bob, content("x"))
	require.ErrorIs(t, err, store.ErrPolicyAppendixNotFound)
	_, _, err = e.st.CheckpointDraft(e.ctx, a.ID, uuid.New(), e.bob, content("x"))
	require.ErrorIs(t, err, store.ErrAppendixPolicyMismatch)
	require.JSONEq(t, content("Terms."), e.list(t, false)[0].Draft().ContentJSON, "nothing stored")
}

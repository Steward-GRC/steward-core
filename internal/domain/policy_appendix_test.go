// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAppendixLetter(t *testing.T) {
	require.Equal(t, "A", AppendixLetter(0))
	require.Equal(t, "B", AppendixLetter(1))
	require.Equal(t, "Z", AppendixLetter(25))
	require.Equal(t, "AA", AppendixLetter(26))
	require.Equal(t, "AB", AppendixLetter(27))
}

// appendices.test.ts "is Appendix B, then Appendix C, when no title is given".
func TestDefaultAppendixTitleUsesTheLetterOfItsPlace(t *testing.T) {
	require.Equal(t, "Appendix B", DefaultAppendixTitle([]string{"Per-diem rates"}, 1))
	require.Equal(t, "Appendix C", DefaultAppendixTitle([]string{"Per-diem rates", "Appendix B"}, 2))
}

// "skips a name another appendix of the policy already uses" and "skips the name of a
// retired appendix too": the taken titles include retired appendices.
func TestDefaultAppendixTitleSkipsTitlesInUse(t *testing.T) {
	require.Equal(t, "Appendix C", DefaultAppendixTitle([]string{"Appendix B"}, 1))
	require.Equal(t, "Appendix D", DefaultAppendixTitle([]string{" appendix b ", "APPENDIX C"}, 1))
	require.Equal(t, "Appendix A", DefaultAppendixTitle(nil, 0))
}

func TestSameAppendixContent(t *testing.T) {
	v := PolicyAppendixVersion{Title: "Glossary", ContentJSON: `{"text":"Terms.","n":1}`}
	require.True(t, v.Same("Glossary", `{"n":1,"text":"Terms."}`), "key order and spacing do not count")
	require.True(t, v.Same("Glossary", `{ "text": "Terms.", "n": 1 }`))
	require.False(t, v.Same("Glossary of terms", `{"text":"Terms.","n":1}`))
	require.False(t, v.Same("Glossary", `{"text":"More terms.","n":1}`))
}

func TestValidAppendixContent(t *testing.T) {
	require.NoError(t, ValidateAppendixContent(`{"text":"x"}`))
	require.Error(t, ValidateAppendixContent(""))
	require.Error(t, ValidateAppendixContent("   "))
	require.Error(t, ValidateAppendixContent(`{"text":`))
}

func appendixWith(versions ...PolicyAppendixVersion) PolicyAppendix {
	return PolicyAppendix{ID: uuid.New(), Versions: versions}
}

// "lists the published versions newest first, leaving the draft out", and the table itself
// stays oldest first.
func TestPublishedVersionsAreNewestFirstWithoutTheDraft(t *testing.T) {
	a := appendixWith(
		PolicyAppendixVersion{VersionNo: 1, State: AppendixPublished},
		PolicyAppendixVersion{VersionNo: 2, State: AppendixPublished},
		PolicyAppendixVersion{VersionNo: 3, State: AppendixPublished},
		PolicyAppendixVersion{VersionNo: 4, State: AppendixDraft},
	)
	var got []int
	for _, v := range a.PublishedVersions() {
		got = append(got, v.VersionNo)
	}
	require.Equal(t, []int{3, 2, 1}, got)
	require.Equal(t, 3, a.Published().VersionNo)
	require.Equal(t, 4, a.Draft().VersionNo)
}

func TestNoPublishedRowNoDraft(t *testing.T) {
	a := appendixWith(PolicyAppendixVersion{VersionNo: 1, State: AppendixDraft})
	require.Nil(t, a.Published())
	require.NotNil(t, a.Draft())
	require.Empty(t, a.PublishedVersions())
	b := appendixWith(PolicyAppendixVersion{VersionNo: 1, State: AppendixPublished})
	require.Nil(t, b.Draft())
}

// appendix-titles-contributors.test.ts "keeps each version's own contributors": everyone once,
// first to last across the rows.
func TestContributorsAreEveryoneOnceInFirstSaveOrder(t *testing.T) {
	bob, alice, carol := uuid.New(), uuid.New(), uuid.New()
	a := appendixWith(
		PolicyAppendixVersion{VersionNo: 1, Editors: []uuid.UUID{bob, alice}},
		PolicyAppendixVersion{VersionNo: 2, Editors: []uuid.UUID{alice, carol, bob}},
	)
	require.Equal(t, []uuid.UUID{bob, alice, carol}, a.ContributorIDs())
}

func TestNoteEditorKeepsFirstPlace(t *testing.T) {
	bob, alice := uuid.New(), uuid.New()
	require.Equal(t, []uuid.UUID{bob, alice}, NoteEditor([]uuid.UUID{bob}, alice))
	require.Equal(t, []uuid.UUID{bob, alice}, NoteEditor([]uuid.UUID{bob, alice}, bob))
	require.Equal(t, []uuid.UUID{bob}, NoteEditor(nil, bob))
}

func TestAssignAppendixLettersFollowTheLiveOrder(t *testing.T) {
	now := time.Now()
	retired := &now
	list := []PolicyAppendix{
		{ID: uuid.New(), OrderIndex: 0},
		{ID: uuid.New(), OrderIndex: 1, RetiredAt: retired},
		{ID: uuid.New(), OrderIndex: 1},
	}
	AssignAppendixLetters(list)
	require.Equal(t, "A", list[0].Letter)
	require.Equal(t, "", list[1].Letter)
	require.Equal(t, "B", list[2].Letter)
}

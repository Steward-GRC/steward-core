// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

var (
	author   = domain.ReviewSideAuthor
	reviewer = domain.ReviewSideReviewer
	slim     = domain.ReviewLimits{BodyMax: 20, QuoteMax: 12}
)

// suggested builds a thread with an open suggestion proposed by side.
func suggested(t *testing.T, side domain.ReviewSide, text string) (domain.ReviewThread, uuid.UUID, uuid.UUID) {
	t.Helper()
	charlie, bob := uuid.New(), uuid.New()
	proposer := charlie
	if side == author {
		proposer = bob
	}
	th, err := domain.NewReviewThread(slim, reviewer, charlie, uuid.New(), charlie, "seven years", 1, "Shorter?")
	require.NoError(t, err)
	if side == author {
		th, err = domain.NewReviewThread(slim, author, bob, uuid.New(), charlie, "seven years", 1, "How about")
		require.NoError(t, err)
	}
	th, err = th.WithSuggestion(slim, side, proposer, text)
	require.NoError(t, err)
	return th, charlie, bob
}

// counter applies a counter the way a store would: append the round.
func counter(t *testing.T, th domain.ReviewThread, side domain.ReviewSide, actor uuid.UUID, text string) domain.ReviewThread {
	t.Helper()
	r, err := th.Counter(slim, side, actor, text)
	require.NoError(t, err)
	th.Suggestion.Rounds = append(th.Suggestion.Rounds, r)
	return th
}

// mock: starting a suggestion / opens it on the author's turn when a reviewer proposes
func TestReviewerProposalOpensOnTheAuthorsTurn(t *testing.T) {
	th, charlie, _ := suggested(t, reviewer, "  six years ")
	require.NotNil(t, th.Suggestion)
	require.Equal(t, "six years", th.Suggestion.Text(), "the proposal is stored trimmed")
	require.Equal(t, author, th.Suggestion.Turn())
	require.True(t, th.Suggestion.Open())
	require.Len(t, th.Suggestion.Rounds, 1)
	r := th.Suggestion.Rounds[0]
	require.Equal(t, 1, r.Seq)
	require.Equal(t, reviewer, r.Side)
	require.Equal(t, charlie, r.UserID)
}

// mock: starting a suggestion / opens it on the reviewer's turn when the author proposes
func TestAuthorProposalOpensOnTheReviewersTurn(t *testing.T) {
	th, _, _ := suggested(t, author, "five years")
	require.Equal(t, reviewer, th.Suggestion.Turn())
	require.Equal(t, author, th.Suggestion.Rounds[0].Side)
}

// mock: starting a suggestion / is a plain comment when no text, or the same text, is proposed
func TestBlankOrSameTextIsAPlainComment(t *testing.T) {
	charlie := uuid.New()
	th, err := domain.NewReviewThread(slim, reviewer, charlie, uuid.New(), charlie, "seven years", 1, "Why?")
	require.NoError(t, err)
	for _, text := range []string{"", "   ", "seven years", "  seven years  "} {
		got, err := th.WithSuggestion(slim, reviewer, charlie, text)
		require.NoError(t, err)
		require.Nil(t, got.Suggestion, "text %q", text)
	}
}

func TestProposedTextOverTheLimitIsRefused(t *testing.T) {
	charlie := uuid.New()
	th, err := domain.NewReviewThread(slim, reviewer, charlie, uuid.New(), charlie, "seven years", 1, "Why?")
	require.NoError(t, err)
	_, err = th.WithSuggestion(slim, reviewer, charlie, strings.Repeat("a", 13))
	require.ErrorIs(t, err, domain.ErrSuggestionTooLong)
	_, err = th.WithSuggestion(slim, reviewer, charlie, strings.Repeat("é", 12))
	require.NoError(t, err, "length counts characters, not bytes")
}

// mock: countering / lets the side whose turn it is counter, and flips the turn
func TestCounterFlipsTheTurnAndKeepsTheRounds(t *testing.T) {
	th, _, bob := suggested(t, reviewer, "six years")
	r, err := th.Counter(slim, author, bob, "  five years ")
	require.NoError(t, err)
	require.Equal(t, 2, r.Seq)
	require.Equal(t, author, r.Side)
	require.Equal(t, bob, r.UserID)
	require.Equal(t, "five years", r.Text)
	th = counter(t, th, author, bob, "five years")
	require.Equal(t, reviewer, th.Suggestion.Turn())
	require.Equal(t, "five years", th.Suggestion.Text())
	require.Equal(t, "six years", th.Suggestion.Rounds[0].Text)
}

// mock: countering / refuses a counter out of turn, an empty one, and one on a plain comment
func TestCounterRefusedOutOfTurnEmptyAndOnAPlainComment(t *testing.T) {
	th, charlie, bob := suggested(t, reviewer, "six years")
	_, err := th.Counter(slim, reviewer, charlie, "four years")
	require.ErrorIs(t, err, domain.ErrSuggestionNotYourTurn)
	_, err = th.Counter(slim, author, bob, "  ")
	require.ErrorIs(t, err, domain.ErrSuggestionBlank)
	_, err = th.Counter(slim, author, bob, strings.Repeat("a", 13))
	require.ErrorIs(t, err, domain.ErrSuggestionTooLong)

	plain, err := domain.NewReviewThread(slim, reviewer, charlie, uuid.New(), charlie, "seven years", 1, "Why?")
	require.NoError(t, err)
	_, err = plain.Counter(slim, author, bob, "x")
	require.ErrorIs(t, err, domain.ErrSuggestionNone)
}

// mock: countering / keeps someone outside the review out (core: a reviewer must be the thread's approver)
func TestCounterByAReviewerWhoIsNotTheApproverIsRefused(t *testing.T) {
	th, _, bob := suggested(t, reviewer, "six years")
	th = counter(t, th, author, bob, "five years")
	_, err := th.Counter(slim, reviewer, uuid.New(), "four years")
	require.ErrorIs(t, err, domain.ErrReviewNotApprover)
	_, err = th.Counter(slim, domain.ReviewSideUnspecified, bob, "four years")
	require.ErrorIs(t, err, domain.ErrReviewBadSide)
}

func closeIt(th domain.ReviewThread, side domain.ReviewSide, actor uuid.UUID, o domain.SuggestionOutcome, reason string) (domain.SuggestionClose, error) {
	return th.CloseSuggestion(slim, side, actor, o, reason)
}

// mock: closing / lets the author change it, which resolves the thread and records the text
func TestAuthorChangesItAndTheTextAndCloserAreRecorded(t *testing.T) {
	th, _, bob := suggested(t, reviewer, "six years")
	c, err := closeIt(th, author, bob, domain.SuggestionChanged, "")
	require.NoError(t, err)
	require.Equal(t, domain.SuggestionChanged, c.Outcome)
	require.Equal(t, "six years", c.ClosedText)
	require.Equal(t, bob, c.ClosedBy)
	require.Equal(t, 1, c.Rounds, "the close is checked against the rounds it was decided on")
}

// mock: closing / lets the author remove the note, and the reviewer agree on their turn
func TestAuthorRemovesAndReviewerAgreesOnTheirTurn(t *testing.T) {
	th, charlie, bob := suggested(t, reviewer, "six years")
	c, err := closeIt(th, author, bob, domain.SuggestionRemoved, "")
	require.NoError(t, err)
	require.Equal(t, domain.SuggestionRemoved, c.Outcome)

	th = counter(t, th, author, bob, "five years")
	c, err = closeIt(th, reviewer, charlie, domain.SuggestionAgreed, "")
	require.NoError(t, err)
	require.Equal(t, domain.SuggestionAgreed, c.Outcome)
	require.Equal(t, "five years", c.ClosedText)
	require.Equal(t, 2, c.Rounds)
}

// mock: closing / does not let the wrong side use an outcome that is not theirs
func TestWrongSideCannotUseAnOutcomeThatIsNotTheirs(t *testing.T) {
	th, charlie, bob := suggested(t, reviewer, "six years")
	_, err := closeIt(th, reviewer, charlie, domain.SuggestionAgreed, "")
	require.ErrorIs(t, err, domain.ErrSuggestionOutcomeNotYours, "the reviewer cannot agree with themselves")
	th = counter(t, th, author, bob, "five years")
	_, err = closeIt(th, author, bob, domain.SuggestionChanged, "")
	require.ErrorIs(t, err, domain.ErrSuggestionOutcomeNotYours, "it is the reviewer's turn now")
	_, err = closeIt(th, author, bob, domain.SuggestionRemoved, "")
	require.ErrorIs(t, err, domain.ErrSuggestionOutcomeNotYours)
	_, err = closeIt(th, reviewer, uuid.New(), domain.SuggestionAgreed, "")
	require.ErrorIs(t, err, domain.ErrReviewNotApprover)
	_, err = closeIt(th, author, bob, domain.SuggestionUnspecified, "")
	require.ErrorIs(t, err, domain.ErrSuggestionBadOutcome)
}

// mock: closing / needs a reason for not agreed, and records it
func TestNotAgreedNeedsAReasonAndRecordsIt(t *testing.T) {
	th, _, bob := suggested(t, reviewer, "six years")
	_, err := closeIt(th, author, bob, domain.SuggestionNotAgreed, "  ")
	require.ErrorIs(t, err, domain.ErrSuggestionReasonRequired)
	_, err = closeIt(th, author, bob, domain.SuggestionNotAgreed, strings.Repeat("a", 21))
	require.ErrorIs(t, err, domain.ErrReviewBodyTooLong)
	c, err := closeIt(th, author, bob, domain.SuggestionNotAgreed, " Law says seven. ")
	require.NoError(t, err)
	require.Equal(t, "Law says seven.", c.Reason)
	require.Equal(t, "six years", c.ClosedText, "closed_text is recorded on every outcome")
}

// mock: closing / lets the side that proposed first withdraw while it is not their turn
func TestFirstProposerWithdrawsWhileNotTheirTurn(t *testing.T) {
	th, charlie, bob := suggested(t, reviewer, "six years")
	c, err := closeIt(th, reviewer, charlie, domain.SuggestionWithdrawn, "")
	require.NoError(t, err)
	require.Equal(t, domain.SuggestionWithdrawn, c.Outcome)
	_, err = closeIt(th, author, bob, domain.SuggestionWithdrawn, "")
	require.ErrorIs(t, err, domain.ErrSuggestionOutcomeNotYours, "the author did not propose first")

	// once the author countered it is the reviewer's turn: the reviewer can no longer withdraw
	th = counter(t, th, author, bob, "five years")
	_, err = closeIt(th, reviewer, charlie, domain.SuggestionWithdrawn, "")
	require.ErrorIs(t, err, domain.ErrSuggestionOutcomeNotYours)
	// the author did not propose first, so the author cannot withdraw either
	_, err = closeIt(th, author, bob, domain.SuggestionWithdrawn, "")
	require.ErrorIs(t, err, domain.ErrSuggestionOutcomeNotYours)

	// an author's own proposal can be withdrawn by the author while it is the reviewer's turn
	ath, _, bob2 := suggested(t, author, "five years")
	c, err = closeIt(ath, author, bob2, domain.SuggestionWithdrawn, "")
	require.NoError(t, err)
	require.Equal(t, domain.SuggestionWithdrawn, c.Outcome)
}

// mock: closing / refuses to close twice, or to counter a closed suggestion
func TestClosedSuggestionTakesNoCounterAndNoSecondClose(t *testing.T) {
	th, _, bob := suggested(t, reviewer, "six years")
	c, err := closeIt(th, author, bob, domain.SuggestionRemoved, "")
	require.NoError(t, err)
	th.Suggestion.Outcome, th.Suggestion.ClosedBy, th.Suggestion.ClosedText = c.Outcome, c.ClosedBy, c.ClosedText
	th.Resolved = true
	require.False(t, th.Suggestion.Open())

	_, err = closeIt(th, author, bob, domain.SuggestionRemoved, "")
	require.ErrorIs(t, err, domain.ErrSuggestionClosed)
	_, err = th.Counter(slim, author, bob, "x")
	require.ErrorIs(t, err, domain.ErrSuggestionClosed)

	// reopening the thread does not reopen the suggestion or change its outcome
	th.Resolved = false
	_, err = th.Counter(slim, author, bob, "x")
	require.ErrorIs(t, err, domain.ErrSuggestionClosed)
	require.Equal(t, domain.SuggestionRemoved, th.Suggestion.Outcome)
}

func TestCloseOnAPlainCommentIsRefused(t *testing.T) {
	charlie := uuid.New()
	th, err := domain.NewReviewThread(slim, reviewer, charlie, uuid.New(), charlie, "seven years", 1, "Why?")
	require.NoError(t, err)
	_, err = closeIt(th, author, uuid.New(), domain.SuggestionRemoved, "")
	require.ErrorIs(t, err, domain.ErrSuggestionNone)
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// SuggestionOutcome is how a suggestion ended. A suggestion ends in exactly
// one outcome, and reopening its thread does not change it.
type SuggestionOutcome int

// The outcomes.
const (
	SuggestionUnspecified SuggestionOutcome = iota
	SuggestionChanged
	SuggestionAgreed
	SuggestionRemoved
	SuggestionWithdrawn
	SuggestionNotAgreed
)

// String returns the lower-case name used in logs and audit attributes.
func (o SuggestionOutcome) String() string {
	switch o {
	case SuggestionChanged:
		return "changed"
	case SuggestionAgreed:
		return "agreed"
	case SuggestionRemoved:
		return "removed"
	case SuggestionWithdrawn:
		return "withdrawn"
	case SuggestionNotAgreed:
		return "not_agreed"
	default:
		return "unspecified"
	}
}

// Suggestion rule errors.
var (
	ErrSuggestionNone            = errors.New("review: that thread has no suggestion")
	ErrSuggestionClosed          = errors.New("review: that suggestion has ended")
	ErrSuggestionNotYourTurn     = errors.New("review: it is not your turn to answer")
	ErrSuggestionBlank           = errors.New("review: a counter can't be empty")
	ErrSuggestionTooLong         = errors.New("review: the suggested text is too long")
	ErrSuggestionBadOutcome      = errors.New("review: choose an outcome")
	ErrSuggestionOutcomeNotYours = errors.New("review: that outcome is not yours to choose now")
	ErrSuggestionReasonRequired  = errors.New("review: say why it is not agreed")
)

// SuggestionRound is one proposal or counter. Round 1 is the proposal.
type SuggestionRound struct {
	Seq       int
	Side      ReviewSide
	UserID    uuid.UUID
	Text      string
	CreatedAt time.Time
}

// ReviewSuggestion is a proposed replacement for a thread's quoted text,
// answered by counters in turn. The current text and the turn come from the
// last round; they are not stored.
type ReviewSuggestion struct {
	Rounds     []SuggestionRound
	Outcome    SuggestionOutcome // SuggestionUnspecified while open
	Reason     string
	ClosedBy   uuid.UUID
	ClosedText string
	ClosedAt   time.Time
}

// Open reports whether the suggestion has no outcome yet.
func (s ReviewSuggestion) Open() bool { return s.Outcome == SuggestionUnspecified }

// Text is the latest round's text.
func (s ReviewSuggestion) Text() string {
	if len(s.Rounds) == 0 {
		return ""
	}
	return s.Rounds[len(s.Rounds)-1].Text
}

// Turn is the side that answers next: the other side of the latest round. It
// stays that side after the suggestion ends.
func (s ReviewSuggestion) Turn() ReviewSide {
	if len(s.Rounds) == 0 {
		return ReviewSideUnspecified
	}
	return otherSide(s.Rounds[len(s.Rounds)-1].Side)
}

func otherSide(s ReviewSide) ReviewSide {
	switch s {
	case ReviewSideAuthor:
		return ReviewSideReviewer
	case ReviewSideReviewer:
		return ReviewSideAuthor
	default:
		return ReviewSideUnspecified
	}
}

// suggestionText trims proposed text and checks its length in characters.
func suggestionText(l ReviewLimits, text string) (string, error) {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) > l.QuoteMax {
		return "", ErrSuggestionTooLong
	}
	return text, nil
}

// WithSuggestion returns the thread carrying the actor's proposal as round 1,
// on the other side's turn. Blank text, or text equal to the quote, makes a
// plain comment and the thread comes back unchanged.
func (t ReviewThread) WithSuggestion(l ReviewLimits, side ReviewSide, actor uuid.UUID, text string) (ReviewThread, error) {
	text, err := suggestionText(l, text)
	if err != nil {
		return t, err
	}
	if text == "" || text == strings.TrimSpace(t.Quote) {
		return t, nil
	}
	t.Suggestion = &ReviewSuggestion{Rounds: []SuggestionRound{{Seq: 1, Side: side, UserID: actor, Text: text}}}
	return t, nil
}

// openFor checks that the actor may act on the thread and that it carries a
// suggestion that has not ended.
func (t ReviewThread) openFor(side ReviewSide, actor uuid.UUID) (*ReviewSuggestion, error) {
	if err := t.CheckAct(side, actor); err != nil {
		return nil, err
	}
	if t.Suggestion == nil {
		return nil, ErrSuggestionNone
	}
	if !t.Suggestion.Open() {
		return nil, ErrSuggestionClosed
	}
	return t.Suggestion, nil
}

// Counter checks a counter from the actor and returns the round to append. It
// must be the actor's side's turn, and the text may not be blank.
func (t ReviewThread) Counter(l ReviewLimits, side ReviewSide, actor uuid.UUID, text string) (SuggestionRound, error) {
	s, err := t.openFor(side, actor)
	if err != nil {
		return SuggestionRound{}, err
	}
	if s.Turn() != side {
		return SuggestionRound{}, ErrSuggestionNotYourTurn
	}
	text, err = suggestionText(l, text)
	if err != nil {
		return SuggestionRound{}, err
	}
	if text == "" {
		return SuggestionRound{}, ErrSuggestionBlank
	}
	return SuggestionRound{Seq: len(s.Rounds) + 1, Side: side, UserID: actor, Text: text}, nil
}

// SuggestionClose is a checked decision to end a suggestion. Rounds is how many
// rounds it was decided on, so a store can refuse it if a counter landed since.
type SuggestionClose struct {
	Outcome    SuggestionOutcome
	Reason     string
	ClosedBy   uuid.UUID
	ClosedText string
	Rounds     int
}

// CloseSuggestion checks that the actor may end the suggestion with the
// outcome now. CHANGED and REMOVED are the author's on their turn, AGREED the
// reviewer's on their turn, NOT_AGREED either side's on their turn (with a
// reason), and WITHDRAWN the side that proposed first, while it is not their
// turn.
func (t ReviewThread) CloseSuggestion(l ReviewLimits, side ReviewSide, actor uuid.UUID, outcome SuggestionOutcome, reason string) (SuggestionClose, error) {
	s, err := t.openFor(side, actor)
	if err != nil {
		return SuggestionClose{}, err
	}
	mine := s.Turn() == side
	var allowed bool
	switch outcome {
	case SuggestionAgreed:
		allowed = mine && side == ReviewSideReviewer
	case SuggestionChanged, SuggestionRemoved:
		allowed = mine && side == ReviewSideAuthor
	case SuggestionNotAgreed:
		allowed = mine
	case SuggestionWithdrawn:
		allowed = !mine && s.Rounds[0].Side == side
	default:
		return SuggestionClose{}, ErrSuggestionBadOutcome
	}
	if !allowed {
		return SuggestionClose{}, ErrSuggestionOutcomeNotYours
	}
	reason = strings.TrimSpace(reason)
	if outcome == SuggestionNotAgreed && reason == "" {
		return SuggestionClose{}, ErrSuggestionReasonRequired
	}
	if utf8.RuneCountInString(reason) > l.BodyMax {
		return SuggestionClose{}, ErrReviewBodyTooLong
	}
	return SuggestionClose{Outcome: outcome, Reason: reason, ClosedBy: actor, ClosedText: s.Text(), Rounds: len(s.Rounds)}, nil
}

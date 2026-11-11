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

// ReviewSide is the side a caller acts on, decided by the gateway.
type ReviewSide int

// The sides.
const (
	ReviewSideUnspecified ReviewSide = iota
	ReviewSideAuthor
	ReviewSideReviewer
)

// ReviewLimits are the longest texts a thread accepts, in characters.
type ReviewLimits struct{ BodyMax, QuoteMax int }

// DefaultReviewLimits are the limits when the settings do not change them.
var DefaultReviewLimits = ReviewLimits{BodyMax: 10000, QuoteMax: 4000}

// Review thread rule errors.
var (
	ErrReviewBlankBody     = errors.New("review: a comment can't be empty")
	ErrReviewBodyTooLong   = errors.New("review: the comment is too long")
	ErrReviewBlankQuote    = errors.New("review: the quoted text can't be empty")
	ErrReviewQuoteTooLong  = errors.New("review: the quoted text is too long")
	ErrReviewBadOccurrence = errors.New("review: occurrence counts from 1")
	ErrReviewNoApprover    = errors.New("review: a thread needs the approver it is with")
	ErrReviewBadSide       = errors.New("review: actor side must be author or reviewer")
	ErrReviewNotApprover   = errors.New("review: only the approver the thread is with may act on it")
	ErrReviewNotStarter    = errors.New("review: only the person who started a thread may delete it")
)

// ReviewMessage is one message of a thread.
type ReviewMessage struct {
	ID           uuid.UUID
	AuthorUserID uuid.UUID
	Body         string
	CreatedAt    time.Time
}

// ReviewThread is a discussion tied to a piece of text in one policy version.
type ReviewThread struct {
	ID              uuid.UUID
	PolicyVersionID uuid.UUID
	ApproverUserID  uuid.UUID
	CreatedBy       uuid.UUID
	Quote           string
	Occurrence      int
	Resolved        bool
	CreatedAt       time.Time
	Messages        []ReviewMessage
}

// NewReviewMessage validates a message body and returns it trimmed. Length is
// counted in characters, not bytes.
func NewReviewMessage(l ReviewLimits, author uuid.UUID, body string) (ReviewMessage, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return ReviewMessage{}, ErrReviewBlankBody
	}
	if utf8.RuneCountInString(body) > l.BodyMax {
		return ReviewMessage{}, ErrReviewBodyTooLong
	}
	return ReviewMessage{AuthorUserID: author, Body: body}, nil
}

// NewReviewThread validates a new thread and its first message. The REVIEWER
// side may only start a thread for itself; for the AUTHOR side the gateway has
// checked that approver holds a seat. The quote is kept as given, because it is
// matched against the version text.
func NewReviewThread(l ReviewLimits, side ReviewSide, actor, versionID, approver uuid.UUID, quote string, occurrence int, body string) (ReviewThread, error) {
	if side != ReviewSideAuthor && side != ReviewSideReviewer {
		return ReviewThread{}, ErrReviewBadSide
	}
	if approver == uuid.Nil {
		return ReviewThread{}, ErrReviewNoApprover
	}
	if side == ReviewSideReviewer && approver != actor {
		return ReviewThread{}, ErrReviewNotApprover
	}
	if strings.TrimSpace(quote) == "" {
		return ReviewThread{}, ErrReviewBlankQuote
	}
	if utf8.RuneCountInString(quote) > l.QuoteMax {
		return ReviewThread{}, ErrReviewQuoteTooLong
	}
	if occurrence < 1 {
		return ReviewThread{}, ErrReviewBadOccurrence
	}
	msg, err := NewReviewMessage(l, actor, body)
	if err != nil {
		return ReviewThread{}, err
	}
	return ReviewThread{
		PolicyVersionID: versionID, ApproverUserID: approver, CreatedBy: actor,
		Quote: quote, Occurrence: occurrence, Messages: []ReviewMessage{msg},
	}, nil
}

// CheckAct reports whether the actor on side may reply to or resolve the
// thread. The AUTHOR side is the gateway's call; a REVIEWER must be the
// approver the thread is with.
func (t ReviewThread) CheckAct(side ReviewSide, actor uuid.UUID) error {
	switch side {
	case ReviewSideAuthor:
		return nil
	case ReviewSideReviewer:
		if actor != t.ApproverUserID {
			return ErrReviewNotApprover
		}
		return nil
	default:
		return ErrReviewBadSide
	}
}

// CheckDelete reports whether the actor on side may delete the thread: it must
// be allowed to act on it, and be the one who started it.
func (t ReviewThread) CheckDelete(side ReviewSide, actor uuid.UUID) error {
	if err := t.CheckAct(side, actor); err != nil {
		return err
	}
	if actor != t.CreatedBy {
		return ErrReviewNotStarter
	}
	return nil
}

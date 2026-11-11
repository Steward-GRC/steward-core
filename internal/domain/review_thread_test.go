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

var lim = domain.ReviewLimits{BodyMax: 20, QuoteMax: 10}

func TestNewReviewThreadAnchorsOnQuoteAndOccurrence(t *testing.T) {
	actor, version := uuid.New(), uuid.New()
	th, err := domain.NewReviewThread(lim, domain.ReviewSideReviewer, actor, version, actor, "seven", 2, "  What is this?  ")
	require.NoError(t, err)
	require.Equal(t, "seven", th.Quote)
	require.Equal(t, 2, th.Occurrence)
	require.Equal(t, version, th.PolicyVersionID)
	require.Equal(t, actor, th.ApproverUserID)
	require.Equal(t, actor, th.CreatedBy)
	require.False(t, th.Resolved)
	require.Len(t, th.Messages, 1)
	require.Equal(t, "What is this?", th.Messages[0].Body, "the body is stored trimmed")
	require.Equal(t, actor, th.Messages[0].AuthorUserID)
}

func TestNewReviewThreadRefusesBadInput(t *testing.T) {
	a, v, ap := uuid.New(), uuid.New(), uuid.New()
	tests := []struct {
		name     string
		side     domain.ReviewSide
		approver uuid.UUID
		quote    string
		occ      int
		body     string
		want     error
	}{
		{"empty comment", domain.ReviewSideAuthor, ap, "x", 1, "  ", domain.ErrReviewBlankBody},
		{"empty quote", domain.ReviewSideAuthor, ap, " ", 1, "Why?", domain.ErrReviewBlankQuote},
		{"occurrence zero", domain.ReviewSideAuthor, ap, "x", 0, "Why?", domain.ErrReviewBadOccurrence},
		{"occurrence negative", domain.ReviewSideAuthor, ap, "x", -3, "Why?", domain.ErrReviewBadOccurrence},
		{"no approver", domain.ReviewSideAuthor, uuid.Nil, "x", 1, "Why?", domain.ErrReviewNoApprover},
		{"no side", domain.ReviewSideUnspecified, ap, "x", 1, "Why?", domain.ErrReviewBadSide},
		{"body too long", domain.ReviewSideAuthor, ap, "x", 1, strings.Repeat("a", 21), domain.ErrReviewBodyTooLong},
		{"quote too long", domain.ReviewSideAuthor, ap, strings.Repeat("a", 11), 1, "Why?", domain.ErrReviewQuoteTooLong},
		{"reviewer for another approver", domain.ReviewSideReviewer, ap, "x", 1, "Why?", domain.ErrReviewNotApprover},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.NewReviewThread(lim, tc.side, a, v, tc.approver, tc.quote, tc.occ, tc.body)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestReviewLimitsCountCharactersNotBytes(t *testing.T) {
	a, v := uuid.New(), uuid.New()
	_, err := domain.NewReviewThread(lim, domain.ReviewSideReviewer, a, v, a, "x", 1, strings.Repeat("é", 20))
	require.NoError(t, err, "20 two-byte characters fit a 20 character limit")
	_, err = domain.NewReviewThread(lim, domain.ReviewSideReviewer, a, v, a, "x", 1, strings.Repeat("é", 21))
	require.ErrorIs(t, err, domain.ErrReviewBodyTooLong)
}

func TestNewReviewMessage(t *testing.T) {
	a := uuid.New()
	m, err := domain.NewReviewMessage(lim, a, " Fixed. ")
	require.NoError(t, err)
	require.Equal(t, "Fixed.", m.Body)
	require.Equal(t, a, m.AuthorUserID)
	_, err = domain.NewReviewMessage(lim, a, "\n ")
	require.ErrorIs(t, err, domain.ErrReviewBlankBody)
	_, err = domain.NewReviewMessage(lim, a, strings.Repeat("a", 21))
	require.ErrorIs(t, err, domain.ErrReviewBodyTooLong)
}

func TestReviewThreadAudience(t *testing.T) {
	approver, other, author := uuid.New(), uuid.New(), uuid.New()
	th := domain.ReviewThread{ApproverUserID: approver, CreatedBy: approver}

	// the approver it is with, and the author side (gateway checked), may act
	require.NoError(t, th.CheckAct(domain.ReviewSideReviewer, approver))
	require.NoError(t, th.CheckAct(domain.ReviewSideAuthor, author))
	// another approver may not
	require.ErrorIs(t, th.CheckAct(domain.ReviewSideReviewer, other), domain.ErrReviewNotApprover)
	require.ErrorIs(t, th.CheckAct(domain.ReviewSideUnspecified, approver), domain.ErrReviewBadSide)
}

func TestReviewThreadOnlyStarterDeletes(t *testing.T) {
	approver, author, other := uuid.New(), uuid.New(), uuid.New()
	// the author started a thread for the approver: the author deletes, the approver does not
	th := domain.ReviewThread{ApproverUserID: approver, CreatedBy: author}
	require.NoError(t, th.CheckDelete(domain.ReviewSideAuthor, author))
	require.ErrorIs(t, th.CheckDelete(domain.ReviewSideReviewer, approver), domain.ErrReviewNotStarter)
	// the approver started it: the author may not delete it, another approver may not either
	th = domain.ReviewThread{ApproverUserID: approver, CreatedBy: approver}
	require.NoError(t, th.CheckDelete(domain.ReviewSideReviewer, approver))
	require.ErrorIs(t, th.CheckDelete(domain.ReviewSideAuthor, author), domain.ErrReviewNotStarter)
	require.ErrorIs(t, th.CheckDelete(domain.ReviewSideReviewer, other), domain.ErrReviewNotApprover)
	require.ErrorIs(t, th.CheckDelete(domain.ReviewSideUnspecified, approver), domain.ErrReviewBadSide)
}

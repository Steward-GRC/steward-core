// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	pg "github.com/Bugs5382/go-postgres"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// reviewVersion makes a policy with a draft version and returns the version id.
func reviewVersion(t *testing.T, pool *pg.DB) (uuid.UUID, *store.PolicyStore) {
	t.Helper()
	gs, ts, ps := store.NewCategoryStore(pool), store.NewTemplateStore(pool), store.NewPolicyStore(pool)
	g, tv := setupCategoryAndTemplate(t, gs, ts)
	p, err := domain.NewPolicy("Policy", g.ID, domain.SensitivityStandard, uuid.New())
	require.NoError(t, err)
	p, err = ps.CreatePolicy(context.Background(), p)
	require.NoError(t, err)
	draft, err := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	require.NoError(t, err)
	saved, _, err := ps.UpsertDraft(context.Background(), draft)
	require.NoError(t, err)
	return saved.ID, ps
}

func newThread(t *testing.T, versionID, starter, approver uuid.UUID, side domain.ReviewSide, body string) domain.ReviewThread {
	t.Helper()
	th, err := domain.NewReviewThread(domain.DefaultReviewLimits, side, starter, versionID, approver, "seven years", 2, body)
	require.NoError(t, err)
	return th
}

func TestReviewStoreCreateAnchorsOnQuoteAndOccurrence(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewReviewStore(pool)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	charlie := uuid.New()

	got, err := s.CreateThread(ctx, newThread(t, vid, charlie, charlie, domain.ReviewSideReviewer, "What is this?"))
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, got.ID)
	require.Equal(t, "seven years", got.Quote)
	require.Equal(t, 2, got.Occurrence)
	require.False(t, got.Resolved)
	require.False(t, got.CreatedAt.IsZero())
	require.Equal(t, charlie, got.ApproverUserID)
	require.Equal(t, charlie, got.CreatedBy)
	require.Len(t, got.Messages, 1)
	require.Equal(t, "What is this?", got.Messages[0].Body)
	require.Equal(t, charlie, got.Messages[0].AuthorUserID)
	require.NotEqual(t, uuid.Nil, got.Messages[0].ID)

	again, err := s.GetThread(ctx, got.ID)
	require.NoError(t, err)
	require.Equal(t, got.ID, again.ID)
	require.Len(t, again.Messages, 1)
}

func TestReviewStoreCreateOnUnknownVersion(t *testing.T) {
	s := store.NewReviewStore(newTestDB(t))
	a := uuid.New()
	_, err := s.CreateThread(context.Background(), newThread(t, uuid.New(), a, a, domain.ReviewSideReviewer, "Hi"))
	require.ErrorIs(t, err, store.ErrReviewVersionNotFound)
}

func TestReviewStoreGetUnknownThread(t *testing.T) {
	s := store.NewReviewStore(newTestDB(t))
	_, err := s.GetThread(context.Background(), uuid.New())
	require.ErrorIs(t, err, store.ErrReviewThreadNotFound)
}

func TestReviewStoreListsAVersionsThreadsOldestFirst(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewReviewStore(pool)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	other, _ := reviewVersion(t, pool)
	a := uuid.New()

	first, err := s.CreateThread(ctx, newThread(t, vid, a, a, domain.ReviewSideReviewer, "one"))
	require.NoError(t, err)
	second, err := s.CreateThread(ctx, newThread(t, vid, a, a, domain.ReviewSideReviewer, "two"))
	require.NoError(t, err)
	_, err = s.CreateThread(ctx, newThread(t, other, a, a, domain.ReviewSideReviewer, "elsewhere"))
	require.NoError(t, err)

	list, err := s.ListThreads(ctx, vid, uuid.Nil)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, []uuid.UUID{first.ID, second.ID}, []uuid.UUID{list[0].ID, list[1].ID})
	require.Len(t, list[0].Messages, 1)

	// A real version with no threads is an empty list, not an error.
	empty, _ := reviewVersion(t, pool)
	none, err := s.ListThreads(ctx, empty, uuid.Nil)
	require.NoError(t, err)
	require.NotNil(t, none)
	require.Empty(t, none)
}

func TestReviewStoreListOnUnknownVersion(t *testing.T) {
	s := store.NewReviewStore(newTestDB(t))
	_, err := s.ListThreads(context.Background(), uuid.New(), uuid.Nil)
	require.ErrorIs(t, err, store.ErrReviewVersionNotFound)
}

func TestReviewStoreThreadIsWithOneApprover(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewReviewStore(pool)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	author, charlie, alice := uuid.New(), uuid.New(), uuid.New()

	// charlie starts one for himself; the author writes one to charlie
	own, err := s.CreateThread(ctx, newThread(t, vid, charlie, charlie, domain.ReviewSideReviewer, "mine"))
	require.NoError(t, err)
	toCharlie, err := s.CreateThread(ctx, newThread(t, vid, author, charlie, domain.ReviewSideAuthor, "please check"))
	require.NoError(t, err)
	require.Equal(t, charlie, toCharlie.ApproverUserID)
	require.Equal(t, author, toCharlie.CreatedBy)
	require.Equal(t, author, toCharlie.Messages[0].AuthorUserID)

	all, err := s.ListThreads(ctx, vid, uuid.Nil) // the author side sees every thread
	require.NoError(t, err)
	require.Len(t, all, 2)

	mine, err := s.ListThreads(ctx, vid, charlie)
	require.NoError(t, err)
	require.Len(t, mine, 2)
	require.Equal(t, own.ID, mine[0].ID)

	// another approver on the same step does not see either
	theirs, err := s.ListThreads(ctx, vid, alice)
	require.NoError(t, err)
	require.Empty(t, theirs)
}

func TestReviewStoreReplyKeepsMessagesInOrder(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewReviewStore(pool)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	charlie, bob := uuid.New(), uuid.New()
	th, err := s.CreateThread(ctx, newThread(t, vid, charlie, charlie, domain.ReviewSideReviewer, "What is this?"))
	require.NoError(t, err)

	msg, err := domain.NewReviewMessage(domain.DefaultReviewLimits, bob, "Fixed in the next draft.")
	require.NoError(t, err)
	got, err := s.AddMessage(ctx, th.ID, msg)
	require.NoError(t, err)
	require.Len(t, got.Messages, 2)
	require.Equal(t, "What is this?", got.Messages[0].Body)
	require.Equal(t, "Fixed in the next draft.", got.Messages[1].Body)
	require.Equal(t, bob, got.Messages[1].AuthorUserID)

	_, err = s.AddMessage(ctx, uuid.New(), msg)
	require.ErrorIs(t, err, store.ErrReviewThreadNotFound)
}

func TestReviewStoreResolveAndReopen(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewReviewStore(pool)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	a := uuid.New()
	th, err := s.CreateThread(ctx, newThread(t, vid, a, a, domain.ReviewSideReviewer, "Hi"))
	require.NoError(t, err)

	done, err := s.SetResolved(ctx, th.ID, true)
	require.NoError(t, err)
	require.True(t, done.Resolved)
	require.Len(t, done.Messages, 1)
	again, err := s.SetResolved(ctx, th.ID, false)
	require.NoError(t, err)
	require.False(t, again.Resolved)

	_, err = s.SetResolved(ctx, uuid.New(), true)
	require.ErrorIs(t, err, store.ErrReviewThreadNotFound)
}

func TestReviewStoreDeleteRemovesThreadAndMessages(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewReviewStore(pool)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	a := uuid.New()
	th, err := s.CreateThread(ctx, newThread(t, vid, a, a, domain.ReviewSideReviewer, "Hi"))
	require.NoError(t, err)

	require.NoError(t, s.DeleteThread(ctx, th.ID))
	_, err = s.GetThread(ctx, th.ID)
	require.ErrorIs(t, err, store.ErrReviewThreadNotFound)
	var n int
	require.NoError(t, pool.Querier().QueryRow(ctx, `SELECT count(*) FROM review_messages WHERE thread_id = $1`, th.ID).Scan(&n))
	require.Zero(t, n, "the messages go with the thread")
	require.ErrorIs(t, s.DeleteThread(ctx, th.ID), store.ErrReviewThreadNotFound)
}

func TestReviewStoreThreadsFollowTheVersionRow(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewReviewStore(pool)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	a := uuid.New()
	th, err := s.CreateThread(ctx, newThread(t, vid, a, a, domain.ReviewSideReviewer, "Hi"))
	require.NoError(t, err)

	// a status change (a rejected version stays) leaves the thread alone
	_, err = pool.Querier().Exec(ctx, `UPDATE policy_versions SET status = 'rejected' WHERE id = $1`, vid)
	require.NoError(t, err)
	kept, err := s.ListThreads(ctx, vid, uuid.Nil)
	require.NoError(t, err)
	require.Len(t, kept, 1)

	// only removing the version row takes its threads
	_, err = pool.Querier().Exec(ctx, `DELETE FROM policy_versions WHERE id = $1`, vid)
	require.NoError(t, err)
	_, err = s.GetThread(ctx, th.ID)
	require.ErrorIs(t, err, store.ErrReviewThreadNotFound)
}

func TestReviewTablesRefuseBlankTextAndBadOccurrence(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	a := uuid.New()
	_, err := pool.Querier().Exec(ctx, `INSERT INTO review_threads (policy_version_id, approver_user_id, created_by, quote, occurrence) VALUES ($1,$2,$2,'  ',1)`, vid, a)
	require.Error(t, err, "blank quote")
	_, err = pool.Querier().Exec(ctx, `INSERT INTO review_threads (policy_version_id, approver_user_id, created_by, quote, occurrence) VALUES ($1,$2,$2,'x',0)`, vid, a)
	require.Error(t, err, "occurrence below 1")
}

// GetThread backs the gateway's by-id check: it returns the one thread asked
// for, with its version id and every message oldest first.
func TestReviewStoreGetReturnsOneThreadWithItsMessagesInOrder(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewReviewStore(pool)
	ctx := context.Background()
	vid, _ := reviewVersion(t, pool)
	charlie, bob := uuid.New(), uuid.New()

	want, err := s.CreateThread(ctx, newThread(t, vid, charlie, charlie, domain.ReviewSideReviewer, "first"))
	require.NoError(t, err)
	_, err = s.CreateThread(ctx, newThread(t, vid, charlie, charlie, domain.ReviewSideReviewer, "other thread"))
	require.NoError(t, err)
	for _, body := range []string{"second", "third"} {
		m, merr := domain.NewReviewMessage(domain.DefaultReviewLimits, bob, body)
		require.NoError(t, merr)
		_, err = s.AddMessage(ctx, want.ID, m)
		require.NoError(t, err)
	}

	got, err := s.GetThread(ctx, want.ID)
	require.NoError(t, err)
	require.Equal(t, want.ID, got.ID)
	require.Equal(t, vid, got.PolicyVersionID)
	require.Equal(t, charlie, got.ApproverUserID)
	require.Len(t, got.Messages, 3)
	require.Equal(t, []string{"first", "second", "third"}, []string{got.Messages[0].Body, got.Messages[1].Body, got.Messages[2].Body})
}

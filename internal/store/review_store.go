// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// Review store errors.
var (
	ErrReviewThreadNotFound  = errors.New("review thread not found")
	ErrReviewVersionNotFound = errors.New("policy version not found")
)

const reviewThreadColumns = `id, policy_version_id, approver_user_id, created_by, quote, occurrence, resolved, created_at`

// ReviewStore persists review threads and their messages. A thread belongs to
// one policy version row and goes only when that row goes.
type ReviewStore struct{ db *postgres.DB }

// NewReviewStore returns a ReviewStore on db.
func NewReviewStore(db *postgres.DB) *ReviewStore { return &ReviewStore{db: db} }

func scanReviewThread(row pgx.Row) (domain.ReviewThread, error) {
	var t domain.ReviewThread
	err := row.Scan(&t.ID, &t.PolicyVersionID, &t.ApproverUserID, &t.CreatedBy, &t.Quote, &t.Occurrence, &t.Resolved, &t.CreatedAt)
	return t, err
}

// CreateThread stores a thread with its first message (t.Messages[0]) in one
// transaction and returns it as stored.
func (s *ReviewStore) CreateThread(ctx context.Context, t domain.ReviewThread) (domain.ReviewThread, error) {
	if len(t.Messages) == 0 {
		return domain.ReviewThread{}, errors.New("a thread needs its first message")
	}
	var id uuid.UUID
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `
			INSERT INTO review_threads (policy_version_id, approver_user_id, created_by, quote, occurrence)
			VALUES ($1,$2,$3,$4,$5) RETURNING id`,
			t.PolicyVersionID, t.ApproverUserID, t.CreatedBy, t.Quote, t.Occurrence).Scan(&id); err != nil {
			return err
		}
		m := t.Messages[0]
		_, err := tx.Exec(ctx, `
			INSERT INTO review_messages (thread_id, author_user_id, body) VALUES ($1,$2,$3)`,
			id, m.AuthorUserID, m.Body)
		return err
	})
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == pgForeignKeyViolation {
			return domain.ReviewThread{}, ErrReviewVersionNotFound
		}
		return domain.ReviewThread{}, fmt.Errorf("create review thread: %w", err)
	}
	return s.GetThread(ctx, id)
}

// GetThread returns a thread with its messages, oldest first.
func (s *ReviewStore) GetThread(ctx context.Context, id uuid.UUID) (domain.ReviewThread, error) {
	t, err := scanReviewThread(s.db.Querier().QueryRow(ctx,
		`SELECT `+reviewThreadColumns+` FROM review_threads WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ReviewThread{}, ErrReviewThreadNotFound
	}
	if err != nil {
		return domain.ReviewThread{}, fmt.Errorf("get review thread: %w", err)
	}
	msgs, err := s.messages(ctx, []uuid.UUID{id})
	if err != nil {
		return domain.ReviewThread{}, err
	}
	t.Messages = msgs[id]
	return t, nil
}

// ListThreads returns a version's threads, oldest first, each with its
// messages. A non-nil approver keeps only the threads with that approver.
// It returns an empty slice, not nil, for a version with no threads, and
// ErrReviewVersionNotFound when the version does not exist.
func (s *ReviewStore) ListThreads(ctx context.Context, versionID, approver uuid.UUID) ([]domain.ReviewThread, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT `+reviewThreadColumns+` FROM review_threads
		WHERE policy_version_id = $1 AND ($2::uuid IS NULL OR approver_user_id = $2)
		ORDER BY created_at, id`, versionID, nilUUIDToNull(approver))
	if err != nil {
		return nil, fmt.Errorf("list review threads: %w", err)
	}
	defer rows.Close()
	out := []domain.ReviewThread{}
	ids := []uuid.UUID{}
	for rows.Next() {
		t, err := scanReviewThread(rows)
		if err != nil {
			return nil, fmt.Errorf("list review threads: %w", err)
		}
		out = append(out, t)
		ids = append(ids, t.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list review threads: %w", err)
	}
	rows.Close()
	if len(ids) == 0 {
		// An empty list is only right for a version that exists.
		var exists bool
		if err := s.db.Querier().QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM policy_versions WHERE id = $1)`, versionID).Scan(&exists); err != nil {
			return nil, fmt.Errorf("list review threads: %w", err)
		}
		if !exists {
			return nil, ErrReviewVersionNotFound
		}
		return out, nil
	}
	msgs, err := s.messages(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Messages = msgs[out[i].ID]
	}
	return out, nil
}

// AddMessage appends a message to a thread and returns the thread.
func (s *ReviewStore) AddMessage(ctx context.Context, threadID uuid.UUID, m domain.ReviewMessage) (domain.ReviewThread, error) {
	_, err := s.db.Querier().Exec(ctx, `
		INSERT INTO review_messages (thread_id, author_user_id, body) VALUES ($1,$2,$3)`,
		threadID, m.AuthorUserID, m.Body)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == pgForeignKeyViolation {
			return domain.ReviewThread{}, ErrReviewThreadNotFound
		}
		return domain.ReviewThread{}, fmt.Errorf("add review message: %w", err)
	}
	return s.GetThread(ctx, threadID)
}

// SetResolved resolves (true) or reopens (false) a thread and returns it.
func (s *ReviewStore) SetResolved(ctx context.Context, id uuid.UUID, resolved bool) (domain.ReviewThread, error) {
	tag, err := s.db.Querier().Exec(ctx, `UPDATE review_threads SET resolved = $2 WHERE id = $1`, id, resolved)
	if err != nil {
		return domain.ReviewThread{}, fmt.Errorf("set review thread resolved: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.ReviewThread{}, ErrReviewThreadNotFound
	}
	return s.GetThread(ctx, id)
}

// DeleteThread removes a thread and, by cascade, its messages.
func (s *ReviewStore) DeleteThread(ctx context.Context, id uuid.UUID) error {
	tag, err := s.db.Querier().Exec(ctx, `DELETE FROM review_threads WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete review thread: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrReviewThreadNotFound
	}
	return nil
}

// messages returns the messages of the given threads, oldest first, by thread.
func (s *ReviewStore) messages(ctx context.Context, threadIDs []uuid.UUID) (map[uuid.UUID][]domain.ReviewMessage, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT id, thread_id, author_user_id, body, created_at FROM review_messages
		WHERE thread_id = ANY($1) ORDER BY created_at, id`, threadIDs)
	if err != nil {
		return nil, fmt.Errorf("list review messages: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID][]domain.ReviewMessage{}
	for rows.Next() {
		var (
			m      domain.ReviewMessage
			thread uuid.UUID
		)
		if err := rows.Scan(&m.ID, &thread, &m.AuthorUserID, &m.Body, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("list review messages: %w", err)
		}
		out[thread] = append(out[thread], m)
	}
	return out, rows.Err()
}

// nilUUIDToNull maps the zero UUID to SQL NULL, so a query can treat it as "any".
func nilUUIDToNull(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

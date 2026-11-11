// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

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
	// ErrSuggestionMissing: the thread is a plain comment and carries no suggestion.
	ErrSuggestionMissing = errors.New("review thread has no suggestion")
	// ErrSuggestionEnded: the suggestion already has an outcome.
	ErrSuggestionEnded = errors.New("review suggestion has ended")
	// ErrSuggestionChanged: another round landed since the caller read the suggestion.
	ErrSuggestionChanged = errors.New("review suggestion changed since it was read")
)

const reviewThreadColumns = `id, policy_version_id, approver_user_id, created_by, quote, occurrence, resolved, created_at`

// ReviewStore persists review threads, their messages and their suggestions. A thread belongs to
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
		if _, err := tx.Exec(ctx, `
			INSERT INTO review_messages (thread_id, author_user_id, body) VALUES ($1,$2,$3)`,
			id, m.AuthorUserID, m.Body); err != nil {
			return err
		}
		if t.Suggestion == nil || len(t.Suggestion.Rounds) == 0 {
			return nil
		}
		if _, err := tx.Exec(ctx, `INSERT INTO review_suggestions (thread_id) VALUES ($1)`, id); err != nil {
			return err
		}
		r := t.Suggestion.Rounds[0]
		_, err := tx.Exec(ctx, `
			INSERT INTO review_suggestion_rounds (thread_id, seq, side, user_id, text) VALUES ($1,1,$2::review_side,$3,$4)`,
			id, sideName(r.Side), r.UserID, r.Text)
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
	sugg, err := s.suggestions(ctx, []uuid.UUID{id})
	if err != nil {
		return domain.ReviewThread{}, err
	}
	t.Suggestion = sugg[id]
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
	sugg, err := s.suggestions(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Messages = msgs[out[i].ID]
		out[i].Suggestion = sugg[out[i].ID]
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

// AppendRound adds a counter (round.Seq must be the next round) to a thread's
// open suggestion and returns the thread. The suggestion row is locked so two
// counters cannot both take the same turn: ErrSuggestionEnded when it has an
// outcome, ErrSuggestionChanged when the round is not the next one,
// ErrSuggestionMissing for a plain comment, ErrReviewThreadNotFound for none.
func (s *ReviewStore) AppendRound(ctx context.Context, threadID uuid.UUID, round domain.SuggestionRound) (domain.ReviewThread, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		have, err := lockSuggestion(ctx, tx, threadID)
		if err != nil {
			return err
		}
		if have != round.Seq-1 {
			return ErrSuggestionChanged
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO review_suggestion_rounds (thread_id, seq, side, user_id, text) VALUES ($1,$2,$3::review_side,$4,$5)`,
			threadID, round.Seq, sideName(round.Side), round.UserID, round.Text)
		return err
	})
	if err != nil {
		return domain.ReviewThread{}, wrapSuggestionErr("append review suggestion round", err)
	}
	return s.GetThread(ctx, threadID)
}

// CloseSuggestion ends a thread's open suggestion with the checked decision and
// resolves the thread, in one transaction, and returns the thread. The errors
// are AppendRound's; ErrSuggestionChanged when a round landed since the
// decision was made (c.Rounds).
func (s *ReviewStore) CloseSuggestion(ctx context.Context, threadID uuid.UUID, c domain.SuggestionClose) (domain.ReviewThread, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		have, err := lockSuggestion(ctx, tx, threadID)
		if err != nil {
			return err
		}
		if have != c.Rounds {
			return ErrSuggestionChanged
		}
		if _, err := tx.Exec(ctx, `
			UPDATE review_suggestions
			SET outcome = $2::suggestion_outcome, reason = NULLIF($3, ''), closed_by = $4, closed_text = $5, closed_at = now()
			WHERE thread_id = $1`, threadID, c.Outcome.String(), c.Reason, c.ClosedBy, c.ClosedText); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE review_threads SET resolved = true WHERE id = $1`, threadID)
		return err
	})
	if err != nil {
		return domain.ReviewThread{}, wrapSuggestionErr("close review suggestion", err)
	}
	return s.GetThread(ctx, threadID)
}

// lockSuggestion locks a thread's suggestion row and returns how many rounds it
// has. It fails when the thread is unknown, is a plain comment, or has ended.
func lockSuggestion(ctx context.Context, tx pgx.Tx, threadID uuid.UUID) (int, error) {
	var open bool
	err := tx.QueryRow(ctx,
		`SELECT outcome IS NULL FROM review_suggestions WHERE thread_id = $1 FOR UPDATE`, threadID).Scan(&open)
	if errors.Is(err, pgx.ErrNoRows) {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM review_threads WHERE id = $1)`, threadID).Scan(&exists); err != nil {
			return 0, err
		}
		if !exists {
			return 0, ErrReviewThreadNotFound
		}
		return 0, ErrSuggestionMissing
	}
	if err != nil {
		return 0, err
	}
	if !open {
		return 0, ErrSuggestionEnded
	}
	var rounds int
	err = tx.QueryRow(ctx, `SELECT count(*) FROM review_suggestion_rounds WHERE thread_id = $1`, threadID).Scan(&rounds)
	return rounds, err
}

// wrapSuggestionErr leaves the store's own sentinel errors as they are and
// wraps anything else.
func wrapSuggestionErr(op string, err error) error {
	for _, known := range []error{ErrReviewThreadNotFound, ErrSuggestionMissing, ErrSuggestionEnded, ErrSuggestionChanged} {
		if errors.Is(err, known) {
			return known
		}
	}
	return fmt.Errorf("%s: %w", op, err)
}

// suggestions returns the suggestions of the given threads, with their rounds
// oldest first, by thread. A thread with none has no entry.
func (s *ReviewStore) suggestions(ctx context.Context, threadIDs []uuid.UUID) (map[uuid.UUID]*domain.ReviewSuggestion, error) {
	out := map[uuid.UUID]*domain.ReviewSuggestion{}
	rows, err := s.db.Querier().Query(ctx, `
		SELECT thread_id, coalesce(outcome::text, ''), coalesce(reason, ''), closed_by, coalesce(closed_text, ''), closed_at
		FROM review_suggestions WHERE thread_id = ANY($1)`, threadIDs)
	if err != nil {
		return nil, fmt.Errorf("list review suggestions: %w", err)
	}
	for rows.Next() {
		var (
			id       uuid.UUID
			outcome  string
			closedBy *uuid.UUID
			closedAt *time.Time
			sg       domain.ReviewSuggestion
		)
		if err := rows.Scan(&id, &outcome, &sg.Reason, &closedBy, &sg.ClosedText, &closedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("list review suggestions: %w", err)
		}
		sg.Outcome = outcomeFromName(outcome)
		if closedBy != nil {
			sg.ClosedBy = *closedBy
		}
		if closedAt != nil {
			sg.ClosedAt = *closedAt
		}
		out[id] = &sg
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list review suggestions: %w", err)
	}
	if len(out) == 0 {
		return out, nil
	}
	rounds, err := s.db.Querier().Query(ctx, `
		SELECT thread_id, seq, side::text, user_id, text, created_at FROM review_suggestion_rounds
		WHERE thread_id = ANY($1) ORDER BY thread_id, seq`, threadIDs)
	if err != nil {
		return nil, fmt.Errorf("list review suggestion rounds: %w", err)
	}
	defer rounds.Close()
	for rounds.Next() {
		var (
			id   uuid.UUID
			side string
			r    domain.SuggestionRound
		)
		if err := rounds.Scan(&id, &r.Seq, &side, &r.UserID, &r.Text, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("list review suggestion rounds: %w", err)
		}
		r.Side = sideFromName(side)
		if sg := out[id]; sg != nil {
			sg.Rounds = append(sg.Rounds, r)
		}
	}
	return out, rounds.Err()
}

func sideName(s domain.ReviewSide) string {
	if s == domain.ReviewSideReviewer {
		return "reviewer"
	}
	return "author"
}

func sideFromName(n string) domain.ReviewSide {
	if n == "reviewer" {
		return domain.ReviewSideReviewer
	}
	return domain.ReviewSideAuthor
}

func outcomeFromName(n string) domain.SuggestionOutcome {
	for _, o := range []domain.SuggestionOutcome{domain.SuggestionChanged, domain.SuggestionAgreed, domain.SuggestionRemoved, domain.SuggestionWithdrawn, domain.SuggestionNotAgreed} {
		if o.String() == n {
			return o
		}
	}
	return domain.SuggestionUnspecified
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

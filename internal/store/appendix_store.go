// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/google/uuid"
)

// ErrVersionNotDraft is returned when an appendix write targets a non-draft
// version. Handlers map it to codes.FailedPrecondition.
var ErrVersionNotDraft = errors.New("policy version is not a draft (appendices are immutable once published)")

// AppendixStore persists per-version policy appendices.
type AppendixStore struct{ db *postgres.DB }

// NewAppendixStore returns an AppendixStore on db.
func NewAppendixStore(db *postgres.DB) *AppendixStore { return &AppendixStore{db: db} }

func scanAppendix(row pgx.Row) (domain.Appendix, error) {
	var a domain.Appendix
	err := row.Scan(&a.ID, &a.PolicyVersionID, &a.Title, &a.ContentJSON, &a.OrderIndex, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

const appendixCols = `id, policy_version_id, title, content, order_index, created_at, updated_at`

// versionIsDraft reports whether the version exists and is a draft.
func (s *AppendixStore) versionIsDraft(ctx context.Context, q pgx.Tx, versionID uuid.UUID) (bool, error) {
	var status string
	err := q.QueryRow(ctx, `SELECT status FROM policy_versions WHERE id=$1`, versionID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status == "draft", nil
}

// ListByVersion returns the version's appendices ordered by order_index.
func (s *AppendixStore) ListByVersion(ctx context.Context, versionID uuid.UUID) ([]domain.Appendix, error) {
	rows, err := s.db.Querier().Query(ctx,
		`SELECT `+appendixCols+` FROM policy_appendices WHERE policy_version_id=$1 ORDER BY order_index`,
		versionID)
	if err != nil {
		return nil, fmt.Errorf("AppendixStore.ListByVersion: %w", err)
	}
	defer rows.Close()
	var out []domain.Appendix
	for rows.Next() {
		a, err := scanAppendix(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Add appends a new appendix to a draft version at MAX(order_index)+1.
func (s *AppendixStore) Add(ctx context.Context, a domain.Appendix) (domain.Appendix, error) {
	var out domain.Appendix
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		draft, err := s.versionIsDraft(ctx, tx, a.PolicyVersionID)
		if err != nil {
			return err
		}
		if !draft {
			return ErrVersionNotDraft
		}
		var next int
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(order_index)+1, 0) FROM policy_appendices WHERE policy_version_id=$1`,
			a.PolicyVersionID).Scan(&next); err != nil {
			return err
		}
		out, err = scanAppendix(tx.QueryRow(ctx,
			`INSERT INTO policy_appendices (id, policy_version_id, title, content, order_index)
			 VALUES ($1,$2,$3,$4::jsonb,$5) RETURNING `+appendixCols,
			a.ID, a.PolicyVersionID, a.Title, a.ContentJSON, next))
		return err
	})
	if err != nil {
		return domain.Appendix{}, err
	}
	return out, nil
}

// Update edits an appendix's title/content on a draft version.
func (s *AppendixStore) Update(ctx context.Context, id uuid.UUID, title, contentJSON string) (domain.Appendix, error) {
	var out domain.Appendix
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var versionID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT policy_version_id FROM policy_appendices WHERE id=$1`, id).Scan(&versionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrVersionNotDraft // a missing appendix is not writable
			}
			return err
		}
		draft, err := s.versionIsDraft(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if !draft {
			return ErrVersionNotDraft
		}
		out, err = scanAppendix(tx.QueryRow(ctx,
			`UPDATE policy_appendices SET title=$2, content=$3::jsonb, updated_at=now()
			 WHERE id=$1 RETURNING `+appendixCols, id, title, contentJSON))
		return err
	})
	if err != nil {
		return domain.Appendix{}, err
	}
	return out, nil
}

// renumberAppendices closes the gaps in a version's order_index. Rows move to
// a high offset first, so no step collides with the unique (version, order)
// constraint.
func renumberAppendices(ctx context.Context, tx pgx.Tx, versionID uuid.UUID) error {
	rows, err := tx.Query(ctx,
		`SELECT id FROM policy_appendices WHERE policy_version_id=$1 ORDER BY order_index`,
		versionID)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.Exec(ctx,
			`UPDATE policy_appendices SET order_index=$2 WHERE id=$1`,
			id, 10000+i); err != nil {
			return err
		}
	}
	for i, id := range ids {
		if _, err := tx.Exec(ctx,
			`UPDATE policy_appendices SET order_index=$2, updated_at=now() WHERE id=$1`,
			id, i); err != nil {
			return err
		}
	}
	return nil
}

// Delete removes an appendix from a draft version and renumbers the remaining
// appendices to maintain a contiguous 0-based order_index.
func (s *AppendixStore) Delete(ctx context.Context, id uuid.UUID) error {
	return s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var versionID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT policy_version_id FROM policy_appendices WHERE id=$1`, id).Scan(&versionID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // already gone
			}
			return err
		}
		draft, err := s.versionIsDraft(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if !draft {
			return ErrVersionNotDraft
		}
		if _, err := tx.Exec(ctx, `DELETE FROM policy_appendices WHERE id=$1`, id); err != nil {
			return err
		}
		return renumberAppendices(ctx, tx, versionID)
	})
}

// Reorder sets each appendix's order_index from its position in orderedIDs.
func (s *AppendixStore) Reorder(ctx context.Context, versionID uuid.UUID, orderedIDs []uuid.UUID) ([]domain.Appendix, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		draft, err := s.versionIsDraft(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if !draft {
			return ErrVersionNotDraft
		}
		// Move every row to a high offset first, so no step collides with the
		// unique (version, order) constraint.
		for i, id := range orderedIDs {
			if _, err := tx.Exec(ctx,
				`UPDATE policy_appendices SET order_index=$3 WHERE id=$1 AND policy_version_id=$2`,
				id, versionID, 1000+i); err != nil {
				return err
			}
		}
		for i, id := range orderedIDs {
			if _, err := tx.Exec(ctx,
				`UPDATE policy_appendices SET order_index=$3, updated_at=now() WHERE id=$1 AND policy_version_id=$2`,
				id, versionID, i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ListByVersion(ctx, versionID)
}

// CopyForward copies every appendix from fromVersionID into toVersionID (which
// must be a draft), preserving title/content/order_index with fresh ids.
func (s *AppendixStore) CopyForward(ctx context.Context, fromVersionID, toVersionID uuid.UUID) error {
	return s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		draft, err := s.versionIsDraft(ctx, tx, toVersionID)
		if err != nil {
			return err
		}
		if !draft {
			return ErrVersionNotDraft
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO policy_appendices (id, policy_version_id, title, content, order_index)
			 SELECT gen_random_uuid(), $2, title, content, order_index
			 FROM policy_appendices WHERE policy_version_id=$1`,
			fromVersionID, toVersionID)
		return err
	})
}

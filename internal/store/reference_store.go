// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// ErrReferenceNotFound is returned when a reference mutation targets a missing
// id. Callers map it to codes.NotFound.
var ErrReferenceNotFound = errors.New("reference not found")

// ErrReferenceInUse is returned when a delete is refused because the entry is
// still attached to one or more policies. Callers map it to
// codes.FailedPrecondition so the user can archive instead.
var ErrReferenceInUse = errors.New("reference is attached to one or more policies and cannot be deleted")

// ReferenceStore persists the references library and each document's
// attachments. Attachments are not versioned.
type ReferenceStore struct{ db *postgres.DB }

// NewReferenceStore returns a ReferenceStore on db.
func NewReferenceStore(db *postgres.DB) *ReferenceStore { return &ReferenceStore{db: db} }

// "references" is a reserved SQL keyword and must always be quoted.
const referenceCols = `id, label, kind, clause, body, url, archived, created_by_user_id, created_at`

func scanReference(row pgx.Row) (domain.Reference, error) {
	var r domain.Reference
	var kind string
	err := row.Scan(&r.ID, &r.Label, &kind, &r.Clause, &r.Body, &r.URL, &r.Archived, &r.CreatedByUserID, &r.CreatedAt)
	r.Kind = domain.ReferenceKind(kind)
	return r, err
}

// ListReferences returns the library ordered by label, each with a used-by
// count.
func (s *ReferenceStore) ListReferences(ctx context.Context, includeArchived bool) ([]domain.Reference, error) {
	where := "WHERE NOT r.archived"
	if includeArchived {
		where = ""
	}
	rows, err := s.db.Querier().Query(ctx, `
		SELECT r.id, r.label, r.kind, r.clause, r.body, r.url, r.archived, r.created_by_user_id, r.created_at,
		       COUNT(pr.reference_id) AS used_by
		FROM "references" r
		LEFT JOIN policy_references pr ON pr.reference_id = r.id
		`+where+`
		GROUP BY r.id
		ORDER BY r.label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Reference
	for rows.Next() {
		var r domain.Reference
		var kind string
		if err := rows.Scan(&r.ID, &r.Label, &kind, &r.Clause, &r.Body, &r.URL, &r.Archived, &r.CreatedByUserID, &r.CreatedAt, &r.UsedByCount); err != nil {
			return nil, err
		}
		r.Kind = domain.ReferenceKind(kind)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.Reference{}
	}
	return out, nil
}

// CreateReference inserts an active library entry and returns it.
func (s *ReferenceStore) CreateReference(ctx context.Context, r domain.Reference) (domain.Reference, error) {
	return scanReference(s.db.Querier().QueryRow(ctx, `
		INSERT INTO "references" (label, kind, clause, body, url, created_by_user_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING `+referenceCols,
		r.Label, string(r.Kind), r.Clause, r.Body, r.URL, r.CreatedByUserID))
}

// UpdateReference edits a library entry in place and returns it.
func (s *ReferenceStore) UpdateReference(ctx context.Context, id uuid.UUID, r domain.Reference) (domain.Reference, error) {
	out, err := scanReference(s.db.Querier().QueryRow(ctx, `
		UPDATE "references"
		   SET label=$2, kind=$3, clause=$4, body=$5, url=$6
		 WHERE id=$1
		 RETURNING `+referenceCols,
		id, r.Label, string(r.Kind), r.Clause, r.Body, r.URL))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Reference{}, ErrReferenceNotFound
	}
	return out, err
}

// SetReferenceArchived archives or restores an entry and returns it.
func (s *ReferenceStore) SetReferenceArchived(ctx context.Context, id uuid.UUID, archived bool) (domain.Reference, error) {
	out, err := scanReference(s.db.Querier().QueryRow(ctx, `
		UPDATE "references" SET archived=$2
		 WHERE id=$1
		 RETURNING `+referenceCols, id, archived))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Reference{}, ErrReferenceNotFound
	}
	return out, err
}

// DeleteReference removes an entry, or refuses with ErrReferenceInUse while
// any policy attaches it.
func (s *ReferenceStore) DeleteReference(ctx context.Context, id uuid.UUID) error {
	return s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var usedBy int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM policy_references WHERE reference_id=$1`, id).Scan(&usedBy); err != nil {
			return err
		}
		if usedBy > 0 {
			return ErrReferenceInUse
		}
		tag, err := tx.Exec(ctx, `DELETE FROM "references" WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrReferenceNotFound
		}
		return nil
	})
}

// ListPolicyReferences returns the entries attached to a policy, in
// attachment order, archived ones included.
func (s *ReferenceStore) ListPolicyReferences(ctx context.Context, policyID uuid.UUID) ([]domain.Reference, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT r.id, r.label, r.kind, r.clause, r.body, r.url, r.archived, r.created_by_user_id, r.created_at
		FROM policy_references pr
		JOIN "references" r ON r.id = pr.reference_id
		WHERE pr.policy_id = $1
		ORDER BY pr.order_index`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Reference
	for rows.Next() {
		r, err := scanReference(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.Reference{}
	}
	return out, nil
}

// SetPolicyReferences replaces a policy's attached entries, in order and
// without duplicates. Unknown ids fail on the foreign key.
func (s *ReferenceStore) SetPolicyReferences(ctx context.Context, policyID uuid.UUID, referenceIDs []uuid.UUID) ([]domain.Reference, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM policy_references WHERE policy_id = $1`, policyID); err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		orderIndex := 0
		for _, rid := range referenceIDs {
			if seen[rid] {
				continue
			}
			seen[rid] = true
			if _, err := tx.Exec(ctx, `
				INSERT INTO policy_references (policy_id, reference_id, order_index)
				VALUES ($1,$2,$3)`, policyID, rid, orderIndex); err != nil {
				return err
			}
			orderIndex++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ListPolicyReferences(ctx, policyID)
}

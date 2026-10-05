// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"strings"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// ErrDefinitionEntryNotFound is returned when a definition-library mutation
// targets a missing id. Callers map it to codes.NotFound.
var ErrDefinitionEntryNotFound = errors.New("definition not found")

// ErrDefinitionEntryInUse is returned when a delete is refused because the entry
// is still attached to one or more policies. Callers map it to
// codes.FailedPrecondition so the user can archive instead.
var ErrDefinitionEntryInUse = errors.New("definition is attached to one or more policies and cannot be deleted")

// DefinitionLibraryStore persists the category-scoped definitions library and
// each document's attachments. Attachments are not versioned.
type DefinitionLibraryStore struct{ db *postgres.DB }

// NewDefinitionLibraryStore returns a DefinitionLibraryStore on db.
func NewDefinitionLibraryStore(db *postgres.DB) *DefinitionLibraryStore {
	return &DefinitionLibraryStore{db: db}
}

const definitionEntryCols = `id, category_id, term, definition, archived, created_by_user_id, created_at`

func scanDefinitionEntry(row pgx.Row) (domain.DefinitionEntry, error) {
	var d domain.DefinitionEntry
	err := row.Scan(&d.ID, &d.CategoryID, &d.Term, &d.Definition, &d.Archived, &d.CreatedByUserID, &d.CreatedAt)
	return d, err
}

// scanDefinitionEntryRows iterates rows that also carry a trailing used_by count.
func scanDefinitionEntryRows(rows pgx.Rows) ([]domain.DefinitionEntry, error) {
	var out []domain.DefinitionEntry
	for rows.Next() {
		var d domain.DefinitionEntry
		if err := rows.Scan(&d.ID, &d.CategoryID, &d.Term, &d.Definition, &d.Archived, &d.CreatedByUserID, &d.CreatedAt, &d.UsedByCount); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.DefinitionEntry{}
	}
	return out, nil
}

// ListDefinitionEntries returns the library ordered by term, each with a
// used-by count. A nil categoryID lists every category.
func (s *DefinitionLibraryStore) ListDefinitionEntries(ctx context.Context, categoryID *uuid.UUID, includeArchived bool) ([]domain.DefinitionEntry, error) {
	conds := []string{}
	args := []any{}
	if !includeArchived {
		conds = append(conds, "NOT d.archived")
	}
	if categoryID != nil {
		args = append(args, *categoryID)
		conds = append(conds, "d.category_id = $1")
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	rows, err := s.db.Querier().Query(ctx, `
		SELECT d.id, d.category_id, d.term, d.definition, d.archived, d.created_by_user_id, d.created_at,
		       COUNT(a.definition_id) AS used_by
		FROM definitions d
		LEFT JOIN policy_definition_attachments a ON a.definition_id = d.id
		`+where+`
		GROUP BY d.id
		ORDER BY d.term`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitionEntryRows(rows)
}

// ListPolicyDefinitionCandidates returns the entries a policy may attach:
// those of its home category and its ancestors, ordered by term.
func (s *DefinitionLibraryStore) ListPolicyDefinitionCandidates(ctx context.Context, policyID uuid.UUID, includeArchived bool) ([]domain.DefinitionEntry, error) {
	archivedFilter := "AND NOT d.archived"
	if includeArchived {
		archivedFilter = ""
	}
	rows, err := s.db.Querier().Query(ctx, `
		WITH RECURSIVE chain AS (
		  SELECT g.id, g.parent_id
		  FROM categories g
		  JOIN policies p ON p.home_category_id = g.id
		  WHERE p.id = $1
		  UNION ALL
		  SELECT g.id, g.parent_id
		  FROM categories g
		  JOIN chain c ON g.id = c.parent_id
		)
		SELECT d.id, d.category_id, d.term, d.definition, d.archived, d.created_by_user_id, d.created_at,
		       COUNT(a.definition_id) AS used_by
		FROM definitions d
		LEFT JOIN policy_definition_attachments a ON a.definition_id = d.id
		WHERE d.category_id IN (SELECT id FROM chain) `+archivedFilter+`
		GROUP BY d.id
		ORDER BY d.term`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDefinitionEntryRows(rows)
}

// CreateDefinitionEntry inserts an active library entry and returns it.
func (s *DefinitionLibraryStore) CreateDefinitionEntry(ctx context.Context, d domain.DefinitionEntry) (domain.DefinitionEntry, error) {
	return scanDefinitionEntry(s.db.Querier().QueryRow(ctx, `
		INSERT INTO definitions (category_id, term, definition, created_by_user_id)
		VALUES ($1,$2,$3,$4)
		RETURNING `+definitionEntryCols,
		d.CategoryID, d.Term, d.Definition, d.CreatedByUserID))
}

// UpdateDefinitionEntry edits an entry's term and definition in place and
// returns it. The category never changes.
func (s *DefinitionLibraryStore) UpdateDefinitionEntry(ctx context.Context, id uuid.UUID, d domain.DefinitionEntry) (domain.DefinitionEntry, error) {
	out, err := scanDefinitionEntry(s.db.Querier().QueryRow(ctx, `
		UPDATE definitions
		   SET term=$2, definition=$3
		 WHERE id=$1
		 RETURNING `+definitionEntryCols,
		id, d.Term, d.Definition))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DefinitionEntry{}, ErrDefinitionEntryNotFound
	}
	return out, err
}

// SetDefinitionEntryArchived archives or restores an entry and returns it.
func (s *DefinitionLibraryStore) SetDefinitionEntryArchived(ctx context.Context, id uuid.UUID, archived bool) (domain.DefinitionEntry, error) {
	out, err := scanDefinitionEntry(s.db.Querier().QueryRow(ctx, `
		UPDATE definitions SET archived=$2
		 WHERE id=$1
		 RETURNING `+definitionEntryCols, id, archived))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DefinitionEntry{}, ErrDefinitionEntryNotFound
	}
	return out, err
}

// DeleteDefinitionEntry removes an entry, or refuses with
// ErrDefinitionEntryInUse while any policy attaches it.
func (s *DefinitionLibraryStore) DeleteDefinitionEntry(ctx context.Context, id uuid.UUID) error {
	return s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var usedBy int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM policy_definition_attachments WHERE definition_id=$1`, id).Scan(&usedBy); err != nil {
			return err
		}
		if usedBy > 0 {
			return ErrDefinitionEntryInUse
		}
		tag, err := tx.Exec(ctx, `DELETE FROM definitions WHERE id=$1`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrDefinitionEntryNotFound
		}
		return nil
	})
}

// ListPolicyDefinitionEntries returns the entries attached to a policy, in
// attachment order, archived ones included.
func (s *DefinitionLibraryStore) ListPolicyDefinitionEntries(ctx context.Context, policyID uuid.UUID) ([]domain.DefinitionEntry, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT d.id, d.category_id, d.term, d.definition, d.archived, d.created_by_user_id, d.created_at
		FROM policy_definition_attachments a
		JOIN definitions d ON d.id = a.definition_id
		WHERE a.policy_id = $1
		ORDER BY a.order_index`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.DefinitionEntry
	for rows.Next() {
		d, err := scanDefinitionEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.DefinitionEntry{}
	}
	return out, nil
}

// SetPolicyDefinitionEntries replaces a policy's attached entries, in order
// and without duplicates. Unknown ids fail on the foreign key.
func (s *DefinitionLibraryStore) SetPolicyDefinitionEntries(ctx context.Context, policyID uuid.UUID, definitionIDs []uuid.UUID) ([]domain.DefinitionEntry, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM policy_definition_attachments WHERE policy_id = $1`, policyID); err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		orderIndex := 0
		for _, did := range definitionIDs {
			if seen[did] {
				continue
			}
			seen[did] = true
			if _, err := tx.Exec(ctx, `
				INSERT INTO policy_definition_attachments (policy_id, definition_id, order_index)
				VALUES ($1,$2,$3)`, policyID, did, orderIndex); err != nil {
				return err
			}
			orderIndex++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ListPolicyDefinitionEntries(ctx, policyID)
}

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

// ErrContactBlockNotFound is returned when a contact-block mutation targets a
// missing id. Callers map it to codes.NotFound.
var ErrContactBlockNotFound = errors.New("contact block not found")

// ContactStore persists the contact-block library and each document's
// attachments. Attachments are not versioned.
type ContactStore struct{ db *postgres.DB }

// NewContactStore returns a ContactStore on db.
func NewContactStore(db *postgres.DB) *ContactStore { return &ContactStore{db: db} }

const contactCols = `id, label, name, role, department, email, phone, hours, notes, archived`

func scanContactBlock(row pgx.Row) (domain.ContactBlock, error) {
	var b domain.ContactBlock
	err := row.Scan(&b.ID, &b.Label, &b.Name, &b.Role, &b.Department, &b.Email, &b.Phone, &b.Hours, &b.Notes, &b.Archived)
	return b, err
}

// ListBlocks returns the library ordered by label, each with a used-by count.
func (s *ContactStore) ListBlocks(ctx context.Context, includeArchived bool) ([]domain.ContactBlock, error) {
	where := "WHERE NOT cb.archived"
	if includeArchived {
		where = ""
	}
	rows, err := s.db.Querier().Query(ctx, `
		SELECT cb.id, cb.label, cb.name, cb.role, cb.department, cb.email, cb.phone, cb.hours, cb.notes, cb.archived,
		       COUNT(pcb.id) AS used_by
		FROM contact_blocks cb
		LEFT JOIN policy_contact_blocks pcb ON pcb.contact_block_id = cb.id
		`+where+`
		GROUP BY cb.id
		ORDER BY cb.label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ContactBlock
	for rows.Next() {
		var b domain.ContactBlock
		if err := rows.Scan(&b.ID, &b.Label, &b.Name, &b.Role, &b.Department, &b.Email, &b.Phone, &b.Hours, &b.Notes, &b.Archived, &b.UsedByCount); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.ContactBlock{}
	}
	return out, nil
}

// CreateBlock inserts a library block (active) and returns it.
func (s *ContactStore) CreateBlock(ctx context.Context, b domain.ContactBlock) (domain.ContactBlock, error) {
	return scanContactBlock(s.db.Querier().QueryRow(ctx, `
		INSERT INTO contact_blocks (label, name, role, department, email, phone, hours, notes)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING `+contactCols,
		b.Label, b.Name, b.Role, b.Department, b.Email, b.Phone, b.Hours, b.Notes))
}

// UpdateBlock edits a library block in place and returns it.
func (s *ContactStore) UpdateBlock(ctx context.Context, id uuid.UUID, b domain.ContactBlock) (domain.ContactBlock, error) {
	out, err := scanContactBlock(s.db.Querier().QueryRow(ctx, `
		UPDATE contact_blocks
		   SET label=$2, name=$3, role=$4, department=$5, email=$6, phone=$7, hours=$8, notes=$9, updated_at=now()
		 WHERE id=$1
		 RETURNING `+contactCols,
		id, b.Label, b.Name, b.Role, b.Department, b.Email, b.Phone, b.Hours, b.Notes))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ContactBlock{}, ErrContactBlockNotFound
	}
	return out, err
}

// SetArchived archives or restores a library block and returns it.
func (s *ContactStore) SetArchived(ctx context.Context, id uuid.UUID, archived bool) (domain.ContactBlock, error) {
	out, err := scanContactBlock(s.db.Querier().QueryRow(ctx, `
		UPDATE contact_blocks SET archived=$2, updated_at=now()
		 WHERE id=$1
		 RETURNING `+contactCols, id, archived))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.ContactBlock{}, ErrContactBlockNotFound
	}
	return out, err
}

// DeleteBlock removes a library block; the join's ON DELETE CASCADE detaches it
// from every policy that referenced it.
func (s *ContactStore) DeleteBlock(ctx context.Context, id uuid.UUID) error {
	tag, err := s.db.Querier().Exec(ctx, `DELETE FROM contact_blocks WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrContactBlockNotFound
	}
	return nil
}

// ListByPolicy returns the blocks attached to a policy, in attachment order,
// archived ones included.
func (s *ContactStore) ListByPolicy(ctx context.Context, policyID uuid.UUID) ([]domain.ContactBlock, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT cb.id, cb.label, cb.name, cb.role, cb.department, cb.email, cb.phone, cb.hours, cb.notes, cb.archived
		FROM policy_contact_blocks pcb
		JOIN contact_blocks cb ON cb.id = pcb.contact_block_id
		WHERE pcb.policy_id = $1
		ORDER BY pcb.ordinal`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.ContactBlock
	for rows.Next() {
		b, err := scanContactBlock(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.ContactBlock{}
	}
	return out, nil
}

// SetForPolicy replaces a policy's attached blocks, in order and without
// duplicates. Unknown ids fail on the foreign key.
func (s *ContactStore) SetForPolicy(ctx context.Context, policyID uuid.UUID, blockIDs []uuid.UUID) ([]domain.ContactBlock, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM policy_contact_blocks WHERE policy_id = $1`, policyID); err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		ordinal := 0
		for _, bid := range blockIDs {
			if seen[bid] {
				continue
			}
			seen[bid] = true
			if _, err := tx.Exec(ctx, `
				INSERT INTO policy_contact_blocks (policy_id, contact_block_id, ordinal)
				VALUES ($1,$2,$3)`, policyID, bid, ordinal); err != nil {
				return err
			}
			ordinal++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.ListByPolicy(ctx, policyID)
}

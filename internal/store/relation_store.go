// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"github.com/jackc/pgx/v5"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// RelationStore persists each policy's related links. They are not versioned,
// so there is no draft guard.
type RelationStore struct{ db *postgres.DB }

// NewRelationStore returns a RelationStore on db.
func NewRelationStore(db *postgres.DB) *RelationStore { return &RelationStore{db: db} }

// ListByPolicy returns a policy's related links in order, with each linked
// policy's current number and title. It returns an empty slice, not nil.
func (s *RelationStore) ListByPolicy(ctx context.Context, policyID uuid.UUID) ([]domain.RelatedPolicy, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT pr.related_policy_id, g.code, g.slug, p.sequence, p.document_type, p.title
		FROM policy_relations pr
		JOIN policies p ON p.id = pr.related_policy_id
		JOIN categories   g ON g.id = p.home_category_id
		WHERE pr.policy_id = $1
		ORDER BY pr.ordinal`, policyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.RelatedPolicy
	for rows.Next() {
		var (
			r        domain.RelatedPolicy
			code     *string
			slug     string
			sequence int
			docType  string
		)
		if err := rows.Scan(&r.PolicyID, &code, &slug, &sequence, &docType, &r.Title); err != nil {
			return nil, err
		}
		r.Number = renderDocNumber(domain.DocumentType(docType), numberCodeSegment(code, slug), sequence)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.RelatedPolicy{}
	}
	return out, nil
}

// SetForPolicy replaces a policy's related links, in order. Self links and
// duplicates are dropped; unknown ids fail on the foreign key.
func (s *RelationStore) SetForPolicy(ctx context.Context, policyID uuid.UUID, relatedIDs []uuid.UUID) ([]domain.RelatedPolicy, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM policy_relations WHERE policy_id = $1`, policyID); err != nil {
			return err
		}
		seen := map[uuid.UUID]bool{}
		ordinal := 0
		for _, rid := range relatedIDs {
			if rid == policyID || seen[rid] { // drop self-reference + duplicates
				continue
			}
			seen[rid] = true
			if _, err := tx.Exec(ctx, `
				INSERT INTO policy_relations (policy_id, related_policy_id, ordinal)
				VALUES ($1,$2,$3)`, policyID, rid, ordinal); err != nil {
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

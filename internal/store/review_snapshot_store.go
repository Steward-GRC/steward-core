// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// ReviewSnapshotStore keeps, per approver and version, a copy of the content
// the approver last reviewed. A version in review is edited in place, so the
// copy is the only record of the earlier text. The rows go with the version.
type ReviewSnapshotStore struct{ db *postgres.DB }

// NewReviewSnapshotStore returns a ReviewSnapshotStore on db.
func NewReviewSnapshotStore(db *postgres.DB) *ReviewSnapshotStore {
	return &ReviewSnapshotStore{db: db}
}

// GetOrCreate returns the approver's snapshot beside the version's current
// text. When the approver has none yet it is taken at the current text first,
// so a first read shows nothing. It returns pgx.ErrNoRows (wrapped) when the
// version does not exist. Two reads racing to create the same snapshot both
// end up with the one that was inserted first.
func (s *ReviewSnapshotStore) GetOrCreate(ctx context.Context, versionID, userID uuid.UUID) (domain.ReviewSnapshotView, error) {
	// Two statements: the insert is a no-op when the snapshot exists, and the
	// read after it sees the stored row whichever of two racing reads won. An
	// insert that affected no row means either the snapshot exists already or
	// the version does not; the read tells which.
	tag, err := s.db.Querier().Exec(ctx, `
		INSERT INTO review_snapshots (policy_version_id, user_id, content)
		SELECT id, $2, content FROM policy_versions WHERE id = $1
		ON CONFLICT (policy_version_id, user_id) DO NOTHING`, versionID, userID)
	if err != nil {
		return domain.ReviewSnapshotView{}, fmt.Errorf("ReviewSnapshotStore.GetOrCreate: insert: %w", err)
	}
	v := domain.ReviewSnapshotView{Created: tag.RowsAffected() == 1}
	var tvID *uuid.UUID
	err = s.db.Querier().QueryRow(ctx, `
		SELECT rs.content::text, pv.content::text, pv.template_version_id
		  FROM policy_versions pv
		  JOIN review_snapshots rs ON rs.policy_version_id = pv.id AND rs.user_id = $2
		 WHERE pv.id = $1`, versionID, userID,
	).Scan(&v.Before, &v.Current, &tvID)
	if err != nil {
		return domain.ReviewSnapshotView{}, fmt.Errorf("ReviewSnapshotStore.GetOrCreate: %w", err)
	}
	if tvID != nil {
		v.TemplateVersionID = *tvID
	}
	return v, nil
}

// Mark moves the approver's snapshot to the version's current text, creating it
// when the approver has none. It moves that approver's snapshot only. It
// returns pgx.ErrNoRows (wrapped) when the version does not exist.
func (s *ReviewSnapshotStore) Mark(ctx context.Context, versionID, userID uuid.UUID) error {
	var id uuid.UUID
	err := s.db.Querier().QueryRow(ctx, `
		INSERT INTO review_snapshots (policy_version_id, user_id, content, taken_at)
		SELECT id, $2, content, now() FROM policy_versions WHERE id = $1
		ON CONFLICT (policy_version_id, user_id)
		DO UPDATE SET content = EXCLUDED.content, taken_at = EXCLUDED.taken_at
		RETURNING policy_version_id`, versionID, userID,
	).Scan(&id)
	if err != nil {
		return fmt.Errorf("ReviewSnapshotStore.Mark: %w", err)
	}
	return nil
}

// Count returns how many snapshots a version holds.
func (s *ReviewSnapshotStore) Count(ctx context.Context, versionID uuid.UUID) (int, error) {
	var n int
	if err := s.db.Querier().QueryRow(ctx,
		`SELECT count(*) FROM review_snapshots WHERE policy_version_id = $1`, versionID,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("ReviewSnapshotStore.Count: %w", err)
	}
	return n, nil
}

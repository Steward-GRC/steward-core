// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// Get returns one appendix, live or retired, with its whole version table.
// The letter comes from its place among its policy's live appendices, so it
// is loaded with the siblings and picked out; a retired appendix has none.
func (s *PolicyAppendixStore) Get(ctx context.Context, id uuid.UUID) (domain.PolicyAppendix, error) {
	var policyID uuid.UUID
	err := s.db.Querier().QueryRow(ctx, `SELECT policy_id FROM policy_owned_appendices WHERE id=$1`, id).Scan(&policyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.PolicyAppendix{}, ErrPolicyAppendixNotFound
	}
	if err != nil {
		return domain.PolicyAppendix{}, fmt.Errorf("PolicyAppendixStore.Get: %w", err)
	}
	all, err := loadPolicyAppendices(ctx, s.db.Querier(), policyID)
	if err != nil {
		return domain.PolicyAppendix{}, fmt.Errorf("PolicyAppendixStore.Get: %w", err)
	}
	at := slices.IndexFunc(all, func(a domain.PolicyAppendix) bool { return a.ID == id })
	if at < 0 {
		return domain.PolicyAppendix{}, ErrPolicyAppendixNotFound // deleted between the two reads
	}
	return all[at], nil
}

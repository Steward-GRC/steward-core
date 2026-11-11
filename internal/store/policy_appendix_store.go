// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// Errors the policy appendix store returns. Handlers map them to gRPC codes.
var (
	// ErrPolicyAppendixNotFound is returned when no appendix has the id.
	ErrPolicyAppendixNotFound = errors.New("policy appendix not found")
	// ErrAppendixPolicyNotFound is returned when a new appendix names a policy that does not exist.
	ErrAppendixPolicyNotFound = errors.New("policy not found")
	// ErrAppendixNoDraft is returned when publishing an appendix that has no draft.
	ErrAppendixNoDraft = errors.New("appendix has no draft to publish")
	// ErrAppendixRetired is returned when saving or publishing a retired appendix.
	ErrAppendixRetired = errors.New("appendix is retired; restore it first")
	// ErrAppendixPolicyMismatch is returned when a checkpoint names a policy the appendix does not belong to.
	ErrAppendixPolicyMismatch = errors.New("appendix does not belong to the policy")
	// ErrAppendixReorderMismatch is returned when a reorder list is not every live appendix of the policy exactly once.
	ErrAppendixReorderMismatch = errors.New("ordered ids must name every live appendix of the policy exactly once")
)

// PolicyAppendixStore persists the appendices a policy owns, each with its own
// version table. Every write locks the policy row first, so two writers on one
// policy never compute the same place or title.
type PolicyAppendixStore struct{ db *postgres.DB }

// NewPolicyAppendixStore returns a PolicyAppendixStore on db.
func NewPolicyAppendixStore(db *postgres.DB) *PolicyAppendixStore {
	return &PolicyAppendixStore{db: db}
}

const (
	policyAppendixCols  = `id, policy_id, order_index, created_at, created_by, retired_at, retired_by`
	appendixVersionCols = `id, appendix_id, version_no, state, title, content_json::text, created_at, created_by,
		updated_at, updated_by, published_at, published_by, editors`
)

// loadPolicyAppendices reads every appendix of a policy with its versions:
// the live ones in order, then the retired ones, letters assigned.
func loadPolicyAppendices(ctx context.Context, q postgres.Querier, policyID uuid.UUID) ([]domain.PolicyAppendix, error) {
	rows, err := q.Query(ctx,
		`SELECT `+policyAppendixCols+` FROM policy_owned_appendices WHERE policy_id=$1
		  ORDER BY (retired_at IS NOT NULL), order_index, created_at, id`, policyID)
	if err != nil {
		return nil, fmt.Errorf("list policy appendices: %w", err)
	}
	var out []domain.PolicyAppendix
	for rows.Next() {
		var a domain.PolicyAppendix
		if err := rows.Scan(&a.ID, &a.PolicyID, &a.OrderIndex, &a.CreatedAt, &a.CreatedBy, &a.RetiredAt, &a.RetiredBy); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}

	vrows, err := q.Query(ctx,
		`SELECT `+appendixVersionCols+` FROM policy_appendix_versions
		  WHERE appendix_id IN (SELECT id FROM policy_owned_appendices WHERE policy_id=$1)
		  ORDER BY appendix_id, version_no`, policyID)
	if err != nil {
		return nil, fmt.Errorf("list policy appendix versions: %w", err)
	}
	defer vrows.Close()
	byID := make(map[uuid.UUID]int, len(out))
	for i, a := range out {
		byID[a.ID] = i
	}
	for vrows.Next() {
		var v domain.PolicyAppendixVersion
		var state string
		if err := vrows.Scan(&v.ID, &v.AppendixID, &v.VersionNo, &state, &v.Title, &v.ContentJSON, &v.CreatedAt,
			&v.CreatedBy, &v.UpdatedAt, &v.UpdatedBy, &v.PublishedAt, &v.PublishedBy, &v.Editors); err != nil {
			return nil, err
		}
		v.State = domain.AppendixState(state)
		i := byID[v.AppendixID]
		out[i].Versions = append(out[i].Versions, v)
	}
	if err := vrows.Err(); err != nil {
		return nil, err
	}
	domain.AssignAppendixLetters(out)
	return out, nil
}

// List returns a policy's live appendices in order, and the retired ones after
// them when includeRetired is set. Each carries its whole version table; what
// a reader may see of it is the caller's call.
func (s *PolicyAppendixStore) List(ctx context.Context, policyID uuid.UUID, includeRetired bool) ([]domain.PolicyAppendix, error) {
	all, err := loadPolicyAppendices(ctx, s.db.Querier(), policyID)
	if err != nil {
		return nil, fmt.Errorf("PolicyAppendixStore.List: %w", err)
	}
	if includeRetired {
		return all, nil
	}
	return slices.DeleteFunc(all, func(a domain.PolicyAppendix) bool { return !a.Live() }), nil
}

// lockPolicy takes the policy row for the rest of the transaction.
func lockPolicy(ctx context.Context, tx pgx.Tx, policyID uuid.UUID) error {
	var one int
	err := tx.QueryRow(ctx, `SELECT 1 FROM policies WHERE id=$1 FOR UPDATE`, policyID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAppendixPolicyNotFound
	}
	return err
}

// withAppendix runs fn on one appendix of a policy, in a transaction that holds
// the policy lock. fn sees every appendix of the policy (for places and titles)
// and the index of the target. The appendix returned is read again after fn.
func (s *PolicyAppendixStore) withAppendix(ctx context.Context, id uuid.UUID, fn func(tx pgx.Tx, all []domain.PolicyAppendix, at int) error) (domain.PolicyAppendix, error) {
	var out domain.PolicyAppendix
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var policyID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT policy_id FROM policy_owned_appendices WHERE id=$1`, id).Scan(&policyID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrPolicyAppendixNotFound
			}
			return err
		}
		if err := lockPolicy(ctx, tx, policyID); err != nil {
			return err
		}
		all, err := loadPolicyAppendices(ctx, tx, policyID)
		if err != nil {
			return err
		}
		at := slices.IndexFunc(all, func(a domain.PolicyAppendix) bool { return a.ID == id })
		if at < 0 {
			return ErrPolicyAppendixNotFound // deleted between the two reads
		}
		if err := fn(tx, all, at); err != nil {
			return err
		}
		all, err = loadPolicyAppendices(ctx, tx, policyID)
		if err != nil {
			return err
		}
		out = all[slices.IndexFunc(all, func(a domain.PolicyAppendix) bool { return a.ID == id })]
		return nil
	})
	if err != nil {
		return domain.PolicyAppendix{}, err
	}
	return out, nil
}

// Create adds an appendix as a draft v1, last among the live ones. A blank
// title becomes "Appendix <letter of its place>", skipping any title already
// used by an appendix of the policy, retired ones included.
func (s *PolicyAppendixStore) Create(ctx context.Context, policyID, actor uuid.UUID, title, contentJSON string) (domain.PolicyAppendix, error) {
	var created uuid.UUID
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if err := lockPolicy(ctx, tx, policyID); err != nil {
			return err
		}
		all, err := loadPolicyAppendices(ctx, tx, policyID)
		if err != nil {
			return err
		}
		live := 0
		var taken []string
		for _, a := range all {
			if a.Live() {
				live++
			}
			for _, v := range a.Versions {
				taken = append(taken, v.Title)
			}
		}
		title = strings.TrimSpace(title)
		if title == "" {
			title = domain.DefaultAppendixTitle(taken, live)
		}
		created = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO policy_owned_appendices (id, policy_id, order_index, created_by) VALUES ($1,$2,$3,$4)`,
			created, policyID, live, actor); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO policy_appendix_versions
			   (appendix_id, version_no, state, title, content_json, created_by, updated_by, editors)
			 VALUES ($1, 1, 'DRAFT', $2, $3::jsonb, $4, $4, ARRAY[$4]::uuid[])`,
			created, title, contentJSON, actor)
		return err
	})
	if err != nil {
		return domain.PolicyAppendix{}, err
	}
	all, err := s.List(ctx, policyID, true)
	if err != nil {
		return domain.PolicyAppendix{}, err
	}
	if i := slices.IndexFunc(all, func(a domain.PolicyAppendix) bool { return a.ID == created }); i >= 0 {
		return all[i], nil
	}
	return domain.PolicyAppendix{}, ErrPolicyAppendixNotFound
}

// SaveDraft updates the open draft, or starts draft vN+1 beside the published
// row. With no draft open, a save equal to the published title and content
// changes nothing and reports changed=false.
func (s *PolicyAppendixStore) SaveDraft(ctx context.Context, id, actor uuid.UUID, title, contentJSON string) (domain.PolicyAppendix, bool, error) {
	return s.saveDraft(ctx, id, uuid.Nil, actor, &title, contentJSON)
}

// CheckpointDraft saves the text the collab room holds, for the person editing.
// It is SaveDraft without a title: an open draft keeps its own, and a new draft
// takes the published row's. policyID must be the appendix's policy
// (ErrAppendixPolicyMismatch otherwise). Content equal to the published row
// with no draft open makes no draft and reports changed=false.
func (s *PolicyAppendixStore) CheckpointDraft(ctx context.Context, id, policyID, actor uuid.UUID, contentJSON string) (domain.PolicyAppendix, bool, error) {
	return s.saveDraft(ctx, id, policyID, actor, nil, contentJSON)
}

// saveDraft is the save-draft path SaveDraft and CheckpointDraft share. A nil
// title keeps the open draft's title, or takes the published row's for a new
// draft. A non-nil policyID must be the appendix's policy.
func (s *PolicyAppendixStore) saveDraft(ctx context.Context, id, policyID, actor uuid.UUID, title *string, contentJSON string) (domain.PolicyAppendix, bool, error) {
	changed := false
	a, err := s.withAppendix(ctx, id, func(tx pgx.Tx, all []domain.PolicyAppendix, at int) error {
		cur := all[at]
		if policyID != uuid.Nil && cur.PolicyID != policyID {
			return ErrAppendixPolicyMismatch
		}
		if !cur.Live() {
			return ErrAppendixRetired
		}
		if d := cur.Draft(); d != nil {
			t := d.Title
			if title != nil {
				t = *title
			}
			changed = true
			_, err := tx.Exec(ctx,
				`UPDATE policy_appendix_versions
				    SET title=$2, content_json=$3::jsonb, updated_at=now(), updated_by=$4,
				        editors = CASE WHEN $4 = ANY(editors) THEN editors ELSE array_append(editors, $4) END
				  WHERE id=$1`, d.ID, t, contentJSON, actor)
			return err
		}
		p := cur.Published()
		t := ""
		switch {
		case title != nil:
			t = *title
		case p != nil:
			t = p.Title
		}
		if p != nil && p.Same(t, contentJSON) {
			return nil // only a change of title or content makes a draft
		}
		changed = true
		next := 1
		if n := len(cur.Versions); n > 0 {
			next = cur.Versions[n-1].VersionNo + 1
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO policy_appendix_versions
			   (appendix_id, version_no, state, title, content_json, created_by, updated_by, editors)
			 VALUES ($1, $2, 'DRAFT', $3, $4::jsonb, $5, $5, ARRAY[$5]::uuid[])`,
			id, next, t, contentJSON, actor)
		return err
	})
	if err != nil {
		return domain.PolicyAppendix{}, false, err
	}
	return a, changed, nil
}

// Publish promotes the draft to the next published version, with who and when.
// It leaves no draft.
func (s *PolicyAppendixStore) Publish(ctx context.Context, id, actor uuid.UUID) (domain.PolicyAppendix, error) {
	return s.withAppendix(ctx, id, func(tx pgx.Tx, all []domain.PolicyAppendix, at int) error {
		cur := all[at]
		if !cur.Live() {
			return ErrAppendixRetired
		}
		d := cur.Draft()
		if d == nil {
			return ErrAppendixNoDraft
		}
		_, err := tx.Exec(ctx,
			`UPDATE policy_appendix_versions
			    SET state='PUBLISHED', published_at=now(), published_by=$2, updated_at=now(), updated_by=$2,
			        editors = CASE WHEN $2 = ANY(editors) THEN editors ELSE array_append(editors, $2) END
			  WHERE id=$1`, d.ID, actor)
		return err
	})
}

// renumberLive closes the gaps in a policy's live order_index. Rows move to a
// high offset first, so no step collides with the live-order unique index.
func renumberLive(ctx context.Context, tx pgx.Tx, all []domain.PolicyAppendix) error {
	var live []uuid.UUID
	for _, a := range all {
		if a.Live() {
			live = append(live, a.ID) // all is already in live order
		}
	}
	for i, id := range live {
		if _, err := tx.Exec(ctx, `UPDATE policy_owned_appendices SET order_index=$2 WHERE id=$1`, id, 10000+i); err != nil {
			return err
		}
	}
	for i, id := range live {
		if _, err := tx.Exec(ctx, `UPDATE policy_owned_appendices SET order_index=$2 WHERE id=$1`, id, i); err != nil {
			return err
		}
	}
	return nil
}

// Retire hides an appendix and keeps it with its whole history; the live ones
// after it move up a place. It never touches the version table. Retiring a
// retired appendix changes nothing (changed=false).
func (s *PolicyAppendixStore) Retire(ctx context.Context, id, actor uuid.UUID) (domain.PolicyAppendix, bool, error) {
	changed := false
	a, err := s.withAppendix(ctx, id, func(tx pgx.Tx, all []domain.PolicyAppendix, at int) error {
		if !all[at].Live() {
			return nil
		}
		changed = true
		if _, err := tx.Exec(ctx,
			`UPDATE policy_owned_appendices SET retired_at=now(), retired_by=$2 WHERE id=$1`, id, actor); err != nil {
			return err
		}
		rest := slices.Clone(all)
		gone := time.Now()
		rest[at].RetiredAt = &gone // no longer live, so the renumber skips it
		return renumberLive(ctx, tx, rest)
	})
	if err != nil {
		return domain.PolicyAppendix{}, false, err
	}
	return a, changed, nil
}

// Restore brings a retired appendix back as the last live one. Restoring a
// live appendix changes nothing (changed=false).
func (s *PolicyAppendixStore) Restore(ctx context.Context, id uuid.UUID) (domain.PolicyAppendix, bool, error) {
	changed := false
	a, err := s.withAppendix(ctx, id, func(tx pgx.Tx, all []domain.PolicyAppendix, at int) error {
		if all[at].Live() {
			return nil
		}
		changed = true
		live := 0
		for _, o := range all {
			if o.Live() {
				live++
			}
		}
		_, err := tx.Exec(ctx,
			`UPDATE policy_owned_appendices SET retired_at=NULL, retired_by=NULL, order_index=$2 WHERE id=$1`, id, live)
		return err
	})
	if err != nil {
		return domain.PolicyAppendix{}, false, err
	}
	return a, changed, nil
}

// ReorderAppendices sets the order of a policy's live appendices from their
// place in orderedIDs, which must hold every live appendix exactly once; a
// missing, repeated, foreign, retired or unknown id is
// ErrAppendixReorderMismatch and nothing changes. An unknown policy is
// ErrAppendixPolicyNotFound. It runs under the policy lock like every other
// write, leaves retired appendices and every version row alone, and returns
// the live appendices in the new order.
func (s *PolicyAppendixStore) ReorderAppendices(ctx context.Context, policyID uuid.UUID, orderedIDs []uuid.UUID) ([]domain.PolicyAppendix, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if err := lockPolicy(ctx, tx, policyID); err != nil {
			return err
		}
		all, err := loadPolicyAppendices(ctx, tx, policyID)
		if err != nil {
			return err
		}
		live := make(map[uuid.UUID]struct{}, len(all))
		for _, a := range all {
			if a.Live() {
				live[a.ID] = struct{}{}
			}
		}
		if len(orderedIDs) != len(live) {
			return fmt.Errorf("%d ids for %d live appendices: %w", len(orderedIDs), len(live), ErrAppendixReorderMismatch)
		}
		seen := make(map[uuid.UUID]struct{}, len(orderedIDs))
		for _, id := range orderedIDs {
			if _, ok := live[id]; !ok {
				return fmt.Errorf("%s is not a live appendix of the policy: %w", id, ErrAppendixReorderMismatch)
			}
			if _, dup := seen[id]; dup {
				return fmt.Errorf("%s listed twice: %w", id, ErrAppendixReorderMismatch)
			}
			seen[id] = struct{}{}
		}
		// Park the rows at a high offset first so no step collides with the
		// live-order unique index, then set the final places.
		for i, id := range orderedIDs {
			if _, err := tx.Exec(ctx, `UPDATE policy_owned_appendices SET order_index=$2 WHERE id=$1`, id, 10000+i); err != nil {
				return err
			}
		}
		for i, id := range orderedIDs {
			if _, err := tx.Exec(ctx, `UPDATE policy_owned_appendices SET order_index=$2 WHERE id=$1`, id, i); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.List(ctx, policyID, false)
}

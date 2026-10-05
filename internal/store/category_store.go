// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package store holds the Postgres stores for core's domain: categories,
// templates, policies and the content libraries.
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// ErrCategoryNotFound means no category matched the id.
var ErrCategoryNotFound = errors.New("category not found")

// ErrCategorySlugConflict means a sibling category already uses the slug.
var ErrCategorySlugConflict = errors.New("a sibling category with this slug already exists")

// ErrCategoryHasPolicies means the category or a descendant still holds a
// document, so it can't be deleted.
var ErrCategoryHasPolicies = errors.New("category or a subcategory has policies and cannot be deleted")

// ErrCategoryMoveCycle means the new parent is the category itself or one
// of its descendants.
var ErrCategoryMoveCycle = errors.New("cannot move a category under itself or one of its descendants")

// ErrCategoryMoveTooDeep means the move would put a node deeper than
// maxCategoryDepth.
var ErrCategoryMoveTooDeep = errors.New("move would exceed the maximum category depth of 3")

// maxCategoryDepth is the deepest a category may sit: a root is depth 1.
const maxCategoryDepth = 3

// CategoryStore persists domain.Category aggregates against Postgres.
type CategoryStore struct{ db *postgres.DB }

// NewCategoryStore returns a CategoryStore backed by db.
func NewCategoryStore(db *postgres.DB) *CategoryStore { return &CategoryStore{db: db} }

// codeAssignAttempts bounds the retries when a derived code collides, so a
// runaway loop can't wedge a create.
const codeAssignAttempts = 64

// Create inserts a category; a nil ParentID makes a root. Its globally unique
// code is derived from the effective slug path and disambiguated on
// collision. A slug collision returns ErrCategorySlugConflict.
func (s *CategoryStore) Create(ctx context.Context, g domain.Category) (domain.Category, error) {
	var parentID *uuid.UUID
	if g.ParentID != uuid.Nil {
		parentID = &g.ParentID
	}

	// The code comes from the effective slug path, not the bare slug, so two
	// children sharing a segment under different parents get distinct codes.
	parentPath, err := s.parentSlugPath(ctx, g.ParentID)
	if err != nil {
		return domain.Category{}, fmt.Errorf("CategoryStore.Create parent path: %w", err)
	}
	base := effectiveCodeBase(parentPath, g.Slug)
	// Seeding from the codes in use makes the common case succeed first time;
	// the unique constraint catches a concurrent create.
	taken, err := s.existingCodes(ctx)
	if err != nil {
		return domain.Category{}, fmt.Errorf("CategoryStore.Create codes: %w", err)
	}

	for range codeAssignAttempts {
		code := NextFreeCode(base, taken)
		err := s.db.Querier().QueryRow(ctx,
			`INSERT INTO categories (id, parent_id, name, slug, code)
			 VALUES ($1, $2, $3, $4, $5)
			 RETURNING id, created_at, updated_at`,
			g.ID, parentID, g.Name, g.Slug, code,
		).Scan(&g.ID, &g.CreatedAt, &g.UpdatedAt)
		if err == nil {
			return g, nil
		}
		switch constraintName(err) {
		case "categories_code_key":
			// A concurrent create took the code: try the next suffix.
			taken[code] = true
			continue
		case "categories_slug_key", "categories_parent_id_slug_key", "categories_parent_slug_key":
			return domain.Category{}, fmt.Errorf("CategoryStore.Create %s: %w", g.Slug, ErrCategorySlugConflict)
		default:
			return domain.Category{}, fmt.Errorf("CategoryStore.Create: %w", err)
		}
	}
	return domain.Category{}, fmt.Errorf("CategoryStore.Create: exhausted code disambiguation for slug %q", g.Slug)
}

// existingCodes returns the set of category codes already assigned.
func (s *CategoryStore) existingCodes(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.Querier().Query(ctx, `SELECT code FROM categories WHERE code IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	taken := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		taken[c] = true
	}
	return taken, rows.Err()
}

// parentSlugPath returns the effective slug path a parent contributes, or ""
// for a root.
func (s *CategoryStore) parentSlugPath(ctx context.Context, parentID uuid.UUID) (string, error) {
	if parentID == uuid.Nil {
		return "", nil
	}
	chain, err := s.AncestorChain(ctx, parentID)
	if err != nil {
		return "", fmt.Errorf("parentSlugPath: %w", err)
	}
	return domain.EffectiveSlugPath(chain), nil
}

// effectiveCodeBase derives the candidate code from the parent's effective
// slug path and the category's own segment; a root uses its segment alone.
func effectiveCodeBase(parentPath, segment string) string {
	if parentPath == "" {
		return PolicyNumberCode(segment)
	}
	return PolicyNumberCode(parentPath + "-" + segment)
}

// rederiveSubtreeCodes recomputes the codes of a subtree after a rename or
// move changed its effective slug paths. includeRoot re-derives rootID too.
// Nodes go shallowest first so each parent's new path feeds its children.
func (s *CategoryStore) rederiveSubtreeCodes(ctx context.Context, rootID uuid.UUID, includeRoot bool) error {
	rows, err := s.db.Querier().Query(ctx, `
		WITH RECURSIVE tree AS (
		  SELECT id, 0 AS rel_depth FROM categories WHERE id = $1
		  UNION ALL
		  SELECT g.id, t.rel_depth + 1 FROM categories g JOIN tree t ON g.parent_id = t.id
		)
		SELECT id, rel_depth FROM tree ORDER BY rel_depth ASC`, rootID)
	if err != nil {
		return fmt.Errorf("rederiveSubtreeCodes enumerate: %w", err)
	}
	type node struct {
		id  uuid.UUID
		rel int
	}
	var nodes []node
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.id, &n.rel); err != nil {
			rows.Close()
			return fmt.Errorf("rederiveSubtreeCodes scan: %w", err)
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rederiveSubtreeCodes rows: %w", err)
	}
	for _, n := range nodes {
		if n.rel == 0 && !includeRoot {
			continue
		}
		if err := s.rederiveOneCode(ctx, n.id); err != nil {
			return err
		}
	}
	return nil
}

// rederiveOneCode recomputes one category's code with the same
// disambiguation as Create. Its own current code is left out of the taken
// set, so an unchanged path keeps its code.
func (s *CategoryStore) rederiveOneCode(ctx context.Context, id uuid.UUID) error {
	chain, err := s.AncestorChain(ctx, id)
	if err != nil {
		return fmt.Errorf("rederiveOneCode chain %s: %w", id, err)
	}
	if len(chain) == 0 {
		return fmt.Errorf("rederiveOneCode %s: %w", id, ErrCategoryNotFound)
	}
	base := PolicyNumberCode(domain.EffectiveSlugPath(chain))

	var currentCode *string
	if err := s.db.Querier().QueryRow(ctx, `SELECT code FROM categories WHERE id = $1`, id).Scan(&currentCode); err != nil {
		return fmt.Errorf("rederiveOneCode read %s: %w", id, err)
	}
	taken, err := s.existingCodes(ctx)
	if err != nil {
		return fmt.Errorf("rederiveOneCode codes: %w", err)
	}
	if currentCode != nil {
		delete(taken, *currentCode)
	}

	for range codeAssignAttempts {
		code := NextFreeCode(base, taken)
		tag, err := s.db.Querier().Exec(ctx,
			`UPDATE categories SET code = $2, updated_at = now() WHERE id = $1`, id, code)
		if err == nil {
			if tag.RowsAffected() == 0 {
				return fmt.Errorf("rederiveOneCode %s: %w", id, ErrCategoryNotFound)
			}
			return nil
		}
		if constraintName(err) == "categories_code_key" {
			taken[code] = true
			continue
		}
		return fmt.Errorf("rederiveOneCode update %s: %w", id, err)
	}
	return fmt.Errorf("rederiveOneCode %s: exhausted code disambiguation", id)
}

// Get fetches a category by id. NULL ids come back as uuid.Nil.
func (s *CategoryStore) Get(ctx context.Context, id uuid.UUID) (domain.Category, error) {
	var (
		g                 domain.Category
		parentID          *uuid.UUID
		templateID        *uuid.UUID
		workflowID        *uuid.UUID
		owners            pgtype.FlatArray[uuid.UUID]
		audienceGroupIDs  pgtype.FlatArray[string]
		audienceGroupSet  bool // true when the DB column is non-NULL
		ackTriggers       string
		reviewCad         string
		reviewDate        *time.Time
		exclusionGroupIDs pgtype.FlatArray[string]
		exclusionSet      bool // true when the DB column is non-NULL
		ackEveryone       bool
		ackEveryoneSet    bool // true when the DB column is non-NULL
	)
	err := s.db.Querier().QueryRow(ctx,
		`SELECT id, parent_id, name, slug,
		        default_template_id, default_template_none, default_workflow_id,
		        owners,
		        audience_group_ids IS NOT NULL, COALESCE(audience_group_ids, '{}'),
		        ack_triggers, review_cadence, review_date,
		        exclusion_group_ids IS NOT NULL, COALESCE(exclusion_group_ids, '{}'),
		        ack_everyone IS NOT NULL, COALESCE(ack_everyone, false),
		        created_at, updated_at
		 FROM categories WHERE id = $1`, id,
	).Scan(&g.ID, &parentID, &g.Name, &g.Slug,
		&templateID, &g.DefaultTemplateNone, &workflowID,
		&owners,
		&audienceGroupSet, &audienceGroupIDs,
		&ackTriggers, &reviewCad, &reviewDate,
		&exclusionSet, &exclusionGroupIDs,
		&ackEveryoneSet, &ackEveryone,
		&g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return domain.Category{}, fmt.Errorf("CategoryStore.Get: %w", err)
	}
	if parentID != nil {
		g.ParentID = *parentID
	}
	if templateID != nil {
		g.DefaultTemplateID = *templateID
	}
	if workflowID != nil {
		g.DefaultWorkflowID = *workflowID
	}
	g.Owners = []uuid.UUID(owners)
	if audienceGroupSet {
		ids := []string(audienceGroupIDs)
		g.AudienceGroupIDs = &ids
	}
	g.AckTriggers = domain.AckTrigger(ackTriggers)
	g.ReviewCadence = domain.ReviewCadence(reviewCad)
	g.ReviewDate = reviewDate
	if exclusionSet {
		ids := []string(exclusionGroupIDs)
		g.ExclusionGroupIDs = &ids
	}
	if ackEveryoneSet {
		v := ackEveryone
		g.AckEveryone = &v
	}
	return g, nil
}

// AncestorChain returns categoryID and its ancestors, leaf first, in one
// recursive query.
func (s *CategoryStore) AncestorChain(ctx context.Context, categoryID uuid.UUID) ([]domain.Category, error) {
	rows, err := s.db.Querier().Query(ctx, `
		WITH RECURSIVE chain AS (
		  SELECT id, parent_id, name, slug,
		         default_template_id, default_template_none, default_workflow_id,
		         owners, audience_group_ids, ack_triggers, review_cadence, review_date,
		         exclusion_group_ids, ack_everyone,
		         created_at, updated_at, 0 AS depth
		  FROM categories WHERE id = $1
		  UNION ALL
		  SELECT g.id, g.parent_id, g.name, g.slug,
		         g.default_template_id, g.default_template_none, g.default_workflow_id,
		         g.owners, g.audience_group_ids, g.ack_triggers, g.review_cadence, g.review_date,
		         g.exclusion_group_ids, g.ack_everyone,
		         g.created_at, g.updated_at, c.depth + 1
		  FROM categories g JOIN chain c ON g.id = c.parent_id
		)
		SELECT id, parent_id, name, slug,
		       default_template_id, default_template_none, default_workflow_id,
		       owners,
		       audience_group_ids IS NOT NULL, COALESCE(audience_group_ids, '{}'),
		       ack_triggers, review_cadence, review_date,
		       exclusion_group_ids IS NOT NULL, COALESCE(exclusion_group_ids, '{}'),
		       ack_everyone IS NOT NULL, COALESCE(ack_everyone, false),
		       created_at, updated_at
		FROM chain
		ORDER BY depth ASC`, categoryID)
	if err != nil {
		return nil, fmt.Errorf("CategoryStore.AncestorChain: %w", err)
	}
	defer rows.Close()
	return scanCategories(rows)
}

// ListChildren returns the direct children of parentID; uuid.Nil lists the
// roots.
func (s *CategoryStore) ListChildren(ctx context.Context, parentID uuid.UUID) ([]domain.Category, error) {
	const base = `SELECT id, parent_id, name, slug,
	                     default_template_id, default_template_none, default_workflow_id,
	                     owners,
	                     audience_group_ids IS NOT NULL, COALESCE(audience_group_ids, '{}'),
	                     ack_triggers, review_cadence, review_date,
	                     exclusion_group_ids IS NOT NULL, COALESCE(exclusion_group_ids, '{}'),
	                     ack_everyone IS NOT NULL, COALESCE(ack_everyone, false),
	                     created_at, updated_at
	              FROM categories `
	var (
		query string
		args  []any
	)
	if parentID == uuid.Nil {
		query = base + `WHERE parent_id IS NULL ORDER BY slug`
	} else {
		query = base + `WHERE parent_id = $1 ORDER BY slug`
		args = append(args, parentID)
	}
	rows, err := s.db.Querier().Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("CategoryStore.ListChildren: %w", err)
	}
	defer rows.Close()
	return scanCategories(rows)
}

// SetDefaults sets the default template and workflow; uuid.Nil clears one.
// With templateNone the default is freeform and the template id is stored as
// NULL, so the two can't disagree.
func (s *CategoryStore) SetDefaults(ctx context.Context, id uuid.UUID, templateID, workflowID uuid.UUID, templateNone bool) (domain.Category, error) {
	var templatePtr, workflowPtr *uuid.UUID
	if templateID != uuid.Nil && !templateNone {
		templatePtr = &templateID
	}
	if workflowID != uuid.Nil {
		workflowPtr = &workflowID
	}
	_, err := s.db.Querier().Exec(ctx,
		`UPDATE categories
		    SET default_template_id = $2,
		        default_template_none = $3,
		        default_workflow_id = $4,
		        updated_at = now()
		  WHERE id = $1`,
		id, templatePtr, templateNone, workflowPtr)
	if err != nil {
		return domain.Category{}, fmt.Errorf("CategoryStore.SetDefaults: %w", err)
	}
	return s.Get(ctx, id)
}

// Rename changes a category's name and slug segment, then re-derives the
// codes of the category and its descendants. Document numbers are derived
// from the code on read, so the rename reaches every document in the subtree
// while each document keeps its sequence.
//
// It runs without a wrapping transaction, like Create: a failed UPDATE would
// poison a transaction, and the code retry relies on failing cleanly. The
// unique constraint on the code is the backstop.
func (s *CategoryStore) Rename(ctx context.Context, id uuid.UUID, name, slug string) (domain.Category, error) {
	tag, err := s.db.Querier().Exec(ctx,
		`UPDATE categories SET name = $2, slug = $3, updated_at = now() WHERE id = $1`,
		id, name, slug)
	if err != nil {
		switch constraintName(err) {
		case "categories_slug_key", "categories_parent_id_slug_key", "categories_parent_slug_key":
			return domain.Category{}, fmt.Errorf("CategoryStore.Rename %s: %w", id, ErrCategorySlugConflict)
		default:
			return domain.Category{}, fmt.Errorf("CategoryStore.Rename: %w", err)
		}
	}
	if tag.RowsAffected() == 0 {
		return domain.Category{}, fmt.Errorf("CategoryStore.Rename %s: %w", id, ErrCategoryNotFound)
	}

	if err := s.rederiveSubtreeCodes(ctx, id, true); err != nil {
		return domain.Category{}, fmt.Errorf("CategoryStore.Rename rederive: %w", err)
	}
	return s.Get(ctx, id)
}

// Delete removes a category and its descendants, only when none of them holds
// a document; otherwise it returns ErrCategoryHasPolicies. The check and the
// delete share one transaction so a concurrent create can't slip a document
// in between.
func (s *CategoryStore) Delete(ctx context.Context, id uuid.UUID) error {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var err error

		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM categories WHERE id = $1)`, id).Scan(&exists); err != nil {
			return fmt.Errorf("CategoryStore.Delete exists: %w", err)
		}
		if !exists {
			return fmt.Errorf("CategoryStore.Delete %s: %w", id, ErrCategoryNotFound)
		}

		var hasPolicies bool
		err = tx.QueryRow(ctx, `
			WITH RECURSIVE tree AS (
			  SELECT id FROM categories WHERE id = $1
			  UNION ALL
			  SELECT g.id FROM categories g JOIN tree t ON g.parent_id = t.id
			)
			SELECT EXISTS (
			  SELECT 1 FROM policies WHERE home_category_id IN (SELECT id FROM tree)
			)`, id).Scan(&hasPolicies)
		if err != nil {
			return fmt.Errorf("CategoryStore.Delete guard: %w", err)
		}
		if hasPolicies {
			return fmt.Errorf("CategoryStore.Delete %s: %w", id, ErrCategoryHasPolicies)
		}

		// Deepest first: parent_id is ON DELETE RESTRICT.
		_, err = tx.Exec(ctx, `
			WITH RECURSIVE tree AS (
			  SELECT id, 0 AS depth FROM categories WHERE id = $1
			  UNION ALL
			  SELECT g.id, t.depth + 1 FROM categories g JOIN tree t ON g.parent_id = t.id
			)
			DELETE FROM categories
			 WHERE id IN (SELECT id FROM tree)`, id)
		if err != nil {
			return fmt.Errorf("CategoryStore.Delete delete: %w", err)
		}

		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// MoveCategory re-parents a category and its subtree; a nil newParentID makes
// it a root. Documents keep their home category and governance is resolved
// live from the ancestor chain, so only one row changes.
//
// The moved subtree and the new parent's ancestor chain are locked FOR UPDATE
// before the cycle and depth checks, so no concurrent edit or move can
// interleave. It returns ErrCategoryMoveCycle, ErrCategoryMoveTooDeep or
// ErrCategoryNotFound, the refreshed category and the subtree size.
func (s *CategoryStore) MoveCategory(ctx context.Context, categoryID uuid.UUID, newParentID *uuid.UUID) (domain.Category, int, error) {
	subtreeIDs := map[uuid.UUID]struct{}{}
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		clear(subtreeIDs)

		// Lock the subtree and read each node's depth below the moved root.
		rows, err := tx.Query(ctx, `
			WITH RECURSIVE tree AS (
			  SELECT id, 0 AS rel_depth FROM categories WHERE id = $1
			  UNION ALL
			  SELECT g.id, t.rel_depth + 1 FROM categories g JOIN tree t ON g.parent_id = t.id
			)
			SELECT id, rel_depth FROM tree FOR UPDATE`, categoryID)
		if err != nil {
			return fmt.Errorf("CategoryStore.MoveCategory subtree: %w", err)
		}
		maxRelDepth := 0
		for rows.Next() {
			var id uuid.UUID
			var rel int
			if err := rows.Scan(&id, &rel); err != nil {
				rows.Close()
				return fmt.Errorf("CategoryStore.MoveCategory scan subtree: %w", err)
			}
			subtreeIDs[id] = struct{}{}
			if rel > maxRelDepth {
				maxRelDepth = rel
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("CategoryStore.MoveCategory subtree rows: %w", err)
		}
		if _, ok := subtreeIDs[categoryID]; !ok {
			return fmt.Errorf("CategoryStore.MoveCategory %s: %w", categoryID, ErrCategoryNotFound)
		}
		heightOfMovedSubtree := maxRelDepth + 1

		// The new parent's depth, under the same lock; 0 for a root.
		newParentDepth := 0
		if newParentID != nil {
			if _, inSubtree := subtreeIDs[*newParentID]; inSubtree {
				return fmt.Errorf("CategoryStore.MoveCategory %s -> %s: %w", categoryID, *newParentID, ErrCategoryMoveCycle)
			}
			prows, err := tx.Query(ctx, `
				WITH RECURSIVE chain AS (
				  SELECT id, parent_id, 1 AS depth FROM categories WHERE id = $1
				  UNION ALL
				  SELECT g.id, g.parent_id, c.depth + 1
				  FROM categories g JOIN chain c ON g.id = c.parent_id
				)
				SELECT id, depth FROM chain ORDER BY depth DESC FOR UPDATE`, *newParentID)
			if err != nil {
				return fmt.Errorf("CategoryStore.MoveCategory parent chain: %w", err)
			}
			parentFound := false
			for prows.Next() {
				var id uuid.UUID
				var depth int
				if err := prows.Scan(&id, &depth); err != nil {
					prows.Close()
					return fmt.Errorf("CategoryStore.MoveCategory scan parent chain: %w", err)
				}
				if id == *newParentID {
					newParentDepth = depth
					parentFound = true
				}
			}
			prows.Close()
			if err := prows.Err(); err != nil {
				return fmt.Errorf("CategoryStore.MoveCategory parent chain rows: %w", err)
			}
			if !parentFound {
				return fmt.Errorf("CategoryStore.MoveCategory parent %s: %w", *newParentID, ErrCategoryNotFound)
			}
		}

		if newParentDepth+heightOfMovedSubtree > maxCategoryDepth {
			return fmt.Errorf(
				"CategoryStore.MoveCategory %s: new depth %d exceeds max %d: %w",
				categoryID, newParentDepth+heightOfMovedSubtree, maxCategoryDepth, ErrCategoryMoveTooDeep)
		}

		var parentParam *uuid.UUID
		if newParentID != nil {
			parentParam = newParentID
		}
		if _, err := tx.Exec(ctx,
			`UPDATE categories SET parent_id = $2, updated_at = now() WHERE id = $1`,
			categoryID, parentParam,
		); err != nil {
			if isUniqueViolation(err) {
				return fmt.Errorf("CategoryStore.MoveCategory %s: %w", categoryID, ErrCategorySlugConflict)
			}
			return fmt.Errorf("CategoryStore.MoveCategory update: %w", err)
		}

		return nil
	})
	if err != nil {
		return domain.Category{}, 0, err
	}

	// The codes are re-derived after the transaction, like Rename, so a code
	// retry can't poison the move.
	if err := s.rederiveSubtreeCodes(ctx, categoryID, true); err != nil {
		return domain.Category{}, 0, fmt.Errorf("CategoryStore.MoveCategory rederive: %w", err)
	}

	g, err := s.Get(ctx, categoryID)
	if err != nil {
		return domain.Category{}, 0, fmt.Errorf("CategoryStore.MoveCategory reload: %w", err)
	}
	return g, len(subtreeIDs), nil
}

// SetGovernance writes a category's governance columns and returns the
// refreshed category. A nil audience, exclusion or ackEveryone pointer stores
// NULL (inherit); a non-nil one, even an empty slice, is an explicit value.
func (s *CategoryStore) SetGovernance(
	ctx context.Context,
	id uuid.UUID,
	owners []uuid.UUID,
	audienceGroupIDs *[]string,
	ack domain.AckTrigger,
	cadence domain.ReviewCadence,
	reviewDate *time.Time,
	exclusionGroupIDs *[]string,
	ackEveryone *bool,
) (domain.Category, error) {
	if owners == nil {
		owners = []uuid.UUID{}
	}
	var audienceGroupParam any
	if audienceGroupIDs != nil {
		audienceGroupParam = pgtype.FlatArray[string](*audienceGroupIDs)
	}
	var exclusionParam any
	if exclusionGroupIDs != nil {
		exclusionParam = pgtype.FlatArray[string](*exclusionGroupIDs)
	}
	var ackEveryoneParam any
	if ackEveryone != nil {
		ackEveryoneParam = *ackEveryone
	}
	_, err := s.db.Querier().Exec(ctx,
		`UPDATE categories
		    SET owners              = $2,
		        audience_group_ids        = $3,
		        ack_triggers        = $4,
		        review_cadence      = $5,
		        review_date         = $6,
		        exclusion_group_ids = $7,
		        ack_everyone        = $8,
		        updated_at          = now()
		  WHERE id = $1`,
		id,
		pgtype.FlatArray[uuid.UUID](owners),
		audienceGroupParam,
		string(ack),
		string(cadence),
		reviewDate,
		exclusionParam,
		ackEveryoneParam,
	)
	if err != nil {
		return domain.Category{}, fmt.Errorf("CategoryStore.SetGovernance: %w", err)
	}
	return s.Get(ctx, id)
}

// Subtree returns a category and all its descendants, in no set order.
func (s *CategoryStore) Subtree(ctx context.Context, rootID uuid.UUID) ([]domain.Category, error) {
	rows, err := s.db.Querier().Query(ctx, `
		WITH RECURSIVE tree AS (
		  SELECT id, parent_id, name, slug,
		         default_template_id, default_template_none, default_workflow_id,
		         owners, audience_group_ids, ack_triggers, review_cadence, review_date,
		         exclusion_group_ids, ack_everyone,
		         created_at, updated_at
		  FROM categories WHERE id = $1
		  UNION ALL
		  SELECT g.id, g.parent_id, g.name, g.slug,
		         g.default_template_id, g.default_template_none, g.default_workflow_id,
		         g.owners, g.audience_group_ids, g.ack_triggers, g.review_cadence, g.review_date,
		         g.exclusion_group_ids, g.ack_everyone,
		         g.created_at, g.updated_at
		  FROM categories g JOIN tree t ON g.parent_id = t.id
		)
		SELECT id, parent_id, name, slug,
		       default_template_id, default_template_none, default_workflow_id,
		       owners,
		       audience_group_ids IS NOT NULL, COALESCE(audience_group_ids, '{}'),
		       ack_triggers, review_cadence, review_date,
		       exclusion_group_ids IS NOT NULL, COALESCE(exclusion_group_ids, '{}'),
		       ack_everyone IS NOT NULL, COALESCE(ack_everyone, false),
		       created_at, updated_at
		FROM tree`, rootID)
	if err != nil {
		return nil, fmt.Errorf("CategoryStore.Subtree: %w", err)
	}
	defer rows.Close()
	return scanCategories(rows)
}

// GetCategoryRuleset returns a category's rules in order; never nil.
func (s *CategoryStore) GetCategoryRuleset(ctx context.Context, categoryID uuid.UUID) ([]domain.CategoryRule, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT ordinal, subject_kind, subject_ref, grant_read, grant_ack, grant_approve, grant_author
		FROM category_rules WHERE category_id = $1 ORDER BY ordinal`, categoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.CategoryRule
	for rows.Next() {
		var r domain.CategoryRule
		if err := rows.Scan(&r.Ordinal, &r.SubjectKind, &r.SubjectRef, &r.Read, &r.Ack, &r.Approve, &r.Author); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if out == nil {
		out = []domain.CategoryRule{}
	}
	return out, nil
}

// SetCategoryRuleset replaces a category's rules, numbering them from 1, and
// returns what was stored.
func (s *CategoryStore) SetCategoryRuleset(ctx context.Context, categoryID uuid.UUID, rules []domain.CategoryRule) ([]domain.CategoryRule, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM category_rules WHERE category_id = $1`, categoryID); err != nil {
			return err
		}
		for i, r := range rules {
			if _, err := tx.Exec(ctx, `
				INSERT INTO category_rules
				  (category_id, ordinal, subject_kind, subject_ref, grant_read, grant_ack, grant_approve, grant_author)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				categoryID, i+1, r.SubjectKind, r.SubjectRef, r.Read, r.Ack, r.Approve, r.Author); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetCategoryRuleset(ctx, categoryID)
}

// scanCategories reads rows in the canonical category column order,
// normalising NULL ids to uuid.Nil and NULL governance arrays to nil.
func scanCategories(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]domain.Category, error) {
	var out []domain.Category
	for rows.Next() {
		var (
			g                 domain.Category
			parentID          *uuid.UUID
			templateID        *uuid.UUID
			workflowID        *uuid.UUID
			owners            pgtype.FlatArray[uuid.UUID]
			audienceGroupSet  bool
			audienceGroupIDs  pgtype.FlatArray[string]
			ackTriggers       string
			reviewCad         string
			reviewDate        *time.Time
			exclusionSet      bool
			exclusionGroupIDs pgtype.FlatArray[string]
			ackEveryone       bool
			ackEveryoneSet    bool
		)
		if err := rows.Scan(&g.ID, &parentID, &g.Name, &g.Slug,
			&templateID, &g.DefaultTemplateNone, &workflowID,
			&owners,
			&audienceGroupSet, &audienceGroupIDs,
			&ackTriggers, &reviewCad, &reviewDate,
			&exclusionSet, &exclusionGroupIDs,
			&ackEveryoneSet, &ackEveryone,
			&g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, err
		}
		if parentID != nil {
			g.ParentID = *parentID
		}
		if templateID != nil {
			g.DefaultTemplateID = *templateID
		}
		if workflowID != nil {
			g.DefaultWorkflowID = *workflowID
		}
		g.Owners = []uuid.UUID(owners)
		if audienceGroupSet {
			ids := []string(audienceGroupIDs)
			g.AudienceGroupIDs = &ids
		}
		g.AckTriggers = domain.AckTrigger(ackTriggers)
		g.ReviewCadence = domain.ReviewCadence(reviewCad)
		g.ReviewDate = reviewDate
		if exclusionSet {
			ids := []string(exclusionGroupIDs)
			g.ExclusionGroupIDs = &ids
		}
		if ackEveryoneSet {
			v := ackEveryone
			g.AckEveryone = &v
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// PurgedCategoryRule is a rule a purge removed, or would remove on a dry run,
// with its category.
type PurgedCategoryRule struct {
	CategoryID uuid.UUID
	Rule       domain.CategoryRule
}

// purgeUserRuleColumns is shared by the preview and the delete, so both report
// the same rows.
const purgeUserRuleColumns = `category_id, ordinal, subject_kind, subject_ref,
	grant_read, grant_ack, grant_approve, grant_author`

// PurgeUserCategoryRules removes every user-subject rule naming userID, across
// all categories, in one transaction, and returns what it removed.
//
// Group and everyone rules stay even when their subject_ref equals the id, and
// nothing else is touched: owners, authorship, approvals and audit keep naming
// the user, because a deleted account doesn't rewrite history.
//
// The surviving rules are renumbered 1..n, keeping their precedence. A gap
// would make the next full replace look like a reorder of every rule below
// it, and each reorder is audited as an access change.
//
// dryRun reads the same rows and writes nothing. A second call removes
// nothing and returns an empty slice.
func (s *CategoryStore) PurgeUserCategoryRules(ctx context.Context, userID uuid.UUID, dryRun bool) ([]PurgedCategoryRule, error) {
	if dryRun {
		rows, err := s.db.Querier().Query(ctx, `
			SELECT `+purgeUserRuleColumns+`
			FROM category_rules
			WHERE subject_kind = 'user' AND subject_ref = $1
			ORDER BY category_id, ordinal`, userID.String())
		if err != nil {
			return nil, fmt.Errorf("CategoryStore.PurgeUserCategoryRules preview: %w", err)
		}
		defer rows.Close()
		return scanPurgedRules(rows)
	}

	var removed []PurgedCategoryRule
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			DELETE FROM category_rules
			WHERE subject_kind = 'user' AND subject_ref = $1
			RETURNING `+purgeUserRuleColumns, userID.String())
		if err != nil {
			return fmt.Errorf("CategoryStore.PurgeUserCategoryRules delete: %w", err)
		}
		removed, err = scanPurgedRules(rows)
		rows.Close()
		if err != nil {
			return err
		}
		if len(removed) == 0 {
			return nil
		}

		// DELETE ... RETURNING has no order; audit events must come out stable.
		sort.Slice(removed, func(i, j int) bool {
			if removed[i].CategoryID != removed[j].CategoryID {
				return removed[i].CategoryID.String() < removed[j].CategoryID.String()
			}
			return removed[i].Rule.Ordinal < removed[j].Rule.Ordinal
		})

		affected := make([]uuid.UUID, 0, len(removed))
		for _, r := range removed {
			if !slices.Contains(affected, r.CategoryID) {
				affected = append(affected, r.CategoryID)
			}
		}

		// The unique (category_id, ordinal) constraint isn't deferrable, so the
		// survivors are parked in the negative range first, then rewritten to
		// 1..n; ordering by the parked ordinal DESC keeps their precedence.
		if _, err := tx.Exec(ctx, `
			UPDATE category_rules SET ordinal = -ordinal
			WHERE category_id = ANY($1) AND ordinal > 0`, affected); err != nil {
			return fmt.Errorf("CategoryStore.PurgeUserCategoryRules park ordinals: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE category_rules cr SET ordinal = t.rn
			FROM (
				SELECT id, row_number() OVER (PARTITION BY category_id ORDER BY ordinal DESC) AS rn
				FROM category_rules WHERE category_id = ANY($1)
			) t
			WHERE cr.id = t.id`, affected); err != nil {
			return fmt.Errorf("CategoryStore.PurgeUserCategoryRules renumber: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

// scanPurgedRules reads purgeUserRuleColumns rows.
func scanPurgedRules(rows pgx.Rows) ([]PurgedCategoryRule, error) {
	out := []PurgedCategoryRule{}
	for rows.Next() {
		var p PurgedCategoryRule
		if err := rows.Scan(&p.CategoryID, &p.Rule.Ordinal, &p.Rule.SubjectKind, &p.Rule.SubjectRef,
			&p.Rule.Read, &p.Rule.Ack, &p.Rule.Approve, &p.Rule.Author); err != nil {
			return nil, fmt.Errorf("CategoryStore.PurgeUserCategoryRules scan: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("CategoryStore.PurgeUserCategoryRules rows: %w", err)
	}
	return out, nil
}

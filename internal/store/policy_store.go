// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// ErrDraftAlreadyExists means the policy already has a draft (the partial
// unique index policy_versions_one_draft_idx).
var ErrDraftAlreadyExists = errors.New("a draft policy version already exists for this policy")

// ErrNoDraft means DiscardDraft found no draft to discard.
var ErrNoDraft = errors.New("policy has no draft to discard")

// ErrHasPublishedVersion means DeletePolicy refused a policy that has, or
// had, a published version.
var ErrHasPublishedVersion = errors.New("policy has a published version and cannot be deleted")

// pgUniqueViolation is the SQLSTATE of a unique violation.
const pgUniqueViolation = "23505"

// PolicyStore persists policies and their versions: per-category numbering,
// at most one draft per policy, publishing, and the template-update flag.
type PolicyStore struct{ db *postgres.DB }

// NewPolicyStore returns a PolicyStore backed by db.
func NewPolicyStore(db *postgres.DB) *PolicyStore { return &PolicyStore{db: db} }

// categoryNumberCode returns the category's code inside tx, or a candidate
// derived from the slug when the code is NULL.
func categoryNumberCode(ctx context.Context, tx pgx.Tx, categoryID uuid.UUID) (string, error) {
	var code *string
	var slug string
	if err := tx.QueryRow(ctx,
		`SELECT code, slug FROM categories WHERE id = $1`, categoryID,
	).Scan(&code, &slug); err != nil {
		return "", err
	}
	return numberCodeSegment(code, slug), nil
}

// numberCodeSegment is the read-path counterpart of categoryNumberCode, for
// columns already scanned.
func numberCodeSegment(code *string, slug string) string {
	if code != nil && *code != "" {
		return *code
	}
	return PolicyNumberCode(slug)
}

// initialDraftContent matches the policy_versions.content column default.
const initialDraftContent = "{}"

// CreatePolicy inserts a policy, takes the next number from its category's
// counter and seeds an empty freeform draft, all in one transaction, so a new
// policy can be submitted without a first save. The draft pins no template
// version until the author saves content. The returned policy carries its
// derived number and the draft's id.
func (s *PolicyStore) CreatePolicy(ctx context.Context, p domain.Policy) (domain.Policy, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var err error

		code, err := categoryNumberCode(ctx, tx, p.HomeCategoryID)
		if err != nil {
			return fmt.Errorf("PolicyStore.CreatePolicy code: %w", err)
		}

		// Pin the default here so the counter key, the number and the row agree.
		if p.DocumentType == "" {
			p.DocumentType = domain.DocumentTypePolicy
		}

		// Procedures number apart from policies, so the counter is keyed by
		// category and document type. The upsert avoids a SELECT-then-UPDATE race.
		var seq int
		err = tx.QueryRow(ctx,
			`INSERT INTO category_policy_seq (category_id, document_type, last_seq) VALUES ($1, $2, 1)
			 ON CONFLICT (category_id, document_type) DO UPDATE
			   SET last_seq = category_policy_seq.last_seq + 1
			 RETURNING last_seq`,
			p.HomeCategoryID, string(p.DocumentType),
		).Scan(&seq)
		if err != nil {
			return fmt.Errorf("PolicyStore.CreatePolicy seq: %w", err)
		}

		// Only the sequence is stored; the number is derived on read.
		p.Sequence = seq
		p.Number = renderDocNumber(p.DocumentType, code, seq)

		// With TemplateNone the id is stored as NULL, so the two can't disagree.
		var templatePtr *uuid.UUID
		if p.TemplateID != uuid.Nil && !p.TemplateNone {
			templatePtr = &p.TemplateID
		}

		err = tx.QueryRow(ctx,
			`INSERT INTO policies (id, home_category_id, sequence, document_type, title, sensitivity, owner_user_id, template_id, template_none)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			 RETURNING created_at, updated_at`,
			p.ID, p.HomeCategoryID, p.Sequence, string(p.DocumentType), p.Title, string(p.Sensitivity), p.OwnerUserID, templatePtr, p.TemplateNone,
		).Scan(&p.CreatedAt, &p.UpdatedAt)
		if err != nil {
			return fmt.Errorf("PolicyStore.CreatePolicy insert: %w", err)
		}

		// A policy without a draft must never exist, so the draft is seeded in
		// the same transaction.
		draftID := uuid.New()
		err = tx.QueryRow(ctx,
			`INSERT INTO policy_versions
			     (id, policy_id, version_no, status, template_version_id, content, created_by)
			 VALUES ($1, $2, 0, 'draft', NULL, $3::jsonb, $4)
			 RETURNING id`,
			draftID, p.ID, initialDraftContent, p.OwnerUserID,
		).Scan(&draftID)
		if err != nil {
			return fmt.Errorf("PolicyStore.CreatePolicy seed draft: %w", err)
		}
		p.CurrentDraftVersionID = draftID

		return nil
	})
	if err != nil {
		return domain.Policy{}, err
	}
	return p, nil
}

// SetOwner reassigns a policy's owner. Returns pgx.ErrNoRows if the policy is
// absent.
func (s *PolicyStore) SetOwner(ctx context.Context, policyID, ownerUserID uuid.UUID) (domain.Policy, error) {
	_, err := s.db.Querier().Exec(ctx,
		`UPDATE policies SET owner_user_id = $2, updated_at = now() WHERE id = $1`,
		policyID, ownerUserID)
	if err != nil {
		return domain.Policy{}, fmt.Errorf("PolicyStore.SetOwner: %w", err)
	}
	return s.GetPolicy(ctx, policyID)
}

// SetSensitivity changes a policy's classification in place, with no new
// version and no approval. Returns pgx.ErrNoRows if the policy is absent.
func (s *PolicyStore) SetSensitivity(ctx context.Context, policyID uuid.UUID, sensitivity domain.Sensitivity) (domain.Policy, error) {
	_, err := s.db.Querier().Exec(ctx,
		`UPDATE policies SET sensitivity = $2, updated_at = now() WHERE id = $1`,
		policyID, string(sensitivity))
	if err != nil {
		return domain.Policy{}, fmt.Errorf("PolicyStore.SetSensitivity: %w", err)
	}
	return s.GetPolicy(ctx, policyID)
}

// SetTitle renames a never-published policy in place. Returns pgx.ErrNoRows if
// the policy is absent.
func (s *PolicyStore) SetTitle(ctx context.Context, policyID uuid.UUID, title string) (domain.Policy, error) {
	_, err := s.db.Querier().Exec(ctx,
		`UPDATE policies SET title = $2, updated_at = now() WHERE id = $1`,
		policyID, title)
	if err != nil {
		return domain.Policy{}, fmt.Errorf("PolicyStore.SetTitle: %w", err)
	}
	return s.GetPolicy(ctx, policyID)
}

// SetProposedTitle stages (or, with nil, clears) a rename on a draft version;
// policies.title stays as it is until the draft is published. It fails when
// the version is missing or isn't a draft.
func (s *PolicyStore) SetProposedTitle(ctx context.Context, policyVersionID uuid.UUID, proposedTitle *string) error {
	tag, err := s.db.Querier().Exec(ctx,
		`UPDATE policy_versions SET proposed_title = $2 WHERE id = $1 AND status = 'draft'`,
		policyVersionID, proposedTitle,
	)
	if err != nil {
		return fmt.Errorf("PolicyStore.SetProposedTitle: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("PolicyStore.SetProposedTitle: draft %s not found or not in draft status", policyVersionID)
	}
	return nil
}

// SetHomeCategory moves a policy to targetCategoryID with a fresh sequence from
// that category's counter, since uniqueness is per category and reusing the
// old sequence could collide. The old counter keeps its gap; sequences are
// never reused. Returns pgx.ErrNoRows if the policy is absent.
func (s *PolicyStore) SetHomeCategory(ctx context.Context, policyID, targetCategoryID uuid.UUID) (domain.Policy, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		// The fresh sequence comes from the target's counter for the document's
		// own type, so policies and procedures never collide.
		var docType string
		if err := tx.QueryRow(ctx,
			`SELECT document_type FROM policies WHERE id = $1`, policyID,
		).Scan(&docType); err != nil {
			return fmt.Errorf("PolicyStore.SetHomeCategory doc type: %w", err)
		}

		var seq int
		if err := tx.QueryRow(ctx,
			`INSERT INTO category_policy_seq (category_id, document_type, last_seq) VALUES ($1, $2, 1)
			 ON CONFLICT (category_id, document_type) DO UPDATE
			   SET last_seq = category_policy_seq.last_seq + 1
			 RETURNING last_seq`,
			targetCategoryID, docType,
		).Scan(&seq); err != nil {
			return fmt.Errorf("PolicyStore.SetHomeCategory seq: %w", err)
		}

		ct, err := tx.Exec(ctx,
			`UPDATE policies SET home_category_id = $2, sequence = $3, updated_at = now() WHERE id = $1`,
			policyID, targetCategoryID, seq)
		if err != nil {
			return fmt.Errorf("PolicyStore.SetHomeCategory update: %w", err)
		}
		if ct.RowsAffected() == 0 {
			return fmt.Errorf("PolicyStore.SetHomeCategory %s: %w", policyID, pgx.ErrNoRows)
		}
		return nil
	})
	if err != nil {
		return domain.Policy{}, err
	}
	return s.GetPolicy(ctx, policyID)
}

// GetPolicy fetches a policy by id. NULL ids come back as uuid.Nil and a NULL
// ack_triggers as nil (inherit from the category).
func (s *PolicyStore) GetPolicy(ctx context.Context, id uuid.UUID) (domain.Policy, error) {
	var (
		p                    domain.Policy
		sensitivity          string
		docType              string
		categoryCode         *string
		categorySlug         string
		templateID           *uuid.UUID
		currentPubVID        *uuid.UUID
		currentDraftVID      *uuid.UUID
		ackTriggersRaw       *string
		ackAudienceOverride  pgtype.FlatArray[uuid.UUID]
		effectiveDate        pgtype.Date
		retiredAt            *time.Time
		currentVersionNo     *int
		currentVersionStatus *string
	)
	err := s.db.Querier().QueryRow(ctx,
		`SELECT p.id, p.home_category_id, p.sequence, p.document_type, g.code, g.slug, p.title, p.sensitivity,
		        p.template_id, p.template_none, p.current_published_version_id,
		        (SELECT id FROM policy_versions WHERE policy_id = p.id AND status = 'draft' LIMIT 1),
		        p.owner_user_id,
		        p.ack_triggers, p.ack_audience_override, p.effective_date,
		        p.created_at, p.updated_at, p.retired_at,
		        cv.version_no, cv.status
		   FROM policies p JOIN categories g ON g.id = p.home_category_id
		   LEFT JOIN policy_versions cv ON cv.id = COALESCE(p.current_published_version_id, (SELECT id FROM policy_versions WHERE policy_id = p.id AND status = 'draft' LIMIT 1))
		  WHERE p.id = $1`, id,
	).Scan(&p.ID, &p.HomeCategoryID, &p.Sequence, &docType, &categoryCode, &categorySlug, &p.Title, &sensitivity,
		&templateID, &p.TemplateNone, &currentPubVID,
		&currentDraftVID,
		&p.OwnerUserID,
		&ackTriggersRaw, &ackAudienceOverride, &effectiveDate,
		&p.CreatedAt, &p.UpdatedAt, &retiredAt,
		&currentVersionNo, &currentVersionStatus)
	if err != nil {
		return domain.Policy{}, fmt.Errorf("PolicyStore.GetPolicy: %w", err)
	}
	if effectiveDate.Valid {
		ed := effectiveDate.Time
		p.EffectiveDate = &ed
	}
	p.RetiredAt = retiredAt
	p.DocumentType = domain.DocumentType(docType)
	p.Number = renderDocNumber(p.DocumentType, numberCodeSegment(categoryCode, categorySlug), p.Sequence)
	p.Sensitivity = domain.Sensitivity(sensitivity)
	if templateID != nil {
		p.TemplateID = *templateID
	}
	if currentPubVID != nil {
		p.CurrentPublishedVersionID = *currentPubVID
	}
	if currentDraftVID != nil {
		p.CurrentDraftVersionID = *currentDraftVID
	}
	if ackTriggersRaw != nil {
		v := domain.AckTrigger(*ackTriggersRaw)
		p.AckTriggers = &v
	}
	p.AckAudienceOverride = []uuid.UUID(ackAudienceOverride)
	if currentVersionNo != nil {
		p.CurrentVersionNo = *currentVersionNo
	}
	if currentVersionStatus != nil {
		p.CurrentVersionStatus = domain.PolicyVersionStatus(*currentVersionStatus)
	}
	return p, nil
}

// GetPolicyByNumber fetches a policy by its rendered number (e.g.
// "POL-SAFETY-000007"): <PREFIX>-<category code>-<sequence>, parsed back into
// its category code and sequence. The code segment never contains "-" (it's
// built from uppercase alphanumerics only), so the split is unambiguous.
// Returns pgx.ErrNoRows when the number doesn't parse or match.
func (s *PolicyStore) GetPolicyByNumber(ctx context.Context, number string) (domain.Policy, error) {
	parts := strings.SplitN(number, "-", 3)
	if len(parts) != 3 {
		return domain.Policy{}, pgx.ErrNoRows
	}
	docType, ok := docTypeFromPrefix(parts[0])
	if !ok {
		return domain.Policy{}, pgx.ErrNoRows
	}
	sequence, err := strconv.Atoi(parts[2])
	if err != nil {
		return domain.Policy{}, pgx.ErrNoRows
	}
	var id uuid.UUID
	if err := s.db.Querier().QueryRow(ctx,
		`SELECT p.id FROM policies p JOIN categories g ON g.id = p.home_category_id
		  WHERE g.code = $1 AND p.sequence = $2 AND p.document_type = $3`,
		parts[1], sequence, string(docType),
	).Scan(&id); err != nil {
		return domain.Policy{}, fmt.Errorf("PolicyStore.GetPolicyByNumber: %w", err)
	}
	return s.GetPolicy(ctx, id)
}

// RetirePolicy stamps retired_at. Retiring a retired policy is a no-op. A
// retired policy is hidden from ListPolicies and obligates no one; completed
// acknowledgements stay. Returns pgx.ErrNoRows if the policy is absent.
func (s *PolicyStore) RetirePolicy(ctx context.Context, id uuid.UUID) (domain.Policy, error) {
	if _, err := s.db.Querier().Exec(ctx,
		`UPDATE policies SET retired_at = now(), updated_at = now()
		   WHERE id = $1 AND retired_at IS NULL`, id,
	); err != nil {
		return domain.Policy{}, fmt.Errorf("PolicyStore.RetirePolicy: %w", err)
	}
	return s.GetPolicy(ctx, id)
}

// ListPolicies returns the active documents of docType homed in categoryID,
// or in its whole subtree when includeDescendants is set, in one query. The
// handler maps an unset type to policies, so procedures never show up in a
// list that didn't ask for them.
func (s *PolicyStore) ListPolicies(ctx context.Context, categoryID uuid.UUID, includeDescendants bool, docType domain.DocumentType) ([]domain.Policy, error) {
	var query string
	if includeDescendants {
		query = `WITH RECURSIVE grp AS (
		           SELECT id FROM categories WHERE id = $1
		           UNION ALL
		           SELECT g.id FROM categories g JOIN grp ON g.parent_id = grp.id
		         )
		         SELECT p.id, p.home_category_id, p.sequence, p.document_type, hg.code, hg.slug, p.title, p.sensitivity,
		                p.template_id, p.template_none, p.current_published_version_id,
		                (SELECT pv.id FROM policy_versions pv WHERE pv.policy_id = p.id AND pv.status = 'draft' LIMIT 1),
		                p.owner_user_id,
		                p.ack_triggers, p.ack_audience_override,
		                p.created_at, p.updated_at,
		                cv.version_no, cv.status
		           FROM policies p
		           JOIN categories hg ON hg.id = p.home_category_id
		           LEFT JOIN policy_versions cv ON cv.id = COALESCE(p.current_published_version_id, (SELECT id FROM policy_versions WHERE policy_id = p.id AND status = 'draft' LIMIT 1))
		          WHERE p.home_category_id IN (SELECT id FROM grp)
		            AND p.retired_at IS NULL
		            AND p.document_type = $2`
	} else {
		query = `SELECT p.id, p.home_category_id, p.sequence, p.document_type, hg.code, hg.slug, p.title, p.sensitivity,
		                p.template_id, p.template_none, p.current_published_version_id,
		                (SELECT pv.id FROM policy_versions pv WHERE pv.policy_id = p.id AND pv.status = 'draft' LIMIT 1),
		                p.owner_user_id,
		                p.ack_triggers, p.ack_audience_override,
		                p.created_at, p.updated_at,
		                cv.version_no, cv.status
		           FROM policies p
		           JOIN categories hg ON hg.id = p.home_category_id
		           LEFT JOIN policy_versions cv ON cv.id = COALESCE(p.current_published_version_id, (SELECT id FROM policy_versions WHERE policy_id = p.id AND status = 'draft' LIMIT 1))
		          WHERE p.home_category_id = $1
		            AND p.retired_at IS NULL
		            AND p.document_type = $2`
	}
	rows, err := s.db.Querier().Query(ctx, query, categoryID, string(docType))
	if err != nil {
		return nil, fmt.Errorf("PolicyStore.ListPolicies: %w", err)
	}
	defer rows.Close()

	var out []domain.Policy
	for rows.Next() {
		var (
			p                    domain.Policy
			sensitivity          string
			docType              string
			categoryCode         *string
			categorySlug         string
			templateID           *uuid.UUID
			currentPubVID        *uuid.UUID
			currentDraftVID      *uuid.UUID
			ackTriggersRaw       *string
			ackAudienceOverride  pgtype.FlatArray[uuid.UUID]
			currentVersionNo     *int
			currentVersionStatus *string
		)
		if err := rows.Scan(&p.ID, &p.HomeCategoryID, &p.Sequence, &docType, &categoryCode, &categorySlug, &p.Title, &sensitivity,
			&templateID, &p.TemplateNone, &currentPubVID,
			&currentDraftVID,
			&p.OwnerUserID,
			&ackTriggersRaw, &ackAudienceOverride,
			&p.CreatedAt, &p.UpdatedAt,
			&currentVersionNo, &currentVersionStatus); err != nil {
			return nil, fmt.Errorf("PolicyStore.ListPolicies scan: %w", err)
		}
		p.DocumentType = domain.DocumentType(docType)
		p.Number = renderDocNumber(p.DocumentType, numberCodeSegment(categoryCode, categorySlug), p.Sequence)
		p.Sensitivity = domain.Sensitivity(sensitivity)
		if templateID != nil {
			p.TemplateID = *templateID
		}
		if currentPubVID != nil {
			p.CurrentPublishedVersionID = *currentPubVID
		}
		if currentDraftVID != nil {
			p.CurrentDraftVersionID = *currentDraftVID
		}
		if ackTriggersRaw != nil {
			v := domain.AckTrigger(*ackTriggersRaw)
			p.AckTriggers = &v
		}
		p.AckAudienceOverride = []uuid.UUID(ackAudienceOverride)
		if currentVersionNo != nil {
			p.CurrentVersionNo = *currentVersionNo
		}
		if currentVersionStatus != nil {
			p.CurrentVersionStatus = domain.PolicyVersionStatus(*currentVersionStatus)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PolicyStore.ListPolicies rows: %w", err)
	}
	return out, nil
}

// UpsertDraftResult says what UpsertDraft did. Inserted is true only when a new
// draft row was created, for one-time side effects such as copying appendices
// forward. ContentChanged is false for an autosave that stored the same
// content, so the caller can skip a duplicate audit event.
type UpsertDraftResult struct {
	Inserted       bool
	ContentChanged bool
}

// UpsertDraft creates the policy's draft or updates it in place.
func (s *PolicyStore) UpsertDraft(ctx context.Context, pv domain.PolicyVersion) (domain.PolicyVersion, UpsertDraftResult, error) {
	var tvPtr *uuid.UUID
	if pv.TemplateVersionID != uuid.Nil {
		tvPtr = &pv.TemplateVersionID
	}
	var res UpsertDraftResult
	// The prev CTE reads the draft before the upsert, so RETURNING can report
	// whether the content changed; with no prior row it compares to NULL and
	// reports a change.
	err := s.db.Querier().QueryRow(ctx,
		`WITH prev AS (
		     SELECT content AS c FROM policy_versions
		      WHERE policy_id = $2 AND status = 'draft'
		 )
		 INSERT INTO policy_versions
		     (id, policy_id, version_no, status, template_version_id, content, created_by)
		 VALUES ($1, $2, 0, 'draft', $3, $4::jsonb, $5)
		 ON CONFLICT (policy_id) WHERE status = 'draft'
		 DO UPDATE SET
		     template_version_id = EXCLUDED.template_version_id,
		     content             = EXCLUDED.content,
		     created_at          = now()
		 RETURNING id, version_no, created_at,
		     (xmax = 0) AS inserted,
		     ((xmax = 0) OR content IS DISTINCT FROM (SELECT c FROM prev)) AS changed`,
		pv.ID, pv.PolicyID, tvPtr, pv.ContentJSON, pv.CreatedBy,
	).Scan(&pv.ID, &pv.VersionNo, &pv.CreatedAt, &res.Inserted, &res.ContentChanged)
	if err != nil {
		return domain.PolicyVersion{}, UpsertDraftResult{}, fmt.Errorf("PolicyStore.UpsertDraft: %w", err)
	}
	pv.Status = domain.PolicyVersionStatusDraft
	return pv, res, nil
}

// UpdateDraftContent overwrites a draft version's content. The caller has
// already validated it against the pinned template version. It fails when the
// version is missing or isn't a draft: published versions are immutable.
func (s *PolicyStore) UpdateDraftContent(ctx context.Context, policyVersionID uuid.UUID, contentJSON string) error {
	tag, err := s.db.Querier().Exec(ctx,
		`UPDATE policy_versions
		    SET content = $2::jsonb
		  WHERE id = $1 AND status = 'draft'`,
		policyVersionID, contentJSON,
	)
	if err != nil {
		return fmt.Errorf("PolicyStore.UpdateDraftContent: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("PolicyStore.UpdateDraftContent: draft %s not found or not in draft status", policyVersionID)
	}
	return nil
}

// DiscardDraft deletes the policy's draft and nothing else: never a published
// version, never the policy. Returns ErrNoDraft when there is none.
func (s *PolicyStore) DiscardDraft(ctx context.Context, policyID uuid.UUID) error {
	tag, err := s.db.Querier().Exec(ctx,
		`DELETE FROM policy_versions
		  WHERE policy_id = $1 AND status = 'draft'`,
		policyID,
	)
	if err != nil {
		return fmt.Errorf("PolicyStore.DiscardDraft: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("PolicyStore.DiscardDraft %s: %w", policyID, ErrNoDraft)
	}
	return nil
}

// DeletePolicy hard-deletes a never-published policy; its drafts and
// appendices cascade. The check and the delete share one transaction, so a
// concurrent publish can't slip in between. Returns ErrHasPublishedVersion
// when a version was ever published, pgx.ErrNoRows when the policy is absent.
func (s *PolicyStore) DeletePolicy(ctx context.Context, policyID uuid.UUID) error {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var err error

		var hasPublished bool
		err = tx.QueryRow(ctx,
			`SELECT EXISTS (
			     SELECT 1 FROM policy_versions
			      WHERE policy_id = $1 AND status IN ('published','superseded')
			 ) OR EXISTS (
			     SELECT 1 FROM policies
			      WHERE id = $1 AND current_published_version_id IS NOT NULL
			 )`, policyID,
		).Scan(&hasPublished)
		if err != nil {
			return fmt.Errorf("PolicyStore.DeletePolicy guard: %w", err)
		}
		if hasPublished {
			return fmt.Errorf("PolicyStore.DeletePolicy %s: %w", policyID, ErrHasPublishedVersion)
		}

		tag, err := tx.Exec(ctx, `DELETE FROM policies WHERE id = $1`, policyID)
		if err != nil {
			return fmt.Errorf("PolicyStore.DeletePolicy delete: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("PolicyStore.DeletePolicy %s: %w", policyID, pgx.ErrNoRows)
		}

		return nil
	})
	if err != nil {
		return err
	}
	return nil
}

// InsertDraftRaw inserts a draft with no ON CONFLICT, so a second draft
// surfaces as ErrDraftAlreadyExists. Other callers use UpsertDraft.
func (s *PolicyStore) InsertDraftRaw(ctx context.Context, id, policyID, templateVersionID uuid.UUID, contentJSON string, createdBy uuid.UUID) error {
	_, err := s.db.Querier().Exec(ctx,
		`INSERT INTO policy_versions
		     (id, policy_id, version_no, status, template_version_id, content, created_by)
		 VALUES ($1, $2, 0, 'draft', $3, $4::jsonb, $5)`,
		id, policyID, templateVersionID, contentJSON, createdBy,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDraftAlreadyExists
		}
		return fmt.Errorf("PolicyStore.InsertDraftRaw: %w", err)
	}
	return nil
}

// PublishDraft publishes the policy's draft as its next version, superseding
// the previous published version, in one transaction.
func (s *PolicyStore) PublishDraft(ctx context.Context, policyID, actorUserID uuid.UUID) (domain.PolicyVersion, error) {
	_ = actorUserID

	var pv domain.PolicyVersion
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var err error

		var draftID uuid.UUID
		err = tx.QueryRow(ctx,
			`SELECT id FROM policy_versions
			  WHERE policy_id = $1 AND status = 'draft'`, policyID,
		).Scan(&draftID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("PolicyStore.PublishDraft: no draft for policy %s", policyID)
			}
			return fmt.Errorf("PolicyStore.PublishDraft find draft: %w", err)
		}

		pv, err = s.promoteDraftToPublished(ctx, tx, policyID, draftID)
		if err != nil {
			return fmt.Errorf("PolicyStore.PublishDraft: %w", err)
		}

		return nil
	})
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	return pv, nil
}

// promoteDraftToPublished publishes a draft row inside tx: the next version
// number, the previous published version superseded, the policy pointed at
// it. Both publish paths, PublishDraft and workflow's SetVersionStatus, use
// it, so they can't drift apart.
func (s *PolicyStore) promoteDraftToPublished(ctx context.Context, tx pgx.Tx, policyID, versionID uuid.UUID) (domain.PolicyVersion, error) {
	// Drafts hold version_no 0, so MAX over the rest gives the next number.
	var prevPublishedID *uuid.UUID
	var maxNonDraftVersionNo int
	err := tx.QueryRow(ctx,
		`SELECT
		     (SELECT id FROM policy_versions
		       WHERE policy_id = $1 AND status = 'published'
		       ORDER BY version_no DESC LIMIT 1),
		     COALESCE(
		       (SELECT MAX(version_no) FROM policy_versions
		         WHERE policy_id = $1 AND status <> 'draft'),
		       0)`,
		policyID,
	).Scan(&prevPublishedID, &maxNonDraftVersionNo)
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("find prev: %w", err)
	}

	newVersionNo := maxNonDraftVersionNo + 1
	now := time.Now().UTC()

	// A staged rename is read before promoting so it goes live with the
	// version, on both publish paths.
	var proposedTitle *string
	if err := tx.QueryRow(ctx,
		`SELECT proposed_title FROM policy_versions WHERE id = $1`, versionID,
	).Scan(&proposedTitle); err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("read proposed_title: %w", err)
	}

	if prevPublishedID != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE policy_versions SET status = 'superseded' WHERE id = $1`,
			*prevPublishedID,
		); err != nil {
			return domain.PolicyVersion{}, fmt.Errorf("supersede: %w", err)
		}
	}

	var (
		pv              domain.PolicyVersion
		status          string
		supersedesID    *uuid.UUID
		publishedAtNull *time.Time
		tvNull          *uuid.UUID
	)
	err = tx.QueryRow(ctx,
		`UPDATE policy_versions
		    SET status = 'published',
		        version_no = $2,
		        published_at = COALESCE(published_at, $3),
		        supersedes_version_id = $4,
		        proposed_title = NULL
		  WHERE id = $1
		 RETURNING id, policy_id, version_no, status, template_version_id,
		           content::text, created_at, published_at, created_by,
		           supersedes_version_id`,
		versionID, newVersionNo, now, prevPublishedID,
	).Scan(&pv.ID, &pv.PolicyID, &pv.VersionNo, &status, &tvNull,
		&pv.ContentJSON, &pv.CreatedAt, &publishedAtNull, &pv.CreatedBy,
		&supersedesID)
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("promote: %w", err)
	}
	if tvNull != nil {
		pv.TemplateVersionID = *tvNull
	}
	pv.Status = domain.PolicyVersionStatus(status)
	if publishedAtNull != nil {
		pv.PublishedAt = *publishedAtNull
	}
	if supersedesID != nil {
		pv.SupersedesVersionID = *supersedesID
	}

	// The staged rename is applied in the same statement as the pointer.
	if proposedTitle != nil {
		if _, err := tx.Exec(ctx,
			`UPDATE policies
			    SET current_published_version_id = $2,
			        title = $3,
			        updated_at = now()
			  WHERE id = $1`,
			policyID, pv.ID, *proposedTitle,
		); err != nil {
			return domain.PolicyVersion{}, fmt.Errorf("update policy: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx,
			`UPDATE policies
			    SET current_published_version_id = $2,
			        updated_at = now()
			  WHERE id = $1`,
			policyID, pv.ID,
		); err != nil {
			return domain.PolicyVersion{}, fmt.Errorf("update policy: %w", err)
		}
	}

	return pv, nil
}

// SetVersionStatus moves a version through the core state machine
// (domain.ValidateVersionStatusTransition) in one transaction: publishing
// stamps published_at and points the policy at the version; withdrawing to
// draft clears both. An illegal transition returns an error the handler maps
// to FailedPrecondition.
func (s *PolicyStore) SetVersionStatus(ctx context.Context, policyVersionID uuid.UUID, target domain.PolicyVersionStatus) (domain.PolicyVersion, error) {
	var pv domain.PolicyVersion
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		var err error

		var (
			policyID   uuid.UUID
			currentRaw string
		)
		err = tx.QueryRow(ctx,
			`SELECT policy_id, status FROM policy_versions WHERE id = $1 FOR UPDATE`,
			policyVersionID,
		).Scan(&policyID, &currentRaw)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("PolicyStore.SetVersionStatus: version %s not found", policyVersionID)
			}
			return fmt.Errorf("PolicyStore.SetVersionStatus read: %w", err)
		}
		current := domain.PolicyVersionStatus(currentRaw)

		if err := domain.ValidateVersionStatusTransition(current, target); err != nil {
			return err
		}

		switch target {
		case domain.PolicyVersionStatusPublished:
			if current == domain.PolicyVersionStatusDraft {
				// A draft sits at version_no 0, so it must be promoted like
				// PublishDraft does, or it would collide with the next draft.
				if _, err := s.promoteDraftToPublished(ctx, tx, policyID, policyVersionID); err != nil {
					return fmt.Errorf("PolicyStore.SetVersionStatus promote: %w", err)
				}
			} else {
				// A non-draft row keeps its version_no.
				if _, err := tx.Exec(ctx,
					`UPDATE policy_versions
					    SET status = 'published',
					        published_at = COALESCE(published_at, now())
					  WHERE id = $1`,
					policyVersionID,
				); err != nil {
					return fmt.Errorf("PolicyStore.SetVersionStatus publish: %w", err)
				}
				if _, err := tx.Exec(ctx,
					`UPDATE policies SET current_published_version_id = $2, updated_at = now() WHERE id = $1`,
					policyID, policyVersionID,
				); err != nil {
					return fmt.Errorf("PolicyStore.SetVersionStatus point published: %w", err)
				}
			}
		case domain.PolicyVersionStatusDraft:
			if _, err := tx.Exec(ctx,
				`UPDATE policy_versions SET status = 'draft', published_at = NULL WHERE id = $1`,
				policyVersionID,
			); err != nil {
				return fmt.Errorf("PolicyStore.SetVersionStatus withdraw: %w", err)
			}
			if _, err := tx.Exec(ctx,
				`UPDATE policies SET current_published_version_id = NULL, updated_at = now()
				  WHERE id = $1 AND current_published_version_id = $2`,
				policyID, policyVersionID,
			); err != nil {
				return fmt.Errorf("PolicyStore.SetVersionStatus clear pointer: %w", err)
			}
		default:
			if _, err := tx.Exec(ctx,
				`UPDATE policy_versions SET status = $2 WHERE id = $1`,
				policyVersionID, string(target),
			); err != nil {
				return fmt.Errorf("PolicyStore.SetVersionStatus update: %w", err)
			}
		}

		var (
			statusOut       string
			publishedAtNull *time.Time
			supersedesID    *uuid.UUID
			tvNull          *uuid.UUID
		)
		err = tx.QueryRow(ctx,
			`SELECT id, policy_id, version_no, status, template_version_id,
			        content::text, created_at, published_at, created_by, supersedes_version_id
			   FROM policy_versions WHERE id = $1`,
			policyVersionID,
		).Scan(&pv.ID, &pv.PolicyID, &pv.VersionNo, &statusOut, &tvNull,
			&pv.ContentJSON, &pv.CreatedAt, &publishedAtNull, &pv.CreatedBy, &supersedesID)
		if err != nil {
			return fmt.Errorf("PolicyStore.SetVersionStatus readback: %w", err)
		}
		if tvNull != nil {
			pv.TemplateVersionID = *tvNull
		}
		pv.Status = domain.PolicyVersionStatus(statusOut)
		if publishedAtNull != nil {
			pv.PublishedAt = *publishedAtNull
		}
		if supersedesID != nil {
			pv.SupersedesVersionID = *supersedesID
		}

		return nil
	})
	if err != nil {
		return domain.PolicyVersion{}, err
	}
	return pv, nil
}

// GetPolicyVersion fetches a version by id; NULL columns come back as zero
// values.
func (s *PolicyStore) GetPolicyVersion(ctx context.Context, id uuid.UUID) (domain.PolicyVersion, error) {
	var (
		pv              domain.PolicyVersion
		status          string
		publishedAtNull *time.Time
		supersedesID    *uuid.UUID
		tvNull          *uuid.UUID
		proposedTitle   *string
	)
	err := s.db.Querier().QueryRow(ctx,
		`SELECT id, policy_id, version_no, status, template_version_id,
		        content::text, created_at, published_at, created_by,
		        supersedes_version_id, proposed_title
		   FROM policy_versions WHERE id = $1`, id,
	).Scan(&pv.ID, &pv.PolicyID, &pv.VersionNo, &status, &tvNull,
		&pv.ContentJSON, &pv.CreatedAt, &publishedAtNull, &pv.CreatedBy,
		&supersedesID, &proposedTitle)
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("PolicyStore.GetPolicyVersion: %w", err)
	}
	if tvNull != nil {
		pv.TemplateVersionID = *tvNull
	}
	pv.Status = domain.PolicyVersionStatus(status)
	if publishedAtNull != nil {
		pv.PublishedAt = *publishedAtNull
	}
	if supersedesID != nil {
		pv.SupersedesVersionID = *supersedesID
	}
	pv.ProposedTitle = proposedTitle
	return pv, nil
}

// ListPublishedVersions returns a policy's non-draft versions, oldest-first.
func (s *PolicyStore) ListPublishedVersions(ctx context.Context, policyID uuid.UUID) ([]domain.PolicyVersion, error) {
	rows, err := s.db.Querier().Query(ctx,
		`SELECT id, policy_id, version_no, status, template_version_id,
		        content::text, created_at, published_at, created_by, supersedes_version_id
		   FROM policy_versions
		  WHERE policy_id = $1 AND status <> 'draft'
		  ORDER BY version_no ASC`, policyID)
	if err != nil {
		return nil, fmt.Errorf("PolicyStore.ListPublishedVersions: %w", err)
	}
	defer rows.Close()
	out := []domain.PolicyVersion{}
	for rows.Next() {
		var (
			pv              domain.PolicyVersion
			status          string
			publishedAtNull *time.Time
			supersedesID    *uuid.UUID
			tvNull          *uuid.UUID
		)
		if err := rows.Scan(&pv.ID, &pv.PolicyID, &pv.VersionNo, &status, &tvNull,
			&pv.ContentJSON, &pv.CreatedAt, &publishedAtNull, &pv.CreatedBy, &supersedesID); err != nil {
			return nil, fmt.Errorf("PolicyStore.ListPublishedVersions scan: %w", err)
		}
		if tvNull != nil {
			pv.TemplateVersionID = *tvNull
		}
		pv.Status = domain.PolicyVersionStatus(status)
		if publishedAtNull != nil {
			pv.PublishedAt = *publishedAtNull
		}
		if supersedesID != nil {
			pv.SupersedesVersionID = *supersedesID
		}
		out = append(out, pv)
	}
	return out, rows.Err()
}

// IsTemplateUpdateAvailable reports whether the template's latest published
// version differs from the one the policy version pins. It is false when the
// template has no published version.
func (s *PolicyStore) IsTemplateUpdateAvailable(ctx context.Context, policyVersionID, templateID uuid.UUID) (bool, error) {
	var (
		pinnedID uuid.UUID
		latestID *uuid.UUID
	)
	err := s.db.Querier().QueryRow(ctx,
		`SELECT pv.template_version_id,
		        (SELECT id FROM template_versions
		          WHERE template_id = $2 AND status = 'published'
		          ORDER BY version_no DESC LIMIT 1)
		   FROM policy_versions pv WHERE pv.id = $1`,
		policyVersionID, templateID,
	).Scan(&pinnedID, &latestID)
	if err != nil {
		return false, fmt.Errorf("PolicyStore.IsTemplateUpdateAvailable: %w", err)
	}
	if latestID == nil {
		return false, nil
	}
	return *latestID != pinnedID, nil
}

// SetAck sets the acknowledgement trigger and audience override. A nil trigger
// or an empty override stores NULL, which inherits from the category.
func (s *PolicyStore) SetAck(ctx context.Context, policyID uuid.UUID, ack *domain.AckTrigger, override []uuid.UUID) (domain.Policy, error) {
	var ackRaw *string
	if ack != nil {
		v := string(*ack)
		ackRaw = &v
	}
	var overrideArr any
	if len(override) == 0 {
		overrideArr = nil
	} else {
		overrideArr = pgtype.FlatArray[uuid.UUID](override)
	}
	_, err := s.db.Querier().Exec(ctx,
		`UPDATE policies
		    SET ack_triggers          = $2,
		        ack_audience_override = $3,
		        updated_at            = now()
		  WHERE id = $1`,
		policyID, ackRaw, overrideArr,
	)
	if err != nil {
		return domain.Policy{}, fmt.Errorf("PolicyStore.SetAck: %w", err)
	}
	return s.GetPolicy(ctx, policyID)
}

// SetTemplate sets the policy's own template: templateNone means freeform
// (the id is stored as NULL), uuid.Nil inherits from the category chain.
func (s *PolicyStore) SetTemplate(ctx context.Context, policyID, templateID uuid.UUID, templateNone bool) (domain.Policy, error) {
	var templatePtr *uuid.UUID
	if templateID != uuid.Nil && !templateNone {
		templatePtr = &templateID
	}
	tag, err := s.db.Querier().Exec(ctx,
		`UPDATE policies
		    SET template_id   = $2,
		        template_none = $3,
		        updated_at    = now()
		  WHERE id = $1`,
		policyID, templatePtr, templateNone,
	)
	if err != nil {
		return domain.Policy{}, fmt.Errorf("PolicyStore.SetTemplate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.Policy{}, fmt.Errorf("PolicyStore.SetTemplate %s: %w", policyID, pgx.ErrNoRows)
	}
	return s.GetPolicy(ctx, policyID)
}

// ListAllPolicies returns every policy, unordered, for the obligation
// resolution. It doesn't filter by document type: each policy carries its
// type and the obligation path skips procedures.
func (s *PolicyStore) ListAllPolicies(ctx context.Context) ([]domain.Policy, error) {
	rows, err := s.db.Querier().Query(ctx,
		`SELECT p.id, p.home_category_id, p.sequence, p.document_type, hg.code, hg.slug, p.title, p.sensitivity,
		        p.template_id, p.template_none, p.current_published_version_id,
		        p.owner_user_id,
		        p.ack_triggers, p.ack_audience_override,
		        p.created_at, p.updated_at, p.retired_at
		   FROM policies p
		   JOIN categories hg ON hg.id = p.home_category_id`)
	if err != nil {
		return nil, fmt.Errorf("PolicyStore.ListAllPolicies: %w", err)
	}
	defer rows.Close()

	var out []domain.Policy
	for rows.Next() {
		var (
			p                   domain.Policy
			sensitivity         string
			docType             string
			categoryCode        *string
			categorySlug        string
			templateID          *uuid.UUID
			currentPubVID       *uuid.UUID
			ackTriggersRaw      *string
			ackAudienceOverride pgtype.FlatArray[uuid.UUID]
			retiredAt           *time.Time
		)
		if err := rows.Scan(&p.ID, &p.HomeCategoryID, &p.Sequence, &docType, &categoryCode, &categorySlug, &p.Title, &sensitivity,
			&templateID, &p.TemplateNone, &currentPubVID,
			&p.OwnerUserID,
			&ackTriggersRaw, &ackAudienceOverride,
			&p.CreatedAt, &p.UpdatedAt, &retiredAt); err != nil {
			return nil, fmt.Errorf("PolicyStore.ListAllPolicies scan: %w", err)
		}
		p.RetiredAt = retiredAt
		p.DocumentType = domain.DocumentType(docType)
		p.Number = renderDocNumber(p.DocumentType, numberCodeSegment(categoryCode, categorySlug), p.Sequence)
		p.Sensitivity = domain.Sensitivity(sensitivity)
		if templateID != nil {
			p.TemplateID = *templateID
		}
		if currentPubVID != nil {
			p.CurrentPublishedVersionID = *currentPubVID
		}
		if ackTriggersRaw != nil {
			v := domain.AckTrigger(*ackTriggersRaw)
			p.AckTriggers = &v
		}
		p.AckAudienceOverride = []uuid.UUID(ackAudienceOverride)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PolicyStore.ListAllPolicies rows: %w", err)
	}
	return out, nil
}

// ListPoliciesByOwner returns the policies a user owns, so an admin can hand
// them over before the account is deleted. The owner is also the author.
// Retired policies are left out unless includeRetired is set.
func (s *PolicyStore) ListPoliciesByOwner(ctx context.Context, ownerUserID uuid.UUID, includeRetired bool) ([]domain.Policy, error) {
	q := `SELECT p.id, p.home_category_id, p.sequence, p.document_type, hg.code, hg.slug, p.title, p.sensitivity,
	             p.template_id, p.template_none, p.current_published_version_id,
	             p.owner_user_id,
	             p.ack_triggers, p.ack_audience_override,
	             p.created_at, p.updated_at, p.retired_at
	        FROM policies p
	        JOIN categories hg ON hg.id = p.home_category_id
	       WHERE p.owner_user_id = $1`
	if !includeRetired {
		q += ` AND p.retired_at IS NULL`
	}
	rows, err := s.db.Querier().Query(ctx, q, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("PolicyStore.ListPoliciesByOwner: %w", err)
	}
	defer rows.Close()

	var out []domain.Policy
	for rows.Next() {
		var (
			p                   domain.Policy
			sensitivity         string
			docType             string
			categoryCode        *string
			categorySlug        string
			templateID          *uuid.UUID
			currentPubVID       *uuid.UUID
			ackTriggersRaw      *string
			ackAudienceOverride pgtype.FlatArray[uuid.UUID]
			retiredAt           *time.Time
		)
		if err := rows.Scan(&p.ID, &p.HomeCategoryID, &p.Sequence, &docType, &categoryCode, &categorySlug, &p.Title, &sensitivity,
			&templateID, &p.TemplateNone, &currentPubVID,
			&p.OwnerUserID,
			&ackTriggersRaw, &ackAudienceOverride,
			&p.CreatedAt, &p.UpdatedAt, &retiredAt); err != nil {
			return nil, fmt.Errorf("PolicyStore.ListPoliciesByOwner scan: %w", err)
		}
		p.RetiredAt = retiredAt
		p.DocumentType = domain.DocumentType(docType)
		p.Number = renderDocNumber(p.DocumentType, numberCodeSegment(categoryCode, categorySlug), p.Sequence)
		p.Sensitivity = domain.Sensitivity(sensitivity)
		if templateID != nil {
			p.TemplateID = *templateID
		}
		if currentPubVID != nil {
			p.CurrentPublishedVersionID = *currentPubVID
		}
		if ackTriggersRaw != nil {
			v := domain.AckTrigger(*ackTriggersRaw)
			p.AckTriggers = &v
		}
		p.AckAudienceOverride = []uuid.UUID(ackAudienceOverride)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("PolicyStore.ListPoliciesByOwner rows: %w", err)
	}
	return out, nil
}

// ReassignUserPolicies moves a user's ownership to another user in one
// transaction: every policy they own and every user-subject category rule
// naming them. It never deletes a policy. It returns the policy ids, the
// owner count and the number of rewritten rules.
func (s *PolicyStore) ReassignUserPolicies(ctx context.Context, fromUserID, toUserID uuid.UUID) ([]uuid.UUID, int, int, error) {
	var ids []uuid.UUID
	var authorGrants int
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		ids = nil
		rows, err := tx.Query(ctx,
			`UPDATE policies SET owner_user_id = $2, updated_at = now()
			   WHERE owner_user_id = $1
			 RETURNING id`,
			fromUserID, toUserID)
		if err != nil {
			return fmt.Errorf("PolicyStore.ReassignUserPolicies update policies: %w", err)
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("PolicyStore.ReassignUserPolicies scan: %w", err)
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("PolicyStore.ReassignUserPolicies rows: %w", err)
		}

		tag, err := tx.Exec(ctx,
			`UPDATE category_rules SET subject_ref = $2
			   WHERE subject_kind = 'user' AND subject_ref = $1`,
			fromUserID.String(), toUserID.String())
		if err != nil {
			return fmt.Errorf("PolicyStore.ReassignUserPolicies update rules: %w", err)
		}
		authorGrants = int(tag.RowsAffected())

		return nil
	})
	if err != nil {
		return nil, 0, 0, err
	}
	return ids, len(ids), authorGrants, nil
}

// isUniqueViolation reports whether err is a unique violation (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation
}

// constraintName returns the constraint a unique violation tripped, or "".
// It tells a code collision (retry) from a slug collision (conflict) without
// matching messages.
func constraintName(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName
	}
	return ""
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// ErrVersionNotFound is returned when a link read or write names a policy
// version that does not exist. Callers map it to codes.NotFound.
var ErrVersionNotFound = errors.New("policy version not found")

// ErrVersionNotEditable is returned when links are changed on a version that
// no longer holds the draft slot: a published, superseded, rejected or archived
// version is as it was approved. Callers map it to codes.FailedPrecondition.
var ErrVersionNotEditable = errors.New("links can only change while the version is a draft or in review")

// ErrLinkItemNotFound is returned when a link names a library item (reference,
// contact block or definition) that does not exist. Callers map it to
// codes.NotFound.
var ErrLinkItemNotFound = errors.New("library item not found")

// ErrRelatedPolicyNotFound is returned when a related link names a policy that
// does not exist. Callers map it to codes.NotFound.
var ErrRelatedPolicyNotFound = errors.New("related policy not found")

// VersionLinkStore keeps the library links of each policy version: references
// and contact blocks in the author's order, definitions (listed
// alphabetically) and related policies with a relationship type. The library
// items are not versioned; the links are, so they are approved with the
// version, stay as approved once it is published and are copied into a new
// draft. Links change only while the version holds the draft slot.
type VersionLinkStore struct{ db *postgres.DB }

// NewVersionLinkStore returns a VersionLinkStore on db.
func NewVersionLinkStore(db *postgres.DB) *VersionLinkStore { return &VersionLinkStore{db: db} }

// livePolicyVersion is the SQL condition (over policy_versions v and policies
// p) for a version that counts as a policy's own use of the libraries: its
// published version and the one holding the draft slot.
const livePolicyVersion = `(v.id = p.current_published_version_id OR v.status IN ('draft', 'in_review'))`

// GetVersionLinks returns a version's links: definitions alphabetically,
// references and contact blocks in the author's order, and the related items,
// the version's own first and then the incoming ones (see listIncoming). Each
// library item carries the number of policies that use it. It returns
// ErrVersionNotFound for an unknown version.
func (s *VersionLinkStore) GetVersionLinks(ctx context.Context, versionID uuid.UUID) (domain.VersionLinks, error) {
	var (
		policyID  uuid.UUID
		statusRaw string
	)
	err := s.db.Querier().QueryRow(ctx,
		`SELECT policy_id, status FROM policy_versions WHERE id = $1`, versionID).Scan(&policyID, &statusRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.VersionLinks{}, ErrVersionNotFound
	}
	if err != nil {
		return domain.VersionLinks{}, fmt.Errorf("VersionLinkStore.GetVersionLinks %s: %w", versionID, err)
	}
	out := domain.VersionLinks{
		PolicyVersionID: versionID,
		Editable:        domain.PolicyVersionStatus(statusRaw).HoldsDraftSlot(),
	}
	if out.References, err = s.listReferences(ctx, versionID); err != nil {
		return domain.VersionLinks{}, err
	}
	if out.Contacts, err = s.listContacts(ctx, versionID); err != nil {
		return domain.VersionLinks{}, err
	}
	if out.Definitions, err = s.listDefinitions(ctx, versionID); err != nil {
		return domain.VersionLinks{}, err
	}
	outgoing, err := s.listOutgoing(ctx, versionID)
	if err != nil {
		return domain.VersionLinks{}, err
	}
	incoming, err := s.listIncoming(ctx, policyID)
	if err != nil {
		return domain.VersionLinks{}, err
	}
	out.Related = append(outgoing, incoming...)
	return out, nil
}

func (s *VersionLinkStore) listReferences(ctx context.Context, versionID uuid.UUID) ([]domain.Reference, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT r.id, r.label, r.kind, r.clause, r.body, r.url, r.archived, r.created_by_user_id, r.created_at,
		       `+usedByReferences("r")+`
		FROM policy_version_references l
		JOIN "references" r ON r.id = l.reference_id
		WHERE l.policy_version_id = $1
		ORDER BY l.ordinal`, versionID)
	if err != nil {
		return nil, fmt.Errorf("VersionLinkStore.listReferences: %w", err)
	}
	defer rows.Close()
	out := []domain.Reference{}
	for rows.Next() {
		var (
			r    domain.Reference
			kind string
		)
		if err := rows.Scan(&r.ID, &r.Label, &kind, &r.Clause, &r.Body, &r.URL, &r.Archived, &r.CreatedByUserID, &r.CreatedAt, &r.UsedByCount); err != nil {
			return nil, fmt.Errorf("VersionLinkStore.listReferences scan: %w", err)
		}
		r.Kind = domain.ReferenceKind(kind)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *VersionLinkStore) listContacts(ctx context.Context, versionID uuid.UUID) ([]domain.ContactBlock, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT cb.id, cb.label, cb.name, cb.role, cb.department, cb.email, cb.phone, cb.hours, cb.notes, cb.archived,
		       `+usedByContacts("cb")+`
		FROM policy_version_contact_blocks l
		JOIN contact_blocks cb ON cb.id = l.contact_block_id
		WHERE l.policy_version_id = $1
		ORDER BY l.ordinal`, versionID)
	if err != nil {
		return nil, fmt.Errorf("VersionLinkStore.listContacts: %w", err)
	}
	defer rows.Close()
	out := []domain.ContactBlock{}
	for rows.Next() {
		var b domain.ContactBlock
		if err := rows.Scan(&b.ID, &b.Label, &b.Name, &b.Role, &b.Department, &b.Email, &b.Phone, &b.Hours, &b.Notes, &b.Archived, &b.UsedByCount); err != nil {
			return nil, fmt.Errorf("VersionLinkStore.listContacts scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *VersionLinkStore) listDefinitions(ctx context.Context, versionID uuid.UUID) ([]domain.DefinitionEntry, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT d.id, d.category_id, d.term, d.definition, d.archived, d.created_by_user_id, d.created_at,
		       `+usedByDefinitions("d")+`
		FROM policy_version_definitions l
		JOIN definitions d ON d.id = l.definition_id
		WHERE l.policy_version_id = $1
		ORDER BY lower(d.term), d.term`, versionID)
	if err != nil {
		return nil, fmt.Errorf("VersionLinkStore.listDefinitions: %w", err)
	}
	defer rows.Close()
	return scanDefinitionEntryRows(rows)
}

// listOutgoing returns the related items the version itself names, in order,
// with each linked policy's current number and title.
func (s *VersionLinkStore) listOutgoing(ctx context.Context, versionID uuid.UUID) ([]domain.RelatedLink, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT rel.related_policy_id, rel.relation_type, g.code, g.slug, p.sequence, p.document_type, p.title
		FROM policy_version_relations rel
		JOIN policies   p ON p.id = rel.related_policy_id
		JOIN categories g ON g.id = p.home_category_id
		WHERE rel.policy_version_id = $1
		ORDER BY rel.ordinal`, versionID)
	if err != nil {
		return nil, fmt.Errorf("VersionLinkStore.listOutgoing: %w", err)
	}
	defer rows.Close()
	out := []domain.RelatedLink{}
	for rows.Next() {
		link, typeID, err := scanRelated(rows)
		if err != nil {
			return nil, err
		}
		typ, ok := domain.LookupRelationshipType(typeID)
		if !ok {
			return nil, fmt.Errorf("VersionLinkStore.listOutgoing: stored relationship type %q is not in the list", typeID)
		}
		link.Type, link.Label, link.Direction = typ.ID, typ.Label, domain.RelatedOutgoing
		out = append(out, link)
	}
	return out, rows.Err()
}

// listIncoming returns the links other policies make to this one: the
// published version of every other policy that names it, read from this side
// as the inverse type. They show on every version of the policy, and a link in
// someone's draft shows only once that draft is published.
func (s *VersionLinkStore) listIncoming(ctx context.Context, policyID uuid.UUID) ([]domain.RelatedLink, error) {
	rows, err := s.db.Querier().Query(ctx, `
		SELECT p.id, rel.relation_type, g.code, g.slug, p.sequence, p.document_type, p.title
		FROM policy_version_relations rel
		JOIN policies   p ON p.current_published_version_id = rel.policy_version_id
		JOIN categories g ON g.id = p.home_category_id
		WHERE rel.related_policy_id = $1 AND p.id <> $1
		ORDER BY lower(p.title), p.id, rel.relation_type`, policyID)
	if err != nil {
		return nil, fmt.Errorf("VersionLinkStore.listIncoming: %w", err)
	}
	defer rows.Close()
	out := []domain.RelatedLink{}
	for rows.Next() {
		link, typeID, err := scanRelated(rows)
		if err != nil {
			return nil, err
		}
		typ, ok := domain.LookupRelationshipType(typeID)
		if !ok {
			return nil, fmt.Errorf("VersionLinkStore.listIncoming: stored relationship type %q is not in the list", typeID)
		}
		inverse, ok := domain.LookupRelationshipType(typ.InverseID)
		if !ok {
			return nil, fmt.Errorf("VersionLinkStore.listIncoming: type %q has no inverse", typeID)
		}
		link.Type, link.Label, link.Direction = inverse.ID, inverse.Label, domain.RelatedIncoming
		out = append(out, link)
	}
	return out, rows.Err()
}

// scanRelated reads policy id, stored type, then the pieces of the display
// number and the title.
func scanRelated(rows pgx.Rows) (domain.RelatedLink, string, error) {
	var (
		l        domain.RelatedLink
		typeID   string
		code     *string
		slug     string
		sequence int
		docType  string
	)
	if err := rows.Scan(&l.PolicyID, &typeID, &code, &slug, &sequence, &docType, &l.Title); err != nil {
		return domain.RelatedLink{}, "", fmt.Errorf("VersionLinkStore: scan related link: %w", err)
	}
	l.Number = renderDocNumber(domain.DocumentType(docType), numberCodeSegment(code, slug), sequence)
	return l, typeID, nil
}

// SetReferences replaces a version's references, keeping the order given. It
// refuses a version that is not editable (ErrVersionNotEditable), an id named
// twice (domain.ErrRepeatedLink) and an id not in the library
// (ErrLinkItemNotFound). It never creates a draft.
func (s *VersionLinkStore) SetReferences(ctx context.Context, versionID uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error) {
	return s.replace(ctx, versionID, func(tx pgx.Tx, _ uuid.UUID) error {
		if err := checkItems(ctx, tx, ids, `SELECT count(*) FROM "references" WHERE id = ANY($1)`); err != nil {
			return err
		}
		return replaceOrdered(ctx, tx, versionID, ids, "policy_version_references", "reference_id")
	})
}

// SetContactBlocks replaces a version's contact blocks, keeping the order
// given. Refusals are those of SetReferences.
func (s *VersionLinkStore) SetContactBlocks(ctx context.Context, versionID uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error) {
	return s.replace(ctx, versionID, func(tx pgx.Tx, _ uuid.UUID) error {
		if err := checkItems(ctx, tx, ids, `SELECT count(*) FROM contact_blocks WHERE id = ANY($1)`); err != nil {
			return err
		}
		return replaceOrdered(ctx, tx, versionID, ids, "policy_version_contact_blocks", "contact_block_id")
	})
}

// SetDefinitions replaces a version's definitions. They carry no order of their
// own; reads list them alphabetically. Refusals are those of SetReferences.
func (s *VersionLinkStore) SetDefinitions(ctx context.Context, versionID uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error) {
	return s.replace(ctx, versionID, func(tx pgx.Tx, _ uuid.UUID) error {
		if err := checkItems(ctx, tx, ids, `SELECT count(*) FROM definitions WHERE id = ANY($1)`); err != nil {
			return err
		}
		return replaceOrdered(ctx, tx, versionID, ids, "policy_version_definitions", "definition_id")
	})
}

// SetRelated replaces a version's related items, keeping the order given. It
// refuses a version that is not editable, an unknown relationship type, a link
// to the version's own policy and a repeated (policy, type) with the domain
// errors, and a policy that does not exist with ErrRelatedPolicyNotFound.
func (s *VersionLinkStore) SetRelated(ctx context.Context, versionID uuid.UUID, links []domain.RelatedLinkInput) (domain.VersionLinks, error) {
	return s.replace(ctx, versionID, func(tx pgx.Tx, policyID uuid.UUID) error {
		if err := domain.ValidateRelatedLinks(policyID, links); err != nil {
			return err
		}
		targets := map[uuid.UUID]bool{}
		for _, l := range links {
			targets[l.PolicyID] = true
		}
		ids := make([]uuid.UUID, 0, len(targets))
		for id := range targets {
			ids = append(ids, id)
		}
		var found int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM policies WHERE id = ANY($1)`, ids).Scan(&found); err != nil {
			return fmt.Errorf("VersionLinkStore.SetRelated check policies: %w", err)
		}
		if found != len(ids) {
			return ErrRelatedPolicyNotFound
		}
		if _, err := tx.Exec(ctx, `DELETE FROM policy_version_relations WHERE policy_version_id = $1`, versionID); err != nil {
			return fmt.Errorf("VersionLinkStore.SetRelated clear: %w", err)
		}
		for i, l := range links {
			if _, err := tx.Exec(ctx, `
				INSERT INTO policy_version_relations (policy_version_id, related_policy_id, relation_type, ordinal)
				VALUES ($1, $2, $3, $4)`, versionID, l.PolicyID, l.Type, i); err != nil {
				return fmt.Errorf("VersionLinkStore.SetRelated insert: %w", err)
			}
		}
		return nil
	})
}

// replace runs a full replace of one kind of link in a transaction. It locks
// the version row first, so a change cannot race the version being published or
// rejected, and applies it only while the version holds the draft slot.
func (s *VersionLinkStore) replace(ctx context.Context, versionID uuid.UUID, apply func(tx pgx.Tx, policyID uuid.UUID) error) (domain.VersionLinks, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		policyID, err := lockEditable(ctx, tx, versionID)
		if err != nil {
			return err
		}
		return apply(tx, policyID)
	})
	if err != nil {
		return domain.VersionLinks{}, err
	}
	return s.GetVersionLinks(ctx, versionID)
}

// lockEditable locks the version row and returns its policy, or
// ErrVersionNotFound / ErrVersionNotEditable.
func lockEditable(ctx context.Context, tx pgx.Tx, versionID uuid.UUID) (uuid.UUID, error) {
	var (
		policyID  uuid.UUID
		statusRaw string
	)
	err := tx.QueryRow(ctx,
		`SELECT policy_id, status FROM policy_versions WHERE id = $1 FOR UPDATE`, versionID).Scan(&policyID, &statusRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrVersionNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("lock version %s: %w", versionID, err)
	}
	if !domain.PolicyVersionStatus(statusRaw).HoldsDraftSlot() {
		return uuid.Nil, ErrVersionNotEditable
	}
	return policyID, nil
}

// checkItems refuses repeats and ids that are not in the library. countSQL
// takes the id list as $1 and returns how many exist.
func checkItems(ctx context.Context, tx pgx.Tx, ids []uuid.UUID, countSQL string) error {
	if err := domain.ValidateLinkIDs(ids); err != nil {
		return err
	}
	var found int
	if err := tx.QueryRow(ctx, countSQL, ids).Scan(&found); err != nil {
		return fmt.Errorf("check library items: %w", err)
	}
	if found != len(ids) {
		return ErrLinkItemNotFound
	}
	return nil
}

// replaceOrdered rewrites one link table for a version, row i taking ordinal i.
// The table has an ordinal column except policy_version_definitions, which has
// none and gets the plain two-column insert.
func replaceOrdered(ctx context.Context, tx pgx.Tx, versionID uuid.UUID, ids []uuid.UUID, table, itemCol string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE policy_version_id = $1`, versionID); err != nil {
		return fmt.Errorf("clear %s: %w", table, err)
	}
	for i, id := range ids {
		var err error
		if table == "policy_version_definitions" {
			_, err = tx.Exec(ctx, `INSERT INTO `+table+` (policy_version_id, `+itemCol+`) VALUES ($1, $2)`, versionID, id)
		} else {
			_, err = tx.Exec(ctx, `INSERT INTO `+table+` (policy_version_id, `+itemCol+`, ordinal) VALUES ($1, $2, $3)`, versionID, id, i)
		}
		if err != nil {
			return fmt.Errorf("insert into %s: %w", table, err)
		}
	}
	return nil
}

// CopyForward copies every link of fromVersionID onto toVersionID, which must
// hold the draft slot (ErrVersionNotEditable otherwise) and is expected to be
// new. The rows are copied, not shared, so changing the new draft leaves the
// source as it was approved. The caller names the source the same way it does
// for appendices (PolicyStore.AppendixSource).
func (s *VersionLinkStore) CopyForward(ctx context.Context, fromVersionID, toVersionID uuid.UUID) error {
	return s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if _, err := lockEditable(ctx, tx, toVersionID); err != nil {
			return err
		}
		copies := []string{
			`INSERT INTO policy_version_references (policy_version_id, reference_id, ordinal)
			 SELECT $2, reference_id, ordinal FROM policy_version_references WHERE policy_version_id = $1`,
			`INSERT INTO policy_version_contact_blocks (policy_version_id, contact_block_id, ordinal)
			 SELECT $2, contact_block_id, ordinal FROM policy_version_contact_blocks WHERE policy_version_id = $1`,
			`INSERT INTO policy_version_definitions (policy_version_id, definition_id)
			 SELECT $2, definition_id FROM policy_version_definitions WHERE policy_version_id = $1`,
			`INSERT INTO policy_version_relations (policy_version_id, related_policy_id, relation_type, ordinal)
			 SELECT $2, related_policy_id, relation_type, ordinal FROM policy_version_relations WHERE policy_version_id = $1`,
		}
		for _, q := range copies {
			if _, err := tx.Exec(ctx, q, fromVersionID, toVersionID); err != nil {
				return fmt.Errorf("VersionLinkStore.CopyForward %s -> %s: %w", fromVersionID, toVersionID, err)
			}
		}
		return nil
	})
}

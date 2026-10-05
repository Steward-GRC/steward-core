// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// Sentinel errors for the template lifecycle, mapped to gRPC codes by the
// handler.
var (
	// ErrTemplateNotFound is returned when a retire/delete targets a missing
	// (or already-retired) template.
	ErrTemplateNotFound = errors.New("template not found")
	// ErrTemplateReferenced is returned when a hard delete is blocked because
	// a policy still references the template (FK RESTRICT). Such templates can
	// only be retired.
	ErrTemplateReferenced = errors.New("template is referenced by policies and cannot be deleted")
	// ErrTemplateVersionNotEditable is returned when a section update targets a
	// version that is missing or no longer a draft (published versions are
	// immutable).
	ErrTemplateVersionNotEditable = errors.New("template version not found or not a draft")
	// ErrEmptyTemplateVersion is returned when publishing a version that has no
	// sections (an empty draft can't be published).
	ErrEmptyTemplateVersion = errors.New("cannot publish a template version with no sections")
)

const pgForeignKeyViolation = "23503"

// isForeignKeyViolation reports whether err is a Postgres foreign-key
// constraint violation (SQLSTATE 23503).
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgForeignKeyViolation
}

// TemplateStore persists Template + TemplateVersion aggregates.
type TemplateStore struct{ db *postgres.DB }

// NewTemplateStore returns a TemplateStore backed by db.
func NewTemplateStore(db *postgres.DB) *TemplateStore {
	return &TemplateStore{db: db}
}

// CreateTemplate inserts a new template row. OwnerCategoryID == uuid.Nil is
// persisted as NULL (templates may exist without a category owner).
func (s *TemplateStore) CreateTemplate(ctx context.Context, t domain.Template) (domain.Template, error) {
	var ownerID *uuid.UUID
	if t.OwnerCategoryID != uuid.Nil {
		ownerID = &t.OwnerCategoryID
	}
	err := s.db.Querier().QueryRow(ctx,
		`INSERT INTO templates (id, name, owner_category_id, code)
		 VALUES ($1, $2, $3, 'TPL-' || lpad(nextval('template_code_seq')::text, 3, '0'))
		 RETURNING id, code, created_at`,
		t.ID, t.Name, ownerID,
	).Scan(&t.ID, &t.Code, &t.CreatedAt)
	if err != nil {
		return domain.Template{}, fmt.Errorf("TemplateStore.CreateTemplate: %w", err)
	}
	return t, nil
}

// GetTemplate fetches a single template by ID.
func (s *TemplateStore) GetTemplate(ctx context.Context, id uuid.UUID) (domain.Template, error) {
	var (
		t       domain.Template
		ownerID *uuid.UUID
	)
	err := s.db.Querier().QueryRow(ctx,
		`SELECT id, code, name, owner_category_id, created_at
		   FROM templates WHERE id = $1`, id,
	).Scan(&t.ID, &t.Code, &t.Name, &ownerID, &t.CreatedAt)
	if err != nil {
		return domain.Template{}, fmt.Errorf("TemplateStore.GetTemplate: %w", err)
	}
	if ownerID != nil {
		t.OwnerCategoryID = *ownerID
	}
	return t, nil
}

// ListTemplates returns all active (non-retired) templates, optionally filtered
// by owner category. Retired templates are excluded so they can't be selected for
// new policies; their published versions still resolve via GetTemplateVersion
// for policies that already pinned them.
func (s *TemplateStore) ListTemplates(ctx context.Context, ownerCategoryID *uuid.UUID) ([]domain.Template, error) {
	q := `SELECT id, code, name, owner_category_id, created_at FROM templates WHERE retired_at IS NULL`
	args := []any{}
	if ownerCategoryID != nil {
		q += ` AND owner_category_id = $1`
		args = append(args, *ownerCategoryID)
	}
	q += ` ORDER BY name`
	rows, err := s.db.Querier().Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("TemplateStore.ListTemplates: %w", err)
	}
	defer rows.Close()
	out := []domain.Template{}
	for rows.Next() {
		var (
			t       domain.Template
			ownerID *uuid.UUID
		)
		if err := rows.Scan(&t.ID, &t.Code, &t.Name, &ownerID, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("TemplateStore.ListTemplates scan: %w", err)
		}
		if ownerID != nil {
			t.OwnerCategoryID = *ownerID
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RetireTemplate soft-deletes a template by stamping retired_at. Idempotent
// guard: the WHERE clause matches only an active row, so retiring an already-
// retired or missing template returns ErrTemplateNotFound. Versions and policy
// references are left untouched.
func (s *TemplateStore) RetireTemplate(ctx context.Context, id uuid.UUID) (domain.Template, error) {
	var (
		t       domain.Template
		ownerID *uuid.UUID
	)
	err := s.db.Querier().QueryRow(ctx,
		`UPDATE templates SET retired_at = now()
		   WHERE id = $1 AND retired_at IS NULL
		 RETURNING id, code, name, owner_category_id, created_at, retired_at`, id,
	).Scan(&t.ID, &t.Code, &t.Name, &ownerID, &t.CreatedAt, &t.RetiredAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Template{}, ErrTemplateNotFound
		}
		return domain.Template{}, fmt.Errorf("TemplateStore.RetireTemplate: %w", err)
	}
	if ownerID != nil {
		t.OwnerCategoryID = *ownerID
	}
	return t, nil
}

// RenameTemplate changes a template's name. It touches only the templates
// row, so no new version is created.
func (s *TemplateStore) RenameTemplate(ctx context.Context, id uuid.UUID, name string) (domain.Template, error) {
	var (
		t       domain.Template
		ownerID *uuid.UUID
	)
	err := s.db.Querier().QueryRow(ctx,
		`UPDATE templates SET name = $2, updated_at = now()
		   WHERE id = $1
		 RETURNING id, code, name, owner_category_id, created_at, retired_at`, id, name,
	).Scan(&t.ID, &t.Code, &t.Name, &ownerID, &t.CreatedAt, &t.RetiredAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Template{}, ErrTemplateNotFound
		}
		return domain.Template{}, fmt.Errorf("TemplateStore.RenameTemplate: %w", err)
	}
	if ownerID != nil {
		t.OwnerCategoryID = *ownerID
	}
	return t, nil
}

// DeleteTemplate hard-deletes a template and its versions. A document that
// pins a version or references the template blocks it with a foreign-key
// violation, returned as ErrTemplateReferenced so the caller can retire
// instead. Returns ErrTemplateNotFound when nothing was deleted.
func (s *TemplateStore) DeleteTemplate(ctx context.Context, id uuid.UUID) error {
	tag, err := s.db.Querier().Exec(ctx, `DELETE FROM templates WHERE id = $1`, id)
	if err != nil {
		if isForeignKeyViolation(err) {
			return ErrTemplateReferenced
		}
		return fmt.Errorf("TemplateStore.DeleteTemplate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTemplateNotFound
	}
	return nil
}

// UpdateTemplateVersionSections replaces the sections of a DRAFT version (the
// authoring "save"). The WHERE clause matches only a draft, so updating a
// missing or published version returns ErrTemplateVersionNotEditable.
func (s *TemplateStore) UpdateTemplateVersionSections(ctx context.Context, id uuid.UUID, sections []domain.Section) (domain.TemplateVersion, error) {
	sectionsJSON, err := json.Marshal(sections)
	if err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.UpdateTemplateVersionSections: marshal sections: %w", err)
	}
	var (
		tv      domain.TemplateVersion
		status  string
		outJSON []byte
	)
	err = s.db.Querier().QueryRow(ctx,
		`UPDATE template_versions SET sections = $2
		   WHERE id = $1 AND status = 'draft'
		 RETURNING id, template_id, version_no, status, sections, created_at`,
		id, sectionsJSON,
	).Scan(&tv.ID, &tv.TemplateID, &tv.VersionNo, &status, &outJSON, &tv.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.TemplateVersion{}, ErrTemplateVersionNotEditable
		}
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.UpdateTemplateVersionSections: %w", err)
	}
	tv.Status = domain.TemplateVersionStatus(status)
	if err := json.Unmarshal(outJSON, &tv.Sections); err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.UpdateTemplateVersionSections: unmarshal sections: %w", err)
	}
	return tv, nil
}

// ListTemplateVersions returns every version of a template, drafts included,
// newest first.
func (s *TemplateStore) ListTemplateVersions(ctx context.Context, templateID uuid.UUID) ([]domain.TemplateVersion, error) {
	rows, err := s.db.Querier().Query(ctx,
		`SELECT id, template_id, version_no, status, sections, created_at
		   FROM template_versions WHERE template_id = $1
		 ORDER BY version_no DESC`, templateID)
	if err != nil {
		return nil, fmt.Errorf("TemplateStore.ListTemplateVersions: %w", err)
	}
	defer rows.Close()
	out := []domain.TemplateVersion{}
	for rows.Next() {
		var (
			tv           domain.TemplateVersion
			status       string
			sectionsJSON []byte
		)
		if err := rows.Scan(&tv.ID, &tv.TemplateID, &tv.VersionNo, &status, &sectionsJSON, &tv.CreatedAt); err != nil {
			return nil, fmt.Errorf("TemplateStore.ListTemplateVersions scan: %w", err)
		}
		tv.Status = domain.TemplateVersionStatus(status)
		if err := json.Unmarshal(sectionsJSON, &tv.Sections); err != nil {
			return nil, fmt.Errorf("TemplateStore.ListTemplateVersions unmarshal: %w", err)
		}
		out = append(out, tv)
	}
	return out, rows.Err()
}

// DeleteTemplateVersion discards a DRAFT version. The WHERE clause matches only
// a draft, so deleting a missing or published version returns
// ErrTemplateVersionNotEditable (published versions are immutable).
func (s *TemplateStore) DeleteTemplateVersion(ctx context.Context, id uuid.UUID) error {
	tag, err := s.db.Querier().Exec(ctx,
		`DELETE FROM template_versions WHERE id = $1 AND status = 'draft'`, id)
	if err != nil {
		return fmt.Errorf("TemplateStore.DeleteTemplateVersion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrTemplateVersionNotEditable
	}
	return nil
}

// CreateTemplateVersion inserts a new draft TemplateVersion. version_no is
// assigned server-side as MAX(version_no)+1 for the template (starting at 1).
func (s *TemplateStore) CreateTemplateVersion(ctx context.Context, tv domain.TemplateVersion) (domain.TemplateVersion, error) {
	sectionsJSON, err := json.Marshal(tv.Sections)
	if err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.CreateTemplateVersion: marshal sections: %w", err)
	}
	err = s.db.Querier().QueryRow(ctx,
		`INSERT INTO template_versions (id, template_id, version_no, status, sections)
		 VALUES (
		   $1,
		   $2,
		   COALESCE((SELECT MAX(version_no) FROM template_versions WHERE template_id = $2), 0) + 1,
		   'draft',
		   $3
		 )
		 RETURNING version_no, status, created_at`,
		tv.ID, tv.TemplateID, sectionsJSON,
	).Scan(&tv.VersionNo, &tv.Status, &tv.CreatedAt)
	if err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.CreateTemplateVersion: %w", err)
	}
	return tv, nil
}

// PublishTemplateVersion flips the row to status='published' and returns the
// refreshed version.
func (s *TemplateStore) PublishTemplateVersion(ctx context.Context, id uuid.UUID) (domain.TemplateVersion, error) {
	// A published template version must have content — an empty draft can't be
	// published. The guard lives here (not at create) so drafts may start empty.
	tag, err := s.db.Querier().Exec(ctx,
		`UPDATE template_versions SET status = 'published'
		   WHERE id = $1 AND jsonb_array_length(sections) > 0`, id)
	if err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.PublishTemplateVersion: %w", err)
	}
	if tag.RowsAffected() == 0 {
		var n int
		if e := s.db.Querier().QueryRow(ctx,
			`SELECT jsonb_array_length(sections) FROM template_versions WHERE id = $1`, id,
		).Scan(&n); e != nil {
			return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.PublishTemplateVersion: version %s not found", id)
		}
		return domain.TemplateVersion{}, ErrEmptyTemplateVersion
	}
	return s.GetTemplateVersion(ctx, id)
}

// GetTemplateVersion fetches a single TemplateVersion by ID, including its
// JSONB sections column unmarshaled into []domain.Section.
func (s *TemplateStore) GetTemplateVersion(ctx context.Context, id uuid.UUID) (domain.TemplateVersion, error) {
	var (
		tv           domain.TemplateVersion
		status       string
		sectionsJSON []byte
	)
	err := s.db.Querier().QueryRow(ctx,
		`SELECT id, template_id, version_no, status, sections, created_at
		   FROM template_versions WHERE id = $1`, id,
	).Scan(&tv.ID, &tv.TemplateID, &tv.VersionNo, &status, &sectionsJSON, &tv.CreatedAt)
	if err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.GetTemplateVersion: %w", err)
	}
	tv.Status = domain.TemplateVersionStatus(status)
	if err := json.Unmarshal(sectionsJSON, &tv.Sections); err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.GetTemplateVersion: unmarshal sections: %w", err)
	}
	return tv, nil
}

// GetLatestPublishedVersion returns the published version with the highest
// version_no for templateID, or an error if no published version exists.
func (s *TemplateStore) GetLatestPublishedVersion(ctx context.Context, templateID uuid.UUID) (domain.TemplateVersion, error) {
	var id uuid.UUID
	err := s.db.Querier().QueryRow(ctx,
		`SELECT id FROM template_versions
		  WHERE template_id = $1 AND status = 'published'
		  ORDER BY version_no DESC
		  LIMIT 1`, templateID,
	).Scan(&id)
	if err != nil {
		return domain.TemplateVersion{}, fmt.Errorf("TemplateStore.GetLatestPublishedVersion: %w", err)
	}
	return s.GetTemplateVersion(ctx, id)
}

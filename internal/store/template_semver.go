// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// tvCols are the template_versions columns every read returns.
const tvCols = `id, template_id, version_no, status, sections, created_at,
		           major, minor, fix, change_type, published_at`

// tvSemverScan scans the semantic-version columns that follow the first six of tvCols.
type tvSemverScan struct {
	major, minor, fix int
	changeType        *string
	publishedAt       *time.Time
}

func (x *tvSemverScan) dests() []any {
	return []any{&x.major, &x.minor, &x.fix, &x.changeType, &x.publishedAt}
}

func (x *tvSemverScan) apply(tv *domain.TemplateVersion) {
	tv.Semver = domain.SemVer{Major: x.major, Minor: x.minor, Fix: x.fix}
	if x.changeType != nil {
		tv.ChangeType = domain.ChangeType(*x.changeType)
	}
	tv.PublishedAt = x.publishedAt
}

// templateBase is the highest numbered version of a template: what the next version is compared
// with and numbered from. has is false before the first publish.
type templateBase struct {
	has      bool
	semver   domain.SemVer
	sections []domain.Section
}

// loadTemplateBase reads the highest numbered published or archived version of a template other
// than exclude.
func loadTemplateBase(ctx context.Context, q rowQuerier, templateID, exclude uuid.UUID) (templateBase, error) {
	var (
		raw               []byte
		major, minor, fix int
	)
	err := q.QueryRow(ctx,
		`SELECT sections, major, minor, fix FROM template_versions
		  WHERE template_id = $1 AND id <> $2 AND status IN ('published', 'archived')
		  ORDER BY major DESC, minor DESC, fix DESC LIMIT 1`, templateID, exclude,
	).Scan(&raw, &major, &minor, &fix)
	if errors.Is(err, pgx.ErrNoRows) {
		return templateBase{}, nil
	}
	if err != nil {
		return templateBase{}, fmt.Errorf("read the base template version: %w", err)
	}
	b := templateBase{has: true, semver: domain.SemVer{Major: major, Minor: minor, Fix: fix}}
	if err := json.Unmarshal(raw, &b.sections); err != nil {
		return templateBase{}, fmt.Errorf("unmarshal the base template sections: %w", err)
	}
	return b, nil
}

// number classifies sections against the base and returns the change and the number it gives.
// The first version is Major and 1.0.0.
func (b templateBase) number(sections []domain.Section) (domain.Classification, domain.SemVer) {
	if !b.has {
		return domain.Classification{Type: domain.ChangeTypeMajor, Reasons: []string{"first version"}}, domain.SemVer{Major: 1}
	}
	cls := domain.ClassifyTemplateChange(b.sections, sections)
	return cls, b.semver.Bump(cls.Type)
}

// projectTemplateDraft fills a draft's number and change type as a publish would give them.
func projectTemplateDraft(tv *domain.TemplateVersion, b templateBase) {
	cls, sv := b.number(tv.Sections)
	tv.Semver, tv.ChangeType, tv.Projected = sv, cls.Type, true
}

// projectTemplateVersion projects a draft; other versions already carry their stored number.
func projectTemplateVersion(ctx context.Context, q rowQuerier, tv *domain.TemplateVersion) error {
	if tv.Status != domain.TemplateVersionStatusDraft {
		return nil
	}
	b, err := loadTemplateBase(ctx, q, tv.TemplateID, tv.ID)
	if err != nil {
		return err
	}
	projectTemplateDraft(tv, b)
	return nil
}

type templateRow struct {
	TemplateID uuid.UUID
	Status     domain.TemplateVersionStatus
	Sections   []domain.Section
}

// loadDraftInTx locks a template version and reads what publishing needs.
func (s *TemplateStore) loadDraftInTx(ctx context.Context, tx pgx.Tx, id uuid.UUID) (templateRow, error) {
	var (
		r      templateRow
		status string
		raw    []byte
	)
	if err := tx.QueryRow(ctx,
		`SELECT template_id, status, sections FROM template_versions WHERE id = $1 FOR UPDATE`, id,
	).Scan(&r.TemplateID, &status, &raw); err != nil {
		return templateRow{}, err
	}
	r.Status = domain.TemplateVersionStatus(status)
	if err := json.Unmarshal(raw, &r.Sections); err != nil {
		return templateRow{}, fmt.Errorf("unmarshal sections: %w", err)
	}
	return r, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

var (
	// ErrChangeTypeLocked means a change type was set on a version that is not a draft or in
	// review: a published, rejected or archived version keeps the type it has.
	ErrChangeTypeLocked = errors.New("the change type of this version can no longer be set")
	// ErrFirstVersion means a change type was set on a policy's first version, which is always
	// 1.0.0 and has no earlier version to compare with.
	ErrFirstVersion = errors.New("the first version of a document is always 1.0.0 and has no change type to set")
)

// rowQuerier is what the semantic-version reads need: the pool or a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// pvSemverCols are the version columns that hold the semantic version and the change-type
// record, appended to every policy_versions read.
const pvSemverCols = `major, minor, fix, suggested_change_type, change_type, requested_change_type,
		           change_type_reason, change_type_set_by, change_type_agreed_by`

// pvSemverScan scans pvSemverCols.
type pvSemverScan struct {
	major, minor, fix int
	suggested, ctype  *string
	requested         *string
	reason            string
	setBy             *uuid.UUID
	agreed            pgtype.FlatArray[uuid.UUID]
}

func (x *pvSemverScan) dests() []any {
	return []any{&x.major, &x.minor, &x.fix, &x.suggested, &x.ctype, &x.requested, &x.reason, &x.setBy, &x.agreed}
}

func (x *pvSemverScan) apply(pv *domain.PolicyVersion) {
	pv.Semver = domain.SemVer{Major: x.major, Minor: x.minor, Fix: x.fix}
	pv.Change = domain.ChangeState{Reason: x.reason, AgreedBy: []uuid.UUID(x.agreed)}
	if x.suggested != nil {
		pv.Change.Suggested = domain.ChangeType(*x.suggested)
	}
	if x.ctype != nil {
		pv.Change.Type = domain.ChangeType(*x.ctype)
	}
	if x.requested != nil {
		pv.Change.Requested = domain.ChangeType(*x.requested)
	}
	if x.setBy != nil {
		pv.Change.SetBy = *x.setBy
	}
}

func semverOf(major, minor, fix *int) domain.SemVer {
	var v domain.SemVer
	if major != nil {
		v.Major = *major
	}
	if minor != nil {
		v.Minor = *minor
	}
	if fix != nil {
		v.Fix = *fix
	}
	return v
}

func nullableChange(c domain.ChangeType) *string {
	if c == "" {
		return nil
	}
	s := string(c)
	return &s
}

func nullableUUID(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	return &u
}

// StatusOption adds to SetVersionStatus.
type StatusOption func(*statusOpts)

type statusOpts struct {
	pick   domain.ChangeType
	reason string
	actor  uuid.UUID
}

// WithChangePick carries the author's change-type pick (and the reason, which a lowering needs)
// into a submit. It is read only for the move from draft to in_review.
func WithChangePick(ct domain.ChangeType, reason string, actor uuid.UUID) StatusOption {
	return func(o *statusOpts) { o.pick, o.reason, o.actor = ct, reason, actor }
}

// versionClassification is core's classification of one version against the numbered version
// before it.
type versionClassification struct {
	cls domain.Classification
	// hasBase is false for the first version, which is always Major and numbered 1.0.0.
	hasBase bool
	base    domain.SemVer
}

// templateRefIn loads the template a version is pinned to; nil is freeform.
func templateRefIn(ctx context.Context, q rowQuerier, tvID *uuid.UUID) (*domain.TemplateRef, error) {
	if tvID == nil {
		return nil, nil
	}
	var (
		ref  domain.TemplateRef
		raw  []byte
		secs []domain.Section
	)
	if err := q.QueryRow(ctx, `SELECT template_id, sections FROM template_versions WHERE id = $1`, *tvID).Scan(&ref.TemplateID, &raw); err != nil {
		return nil, fmt.Errorf("read template version %s: %w", *tvID, err)
	}
	if err := json.Unmarshal(raw, &secs); err != nil {
		return nil, fmt.Errorf("unmarshal template sections: %w", err)
	}
	ref.Sections = secs
	return &ref, nil
}

// classifyVersion compares a version with the highest numbered version of its policy.
func classifyVersion(ctx context.Context, q rowQuerier, versionID uuid.UUID) (versionClassification, error) {
	var (
		policyID uuid.UUID
		content  string
		tvID     *uuid.UUID
	)
	if err := q.QueryRow(ctx,
		`SELECT policy_id, content::text, template_version_id FROM policy_versions WHERE id = $1`, versionID,
	).Scan(&policyID, &content, &tvID); err != nil {
		return versionClassification{}, fmt.Errorf("classify: read version %s: %w", versionID, err)
	}
	var (
		baseContent string
		baseTV      *uuid.UUID
		major       int
		minor       int
		fix         int
	)
	err := q.QueryRow(ctx,
		`SELECT content::text, template_version_id, major, minor, fix
		   FROM policy_versions
		  WHERE policy_id = $1 AND id <> $2 AND status IN ('published', 'superseded', 'archived')
		  ORDER BY version_no DESC LIMIT 1`, policyID, versionID,
	).Scan(&baseContent, &baseTV, &major, &minor, &fix)
	if errors.Is(err, pgx.ErrNoRows) {
		logger.Ctx(ctx).Debug("classify: first version of the document", log.F("version_id", versionID.String()))
		return versionClassification{cls: domain.Classification{Type: domain.ChangeTypeMajor, Reasons: []string{"first version"}}}, nil
	}
	if err != nil {
		return versionClassification{}, fmt.Errorf("classify: read base version: %w", err)
	}
	fromTpl, err := templateRefIn(ctx, q, baseTV)
	if err != nil {
		return versionClassification{}, fmt.Errorf("classify: %w", err)
	}
	toTpl, err := templateRefIn(ctx, q, tvID)
	if err != nil {
		return versionClassification{}, fmt.Errorf("classify: %w", err)
	}
	cls := domain.ClassifyPolicyChange(
		domain.PolicyRevision{ContentJSON: baseContent, Template: fromTpl},
		domain.PolicyRevision{ContentJSON: content, Template: toTpl})
	logger.Ctx(ctx).Debug("classify: version compared with the numbered one before",
		log.F("version_id", versionID.String()), log.F("suggested", string(cls.Type)), log.F("reasons", fmt.Sprint(cls.Reasons)))
	return versionClassification{cls: cls, hasBase: true, base: domain.SemVer{Major: major, Minor: minor, Fix: fix}}, nil
}

// loadChangeState reads a version's stored change-type record.
func loadChangeState(ctx context.Context, q rowQuerier, versionID uuid.UUID) (domain.ChangeState, error) {
	var x pvSemverScan
	if err := q.QueryRow(ctx, `SELECT `+pvSemverCols+` FROM policy_versions WHERE id = $1`, versionID).Scan(x.dests()...); err != nil {
		return domain.ChangeState{}, fmt.Errorf("read change state of %s: %w", versionID, err)
	}
	var pv domain.PolicyVersion
	x.apply(&pv)
	return pv.Change, nil
}

// writeChangeState stores a version's change-type record (not the number).
func writeChangeState(ctx context.Context, tx pgx.Tx, versionID uuid.UUID, st domain.ChangeState) error {
	if st.AgreedBy == nil {
		st.AgreedBy = []uuid.UUID{} // the column is NOT NULL; a nil slice would write NULL
	}
	_, err := tx.Exec(ctx,
		`UPDATE policy_versions
		    SET suggested_change_type = $2, change_type = $3, requested_change_type = $4,
		        change_type_reason = $5, change_type_set_by = $6, change_type_agreed_by = $7
		  WHERE id = $1`,
		versionID, nullableChange(st.Suggested), nullableChange(st.Type), nullableChange(st.Requested),
		st.Reason, nullableUUID(st.SetBy), pgtype.FlatArray[uuid.UUID](st.AgreedBy))
	if err != nil {
		return fmt.Errorf("write change state of %s: %w", versionID, err)
	}
	return nil
}

// recordSubmit stores the classification at submit and applies the author's pick, in tx.
func recordSubmit(ctx context.Context, tx pgx.Tx, versionID uuid.UUID, o statusOpts) error {
	vc, err := classifyVersion(ctx, tx, versionID)
	if err != nil {
		return err
	}
	prev, err := loadChangeState(ctx, tx, versionID)
	if err != nil {
		return err
	}
	if !vc.hasBase {
		if o.pick != "" {
			logger.Ctx(ctx).Debug("submit: change-type pick ignored, the first version is always 1.0.0", log.F("version_id", versionID.String()))
		}
		return writeChangeState(ctx, tx, versionID, domain.ChangeState{Suggested: domain.ChangeTypeMajor, Type: domain.ChangeTypeMajor})
	}
	next, err := domain.ResolveChange(prev, vc.cls.Type, o.pick, o.reason, o.actor)
	if err != nil {
		logger.Ctx(ctx).Debug("submit refused: change type", log.F("version_id", versionID.String()), log.F("error", err.Error()))
		return err
	}
	logger.Ctx(ctx).Debug("submit: change type recorded", log.F("version_id", versionID.String()),
		log.F("suggested", string(next.Suggested)), log.F("type", string(next.Type)), log.F("requested", string(next.Requested)))
	return writeChangeState(ctx, tx, versionID, next)
}

// projectVersion fills the number and change type a publish would give a draft or a version in
// review, from its content as it is now. Nothing is stored.
func projectVersion(ctx context.Context, q rowQuerier, pv *domain.PolicyVersion) error {
	if !pv.Status.HoldsDraftSlot() {
		return nil
	}
	vc, err := classifyVersion(ctx, q, pv.ID)
	if err != nil {
		return err
	}
	final := domain.FinalizeChange(pv.Change, vc.cls.Type)
	if !vc.hasBase {
		final = domain.ChangeTypeMajor
	}
	pv.Change.Suggested = vc.cls.Type
	pv.Change.Type = final
	pv.Semver = vc.base.Bump(final)
	pv.Projected = true
	return nil
}

// SetVersionChangeType sets the author's pick for a draft or a version in review. A pick at or
// above the suggestion takes effect; a pick below it needs a reason and waits for the approvers'
// agreement (RecordChangeTypeAgreement) before it lowers anything.
func (s *PolicyStore) SetVersionChangeType(ctx context.Context, versionID uuid.UUID, ct domain.ChangeType, reason string, actor uuid.UUID) (domain.PolicyVersion, error) {
	if !ct.Valid() {
		return domain.PolicyVersion{}, fmt.Errorf("PolicyStore.SetVersionChangeType: %q is not a change type", ct)
	}
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if err := lockEditableVersion(ctx, tx, versionID); err != nil {
			return err
		}
		vc, err := classifyVersion(ctx, tx, versionID)
		if err != nil {
			return err
		}
		if !vc.hasBase {
			return ErrFirstVersion
		}
		prev, err := loadChangeState(ctx, tx, versionID)
		if err != nil {
			return err
		}
		next, err := domain.ResolveChange(prev, vc.cls.Type, ct, reason, actor)
		if err != nil {
			return err
		}
		return writeChangeState(ctx, tx, versionID, next)
	})
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("PolicyStore.SetVersionChangeType: %w", err)
	}
	return s.GetPolicyVersion(ctx, versionID)
}

// RecordChangeTypeAgreement stores who agreed to the pending lowering of a version in review
// and applies it. Workflow writes it once the approvers' quorum says yes.
func (s *PolicyStore) RecordChangeTypeAgreement(ctx context.Context, versionID uuid.UUID, agreedBy []uuid.UUID) (domain.PolicyVersion, error) {
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		if err := lockEditableVersion(ctx, tx, versionID); err != nil {
			return err
		}
		prev, err := loadChangeState(ctx, tx, versionID)
		if err != nil {
			return err
		}
		next, err := domain.RecordAgreement(prev, agreedBy)
		if err != nil {
			return err
		}
		return writeChangeState(ctx, tx, versionID, next)
	})
	if err != nil {
		return domain.PolicyVersion{}, fmt.Errorf("PolicyStore.RecordChangeTypeAgreement: %w", err)
	}
	return s.GetPolicyVersion(ctx, versionID)
}

// lockEditableVersion locks the row and requires a draft or a version in review.
func lockEditableVersion(ctx context.Context, tx pgx.Tx, versionID uuid.UUID) error {
	var status string
	if err := tx.QueryRow(ctx, `SELECT status FROM policy_versions WHERE id = $1 FOR UPDATE`, versionID).Scan(&status); err != nil {
		return err
	}
	if !domain.PolicyVersionStatus(status).HoldsDraftSlot() {
		return ErrChangeTypeLocked
	}
	return nil
}

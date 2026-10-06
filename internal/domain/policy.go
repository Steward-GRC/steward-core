// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidVersionStatusTransition is the sentinel wrapped by
// ValidateVersionStatusTransition when from→to is not a legal stored-status
// change. Callers use errors.Is to map it to a FailedPrecondition gRPC code.
var ErrInvalidVersionStatusTransition = errors.New("invalid policy version status transition")

type Sensitivity string

const (
	SensitivityStandard  Sensitivity = "standard"
	SensitivitySensitive Sensitivity = "sensitive"
)

// DocumentType tells policies and procedures apart. They share everything
// except acknowledgements, which procedures never raise. The type picks the
// number prefix (POL- or PRC-) and its own sequence per category.
type DocumentType string

const (
	DocumentTypePolicy    DocumentType = "policy"
	DocumentTypeProcedure DocumentType = "procedure"
)

type PolicyVersionStatus string

const (
	PolicyVersionStatusDraft      PolicyVersionStatus = "draft"
	PolicyVersionStatusPublished  PolicyVersionStatus = "published"
	PolicyVersionStatusSuperseded PolicyVersionStatus = "superseded"
	PolicyVersionStatusArchived   PolicyVersionStatus = "archived"
)

type Policy struct {
	ID             uuid.UUID
	HomeCategoryID uuid.UUID
	// Number is POL-<category code>-<sequence>, derived on read from the home
	// category's current code, so a category rename renumbers its documents'
	// display numbers.
	Number string
	// Sequence is the NNNNNN segment, unique within the home category. It
	// survives renames; a move takes a fresh one from the target category.
	Sequence int
	// DocumentType is set at creation and never changes. The zero value
	// reads as DocumentTypePolicy.
	DocumentType              DocumentType
	Title                     string
	Sensitivity               Sensitivity
	EffectiveDate             *time.Time
	ReviewDate                *time.Time
	OwnerUserID               uuid.UUID
	CurrentPublishedVersionID uuid.UUID // uuid.Nil until first publish
	CurrentDraftVersionID     uuid.UUID // uuid.Nil if no working draft
	TemplateID                uuid.UUID // uuid.Nil = use category default
	// TemplateNone means freeform: TemplateID is ignored and the category
	// chain isn't consulted.
	TemplateNone bool
	// nil inherits from the category.
	AckTriggers         *AckTrigger
	AckAudienceOverride []uuid.UUID // nil/empty = subtree
	// RetiredAt is nil while active. A retired policy is hidden and obligates
	// no one; completed acknowledgements are kept.
	RetiredAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
	// CurrentVersionNo/CurrentVersionStatus describe whichever version is
	// current: CurrentPublishedVersionID if set, else CurrentDraftVersionID.
	// Zero/empty when the policy has neither (never saved a draft).
	CurrentVersionNo     int
	CurrentVersionStatus PolicyVersionStatus
}

func NewPolicy(title string, homeCategoryID uuid.UUID, sensitivity Sensitivity, ownerUserID uuid.UUID) (Policy, error) {
	if title == "" {
		return Policy{}, fmt.Errorf("policy title must not be empty")
	}
	now := time.Now().UTC()
	return Policy{
		ID:             uuid.New(),
		HomeCategoryID: homeCategoryID,
		Title:          title,
		Sensitivity:    sensitivity,
		DocumentType:   DocumentTypePolicy,
		OwnerUserID:    ownerUserID,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

type PolicyVersion struct {
	ID                  uuid.UUID
	PolicyID            uuid.UUID
	VersionNo           int // 0 = not yet assigned
	Status              PolicyVersionStatus
	TemplateVersionID   uuid.UUID // pinned
	ContentJSON         string    // opaque Lexical state (jsonb)
	CreatedAt           time.Time
	PublishedAt         time.Time
	CreatedBy           uuid.UUID
	SupersedesVersionID uuid.UUID // uuid.Nil = first version
	// ProposedTitle is a staged rename of a published policy, applied when this
	// draft is published.
	ProposedTitle *string
}

// NewPolicyVersionDraft creates an unsaved draft. VersionNo is assigned by the store on insert.
func NewPolicyVersionDraft(policyID, templateVersionID, createdBy uuid.UUID, contentJSON string) (PolicyVersion, error) {
	if contentJSON == "" {
		return PolicyVersion{}, fmt.Errorf("content must not be empty")
	}
	return PolicyVersion{
		ID:                uuid.New(),
		PolicyID:          policyID,
		Status:            PolicyVersionStatusDraft,
		TemplateVersionID: templateVersionID,
		ContentJSON:       contentJSON,
		CreatedAt:         time.Now().UTC(),
		CreatedBy:         createdBy,
	}, nil
}

// IsValidPolicyVersionStatus reports whether s is one of the four stored
// policy-version statuses (matches the policy_version_status DB enum).
func IsValidPolicyVersionStatus(s PolicyVersionStatus) bool {
	switch s {
	case PolicyVersionStatusDraft, PolicyVersionStatusPublished,
		PolicyVersionStatusSuperseded, PolicyVersionStatusArchived:
		return true
	default:
		return false
	}
}

// versionStatusTransitions defines the legal stored-status changes for the core
// version state machine. The approval lifecycle (in_review/approved/...) lives
// in the Workflow service; here we only guard the four stored statuses.
//
//   - draft → published   : publish on approval
//   - published → draft    : withdraw a published version back to working draft
//   - published → superseded: a newer version was published
//   - published → archived : retire a live version
//   - draft → archived     : retire an un-published draft
//   - superseded → archived: archive an old version
//
// Self-transitions (status == current) are always allowed (idempotent). The
// obviously-invalid published → draft case is permitted ONLY here because it is
// the explicit withdraw path the saga drives; the workflow layer is responsible
// for not abusing it.
var versionStatusTransitions = map[PolicyVersionStatus]map[PolicyVersionStatus]bool{
	PolicyVersionStatusDraft: {
		PolicyVersionStatusPublished: true,
		PolicyVersionStatusArchived:  true,
	},
	PolicyVersionStatusPublished: {
		PolicyVersionStatusDraft:      true, // withdraw
		PolicyVersionStatusSuperseded: true,
		PolicyVersionStatusArchived:   true,
	},
	PolicyVersionStatusSuperseded: {
		PolicyVersionStatusArchived: true,
	},
	// Archived is terminal.
}

// ValidateVersionStatusTransition returns an error when moving from → to is not
// a legal stored-status transition. A no-op (from == to) is always allowed.
func ValidateVersionStatusTransition(from, to PolicyVersionStatus) error {
	if !IsValidPolicyVersionStatus(to) {
		return fmt.Errorf("invalid target status %q", to)
	}
	if from == to {
		return nil
	}
	if versionStatusTransitions[from][to] {
		return nil
	}
	return fmt.Errorf("%w: %q→%q", ErrInvalidVersionStatusTransition, from, to)
}

// Publish returns an immutable published copy. VersionNo must be set by the store before calling.
func (pv PolicyVersion) Publish() (PolicyVersion, error) {
	if pv.VersionNo == 0 {
		return PolicyVersion{}, fmt.Errorf("VersionNo must be assigned before publishing")
	}
	published := pv
	published.Status = PolicyVersionStatusPublished
	published.PublishedAt = time.Now().UTC()
	return published, nil
}

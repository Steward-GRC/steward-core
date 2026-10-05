// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// AckTrigger controls when acknowledgement obligations are raised for a category
// or policy.
type AckTrigger string

const (
	AckNone      AckTrigger = "none"
	AckOnPublish AckTrigger = "on-publish"
	AckOnChange  AckTrigger = "on-change"
)

// ReviewCadence controls how frequently a category's policy set must be reviewed.
type ReviewCadence string

const (
	CadenceNone     ReviewCadence = "none"
	CadenceAnnual   ReviewCadence = "annual"
	CadenceBiennial ReviewCadence = "biennial"
	CadenceOnDate   ReviewCadence = "on-date"
)

type Category struct {
	ID                uuid.UUID
	ParentID          uuid.UUID // uuid.Nil = root
	Name              string
	Slug              string
	DefaultTemplateID uuid.UUID
	// DefaultTemplateNone means freeform: DefaultTemplateID is ignored and
	// inheritance stops here. Otherwise uuid.Nil inherits and an id names the
	// template.
	DefaultTemplateNone bool
	DefaultWorkflowID   uuid.UUID
	Owners              []uuid.UUID
	// AudienceGroupIDs names the directory groups that must acknowledge this
	// category's documents. nil inherits from the nearest ancestor that sets
	// it; an empty slice is an explicit "no audience" that stops inheritance.
	// ExclusionGroupIDs and AckEveryone follow the same rule.
	AudienceGroupIDs *[]string
	AckTriggers      AckTrigger
	ReviewCadence    ReviewCadence
	ReviewDate       *time.Time
	// ExclusionGroupIDs names directory groups left out of the audience.
	ExclusionGroupIDs *[]string
	// AckEveryone, when it resolves to true, makes every user the audience.
	// AckTriggers still decides whether an acknowledgement is needed at all.
	AckEveryone *bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func NewCategory(name, slug string, parentID uuid.UUID) (Category, error) {
	if name == "" {
		return Category{}, fmt.Errorf("category name must not be empty")
	}
	if slug == "" {
		return Category{}, fmt.Errorf("category slug must not be empty")
	}
	now := time.Now().UTC()
	return Category{
		ID:        uuid.New(),
		ParentID:  parentID,
		Name:      name,
		Slug:      slug,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

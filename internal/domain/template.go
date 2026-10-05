// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

type BlockType string

const (
	BlockTypeBoilerplate BlockType = "boilerplate"
	BlockTypeEditable    BlockType = "editable"
)

type Block struct {
	Type        BlockType
	ContentJSON string // non-empty only for boilerplate
}

type Section struct {
	Key      string
	Title    string
	Order    int
	Level    int  // heading depth H1-H5 (1-5); 0 normalises to 1
	Required bool // must be filled before a policy can be submitted
	Blocks   []Block
}

type TemplateVersionStatus string

const (
	TemplateVersionStatusDraft     TemplateVersionStatus = "draft"
	TemplateVersionStatusPublished TemplateVersionStatus = "published"
	TemplateVersionStatusArchived  TemplateVersionStatus = "archived"
)

type Template struct {
	ID              uuid.UUID
	Code            string // human-readable display code, TPL-NNN (assigned by store)
	Name            string
	OwnerCategoryID uuid.UUID
	CreatedAt       time.Time
	RetiredAt       *time.Time // nil = active; set = soft-deleted (hidden from listings)
}

type TemplateVersion struct {
	ID         uuid.UUID
	TemplateID uuid.UUID
	VersionNo  int
	Status     TemplateVersionStatus
	Sections   []Section // sorted by Order ascending
	CreatedAt  time.Time
}

func NewTemplate(name string, ownerCategoryID uuid.UUID) (Template, error) {
	if name == "" {
		return Template{}, fmt.Errorf("template name must not be empty")
	}
	return Template{ID: uuid.New(), Name: name, OwnerCategoryID: ownerCategoryID, CreatedAt: time.Now().UTC()}, nil
}

// NewTemplateVersion creates a new draft TemplateVersion (version_no assigned
// by store). A draft may start empty — sections are added via the authoring
// editor (UpdateTemplateVersionSections). The "at least one section" invariant
// is enforced at publish time, not here.
func NewTemplateVersion(templateID string, sections []Section) (TemplateVersion, error) {
	tid, err := uuid.Parse(templateID)
	if err != nil {
		return TemplateVersion{}, fmt.Errorf("invalid templateID: %w", err)
	}
	sorted := make([]Section, len(sections))
	copy(sorted, sections)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Order < sorted[j].Order })
	return TemplateVersion{
		ID:         uuid.New(),
		TemplateID: tid,
		Status:     TemplateVersionStatusDraft,
		Sections:   sorted,
		CreatedAt:  time.Now().UTC(),
	}, nil
}

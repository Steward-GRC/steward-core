// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package validate checks Lexical editor JSON against a pinned
// TemplateVersion. The collab service does only an envelope pre-check and
// delegates here through UpdateDraftContent.
//
// The strict Validate below runs only for content that is already
// section-shaped: ValidateDraft (draft.go) hands over when the first root
// child is a policy-section node. Nothing the editor produces today has that
// shape (see domain.ContentValidator).
//
// Rules enforced by Validate:
//  1. Root has exactly TemplateVersion.sections.length policy-section children, in order.
//  2. Each section's children are policy-boilerplate and/or policy-editable-region nodes.
//  3. Each policy-boilerplate.content matches a boilerplate block in the template
//     section byte-for-byte.
//  4. Each policy-editable-region.regionKey matches an editable block's regionKey
//     in the template section. domain.Block has no RegionKey field, so the
//     caller passes ContentJSON as the region key; for editable blocks it is
//     empty, and this rule can only match the empty string.
//  5. No unknown node types appear anywhere in the document.
//
// This list is the source of truth for what Validate does.
package validate

import (
	"encoding/json"
	"fmt"
)

// TemplateBlock mirrors a block within a TemplateVersion section.
type TemplateBlock struct {
	Type      string // "boilerplate" | "editable"
	Content   string // non-empty when Type == "boilerplate"
	RegionKey string // non-empty when Type == "editable"
}

// TemplateSection mirrors a section entry in a TemplateVersion.
type TemplateSection struct {
	Key    string
	Order  int
	Blocks []TemplateBlock
}

// Validate checks that contentJSON (a Lexical EditorState JSON string) conforms
// to the structure defined by sections. Returns a non-nil error describing the
// first violation encountered.
func Validate(contentJSON string, sections []TemplateSection) error {
	if contentJSON == "" {
		return fmt.Errorf("empty content JSON")
	}

	var doc struct {
		Root struct {
			Type     string           `json:"type"`
			Children []map[string]any `json:"children"`
		} `json:"root"`
	}
	if err := json.Unmarshal([]byte(contentJSON), &doc); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}

	rootChildren := doc.Root.Children
	if len(rootChildren) != len(sections) {
		return fmt.Errorf("root has %d children; template requires %d sections",
			len(rootChildren), len(sections))
	}

	for i, tmpl := range sections {
		node := rootChildren[i]
		nodeType, _ := node["type"].(string)
		if nodeType != "policy-section" {
			return fmt.Errorf("root[%d]: expected policy-section, got %q", i, nodeType)
		}
		if key, _ := node["sectionKey"].(string); key != tmpl.Key {
			return fmt.Errorf("root[%d]: sectionKey %q != template key %q", i, key, tmpl.Key)
		}
		orderRaw, _ := node["order"].(float64) // JSON numbers decode as float64
		if int(orderRaw) != tmpl.Order {
			return fmt.Errorf("root[%d]: order %d != template order %d", i, int(orderRaw), tmpl.Order)
		}
		children, _ := node["children"].([]any)
		if err := validateSectionChildren(i, children, tmpl.Blocks); err != nil {
			return err
		}
	}
	return nil
}

func validateSectionChildren(sectionIdx int, children []any, blocks []TemplateBlock) error {
	// Build lookup sets from the template blocks.
	validBoilerplate := make(map[string]bool) // key: content string
	validRegionKeys := make(map[string]bool)  // key: regionKey string
	for _, b := range blocks {
		switch b.Type {
		case "boilerplate":
			validBoilerplate[b.Content] = true
		case "editable":
			validRegionKeys[b.RegionKey] = true
		}
	}

	for _, raw := range children {
		child, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("section[%d]: non-object child", sectionIdx)
		}
		childType, _ := child["type"].(string)
		switch childType {
		case "policy-boilerplate":
			content, _ := child["content"].(string)
			if !validBoilerplate[content] {
				return fmt.Errorf("section[%d]: boilerplate content does not match template", sectionIdx)
			}
		case "policy-editable-region":
			regionKey, _ := child["regionKey"].(string)
			if !validRegionKeys[regionKey] {
				return fmt.Errorf("section[%d]: editable region regionKey %q not found in template", sectionIdx, regionKey)
			}
		case "":
			return fmt.Errorf("section[%d]: child missing type field", sectionIdx)
		default:
			return fmt.Errorf("section[%d]: unknown node type %q in section", sectionIdx, childType)
		}
	}
	return nil
}

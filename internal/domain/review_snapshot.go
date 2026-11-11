// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"sort"
	"strings"

	"github.com/google/uuid"
)

// ReviewSnapshotView is an approver's snapshot of a version beside the
// version's current text, which is all that is needed to diff what changed
// since they last reviewed it.
type ReviewSnapshotView struct {
	// Before is the text the approver last reviewed.
	Before string
	// Current is the version's text now.
	Current string
	// TemplateVersionID is the version's pinned template version; uuid.Nil for
	// a freeform version.
	TemplateVersionID uuid.UUID
	// Created is true when this read took the snapshot.
	Created bool
}

// FreeformSections returns the section definitions for a version that has no
// pinned template: every section key present in either text, sorted by key,
// with the key as the title ("extra-notes" becomes "Extra notes"). It
// errors when either text cannot be parsed.
func FreeformSections(beforeJSON, currentJSON string) ([]Section, error) {
	before, err := parseContent(beforeJSON)
	if err != nil {
		return nil, err
	}
	current, err := parseContent(currentJSON)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range current {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	out := make([]Section, len(sorted))
	for i, k := range sorted {
		out[i] = Section{Key: k, Title: titleFromKey(k), Order: i + 1}
	}
	return out, nil
}

// titleFromKey turns a section key into a display title: dashes become
// spaces and the first letter is capitalised.
func titleFromKey(key string) string {
	spaced := strings.ReplaceAll(key, "-", " ")
	if spaced == "" {
		return spaced
	}
	return strings.ToUpper(spaced[:1]) + spaced[1:]
}

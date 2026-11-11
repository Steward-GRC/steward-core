// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"encoding/json"
	"strings"
)

// maxDraftScanDepth bounds the walk over a pathologically nested document.
const maxDraftScanDepth = 64

// DraftUntouched reports whether a draft still holds only what it started with: no text, or text
// that is only the pinned template's section titles. sectionTitles is empty for a freeform draft.
// Malformed content reports false, so an unreadable draft is never overwritten.
func DraftUntouched(contentJSON string, sectionTitles []string) bool {
	if strings.TrimSpace(contentJSON) == "" {
		return true
	}
	var doc struct {
		Root json.RawMessage `json:"root"`
	}
	if err := json.Unmarshal([]byte(contentJSON), &doc); err != nil {
		return false
	}
	if len(doc.Root) == 0 {
		return true
	}
	titles := make(map[string]bool, len(sectionTitles))
	for _, t := range sectionTitles {
		titles[strings.TrimSpace(t)] = true
	}
	return onlyTitles(doc.Root, titles, 0)
}

// onlyTitles is true when every non-blank text under raw is a section title.
func onlyTitles(raw json.RawMessage, titles map[string]bool, depth int) bool {
	if depth > maxDraftScanDepth {
		return false
	}
	var node struct {
		Text     string            `json:"text"`
		Children []json.RawMessage `json:"children"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return false
	}
	if text := strings.TrimSpace(node.Text); text != "" && !titles[text] {
		return false
	}
	for _, c := range node.Children {
		if !onlyTitles(c, titles, depth+1) {
			return false
		}
	}
	return true
}

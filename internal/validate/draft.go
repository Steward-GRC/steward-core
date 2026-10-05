// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"encoding/json"
	"fmt"
)

// SectionNodeType is the Lexical node type that encodes a template-defined
// section at the document root: a fully template-structured document would be
// an array of these. No content the editor produces today has this shape.
const SectionNodeType = "policy-section"

// ValidateDraft validates a draft-tier content snapshot (the collab
// checkpoint path, PolicyService.UpdateDraftContent).
//
// It is deliberately SHAPE-AWARE rather than shape-imposing, because the
// platform currently has two live content vocabularies:
//
//   - The flat authoring vocabulary the editor actually produces and that
//     PolicyService.SaveDraft (the autosave endpoint) accepts: root children
//     are ordinary Lexical block nodes (heading, paragraph, list, …). Every
//     draft and published version in existence is this shape.
//   - The fully template-structured vocabulary the strict Validate below
//     enforces: root children are policy-section nodes carrying
//     policy-boilerplate / policy-editable-region children.
//
// Behaviour:
//
//   - Section-shaped documents (first root child is a policy-section) get the
//     FULL strict Validate. Structural enforcement is therefore never weaker
//     than it is today for any content that can already satisfy it, and the
//     day the editor gains real section nodes it is strictly validated with
//     no further change here.
//   - Anything else is validated against the envelope invariants only, which
//     is exactly the guarantee the other write path (SaveDraft) offers. This
//     is what lets an author's edit persist at all.
//
// Envelope invariants (both shapes):
//
//  1. Non-empty, well-formed JSON.
//  2. Top-level value is an object carrying a "root" object.
//  3. root.children is an array with at least one entry.
//  4. Every root child is an object with a non-empty string "type".
//
// Rules 3 and 4 are the load-bearing anti-corruption checks: a Lexical editor
// that fails to parse its input empties or mangles the root, and this rejects
// the mangled result rather than checkpointing it over a real draft. An empty
// root is never legitimate — the editor's own normalizer
// (apps/staff/src/authoring.ts normalizeEditorState) coerces an empty
// root.children to a single empty paragraph before it can reach the wire, so a
// childless root on this path means the state was destroyed, not authored.
//
// The complementary text-preservation guard lives in the caller, which is the
// only layer that can see the draft's stored content (see HasTextContent).
//
// Publish-time and template-migration strictness are unaffected: this function
// is only reachable from the draft snapshot path.
func ValidateDraft(contentJSON string, sections []TemplateSection) error {
	if contentJSON == "" {
		return fmt.Errorf("empty content JSON")
	}

	var doc struct {
		Root *struct {
			Type     string             `json:"type"`
			Children *[]json.RawMessage `json:"children"`
		} `json:"root"`
	}
	if err := json.Unmarshal([]byte(contentJSON), &doc); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if doc.Root == nil {
		return fmt.Errorf("missing required field: root")
	}

	if doc.Root.Children == nil {
		return fmt.Errorf("root has no children array")
	}
	children := *doc.Root.Children
	if len(children) == 0 {
		return fmt.Errorf("root has no children; refusing an emptied document")
	}

	// Delegate to the strict validator once the document is section-shaped.
	if rawNodeType(children[0]) == SectionNodeType {
		return Validate(contentJSON, sections)
	}

	for i, raw := range children {
		var node map[string]json.RawMessage
		if err := json.Unmarshal(raw, &node); err != nil {
			return fmt.Errorf("root[%d]: not a JSON object", i)
		}
		if rawNodeType(raw) == "" {
			return fmt.Errorf("root[%d]: child missing type field", i)
		}
	}
	return nil
}

// rawNodeType returns a Lexical node's "type" field, or "" when the value is
// not a JSON object, has no "type", or has a non-string / empty "type".
func rawNodeType(raw json.RawMessage) string {
	var node struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	return node.Type
}

// HasTextContent reports whether contentJSON carries any non-whitespace author
// text anywhere in its node tree. Malformed content reports false.
//
// This exists for the caller's text-preservation guard: a collab checkpoint
// must never replace a draft that has text with one that has none. That is the
// concrete data-loss mode of the snapshot path — an editor which cannot parse
// its initial state renders an empty document and then checkpoints that
// emptiness over the real draft.
func HasTextContent(contentJSON string) bool {
	if contentJSON == "" {
		return false
	}
	var doc struct {
		Root json.RawMessage `json:"root"`
	}
	if err := json.Unmarshal([]byte(contentJSON), &doc); err != nil {
		return false
	}
	if len(doc.Root) == 0 {
		return false
	}
	return nodeHasText(doc.Root, 0)
}

// maxTextScanDepth bounds nodeHasText against a pathologically deep or
// maliciously nested document.
const maxTextScanDepth = 64

func nodeHasText(raw json.RawMessage, depth int) bool {
	if depth > maxTextScanDepth {
		return false
	}
	var node struct {
		Text     string            `json:"text"`
		Children []json.RawMessage `json:"children"`
	}
	if err := json.Unmarshal(raw, &node); err != nil {
		return false
	}
	if hasNonSpace(node.Text) {
		return true
	}
	for _, child := range node.Children {
		if nodeHasText(child, depth+1) {
			return true
		}
	}
	return false
}

// hasNonSpace reports whether s contains a character other than the ASCII and
// Unicode whitespace Lexical can produce for an "empty" text node.
func hasNonSpace(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\v', '\f',
			0x85,   // NEL
			0xA0,   // NBSP
			0x200B, // zero-width space
			0xFEFF: // BOM / zero-width no-break space
			continue
		}
		return true
	}
	return false
}

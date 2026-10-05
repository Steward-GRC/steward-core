// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type DiffChangeType string

const (
	DiffChangeAdded     DiffChangeType = "added"
	DiffChangeRemoved   DiffChangeType = "removed"
	DiffChangeChanged   DiffChangeType = "changed"
	DiffChangeUnchanged DiffChangeType = "unchanged"
)

type SectionDiff struct {
	SectionKey    string
	SectionTitle  string
	ChangeType    DiffChangeType
	WordDiffHTML  string // non-empty only when ChangeType == changed/added/removed
	IsBoilerplate bool   // true when the change was introduced by a template migration
}

// lexicalContent is the older simplified envelope
// ({"sections": {"key": "text"}}). Older versions may still carry it.
type lexicalContent struct {
	Sections map[string]string `json:"sections"`
}

// lexicalDoc and lexicalNode decode only what the text extractor needs from
// the Lexical editor state.
type lexicalDoc struct {
	Root *lexicalNode `json:"root"`
}

type lexicalNode struct {
	Type     string        `json:"type"`
	Text     string        `json:"text"`
	Children []lexicalNode `json:"children"`
}

// inlineLexicalTypes are node types whose text flows inline within a block
// (adjacent runs concatenate without a separator). Everything else is treated
// as block-level, so its text is separated from siblings to stop words from
// running together across paragraphs, list items, quotes and table cells.
var inlineLexicalTypes = map[string]bool{
	"text":     true,
	"link":     true,
	"autolink": true,
	"hashtag":  true,
	"mark":     true,
}

// parseContent decodes a policy version's serialized content into a map of
// section key -> plain text. It accepts two shapes:
//
//   - the older envelope {"sections": {"key": "text"}}, returned as it is; and
//   - a Lexical node tree {"root": {...}}, split into sections on headings.
//
// Empty content yields an empty map; unparseable content yields an error,
// which ExtractSections swallows so a malformed row never breaks publish.
func parseContent(contentJSON string) (map[string]string, error) {
	if contentJSON == "" {
		return map[string]string{}, nil
	}
	// Legacy simplified envelope takes precedence when a top-level "sections"
	// object is present.
	var lc lexicalContent
	if err := json.Unmarshal([]byte(contentJSON), &lc); err == nil && lc.Sections != nil {
		return lc.Sections, nil
	}
	// Otherwise decode as a real Lexical document and extract node text.
	var doc lexicalDoc
	if err := json.Unmarshal([]byte(contentJSON), &doc); err != nil {
		return nil, fmt.Errorf("parse content: %w", err)
	}
	if doc.Root == nil {
		return map[string]string{}, nil
	}
	return extractLexicalSections(doc.Root), nil
}

// extractLexicalSections walks a Lexical root's top-level children and groups
// their plain text into sections. Each heading starts a new section keyed by a
// slug of the heading text (with the heading text kept as the first line so it
// is retrievable), and every block up to the next heading is appended to it.
// Content before the first heading (or a heading-less document) lands under
// the "body" key, which also serves as the single-section fallback.
func extractLexicalSections(root *lexicalNode) map[string]string {
	out := map[string]string{}
	used := map[string]int{}
	curKey := reserveSectionKey("body", used)
	var buf strings.Builder

	flush := func() {
		if t := strings.TrimSpace(buf.String()); t != "" {
			out[curKey] = t
		}
		buf.Reset()
	}

	for i := range root.Children {
		c := &root.Children[i]
		if c.Type == "heading" {
			flush()
			title := strings.TrimSpace(nodeText(c))
			curKey = reserveSectionKey(slugify(title), used)
			buf.WriteString(title)
			continue
		}
		t := strings.TrimSpace(nodeText(c))
		if t == "" {
			continue
		}
		if buf.Len() > 0 {
			buf.WriteString("\n")
		}
		buf.WriteString(t)
	}
	flush()
	return out
}

// nodeText returns the concatenated plain text of a Lexical node subtree.
// Inline runs concatenate directly; block-level children are joined with a
// newline so words never run together across paragraphs, list items or table
// cells.
func nodeText(n *lexicalNode) string {
	if n == nil {
		return ""
	}
	if n.Type == "text" {
		return n.Text
	}
	var b strings.Builder
	for i := range n.Children {
		c := &n.Children[i]
		ct := nodeText(c)
		if ct == "" {
			continue
		}
		if b.Len() > 0 && !inlineLexicalTypes[c.Type] {
			b.WriteString("\n")
		}
		b.WriteString(ct)
	}
	return b.String()
}

// reserveSectionKey returns a unique map key for base, disambiguating repeats
// (e.g. two headings with the same text) with a numeric suffix so no section's
// text is silently overwritten. An empty base falls back to "section".
func reserveSectionKey(base string, used map[string]int) string {
	if base == "" {
		base = "section"
	}
	used[base]++
	if used[base] == 1 {
		return base
	}
	return fmt.Sprintf("%s-%d", base, used[base])
}

// slugify lowercases s and collapses runs of non-alphanumeric characters into
// single hyphens, trimming leading/trailing hyphens — a stable, deterministic
// section key derived from heading text.
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// ExtractedSection is one section's key and plain text.
type ExtractedSection struct {
	Key  string
	Text string
}

// ExtractSections returns the section texts from contentJSON in the order
// given by sectionDefs (template order), followed by any sections present
// in the content but missing from the template definitions (sorted by key
// for determinism). Unknown / unparseable content yields an empty slice
// rather than an error so a malformed-content row never breaks an
// otherwise-successful publish — the lifecycle event still goes out with
// identity metadata so downstream consumers can decide what to do.
func ExtractSections(contentJSON string, sectionDefs []Section) []ExtractedSection {
	sections, err := parseContent(contentJSON)
	if err != nil || len(sections) == 0 {
		return nil
	}
	out := make([]ExtractedSection, 0, len(sections))
	seen := map[string]bool{}
	for _, def := range sectionDefs {
		if text, ok := sections[def.Key]; ok {
			out = append(out, ExtractedSection{Key: def.Key, Text: text})
			seen[def.Key] = true
		}
	}
	// Any orphans (content has it, template doesn't) get appended in
	// deterministic order so re-emits stay byte-stable.
	var orphans []string
	for k := range sections {
		if !seen[k] {
			orphans = append(orphans, k)
		}
	}
	sort.Strings(orphans)
	for _, k := range orphans {
		out = append(out, ExtractedSection{Key: k, Text: sections[k]})
	}
	return out
}

// wordDiffHTML returns an HTML string with <del> and <ins> tags around changed words.
func wordDiffHTML(from, to string) string {
	fromWords := strings.Fields(from)
	toWords := strings.Fields(to)
	// Simple Myers-style LCS word diff using DP table.
	m, n := len(fromWords), len(toWords)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if fromWords[i-1] == toWords[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] > dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	// Backtrack to produce diff tokens.
	var sb strings.Builder
	i, j := m, n
	type op struct {
		kind string
		word string
	}
	var ops []op
	for i > 0 || j > 0 {
		switch {
		case i > 0 && j > 0 && fromWords[i-1] == toWords[j-1]:
			ops = append(ops, op{"eq", fromWords[i-1]})
			i--
			j--
		case j > 0 && (i == 0 || dp[i][j-1] >= dp[i-1][j]):
			ops = append(ops, op{"ins", toWords[j-1]})
			j--
		default:
			ops = append(ops, op{"del", fromWords[i-1]})
			i--
		}
	}
	for l, r := 0, len(ops)-1; l < r; l, r = l+1, r-1 {
		ops[l], ops[r] = ops[r], ops[l]
	}
	for _, o := range ops {
		switch o.kind {
		case "eq":
			sb.WriteString(o.word + " ")
		case "ins":
			sb.WriteString("<ins>" + o.word + "</ins> ")
		case "del":
			sb.WriteString("<del>" + o.word + "</del> ")
		}
	}
	return strings.TrimSpace(sb.String())
}

// DiffVersions computes a section-aware word diff between two serialized Lexical content blobs.
// sectionDefs provides the ordered section definitions from the pinned template version.
// isTemplateMigration=true marks all changed sections as IsBoilerplate to surface template churn distinctly.
func DiffVersions(fromJSON, toJSON string, sectionDefs []Section, isTemplateMigration bool) ([]SectionDiff, error) {
	fromSections, err := parseContent(fromJSON)
	if err != nil {
		return nil, fmt.Errorf("from content: %w", err)
	}
	toSections, err := parseContent(toJSON)
	if err != nil {
		return nil, fmt.Errorf("to content: %w", err)
	}

	var result []SectionDiff
	for _, sec := range sectionDefs {
		fromText := fromSections[sec.Key]
		toText := toSections[sec.Key]

		var ct DiffChangeType
		var html string
		switch {
		case fromText == "" && toText != "":
			ct = DiffChangeAdded
			html = "<ins>" + toText + "</ins>"
		case fromText != "" && toText == "":
			ct = DiffChangeRemoved
			html = "<del>" + fromText + "</del>"
		case fromText == toText:
			ct = DiffChangeUnchanged
		default:
			ct = DiffChangeChanged
			html = wordDiffHTML(fromText, toText)
		}

		result = append(result, SectionDiff{
			SectionKey:    sec.Key,
			SectionTitle:  sec.Title,
			ChangeType:    ct,
			WordDiffHTML:  html,
			IsBoilerplate: isTemplateMigration && ct != DiffChangeUnchanged,
		})
	}
	return result, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"strings"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// makeContent builds a minimal Lexical JSON string with section text.
// Real Lexical JSON is more complex; this exercises diff logic without the editor.
func makeContent(sections map[string]string) string {
	// Format: {"sections":{"key":"text",...}}
	parts := []string{}
	for k, v := range sections {
		parts = append(parts, `"`+k+`":"`+v+`"`)
	}
	return `{"sections":{` + strings.Join(parts, ",") + `}}`
}

func TestDiffUnchangedSection(t *testing.T) {
	from := makeContent(map[string]string{"purpose": "To define security policy."})
	to := makeContent(map[string]string{"purpose": "To define security policy."})
	sectionDefs := []domain.Section{{Key: "purpose", Title: "Purpose", Order: 1}}
	diffs, err := domain.DiffVersions(from, to, sectionDefs, false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(diffs) != 1 {
		t.Fatalf("expected 1 diff, got %d", len(diffs))
	}
	if diffs[0].ChangeType != domain.DiffChangeUnchanged {
		t.Fatalf("expected unchanged, got %q", diffs[0].ChangeType)
	}
	if diffs[0].WordDiffHTML != "" {
		t.Fatal("expected empty WordDiffHTML for unchanged section")
	}
}

func TestDiffChangedSectionProducesWordDiff(t *testing.T) {
	from := makeContent(map[string]string{"purpose": "To define security policy."})
	to := makeContent(map[string]string{"purpose": "To define information security policy."})
	sectionDefs := []domain.Section{{Key: "purpose", Title: "Purpose", Order: 1}}
	diffs, err := domain.DiffVersions(from, to, sectionDefs, false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if diffs[0].ChangeType != domain.DiffChangeChanged {
		t.Fatalf("expected changed, got %q", diffs[0].ChangeType)
	}
	if !strings.Contains(diffs[0].WordDiffHTML, "information") {
		t.Fatalf("expected 'information' in diff HTML, got: %q", diffs[0].WordDiffHTML)
	}
}

func TestDiffAddedSection(t *testing.T) {
	from := makeContent(map[string]string{})
	to := makeContent(map[string]string{"scope": "All employees."})
	sectionDefs := []domain.Section{{Key: "scope", Title: "Scope", Order: 1}}
	diffs, err := domain.DiffVersions(from, to, sectionDefs, false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if diffs[0].ChangeType != domain.DiffChangeAdded {
		t.Fatalf("expected added, got %q", diffs[0].ChangeType)
	}
}

func TestDiffBoilerplateFlaggedWhenTemplateMigration(t *testing.T) {
	from := makeContent(map[string]string{"boilerplate": "Old legal text."})
	to := makeContent(map[string]string{"boilerplate": "New legal text."})
	sectionDefs := []domain.Section{{Key: "boilerplate", Title: "Legal", Order: 1}}
	diffs, err := domain.DiffVersions(from, to, sectionDefs, true /* isTemplateMigration */)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !diffs[0].IsBoilerplate {
		t.Fatal("expected IsBoilerplate=true for template migration diff")
	}
}

// TestExtractSectionsFollowsTemplateOrder asserts the lifecycle-event helper
// returns sections in the template's declared order so AI chunk indices
// (which are derived from input order) stay stable across re-emits.
func TestExtractSectionsFollowsTemplateOrder(t *testing.T) {
	content := makeContent(map[string]string{
		"scope":   "All staff.",
		"purpose": "To define security policy.",
	})
	defs := []domain.Section{
		{Key: "purpose", Title: "Purpose", Order: 1},
		{Key: "scope", Title: "Scope", Order: 2},
	}
	got := domain.ExtractSections(content, defs)
	if len(got) != 2 || got[0].Key != "purpose" || got[1].Key != "scope" {
		t.Fatalf("expected template order [purpose, scope], got %+v", got)
	}
	if got[0].Text != "To define security policy." || got[1].Text != "All staff." {
		t.Fatalf("text mismatch: %+v", got)
	}
}

// TestExtractSectionsAppendsOrphansAlphabetically ensures content sections
// the template no longer defines (e.g. mid-template-migration) still ship
// in the event, in deterministic order, so AI doesn't silently lose chunks.
func TestExtractSectionsAppendsOrphansAlphabetically(t *testing.T) {
	content := makeContent(map[string]string{
		"purpose": "P.",
		"zeta":    "Z.",
		"alpha":   "A.",
	})
	defs := []domain.Section{{Key: "purpose", Title: "Purpose", Order: 1}}
	got := domain.ExtractSections(content, defs)
	if len(got) != 3 {
		t.Fatalf("len: %d, %+v", len(got), got)
	}
	if got[0].Key != "purpose" || got[1].Key != "alpha" || got[2].Key != "zeta" {
		t.Fatalf("order: %+v", got)
	}
}

// TestExtractSectionsTolerantOfMalformedContent guards against a malformed
// content row breaking publish: ExtractSections must return nil, not error.
func TestExtractSectionsTolerantOfMalformedContent(t *testing.T) {
	if got := domain.ExtractSections("not-json", nil); got != nil {
		t.Fatalf("expected nil for malformed content, got %+v", got)
	}
	if got := domain.ExtractSections("", nil); got != nil {
		t.Fatalf("expected nil for empty content, got %+v", got)
	}
	// A well-formed Lexical doc with a nil/empty root yields no sections.
	if got := domain.ExtractSections(`{"root":null}`, nil); got != nil {
		t.Fatalf("expected nil for empty root, got %+v", got)
	}
}

// lexicalSample is a real Lexical node tree matching the shape the editor
// persists: a flat document of headings, paragraphs, a list and a table.
const lexicalSample = `{"root":{"type":"root","children":[
  {"tag":"h1","type":"heading","children":[{"text":"Introduction","type":"text"}]},
  {"type":"paragraph","children":[{"text":"Information technology systems are ","type":"text"},{"text":"foundational to Example Organisation's ability to deliver services.","type":"text"}]},
  {"tag":"h2","type":"heading","children":[{"text":"Disaster Recovery","type":"text"}]},
  {"type":"paragraph","children":[{"text":"Backups run nightly.","type":"text"}]},
  {"type":"list","children":[
    {"type":"listitem","children":[{"text":"Offsite copy","type":"text"}]},
    {"type":"listitem","children":[{"text":"Restore drill","type":"text"}]}
  ]},
  {"type":"table","children":[
    {"type":"tablerow","children":[
      {"type":"tablecell","children":[{"type":"paragraph","children":[{"text":"RTO","type":"text"}]}]},
      {"type":"tablecell","children":[{"type":"paragraph","children":[{"text":"4 hours","type":"text"}]}]}
    ]}
  ]}
]}}`

// TestExtractSectionsFromLexicalTree: a real Lexical
// document must yield non-empty section text (the whole policy body) so the
// AI service has a corpus to index. It also proves table-cell text is captured
// and that inline runs concatenate without a separator.
func TestExtractSectionsFromLexicalTree(t *testing.T) {
	got := domain.ExtractSections(lexicalSample, nil)
	if len(got) == 0 {
		t.Fatal("expected non-empty sections from a real Lexical tree, got none")
	}

	// Segmented by heading: an "introduction" and a "disaster-recovery" key.
	byKey := map[string]string{}
	var all strings.Builder
	for _, s := range got {
		byKey[s.Key] = s.Text
		all.WriteString(s.Text)
		all.WriteString("\n")
	}
	if _, ok := byKey["introduction"]; !ok {
		t.Fatalf("expected an 'introduction' section, got keys %v", keysOf(got))
	}
	if _, ok := byKey["disaster-recovery"]; !ok {
		t.Fatalf("expected a 'disaster-recovery' section, got keys %v", keysOf(got))
	}

	full := all.String()
	for _, want := range []string{
		"Introduction",
		"Information technology systems are foundational", // inline runs joined w/o separator
		"Disaster Recovery",
		"Backups run nightly.",
		"Offsite copy",
		"Restore drill",
		"RTO",     // table cell text
		"4 hours", // table cell text
	} {
		if !strings.Contains(full, want) {
			t.Fatalf("extracted text missing %q; got:\n%s", want, full)
		}
	}

	// Words across block boundaries must not run together.
	if strings.Contains(full, "nightlyOffsite") || strings.Contains(full, "RTO4") {
		t.Fatalf("block boundaries not separated; got:\n%s", full)
	}
}

// TestExtractSectionsLegacyEnvelopeStillWorks preserves backward compatibility:
// the old {"sections":{...}} envelope must extract exactly as before.
func TestExtractSectionsLegacyEnvelopeStillWorks(t *testing.T) {
	content := makeContent(map[string]string{"purpose": "To define security policy."})
	got := domain.ExtractSections(content, []domain.Section{{Key: "purpose", Title: "Purpose", Order: 1}})
	if len(got) != 1 || got[0].Key != "purpose" || got[0].Text != "To define security policy." {
		t.Fatalf("legacy envelope extraction broke: %+v", got)
	}
}

// TestExtractSectionsHeadinglessLexicalUsesBody proves a heading-less document
// (or content before the first heading) lands under the single "body" key.
func TestExtractSectionsHeadinglessLexicalUsesBody(t *testing.T) {
	content := `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"text":"Just a body paragraph.","type":"text"}]}]}}`
	got := domain.ExtractSections(content, nil)
	if len(got) != 1 || got[0].Key != "body" || got[0].Text != "Just a body paragraph." {
		t.Fatalf("expected single 'body' section, got %+v", got)
	}
}

// TestDiffVersionsUnaffectedByLexicalContent proves the diff feature does not
// panic or error on real Lexical content (it diffs on template section keys,
// which Lexical slug keys won't match — the same all-unchanged behavior as
// before the fix, no regression).
func TestDiffVersionsUnaffectedByLexicalContent(t *testing.T) {
	defs := []domain.Section{{Key: "purpose", Title: "Purpose", Order: 1}}
	diffs, err := domain.DiffVersions(lexicalSample, lexicalSample, defs, false)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(diffs) != 1 || diffs[0].ChangeType != domain.DiffChangeUnchanged {
		t.Fatalf("expected 1 unchanged diff, got %+v", diffs)
	}
}

func keysOf(secs []domain.ExtractedSection) []string {
	ks := make([]string, 0, len(secs))
	for _, s := range secs {
		ks = append(ks, s.Key)
	}
	return ks
}

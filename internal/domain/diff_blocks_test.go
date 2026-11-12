// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// lex builds a Lexical document from ("h"|"p", text) pairs: "h" is a heading, "p" a paragraph.
func lex(t *testing.T, parts ...string) string {
	t.Helper()
	var children []map[string]any
	for i := 0; i+1 < len(parts); i += 2 {
		typ := "paragraph"
		if parts[i] == "h" {
			typ = "heading"
		}
		children = append(children, map[string]any{
			"type":     typ,
			"children": []map[string]any{{"type": "text", "text": parts[i+1]}},
		})
	}
	b, err := json.Marshal(map[string]any{"root": map[string]any{"type": "root", "children": children}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSplitBlocksHeadingsAndIntroduction(t *testing.T) {
	got, err := domain.SplitBlocks(lex(t,
		"p", "Preamble text.",
		"h", "Purpose", "p", "Why we exist.",
		"h", "Scope", "p", "Everyone."))
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ContentBlock{
		{Key: "body", Title: "Introduction", Text: "Preamble text."},
		{Key: "purpose", Title: "Purpose", Text: "Purpose\nWhy we exist."},
		{Key: "scope", Title: "Scope", Text: "Scope\nEveryone."},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("block %d: got %+v want %+v", i, got[i], want[i])
		}
	}
}

func TestSplitBlocksNoLeadingTextNoIntroduction(t *testing.T) {
	got, err := domain.SplitBlocks(lex(t, "h", "Purpose", "p", "Why."))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "purpose" {
		t.Fatalf("got %+v", got)
	}
}

func TestSplitBlocksHeadinglessDocumentIsOneBlock(t *testing.T) {
	got, err := domain.SplitBlocks(lex(t, "p", "One.", "p", "Two."))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "body" || got[0].Text != "One.\nTwo." {
		t.Fatalf("got %+v", got)
	}
}

func TestSplitBlocksDuplicateHeadingsKeyedByOrder(t *testing.T) {
	got, err := domain.SplitBlocks(lex(t,
		"h", "Notes", "p", "a", "h", "Notes", "p", "b", "h", "Notes", "p", "c"))
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, b := range got {
		keys = append(keys, b.Key)
		if b.Title != "Notes" {
			t.Fatalf("title %q", b.Title)
		}
	}
	if strings.Join(keys, ",") != "notes,notes-2,notes-3" {
		t.Fatalf("keys %v", keys)
	}
}

func TestSplitBlocksEmptyContent(t *testing.T) {
	for _, in := range []string{"", `{"root":null}`, `{"root":{"type":"root","children":[]}}`} {
		got, err := domain.SplitBlocks(in)
		if err != nil || len(got) != 0 {
			t.Fatalf("%q: got %+v err %v", in, got, err)
		}
	}
	if _, err := domain.SplitBlocks("not-json"); err == nil {
		t.Fatal("expected an error for malformed content")
	}
}

func TestSplitBlocksEmptyHeading(t *testing.T) {
	got, err := domain.SplitBlocks(lex(t, "h", "", "p", "text"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != "section" || got[0].Title != "Untitled section" {
		t.Fatalf("got %+v", got)
	}
}

func TestSplitBlocksLegacyEnvelopeSortedByKey(t *testing.T) {
	got, err := domain.SplitBlocks(`{"sections":{"b":"two","a":"one"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Key != "a" || got[1].Key != "b" || got[0].Title != "a" {
		t.Fatalf("got %+v", got)
	}
}

func TestDiffBlocksAddedRemovedChangedUnchanged(t *testing.T) {
	from := lex(t,
		"h", "Purpose", "p", "Keep this.",
		"h", "Scope", "p", "All staff.",
		"h", "Gone", "p", "Old section.")
	to := lex(t,
		"h", "Purpose", "p", "Keep this.",
		"h", "Scope", "p", "All staff and contractors.",
		"h", "Fresh", "p", "New section.")
	diffs, err := domain.DiffBlocks(from, to)
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		key, title string
		ct         domain.DiffChangeType
	}
	want := []row{
		{"purpose", "Purpose", domain.DiffChangeUnchanged},
		{"scope", "Scope", domain.DiffChangeChanged},
		{"gone", "Gone", domain.DiffChangeRemoved},
		{"fresh", "Fresh", domain.DiffChangeAdded},
	}
	// Order: the "to" order, with a removed block right after the block it followed.
	if len(diffs) != len(want) {
		t.Fatalf("got %+v", diffs)
	}
	for i, w := range want {
		d := diffs[i]
		if d.SectionKey != w.key || d.SectionTitle != w.title || d.ChangeType != w.ct {
			t.Fatalf("diff %d: got %+v want %+v", i, d, w)
		}
		if d.IsBoilerplate {
			t.Fatalf("diff %d: a block diff is never boilerplate", i)
		}
	}
	if diffs[0].WordDiffHTML != "" {
		t.Fatalf("unchanged has html %q", diffs[0].WordDiffHTML)
	}
	if !strings.Contains(diffs[1].WordDiffHTML, "<ins>and</ins>") {
		t.Fatalf("changed html %q", diffs[1].WordDiffHTML)
	}
	if diffs[2].WordDiffHTML != "<del>Gone\nOld section.</del>" {
		t.Fatalf("removed html %q", diffs[2].WordDiffHTML)
	}
	if diffs[3].WordDiffHTML != "<ins>Fresh\nNew section.</ins>" {
		t.Fatalf("added html %q", diffs[3].WordDiffHTML)
	}
}

func TestDiffBlocksRemovedFirstBlockComesFirst(t *testing.T) {
	from := lex(t, "h", "Old", "p", "x", "h", "Keep", "p", "y")
	to := lex(t, "h", "Keep", "p", "y")
	diffs, err := domain.DiffBlocks(from, to)
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 || diffs[0].SectionKey != "old" || diffs[0].ChangeType != domain.DiffChangeRemoved ||
		diffs[1].SectionKey != "keep" || diffs[1].ChangeType != domain.DiffChangeUnchanged {
		t.Fatalf("got %+v", diffs)
	}
}

func TestDiffBlocksIntroductionAndDuplicateHeadings(t *testing.T) {
	from := lex(t, "p", "Intro.", "h", "Notes", "p", "a", "h", "Notes", "p", "b")
	to := lex(t, "p", "Intro changed.", "h", "Notes", "p", "a", "h", "Notes", "p", "b2")
	diffs, err := domain.DiffBlocks(from, to)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, d := range diffs {
		got = append(got, d.SectionKey+":"+string(d.ChangeType))
	}
	if strings.Join(got, ",") != "body:changed,notes:unchanged,notes-2:changed" {
		t.Fatalf("got %v", got)
	}
	if diffs[0].SectionTitle != "Introduction" {
		t.Fatalf("title %q", diffs[0].SectionTitle)
	}
}

func TestDiffBlocksEmptySides(t *testing.T) {
	to := lex(t, "h", "Purpose", "p", "x")
	diffs, err := domain.DiffBlocks("", to)
	if err != nil || len(diffs) != 1 || diffs[0].ChangeType != domain.DiffChangeAdded {
		t.Fatalf("added: %+v %v", diffs, err)
	}
	diffs, err = domain.DiffBlocks(to, "")
	if err != nil || len(diffs) != 1 || diffs[0].ChangeType != domain.DiffChangeRemoved {
		t.Fatalf("removed: %+v %v", diffs, err)
	}
	diffs, err = domain.DiffBlocks("", "")
	if err != nil || len(diffs) != 0 {
		t.Fatalf("both empty: %+v %v", diffs, err)
	}
	if _, err = domain.DiffBlocks("not-json", to); err == nil {
		t.Fatal("expected an error for malformed from content")
	}
}

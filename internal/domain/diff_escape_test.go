// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"html"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// run is one piece of a parsed word diff: its kind ("same", "ins", "del") and its words.
type run struct {
	kind  string
	words []string
}

var diffTag = regexp.MustCompile(`</?(ins|del)>`)

// parseDiff is a small reference reader for the diff HTML, the way the web reads it: only
// <ins>/<del> are markers, everything else is text, and entities are decoded.
func parseDiff(h string) []run {
	var runs []run
	kind := "same"
	pos := 0
	add := func(raw string) {
		for _, w := range strings.Fields(raw) {
			w = html.UnescapeString(w)
			if n := len(runs); n > 0 && runs[n-1].kind == kind {
				runs[n-1].words = append(runs[n-1].words, w)
			} else {
				runs = append(runs, run{kind, []string{w}})
			}
		}
	}
	for _, loc := range diffTag.FindAllStringIndex(h, -1) {
		add(h[pos:loc[0]])
		tag := h[loc[0]:loc[1]]
		switch tag {
		case "<ins>":
			kind = "ins"
		case "<del>":
			kind = "del"
		default:
			kind = "same"
		}
		pos = loc[1]
	}
	add(h[pos:])
	return runs
}

func TestDiffVersionsEscapesChangedWords(t *testing.T) {
	defs := []domain.Section{{Key: "a", Title: "A"}}
	from := makeContent(map[string]string{"a": "keep <b>bold</b> &lt; old"})
	to := makeContent(map[string]string{"a": "keep <b>bold</b> &lt; <ins>x</ins> new"})
	diffs, err := domain.DiffVersions(from, to, defs, false)
	if err != nil {
		t.Fatal(err)
	}
	h := diffs[0].WordDiffHTML
	if strings.Contains(h, "<b>") || strings.Contains(h, "<ins>x") {
		t.Fatalf("raw markup leaked: %q", h)
	}
	want := []run{
		{"same", []string{"keep", "<b>bold</b>", "&lt;"}},
		{"del", []string{"old"}},
		{"ins", []string{"<ins>x</ins>", "new"}},
	}
	if got := parseDiff(h); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v from %q", got, h)
	}
}

func TestDiffVersionsEscapesAddedAndRemoved(t *testing.T) {
	defs := []domain.Section{{Key: "a", Title: "A"}, {Key: "b", Title: "B"}}
	text := "a <b>c</b> & <del>y</del>"
	added, err := domain.DiffVersions(makeContent(map[string]string{}),
		makeContent(map[string]string{"a": text}), defs, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []run{{"ins", strings.Fields(text)}}; !reflect.DeepEqual(parseDiff(added[0].WordDiffHTML), want) {
		t.Fatalf("added: %q", added[0].WordDiffHTML)
	}
	removed, err := domain.DiffVersions(makeContent(map[string]string{"a": text}),
		makeContent(map[string]string{}), defs, false)
	if err != nil {
		t.Fatal(err)
	}
	if want := []run{{"del", strings.Fields(text)}}; !reflect.DeepEqual(parseDiff(removed[0].WordDiffHTML), want) {
		t.Fatalf("removed: %q", removed[0].WordDiffHTML)
	}
}

func TestDiffBlocksEscapesEveryBranch(t *testing.T) {
	from := lex(t, "h", "Same", "p", "x <b>1</b>", "h", "Gone", "p", "a & <ins>b</ins>",
		"h", "Edit", "p", "keep <i>old</i> &amp;")
	to := lex(t, "h", "Same", "p", "x <b>1</b>", "h", "Edit", "p", "keep <i>new</i> &amp;",
		"h", "Fresh", "p", "<del>c</del> > d")
	diffs, err := domain.DiffBlocks(from, to)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]run{}
	for _, d := range diffs {
		if strings.Contains(d.WordDiffHTML, "<b>") || strings.Contains(d.WordDiffHTML, "<i>") ||
			strings.Contains(d.WordDiffHTML, "<ins>b") || strings.Contains(d.WordDiffHTML, "<del>c") {
			t.Fatalf("raw markup leaked in %s: %q", d.SectionKey, d.WordDiffHTML)
		}
		got[d.SectionKey] = parseDiff(d.WordDiffHTML)
	}
	if r := got["gone"]; !reflect.DeepEqual(r, []run{{"del", []string{"Gone", "a", "&", "<ins>b</ins>"}}}) {
		t.Fatalf("removed: %+v", r)
	}
	if r := got["fresh"]; !reflect.DeepEqual(r, []run{{"ins", []string{"Fresh", "<del>c</del>", ">", "d"}}}) {
		t.Fatalf("added: %+v", r)
	}
	want := []run{
		{"same", []string{"Edit", "keep"}},
		{"del", []string{"<i>old</i>"}},
		{"ins", []string{"<i>new</i>"}},
		{"same", []string{"&amp;"}},
	}
	if r := got["edit"]; !reflect.DeepEqual(r, want) {
		t.Fatalf("changed: %+v", r)
	}
}

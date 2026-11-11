// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// A version with no pinned template is compared by the section keys present in
// either text, so a freeform draft can still show what changed (issue #55).
func TestFreeformSectionsUnionOfKeysInEitherText(t *testing.T) {
	before := makeContent(map[string]string{"steps": "Book it.", "afterwards": "Ask."})
	current := makeContent(map[string]string{"steps": "Book it today.", "extra-notes": "New."})

	defs, err := domain.FreeformSections(before, current)
	if err != nil {
		t.Fatalf("FreeformSections: %v", err)
	}
	var keys, titles []string
	for _, d := range defs {
		keys = append(keys, d.Key)
		titles = append(titles, d.Title)
	}
	// Sorted by key so the answer is stable.
	wantKeys := []string{"afterwards", "extra-notes", "steps"}
	wantTitles := []string{"Afterwards", "Extra notes", "Steps"}
	for i := range wantKeys {
		if i >= len(keys) || keys[i] != wantKeys[i] || titles[i] != wantTitles[i] {
			t.Fatalf("got keys %v titles %v, want %v %v", keys, titles, wantKeys, wantTitles)
		}
	}
	if len(keys) != len(wantKeys) {
		t.Fatalf("got %d sections, want %d: %v", len(keys), len(wantKeys), keys)
	}
}

func TestFreeformSectionsDiffShowsOnlyTheEdit(t *testing.T) {
	before := makeContent(map[string]string{"steps": "Book it.", "afterwards": "Ask."})
	same, err := domain.FreeformSections(before, before)
	if err != nil {
		t.Fatalf("FreeformSections: %v", err)
	}
	diffs, err := domain.DiffVersions(before, before, same, false)
	if err != nil {
		t.Fatalf("DiffVersions: %v", err)
	}
	for _, d := range diffs {
		if d.ChangeType != domain.DiffChangeUnchanged {
			t.Fatalf("section %s: want unchanged, got %s", d.SectionKey, d.ChangeType)
		}
	}

	current := makeContent(map[string]string{"steps": "Book it today.", "afterwards": "Ask."})
	defs, _ := domain.FreeformSections(before, current)
	diffs, err = domain.DiffVersions(before, current, defs, false)
	if err != nil {
		t.Fatalf("DiffVersions: %v", err)
	}
	changed := map[string]domain.DiffChangeType{}
	for _, d := range diffs {
		changed[d.SectionKey] = d.ChangeType
	}
	if changed["steps"] != domain.DiffChangeChanged || changed["afterwards"] != domain.DiffChangeUnchanged {
		t.Fatalf("unexpected diff: %v", changed)
	}
}

func TestFreeformSectionsRejectsUnparseableContent(t *testing.T) {
	if _, err := domain.FreeformSections(`{not json`, `{}`); err == nil {
		t.Fatal("want an error for unparseable content")
	}
}

func TestFreeformSectionsEmptyTexts(t *testing.T) {
	defs, err := domain.FreeformSections(``, `{}`)
	if err != nil {
		t.Fatalf("FreeformSections: %v", err)
	}
	if len(defs) != 0 {
		t.Fatalf("want no sections, got %v", defs)
	}
}

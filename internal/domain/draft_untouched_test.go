// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

func TestDraftUntouched(t *testing.T) {
	doc := func(texts ...string) string {
		kids := ""
		for i, tx := range texts {
			if i > 0 {
				kids += ","
			}
			kids += `{"type":"heading","children":[{"type":"text","text":"` + tx + `"}]}`
		}
		return `{"root":{"type":"root","children":[` + kids + `]}}`
	}
	titles := []string{"Purpose", "Scope"}
	cases := []struct {
		name    string
		content string
		titles  []string
		want    bool
	}{
		{"empty object", "{}", nil, true},
		{"empty string", "", nil, true},
		{"root with no text", doc(), nil, true},
		{"only the section titles", doc("Purpose", "Scope"), titles, true},
		{"titles with spacing", doc(" Purpose ", ""), titles, true},
		{"text the author wrote", doc("Purpose", "My own words."), titles, false},
		{"text with no template", doc("Purpose"), nil, false},
		{"malformed content is not touched by us", "not json", titles, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := domain.DraftUntouched(c.content, c.titles); got != c.want {
				t.Fatalf("DraftUntouched = %v, want %v", got, c.want)
			}
		})
	}
}

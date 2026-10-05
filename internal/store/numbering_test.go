// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"

	"github.com/Steward-GRC/steward-core/internal/store"
)

func TestPolicyNumberCode(t *testing.T) {
	cases := map[string]string{
		"information-security":   "INFORM",
		"information-technology": "INFORM", // shares a candidate with other slugs; NextFreeCode disambiguates
		"it":                     "IT",
		"hr":                     "HR",
		"":                       "GEN",
		"--!!--":                 "GEN",
		"safety-7":               "SAFETY",
	}
	for slug, want := range cases {
		if got := store.PolicyNumberCode(slug); got != want {
			t.Errorf("PolicyNumberCode(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestNextFreeCode(t *testing.T) {
	taken := map[string]bool{"INFORM": true, "INFORM2": true}
	if got := store.NextFreeCode("INFORM", taken); got != "INFORM3" {
		t.Errorf("NextFreeCode collision: got %q, want INFORM3", got)
	}
	if got := store.NextFreeCode("HR", taken); got != "HR" {
		t.Errorf("NextFreeCode free: got %q, want HR", got)
	}
}

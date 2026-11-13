// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

func TestSemVerBump(t *testing.T) {
	cases := []struct {
		name string
		from domain.SemVer
		ct   domain.ChangeType
		want string
	}{
		{"first publish is 1.0.0 whatever the type", domain.SemVer{}, domain.ChangeTypeFix, "1.0.0"},
		{"major resets minor and fix", domain.SemVer{Major: 2, Minor: 3, Fix: 4}, domain.ChangeTypeMajor, "3.0.0"},
		{"minor resets fix", domain.SemVer{Major: 2, Minor: 3, Fix: 4}, domain.ChangeTypeMinor, "2.4.0"},
		{"fix raises the last part", domain.SemVer{Major: 2, Minor: 3, Fix: 4}, domain.ChangeTypeFix, "2.3.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.from.Bump(c.ct).String(); got != c.want {
				t.Fatalf("Bump = %s, want %s", got, c.want)
			}
		})
	}
}

func TestChangeTypeRankAndMax(t *testing.T) {
	if domain.ChangeTypeFix.Rank() >= domain.ChangeTypeMinor.Rank() || domain.ChangeTypeMinor.Rank() >= domain.ChangeTypeMajor.Rank() {
		t.Fatal("rank must order fix < minor < major")
	}
	if domain.ChangeType("").Rank() != 0 || domain.ChangeType("patch").Valid() {
		t.Fatal("unset ranks 0 and an unknown name is not valid")
	}
	if got := domain.MaxChangeType(domain.ChangeTypeFix, domain.ChangeTypeMinor); got != domain.ChangeTypeMinor {
		t.Fatalf("MaxChangeType = %q", got)
	}
}

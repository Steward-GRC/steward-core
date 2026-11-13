// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import "fmt"

// ChangeType is how big a change is: Major, Minor or Fix. It decides which part of the
// semantic version a publish raises.
type ChangeType string

const (
	ChangeTypeMajor ChangeType = "major"
	ChangeTypeMinor ChangeType = "minor"
	ChangeTypeFix   ChangeType = "fix"
)

// Valid reports whether c is one of the three change types.
func (c ChangeType) Valid() bool {
	return c.Rank() > 0
}

// Rank orders the types: fix 1, minor 2, major 3. An unset or unknown type ranks 0.
func (c ChangeType) Rank() int {
	switch c {
	case ChangeTypeFix:
		return 1
	case ChangeTypeMinor:
		return 2
	case ChangeTypeMajor:
		return 3
	default:
		return 0
	}
}

// MaxChangeType returns the larger of a and b.
func MaxChangeType(a, b ChangeType) ChangeType {
	if b.Rank() > a.Rank() {
		return b
	}
	return a
}

// SemVer is MAJOR.MINOR.FIX. The zero value is "no number yet", as for a draft.
type SemVer struct {
	Major, Minor, Fix int
}

// IsZero reports whether the version has no number.
func (v SemVer) IsZero() bool { return v == SemVer{} }

// String renders "2.1.0".
func (v SemVer) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Fix) }

// Bump returns the version a change of type c produces. From the zero version (the first
// publish) the result is always 1.0.0, whatever the type.
func (v SemVer) Bump(c ChangeType) SemVer {
	switch {
	case v.IsZero():
		return SemVer{Major: 1}
	case c == ChangeTypeMajor:
		return SemVer{Major: v.Major + 1}
	case c == ChangeTypeMinor:
		return SemVer{Major: v.Major, Minor: v.Minor + 1}
	default:
		return SemVer{Major: v.Major, Minor: v.Minor, Fix: v.Fix + 1}
	}
}

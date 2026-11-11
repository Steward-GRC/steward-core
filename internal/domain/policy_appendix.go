// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// AppendixState is the state of one row of an appendix's version table.
type AppendixState string

// The states. An appendix has at most one draft beside its published history.
const (
	AppendixDraft     AppendixState = "DRAFT"
	AppendixPublished AppendixState = "PUBLISHED"
)

// PolicyAppendixVersion is one row of a policy appendix's version table.
type PolicyAppendixVersion struct {
	ID          uuid.UUID
	AppendixID  uuid.UUID
	VersionNo   int
	State       AppendixState
	Title       string
	ContentJSON string
	CreatedAt   time.Time
	CreatedBy   uuid.UUID
	UpdatedAt   time.Time
	UpdatedBy   uuid.UUID
	PublishedAt *time.Time
	PublishedBy *uuid.UUID
	// Editors is everyone who saved or published this row, each once, in the
	// order they first did.
	Editors []uuid.UUID
}

// PolicyAppendix is an appendix that belongs to a policy or procedure, never
// to one of its versions, with a version table of its own.
type PolicyAppendix struct {
	ID        uuid.UUID
	PolicyID  uuid.UUID
	Letter    string
	CreatedAt time.Time
	CreatedBy uuid.UUID
	// OrderIndex is zero-based among the live appendices of the policy.
	OrderIndex int
	RetiredAt  *time.Time
	RetiredBy  *uuid.UUID
	// Versions is oldest first, the draft (if any) last.
	Versions []PolicyAppendixVersion
}

// Live reports whether the appendix is not retired.
func (a PolicyAppendix) Live() bool { return a.RetiredAt == nil }

// Published is the latest published row, or nil before the first publish.
func (a PolicyAppendix) Published() *PolicyAppendixVersion {
	for i := len(a.Versions) - 1; i >= 0; i-- {
		if a.Versions[i].State == AppendixPublished {
			return &a.Versions[i]
		}
	}
	return nil
}

// Draft is the open draft, or nil.
func (a PolicyAppendix) Draft() *PolicyAppendixVersion {
	for i := range a.Versions {
		if a.Versions[i].State == AppendixDraft {
			return &a.Versions[i]
		}
	}
	return nil
}

// PublishedVersions are the published rows, newest first, without the draft.
func (a PolicyAppendix) PublishedVersions() []PolicyAppendixVersion {
	var out []PolicyAppendixVersion
	for i := len(a.Versions) - 1; i >= 0; i-- {
		if a.Versions[i].State == AppendixPublished {
			out = append(out, a.Versions[i])
		}
	}
	return out
}

// ContributorIDs is everyone who saved or published any row, each once, in the
// order they first did.
func (a PolicyAppendix) ContributorIDs() []uuid.UUID {
	var out []uuid.UUID
	for _, v := range a.Versions {
		for _, e := range v.Editors {
			if !slices.Contains(out, e) {
				out = append(out, e)
			}
		}
	}
	return out
}

// NoteEditor records that a person saved or published a row. A person already
// listed keeps their place.
func NoteEditor(editors []uuid.UUID, id uuid.UUID) []uuid.UUID {
	if slices.Contains(editors, id) {
		return editors
	}
	return append(slices.Clone(editors), id)
}

// AppendixLetter is the letter of a place among the live appendices: A to Z,
// then AA, AB and on.
func AppendixLetter(index int) string {
	if index < 0 {
		return ""
	}
	var out []byte
	for n := index; ; n = n/26 - 1 {
		out = append([]byte{byte('A' + n%26)}, out...)
		if n < 26 {
			break
		}
	}
	return string(out)
}

// AssignAppendixLetters sets the letter of each live appendix from its place
// in order_index, and clears the letter of a retired one.
func AssignAppendixLetters(list []PolicyAppendix) {
	var live []int
	for i := range list {
		if list[i].Live() {
			live = append(live, i)
		} else {
			list[i].Letter = ""
		}
	}
	slices.SortStableFunc(live, func(x, y int) int { return list[x].OrderIndex - list[y].OrderIndex })
	for place, i := range live {
		list[i].Letter = AppendixLetter(place)
	}
}

// DefaultAppendixTitle is "Appendix <letter>" for a new appendix placed after
// liveCount live ones. It moves on to the next letter while taken (the titles
// of every version of every appendix of the policy, retired ones included)
// already holds that name, ignoring case and outer spaces.
func DefaultAppendixTitle(taken []string, liveCount int) string {
	used := make(map[string]bool, len(taken))
	for _, t := range taken {
		used[strings.ToLower(strings.TrimSpace(t))] = true
	}
	for i := liveCount; ; i++ {
		title := "Appendix " + AppendixLetter(i)
		if !used[strings.ToLower(title)] {
			return title
		}
	}
}

// ErrAppendixContentInvalid is returned for appendix content that is empty or
// not JSON.
var ErrAppendixContentInvalid = errors.New("appendix content must be a JSON document")

// ValidateAppendixContent checks the serialized editor state.
func ValidateAppendixContent(contentJSON string) error {
	if strings.TrimSpace(contentJSON) == "" || !json.Valid([]byte(contentJSON)) {
		return ErrAppendixContentInvalid
	}
	return nil
}

// Same reports whether a title and content equal this row's. Content counts
// as equal when it is the same JSON, whatever its spacing or key order.
func (v PolicyAppendixVersion) Same(title, contentJSON string) bool {
	return v.Title == title && sameJSON(v.ContentJSON, contentJSON)
}

func sameJSON(a, b string) bool {
	if a == b {
		return true
	}
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	nx, errX := json.Marshal(x)
	ny, errY := json.Marshal(y)
	return errX == nil && errY == nil && bytes.Equal(nx, ny)
}

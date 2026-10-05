// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DefinitionEntry is a glossary entry in the shared library, scoped to a
// category. A document can attach the entries of its home category and its
// ancestors. Term and Definition are required plain text. An archived entry
// is hidden from the picker but still shows where it is attached.
type DefinitionEntry struct {
	ID              uuid.UUID
	CategoryID      uuid.UUID
	Term            string
	Definition      string
	Archived        bool
	CreatedByUserID string    // actor who created the entry
	CreatedAt       time.Time // set by the store on create
	UsedByCount     int       // number of policies attaching this entry (list reads only)
}

// ValidateDefinitionEntry reports whether the editable fields are well-formed.
// Both the term and its definition must be non-empty.
func ValidateDefinitionEntry(term, definition string) error {
	if term == "" {
		return fmt.Errorf("definition term must not be empty")
	}
	if definition == "" {
		return fmt.Errorf("definition text must not be empty")
	}
	return nil
}

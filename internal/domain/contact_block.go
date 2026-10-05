// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"

	"github.com/google/uuid"
)

// ContactBlock is a reusable block of contact details in the shared library.
// Label is required. An archived block is hidden from the picker but still
// shows where it is attached.
type ContactBlock struct {
	ID          uuid.UUID
	Label       string
	Name        string
	Role        string
	Department  string
	Email       string
	Phone       string
	Hours       string
	Notes       string
	Archived    bool
	UsedByCount int // number of policies referencing this block (list reads only)
}

// ValidateContactBlock reports whether the editable fields are well-formed.
// The label is required.
func ValidateContactBlock(label string) error {
	if label == "" {
		return fmt.Errorf("contact block label must not be empty")
	}
	return nil
}

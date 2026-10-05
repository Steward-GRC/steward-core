// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ReferenceKind enumerates the shapes a library Reference can take:
//   - STANDARD: a citation of an external standard/regulation; clause + url are
//     optional supporting detail.
//   - TEXT: a free-text reference note; the body carries the reference.
//   - LINK: an external link; the url is the reference.
type ReferenceKind string

const (
	// ReferenceKindStandard cites an external standard/regulation (clause + url optional).
	ReferenceKindStandard ReferenceKind = "STANDARD"
	// ReferenceKindText is a free-text reference note (body required).
	ReferenceKindText ReferenceKind = "TEXT"
	// ReferenceKindLink is an external link (url required).
	ReferenceKindLink ReferenceKind = "LINK"
)

// Reference is an entry in the shared references library. Label is required;
// the kind-specific fields carry the reference. An archived entry is hidden
// from the picker but still shows where it is attached.
type Reference struct {
	ID              uuid.UUID
	Label           string
	Kind            ReferenceKind
	Clause          string
	Body            string
	URL             string
	Archived        bool
	CreatedByUserID string    // actor who created the entry
	CreatedAt       time.Time // set by the store on create
	UsedByCount     int       // number of policies referencing this entry (list reads only)
}

// ValidateReference reports whether the editable fields are well-formed. The
// label is always required; each kind has its own required fields:
// STANDARD (clause/url optional), TEXT (body required), LINK (url required).
func ValidateReference(label string, kind ReferenceKind, body, url string) error {
	if label == "" {
		return fmt.Errorf("reference label must not be empty")
	}
	switch kind {
	case ReferenceKindStandard:
		// clause + url are optional supporting detail for a standard citation.
	case ReferenceKindText:
		if body == "" {
			return fmt.Errorf("reference body is required for a TEXT reference")
		}
	case ReferenceKindLink:
		if url == "" {
			return fmt.Errorf("reference url is required for a LINK reference")
		}
	default:
		return fmt.Errorf("reference kind must be STANDARD, TEXT, or LINK")
	}
	return nil
}

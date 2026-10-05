// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import "github.com/google/uuid"

// RelatedPolicy is one related link, with the linked policy's current number
// and title read live. Links are not versioned.
type RelatedPolicy struct {
	PolicyID uuid.UUID
	Number   string
	Title    string
}

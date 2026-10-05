// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Appendix is rich-text supporting material attached to a policy version.
type Appendix struct {
	ID              uuid.UUID
	PolicyVersionID uuid.UUID
	Title           string
	ContentJSON     string
	OrderIndex      int
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// NewAppendix builds a validated, unsaved appendix. The store assigns
// OrderIndex.
func NewAppendix(policyVersionID uuid.UUID, title, contentJSON string) (Appendix, error) {
	if title == "" {
		return Appendix{}, fmt.Errorf("appendix title must not be empty")
	}
	if contentJSON == "" {
		return Appendix{}, fmt.Errorf("appendix content must not be empty")
	}
	return Appendix{
		ID:              uuid.New(),
		PolicyVersionID: policyVersionID,
		Title:           title,
		ContentJSON:     contentJSON,
	}, nil
}

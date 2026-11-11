// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import "fmt"

// The reasons a document cannot be moved to a category. The codes travel to the caller as they
// are; the gateway shows the message.
const (
	MoveRefusalRetired             = "RETIRED"
	MoveRefusalCategoryRefusesType = "CATEGORY_REFUSES_TYPE"
)

// MoveRefusal says why one document cannot move.
type MoveRefusal struct {
	// Code is one of the MoveRefusal constants.
	Code string
	// Message is the sentence to show, starting with the code.
	Message string
}

// RefuseMove returns why p cannot move to target, or nil when it can. A retired document never
// moves; a document whose type the target does not hold is refused even when it is already there.
// accepts says whether a category holds a document type; nil means every category holds both, which
// is what an existing category reads as.
func RefuseMove(p Policy, target Category, accepts func(Category, DocumentType) bool) *MoveRefusal {
	if p.RetiredAt != nil {
		return &MoveRefusal{Code: MoveRefusalRetired, Message: "RETIRED: a retired document cannot be moved."}
	}
	if accepts != nil && !accepts(target, p.DocumentType) {
		kind := p.DocumentType
		if kind == "" {
			kind = DocumentTypePolicy
		}
		return &MoveRefusal{
			Code:    MoveRefusalCategoryRefusesType,
			Message: fmt.Sprintf("CATEGORY_REFUSES_TYPE: %s does not hold %s documents.", target.Name, kind),
		}
	}
	return nil
}

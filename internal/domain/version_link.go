// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

// Errors returned by the link validators. Callers map them to
// codes.InvalidArgument.
var (
	ErrUnknownRelationshipType = errors.New("unknown relationship type")
	ErrRelatedToItself         = errors.New("a policy cannot relate to itself")
	ErrRepeatedLink            = errors.New("each item can be named only once")
	ErrEmptyDefinitionTerm     = errors.New("a definition needs a title")
)

// RelationshipType is how one policy relates to another, with the type the
// other side of the link reads as.
type RelationshipType struct {
	ID           string
	Label        string
	InverseID    string
	InverseLabel string
}

// relationshipPairs is the fixed list: id, label, inverse id, inverse label.
// Every type has an inverse and the inverse of the inverse is the type itself.
var relationshipPairs = [][4]string{
	{"RELATED_TO", "is related to", "RELATED_BY", "is related by"},
	{"SUPERSEDES", "supersedes", "SUPERSEDED_BY", "is superseded by"},
	{"DEPENDS_ON", "depends on", "REQUIRED_BY", "is required by"},
	{"IMPLEMENTS", "implements", "IMPLEMENTED_BY", "is implemented by"},
	{"REFERENCES", "references", "REFERENCED_BY", "is referenced by"},
}

var relationshipTypes = func() []RelationshipType {
	out := make([]RelationshipType, 0, 2*len(relationshipPairs))
	for _, p := range relationshipPairs {
		out = append(out,
			RelationshipType{ID: p[0], Label: p[1], InverseID: p[2], InverseLabel: p[3]},
			RelationshipType{ID: p[2], Label: p[3], InverseID: p[0], InverseLabel: p[1]})
	}
	return out
}()

// RelationshipTypes returns the fixed list in a stable order (each pair side by
// side). The slice is a copy.
func RelationshipTypes() []RelationshipType {
	return append([]RelationshipType(nil), relationshipTypes...)
}

// LookupRelationshipType finds a type by id.
func LookupRelationshipType(id string) (RelationshipType, bool) {
	for _, t := range relationshipTypes {
		if t.ID == id {
			return t, true
		}
	}
	return RelationshipType{}, false
}

// RelatedDirection says which side of a link this version is on.
type RelatedDirection string

const (
	// RelatedOutgoing means this version names the other policy.
	RelatedOutgoing RelatedDirection = "OUTGOING"
	// RelatedIncoming means the other policy's published version names this
	// one; the type and label read as the inverse.
	RelatedIncoming RelatedDirection = "INCOMING"
)

// RelatedLinkInput is one related item a caller sets: another policy and how
// this version relates to it.
type RelatedLinkInput struct {
	PolicyID uuid.UUID
	Type     string
}

// RelatedLink is one related item as read, with the other policy's current
// number and title.
type RelatedLink struct {
	PolicyID  uuid.UUID
	Number    string
	Title     string
	Type      string
	Label     string
	Direction RelatedDirection
}

// VersionLinks is what one policy version links to from the libraries. The
// library items are not versioned; the links are. References and contacts keep
// the author's order, definitions are alphabetical.
type VersionLinks struct {
	PolicyVersionID uuid.UUID
	// Editable is status only: the version holds the draft slot. The gateway
	// combines it with the caller's permission.
	Editable    bool
	Definitions []DefinitionEntry
	Related     []RelatedLink
	References  []Reference
	Contacts    []ContactBlock
}

// ValidateRelatedLinks checks a full replace of a version's related items: a
// known type each, none to the version's own policy, and no (policy, type)
// twice.
func ValidateRelatedLinks(ownPolicyID uuid.UUID, links []RelatedLinkInput) error {
	type key struct {
		policy uuid.UUID
		typ    string
	}
	seen := map[key]bool{}
	for _, l := range links {
		if _, ok := LookupRelationshipType(l.Type); !ok {
			return ErrUnknownRelationshipType
		}
		if l.PolicyID == ownPolicyID {
			return ErrRelatedToItself
		}
		k := key{l.PolicyID, l.Type}
		if seen[k] {
			return ErrRepeatedLink
		}
		seen[k] = true
	}
	return nil
}

// ValidateLinkIDs refuses a list that names an item twice.
func ValidateLinkIDs(ids []uuid.UUID) error {
	seen := make(map[uuid.UUID]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return ErrRepeatedLink
		}
		seen[id] = true
	}
	return nil
}

// NormalizeDefinitionTerm trims a definition's title and refuses an empty one.
func NormalizeDefinitionTerm(term string) (string, error) {
	term = strings.TrimSpace(term)
	if term == "" {
		return "", ErrEmptyDefinitionTerm
	}
	return term, nil
}

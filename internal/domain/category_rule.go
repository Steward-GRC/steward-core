// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	apperr "github.com/Bugs5382/go-apperr"
	"github.com/Bugs5382/go-authz/ordered"

	"github.com/Steward-GRC/steward-core/internal/errcodes"
)

// Action is one governed action on a category's documents. The ids are the
// ones go-authz's ordered package decides on.
type Action string

// The actions a rule has a cell for.
const (
	ActionRead        Action = "read"
	ActionAcknowledge Action = "ack"
	ActionApprove     Action = "approve"
	ActionAuthor      Action = "author"
	ActionManage      Action = "manage"
)

// Actions returns the actions a rule may carry a grant for. The set is the
// ordered engine's own, so a stored cell is always one it can decide.
func Actions() []Action {
	defs := ordered.DefaultActions()
	out := make([]Action, len(defs))
	for i, d := range defs {
		out[i] = Action(d.ID)
	}
	return out
}

// Grant is the three-state value of one action cell in a rule. A blank cell
// has no opinion, so evaluation falls through to the next rule.
type Grant string

// The grants. Allow and deny are the ordered engine's effects.
const (
	GrantBlank Grant = ""
	GrantAllow Grant = Grant(ordered.Allow)
	GrantDeny  Grant = Grant(ordered.Deny)
)

// SubjectKind says what a rule targets.
type SubjectKind string

// The subject kinds.
const (
	SubjectEveryone SubjectKind = "everyone"
	SubjectGroup    SubjectKind = "group"
	SubjectUser     SubjectKind = "user"
)

// RuleSubject is who a rule targets. Name is a directory group name or a user
// id, and empty for SubjectEveryone.
type RuleSubject struct {
	Kind SubjectKind
	Name string
}

// ErrInvalidRule marks a category ruleset that failed validation. It reaches a
// caller coded errcodes.CodeInvalidCategoryRule, with the category, the 1-based
// rule number and the problem as metadata.
var ErrInvalidRule = errors.New("core: invalid access rule")

// CategoryRule is one ordered access rule of a category, in the shape it is
// stored in: one column per action.
type CategoryRule struct {
	Ordinal     int
	SubjectKind SubjectKind
	// A directory group name or a user id; empty for everyone.
	SubjectRef string
	Read       Grant
	Ack        Grant
	Approve    Grant
	Author     Grant
	Manage     Grant
}

// Subject returns who the rule targets.
func (r CategoryRule) Subject() RuleSubject {
	return RuleSubject{Kind: r.SubjectKind, Name: r.SubjectRef}
}

// Grants returns the rule's cell for every action.
func (r CategoryRule) Grants() map[Action]Grant {
	return map[Action]Grant{
		ActionRead:        r.Read,
		ActionAcknowledge: r.Ack,
		ActionApprove:     r.Approve,
		ActionAuthor:      r.Author,
		ActionManage:      r.Manage,
	}
}

// ValidateCategoryRules checks a category's rules before they are stored: the
// subject kind is known, a group or user rule names its subject, an everyone
// rule does not, and every cell holds a known action's blank, allow or deny.
// The error matches ErrInvalidRule and is coded errcodes.CodeInvalidCategoryRule.
func ValidateCategoryRules(category string, rules []CategoryRule) error {
	known := Actions()
	for i, r := range rules {
		n := strconv.Itoa(i + 1)
		named := strings.TrimSpace(r.SubjectRef) != ""
		switch r.SubjectKind {
		case SubjectEveryone:
			if r.SubjectRef != "" {
				return invalidCategoryRule(category, n, "subject")
			}
		case SubjectGroup, SubjectUser:
			if !named {
				return invalidCategoryRule(category, n, "subject")
			}
		default:
			return invalidCategoryRule(category, n, "subject")
		}
		grants := r.Grants()
		keys := make([]Action, 0, len(grants))
		for a := range grants {
			keys = append(keys, a)
		}
		slices.Sort(keys)
		for _, a := range keys {
			if !slices.Contains(known, a) {
				return invalidCategoryRule(category, n, "action")
			}
			switch grants[a] {
			case GrantBlank, GrantAllow, GrantDeny:
			default:
				return invalidCategoryRule(category, n, "grant")
			}
		}
	}
	return nil
}

// ValidateOwners checks a category's owner ids: none may be blank.
func ValidateOwners(category string, owners []string) error {
	for _, o := range owners {
		if strings.TrimSpace(o) == "" {
			return invalidCategoryRule(category, "", "owner")
		}
	}
	return nil
}

func invalidCategoryRule(category, rule, problem string) error {
	meta := []apperr.MetaPair{apperr.Meta("category", category), apperr.Meta("problem", problem)}
	if rule != "" {
		meta = append(meta, apperr.Meta("rule", rule))
	}
	return apperr.WithMeta(apperr.Coded(errcodes.CodeInvalidCategoryRule, ErrInvalidRule), meta...)
}

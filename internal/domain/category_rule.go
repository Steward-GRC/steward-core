// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"strconv"

	apperr "github.com/Bugs5382/go-apperr"
	stewardauthz "github.com/Steward-GRC/steward-authz"
)

// CategoryRule is one ordered access rule of a category, in the shape it is
// stored in: one column per action. The values are steward-authz's, so the
// stored rule is the rule the access engine evaluates.
type CategoryRule struct {
	Ordinal     int
	SubjectKind stewardauthz.SubjectKind
	// A directory group name or a user id; empty for everyone.
	SubjectRef string
	Read       stewardauthz.Grant
	Ack        stewardauthz.Grant
	Approve    stewardauthz.Grant
	Author     stewardauthz.Grant
	// Manage is stored and returned like the other cells. steward-authz does
	// not know the action yet, so AuthzRule leaves it out and this package
	// validates it.
	Manage stewardauthz.Grant
}

// AuthzRule returns the rule as steward-authz evaluates it.
func (r CategoryRule) AuthzRule() stewardauthz.Rule {
	return stewardauthz.Rule{
		Subject: stewardauthz.RuleSubject{Kind: r.SubjectKind, Name: r.SubjectRef},
		Grants: map[stewardauthz.Action]stewardauthz.Grant{
			stewardauthz.ActionRead:        r.Read,
			stewardauthz.ActionAcknowledge: r.Ack,
			stewardauthz.ActionApprove:     r.Approve,
			stewardauthz.ActionAuthor:      r.Author,
		},
	}
}

// ValidateCategoryRules checks rules with steward-authz before they are
// stored, then checks the Manage cell the same way as the other four. The
// error carries steward-authz's code and metadata.
func ValidateCategoryRules(category string, rules []CategoryRule) error {
	rs := stewardauthz.CategoryRuleset{Name: category, Rules: make([]stewardauthz.Rule, len(rules))}
	for i, r := range rules {
		rs.Rules[i] = r.AuthzRule()
	}
	if err := rs.Validate(); err != nil {
		return err
	}
	for i, r := range rules {
		switch r.Manage {
		case stewardauthz.GrantBlank, stewardauthz.GrantAllow, stewardauthz.GrantDeny:
		default:
			return invalidCategoryRule(category, i+1, "grant")
		}
	}
	return nil
}

// invalidCategoryRule builds the error steward-authz would for the same rule,
// so a caller handles both alike.
func invalidCategoryRule(category string, rule int, problem string) error {
	return apperr.WithMeta(apperr.Coded(stewardauthz.CodeInvalidRule, stewardauthz.ErrInvalidRule),
		apperr.Meta("category", category), apperr.Meta("problem", problem), apperr.Meta("rule", strconv.Itoa(rule)))
}

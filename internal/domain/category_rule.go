// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
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
// stored. The error carries steward-authz's code and metadata.
func ValidateCategoryRules(category string, rules []CategoryRule) error {
	rs := stewardauthz.CategoryRuleset{Name: category, Rules: make([]stewardauthz.Rule, len(rules))}
	for i, r := range rules {
		rs.Rules[i] = r.AuthzRule()
	}
	return rs.Validate()
}

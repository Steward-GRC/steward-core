// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"errors"
	"testing"

	apperr "github.com/Bugs5382/go-apperr"
	stewardauthz "github.com/Steward-GRC/steward-authz"
	"github.com/stretchr/testify/require"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

func TestCategoryRuleMapsToTheAuthzRule(t *testing.T) {
	r := domain.CategoryRule{SubjectKind: "group", SubjectRef: "facilities-team", Read: "allow", Approve: "deny"}
	got := r.AuthzRule()
	require.Equal(t, stewardauthz.RuleSubject{Kind: stewardauthz.SubjectGroup, Name: "facilities-team"}, got.Subject)
	require.Equal(t, map[stewardauthz.Action]stewardauthz.Grant{
		stewardauthz.ActionRead:        stewardauthz.GrantAllow,
		stewardauthz.ActionAcknowledge: stewardauthz.GrantBlank,
		stewardauthz.ActionApprove:     stewardauthz.GrantDeny,
		stewardauthz.ActionAuthor:      stewardauthz.GrantBlank,
	}, got.Grants)
}

func TestValidateCategoryRulesAcceptsAWellFormedRuleset(t *testing.T) {
	require.NoError(t, domain.ValidateCategoryRules("Facilities", []domain.CategoryRule{
		{SubjectKind: "everyone", Read: "allow"},
		{SubjectKind: "user", SubjectRef: "8b0c7a52-6f1e-4c55-9d3a-2f4b1c0e9a77", Author: "allow"},
	}))
}

func TestValidateCategoryRulesRefusesWithTheAuthzCode(t *testing.T) {
	for name, rules := range map[string][]domain.CategoryRule{
		"unknown subject kind":   {{SubjectKind: "team", SubjectRef: "x"}},
		"everyone with a name":   {{SubjectKind: "everyone", SubjectRef: "x"}},
		"group without a name":   {{SubjectKind: "group"}},
		"grant outside the list": {{SubjectKind: "everyone", Read: "maybe"}},
	} {
		err := domain.ValidateCategoryRules("Facilities", rules)
		require.Error(t, err, name)
		require.True(t, errors.Is(err, stewardauthz.ErrInvalidRule), name)
		code, _ := apperr.Code(err)
		require.Equal(t, stewardauthz.CodeInvalidRule, code, name)
		require.Equal(t, "1", apperr.Metadata(err)["rule"], name)
	}
}

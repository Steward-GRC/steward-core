// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"errors"
	"strconv"
	"testing"

	apperr "github.com/Bugs5382/go-apperr"
	"github.com/stretchr/testify/require"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/errcodes"
)

func TestCategoryRuleGrantsListEveryAction(t *testing.T) {
	r := domain.CategoryRule{SubjectKind: "group", SubjectRef: "facilities-team", Read: "allow", Approve: "deny", Manage: "allow"}
	require.Equal(t, domain.RuleSubject{Kind: domain.SubjectGroup, Name: "facilities-team"}, r.Subject())
	require.Equal(t, map[domain.Action]domain.Grant{
		domain.ActionRead:        domain.GrantAllow,
		domain.ActionAcknowledge: domain.GrantBlank,
		domain.ActionApprove:     domain.GrantDeny,
		domain.ActionAuthor:      domain.GrantBlank,
		domain.ActionManage:      domain.GrantAllow,
	}, r.Grants())
}

func TestActionsIncludeManage(t *testing.T) {
	require.ElementsMatch(t, []domain.Action{"read", "ack", "approve", "author", "manage"}, domain.Actions())
}

func TestValidateCategoryRulesAcceptsAWellFormedRuleset(t *testing.T) {
	require.NoError(t, domain.ValidateCategoryRules("Facilities", []domain.CategoryRule{
		{SubjectKind: "everyone", Read: "allow"},
		{SubjectKind: "user", SubjectRef: "8b0c7a52-6f1e-4c55-9d3a-2f4b1c0e9a77", Author: "allow"},
	}))
}

func TestValidateCategoryRulesRefusesWithTheInvalidRuleCode(t *testing.T) {
	for name, tc := range map[string]struct {
		rules   []domain.CategoryRule
		problem string
	}{
		"unknown subject kind":   {[]domain.CategoryRule{{SubjectKind: "team", SubjectRef: "x"}}, "subject"},
		"everyone with a name":   {[]domain.CategoryRule{{SubjectKind: "everyone", SubjectRef: "x"}}, "subject"},
		"group without a name":   {[]domain.CategoryRule{{SubjectKind: "group"}}, "subject"},
		"user with a blank name": {[]domain.CategoryRule{{SubjectKind: "user", SubjectRef: "  "}}, "subject"},
		"grant outside the list": {[]domain.CategoryRule{{SubjectKind: "everyone", Read: "maybe"}}, "grant"},
		"second rule is bad":     {[]domain.CategoryRule{{SubjectKind: "everyone", Read: "allow"}, {SubjectKind: "group"}}, "subject"},
	} {
		err := domain.ValidateCategoryRules("Facilities", tc.rules)
		require.Error(t, err, name)
		require.True(t, errors.Is(err, domain.ErrInvalidRule), name)
		code, _ := apperr.Code(err)
		require.Equal(t, errcodes.CodeInvalidCategoryRule, code, name)
		require.Equal(t, 4003, code, name)
		meta := apperr.Metadata(err)
		require.Equal(t, "Facilities", meta["category"], name)
		require.Equal(t, tc.problem, meta["problem"], name)
		require.Equal(t, strconv.Itoa(len(tc.rules)), meta["rule"], name)
	}
}

func TestValidateCategoryRulesAcceptsARuleWithOnlyManage(t *testing.T) {
	require.NoError(t, domain.ValidateCategoryRules("Facilities", []domain.CategoryRule{
		{SubjectKind: "group", SubjectRef: "doc-managers", Manage: "allow"},
		{SubjectKind: "user", SubjectRef: "8b0c7a52-6f1e-4c55-9d3a-2f4b1c0e9a77", Manage: "deny"},
	}))
}

func TestValidateCategoryRulesRefusesAnUnknownManageGrant(t *testing.T) {
	err := domain.ValidateCategoryRules("Facilities", []domain.CategoryRule{{SubjectKind: "everyone", Manage: "maybe"}})
	require.True(t, errors.Is(err, domain.ErrInvalidRule))
	code, _ := apperr.Code(err)
	require.Equal(t, 4003, code)
	require.Equal(t, "grant", apperr.Metadata(err)["problem"])
	require.Equal(t, "1", apperr.Metadata(err)["rule"])
}

func TestValidateOwnersRefusesABlankOwner(t *testing.T) {
	require.NoError(t, domain.ValidateOwners("Facilities", []string{"8b0c7a52-6f1e-4c55-9d3a-2f4b1c0e9a77"}))
	err := domain.ValidateOwners("Facilities", []string{"8b0c7a52-6f1e-4c55-9d3a-2f4b1c0e9a77", " "})
	require.True(t, errors.Is(err, domain.ErrInvalidRule))
	require.Equal(t, "owner", apperr.Metadata(err)["problem"])
	_, hasRule := apperr.Metadata(err)["rule"]
	require.False(t, hasRule)
}

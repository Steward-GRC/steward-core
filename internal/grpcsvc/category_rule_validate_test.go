// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	"github.com/Bugs5382/go-apperr/apperrgrpc"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
)

func TestSetCategoryRulesetRefusesARuleStewardAuthzRejects(t *testing.T) {
	ctx := context.Background()
	fs := newFakeCategoryStoreWithRules()
	cat, err := fs.Create(ctx, domain.Category{Name: "Facilities"})
	require.NoError(t, err)
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, audit.New(cap))

	_, err = h.SetCategoryRuleset(ctx, &corev1.SetCategoryRulesetRequest{
		CategoryId: cat.ID.String(),
		Rules: []*corev1.CategoryRule{
			{SubjectKind: corev1.RuleSubjectKind_RULE_SUBJECT_KIND_EVERYONE, Read: corev1.GrantEffect_GRANT_EFFECT_ALLOW},
			{SubjectKind: corev1.RuleSubjectKind_RULE_SUBJECT_KIND_USER, Author: corev1.GrantEffect_GRANT_EFFECT_ALLOW},
		},
	})
	st := status.Convert(err)
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Equal(t, "Access rule 2 in Facilities is not valid (subject).", st.Message())
	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok)
	require.Equal(t, 4003, info.Code)

	stored, err := fs.GetCategoryRuleset(ctx, cat.ID)
	require.NoError(t, err)
	require.Empty(t, stored, "a refused ruleset is not saved")
	require.Empty(t, cap.calls, "a refused ruleset emits no audit event")
}

func TestSetCategoryRulesetNamesAnUnknownCategoryByID(t *testing.T) {
	id := uuid.New()
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStoreWithRules(), nil)
	_, err := h.SetCategoryRuleset(context.Background(), &corev1.SetCategoryRulesetRequest{
		CategoryId: id.String(),
		Rules:      []*corev1.CategoryRule{{SubjectKind: corev1.RuleSubjectKind_RULE_SUBJECT_KIND_GROUP}},
	})
	require.Contains(t, status.Convert(err).Message(), id.String())
}

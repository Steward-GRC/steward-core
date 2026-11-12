// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// The plain fake has no rulesets: every level is empty.
func (f *fakeCategoryStore) GetEffectiveRuleset(ctx context.Context, categoryID uuid.UUID) ([]store.EffectiveLevel, error) {
	return (&fakeCategoryStoreWithRules{fakeCategoryStore: f}).GetEffectiveRuleset(ctx, categoryID)
}

// GetEffectiveRuleset on the fake walks the parents and joins the rulesets;
// the SQL itself is tested in the store tests.
func (f *fakeCategoryStoreWithRules) GetEffectiveRuleset(ctx context.Context, categoryID uuid.UUID) ([]store.EffectiveLevel, error) {
	chain, _ := f.AncestorChain(ctx, categoryID)
	if len(chain) == 0 {
		return nil, store.ErrCategoryNotFound
	}
	out := make([]store.EffectiveLevel, 0, len(chain))
	for _, g := range chain {
		rules := f.rulesets[g.ID]
		if rules == nil {
			rules = []domain.CategoryRule{}
		}
		out = append(out, store.EffectiveLevel{CategoryID: g.ID, Name: g.Name, Owners: g.Owners, Rules: rules})
	}
	return out, nil
}

func TestCategoryHandler_GetEffectiveRulesetReturnsTheChainNearestFirst(t *testing.T) {
	f := newFakeCategoryStoreWithRules()
	owner := uuid.New()
	top := domain.Category{ID: uuid.New(), Name: "Top", Owners: []uuid.UUID{owner}}
	leaf := domain.Category{ID: uuid.New(), Name: "Leaf", ParentID: top.ID}
	f.categories[top.ID], f.categories[leaf.ID] = top, leaf
	f.rulesets[top.ID] = []domain.CategoryRule{{Ordinal: 1, SubjectKind: "everyone", Read: "allow"}}
	f.rulesets[leaf.ID] = []domain.CategoryRule{
		{Ordinal: 1, SubjectKind: "group", SubjectRef: "g", Manage: "allow"},
		{Ordinal: 2, SubjectKind: "user", SubjectRef: "u", Read: "deny", Ack: "allow", Approve: "allow", Author: "deny"},
	}
	h := grpcsvc.NewCategoryHandler(f, nil)

	resp, err := h.GetEffectiveRuleset(context.Background(), &corev1.GetEffectiveRulesetRequest{CategoryId: leaf.ID.String()})
	require.NoError(t, err)
	require.Len(t, resp.GetLevels(), 2)
	l0, l1 := resp.GetLevels()[0], resp.GetLevels()[1]
	require.Equal(t, leaf.ID.String(), l0.GetCategoryId())
	require.Equal(t, "Leaf", l0.GetName())
	require.Empty(t, l0.GetOwners())
	require.Len(t, l0.GetRules(), 2)
	require.Equal(t, corev1.GrantEffect_GRANT_EFFECT_ALLOW, l0.GetRules()[0].GetManage())
	require.Equal(t, corev1.GrantEffect_GRANT_EFFECT_DENY, l0.GetRules()[1].GetRead())
	require.Equal(t, top.ID.String(), l1.GetCategoryId())
	require.Equal(t, []string{owner.String()}, l1.GetOwners())
}

func TestCategoryHandler_GetEffectiveRulesetErrors(t *testing.T) {
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStoreWithRules(), nil)
	_, err := h.GetEffectiveRuleset(context.Background(), &corev1.GetEffectiveRulesetRequest{CategoryId: "nope"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = h.GetEffectiveRuleset(context.Background(), &corev1.GetEffectiveRulesetRequest{CategoryId: uuid.NewString()})
	require.Equal(t, codes.NotFound, status.Code(err))
}

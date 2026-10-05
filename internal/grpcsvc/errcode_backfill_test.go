// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Bugs5382/go-apperr/apperrgrpc"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// errStoreDown is a store fault that isn't pgx.ErrNoRows, so the handler
// codes it STORE_UNAVAILABLE rather than NotFound.
var errStoreDown = errors.New("dial tcp core-pg:5432: connect: connection refused")

// deleteBlockedCategoryStore refuses every delete with ErrCategoryHasPolicies;
// the category still exists, so the handler can resolve its name.
type deleteBlockedCategoryStore struct {
	*fakeCategoryStore
}

func (s *deleteBlockedCategoryStore) Delete(context.Context, uuid.UUID) error {
	return store.ErrCategoryHasPolicies
}

// failingPolicyStore fails every read with a store fault.
type failingPolicyStore struct {
	*fakePolicyStore
}

func (s *failingPolicyStore) GetPolicy(context.Context, uuid.UUID) (domain.Policy, error) {
	return domain.Policy{}, errStoreDown
}

func (s *failingPolicyStore) ListPolicies(context.Context, uuid.UUID, bool, domain.DocumentType) ([]domain.Policy, error) {
	return nil, errStoreDown
}

func TestDeleteCategoryHasPoliciesCarriesCategoryName(t *testing.T) {
	ctx := context.Background()
	base := newFakeCategoryStore()
	g, err := base.Create(ctx, domain.Category{Name: "Finance"})
	require.NoError(t, err)

	h := grpcsvc.NewCategoryHandler(&deleteBlockedCategoryStore{base}, nil)
	_, err = h.DeleteCategory(ctx, &corev1.DeleteCategoryRequest{Id: g.ID.String()})
	require.Error(t, err)

	st, ok := status.FromError(err)
	require.True(t, ok, "error must be a gRPC status")
	require.Equal(t, codes.FailedPrecondition, st.Code(), "must preserve the FailedPrecondition gRPC code")
	require.Contains(t, st.Message(), "Finance", "message must carry the resolved category name")

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok, "status must carry ErrorInfo")
	require.Equal(t, "CATEGORY_NOT_DELETABLE", info.Symbol)
	require.Equal(t, 4002, info.Code)
	require.Equal(t, "core", info.Domain)
	require.Equal(t, "Finance", info.Metadata["category"], "metadata must carry the resolved category NAME, not a raw id")
}

func TestGetPolicyStoreFaultCarriesCode(t *testing.T) {
	ps := &failingPolicyStore{newFakePolicyStore()}
	h := grpcsvc.NewPolicyHandler(ps, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil)

	_, err := h.GetPolicy(context.Background(), &corev1.GetPolicyRequest{Id: uuid.NewString()})
	requireStoreUnavailable(t, err, "get_policy")
}

func TestListPoliciesStoreFaultCarriesCode(t *testing.T) {
	ps := &failingPolicyStore{newFakePolicyStore()}
	h := grpcsvc.NewPolicyHandler(ps, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil)

	_, err := h.ListPolicies(context.Background(), &corev1.ListPoliciesRequest{CategoryId: uuid.NewString()})
	requireStoreUnavailable(t, err, "list_policies")
}

func requireStoreUnavailable(t *testing.T, err error, wantOp string) {
	t.Helper()
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok, "error must be a gRPC status")
	require.Equal(t, codes.Internal, st.Code(), "must preserve the Internal gRPC code")

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok, "status must carry ErrorInfo")
	require.Equal(t, "STORE_UNAVAILABLE", info.Symbol)
	require.Equal(t, 4001, info.Code)
	require.Equal(t, "core", info.Domain)
	require.Equal(t, wantOp, info.Metadata["op"])
}

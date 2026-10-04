// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package errcodes_test

import (
	"context"
	"errors"
	"os"
	"testing"

	apperr "github.com/Bugs5382/go-apperr"
	"github.com/Bugs5382/go-apperr/apperrgrpc"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Steward-GRC/steward-core/internal/errcodes"
)

func TestCategoryNotDeletableRoundTrip(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(), errcodes.CategoryNotDeletable("Facilities", nil)))
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Contains(t, st.Message(), "Facilities", "message must interpolate the resolved category name")

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok, "status must carry ErrorInfo")
	require.Equal(t, "CATEGORY_NOT_DELETABLE", info.Symbol)
	require.Equal(t, 4002, info.Code)
	require.Equal(t, "core", info.Domain)
	require.Equal(t, "Facilities", info.Metadata["category"])
}

func TestStoreUnavailableRoundTrip(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(),
		errcodes.StoreUnavailable("get_policy", errors.New("dial tcp 192.0.2.10:5432: connect: connection refused"))))
	require.Equal(t, codes.Internal, st.Code())
	require.Equal(t, "Code 4001: Internal Error", st.Message(), "a store cause never reaches the wire")

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok, "status must carry ErrorInfo")
	require.Equal(t, "STORE_UNAVAILABLE", info.Symbol)
	require.Equal(t, 4001, info.Code)
	require.Equal(t, "core", info.Domain)
	require.Equal(t, "get_policy", info.Metadata["op"])
	entry, _ := errcodes.Registry().Describe(4001)
	require.False(t, entry.UserSafe, "store faults are not user-safe")
}

func TestInvalidCategoryRuleRoundTrip(t *testing.T) {
	cause := apperr.WithMeta(errors.New("authz: invalid access rule"),
		apperr.Meta("category", "Facilities"), apperr.Meta("rule", "2"), apperr.Meta("problem", "subject"))
	st := status.Convert(errcodes.Error(context.Background(), errcodes.InvalidCategoryRule(cause)))
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Equal(t, "Access rule 2 in Facilities is not valid (subject).", st.Message())

	info, _ := apperrgrpc.FromStatus(st)
	require.Equal(t, "INVALID_CATEGORY_RULE", info.Symbol)
	require.Equal(t, 4003, info.Code)
}

func TestUncodedErrorsFallBackToInternal(t *testing.T) {
	info, _ := apperrgrpc.FromError(errcodes.Error(context.Background(), errors.New("boom")))
	require.Equal(t, errcodes.CodeInternal, info.Code)
	require.Equal(t, "INTERNAL", info.Symbol)
}

func TestRegistryBandAndDomain(t *testing.T) {
	require.NotEmpty(t, errcodes.Entries())
	for _, e := range errcodes.Entries() {
		require.Equal(t, 4, e.Code/1000, "code %d must be in band 4", e.Code)
		_, ok := errcodes.Registry().Describe(e.Code)
		require.True(t, ok)
	}
}

// docs/error-codes.md is generated from the registry; refresh it with
// UPDATE_DOCS=1 go test ./internal/errcodes.
func TestErrorCodesDocIsCurrent(t *testing.T) {
	const path = "../../docs/error-codes.md"
	want := errcodes.Doc()
	if os.Getenv("UPDATE_DOCS") == "1" {
		require.NoError(t, os.WriteFile(path, []byte(want), 0o600))
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, want, string(got), "docs/error-codes.md is stale; run UPDATE_DOCS=1 go test ./internal/errcodes")
}

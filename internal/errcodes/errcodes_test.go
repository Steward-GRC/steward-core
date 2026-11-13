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
	cause := apperr.WithMeta(errors.New("core: invalid access rule"),
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

func TestCategoryTemplateLockedRoundTrip(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(), errcodes.CategoryTemplateLocked("Standard Policy")))
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Equal(t, "This category requires the Standard Policy template.", st.Message())

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok, "status must carry ErrorInfo")
	require.Equal(t, "CATEGORY_TEMPLATE_LOCKED", info.Symbol)
	require.Equal(t, 4004, info.Code)
	require.Equal(t, "Standard Policy", info.Metadata["template"])
}

func TestTemplateLockNeedsDefaultRoundTrip(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(), errcodes.TemplateLockNeedsDefault("Travel")))
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Equal(t, "Choose a default template for Travel before locking it.", st.Message())

	info, _ := apperrgrpc.FromStatus(st)
	require.Equal(t, "TEMPLATE_LOCK_NEEDS_DEFAULT", info.Symbol)
	require.Equal(t, 4005, info.Code)
	require.Equal(t, "Travel", info.Metadata["category"])
}

func TestMoveRefusedRoundTrip(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(),
		errcodes.MoveRefused("POL-EXP-000001", "CATEGORY_REFUSES_TYPE", "CATEGORY_REFUSES_TYPE: Conduct does not hold policy documents.")))
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Contains(t, st.Message(), "CATEGORY_REFUSES_TYPE")
	require.Contains(t, st.Message(), "POL-EXP-000001")

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok, "status must carry ErrorInfo")
	require.Equal(t, "MOVE_REFUSED", info.Symbol)
	require.Equal(t, 4006, info.Code)
	require.Equal(t, "CATEGORY_REFUSES_TYPE", info.Metadata["reason"])
}

func TestCategoryRefusesTypeRoundTrip(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(),
		errcodes.CategoryRefusesType("Expenses", "procedure", "policies")))
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Contains(t, st.Message(), "Expenses")
	require.Contains(t, st.Message(), "procedures")

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok, "status must carry ErrorInfo")
	require.Equal(t, "CATEGORY_REFUSES_TYPE", info.Symbol)
	require.Equal(t, 4007, info.Code)
	require.Equal(t, "Expenses", info.Metadata["category"])
	require.Equal(t, "procedure", info.Metadata["type"])
	require.Equal(t, "policies", info.Metadata["accepts"])
}

func TestCategoryRefusesTypeContainerSaysPickAChild(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(),
		errcodes.CategoryRefusesType("Workplace", "policy", "none")))
	require.Contains(t, st.Message(), "child category")
}

func TestCategoryHoldsDocumentsRoundTrip(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(), errcodes.CategoryHoldsDocuments("Expenses", 3, 2)))
	require.Equal(t, codes.FailedPrecondition, st.Code())
	require.Contains(t, st.Message(), "Expenses")
	require.Contains(t, st.Message(), "5 documents")

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok, "status must carry ErrorInfo")
	require.Equal(t, "CATEGORY_HOLDS_DOCUMENTS", info.Symbol)
	require.Equal(t, 4008, info.Code)
	require.Equal(t, "5", info.Metadata["count"])
	require.Equal(t, "3", info.Metadata["policies"])
	require.Equal(t, "2", info.Metadata["procedures"])
}

func TestInvalidWorkCalendarRoundTrip(t *testing.T) {
	st := status.Convert(errcodes.Error(context.Background(), errcodes.InvalidWorkCalendar("holiday date 2026-02-30 is not a real YYYY-MM-DD date")))
	require.Equal(t, codes.InvalidArgument, st.Code())
	require.Contains(t, st.Message(), "work calendar is not valid")
	require.Contains(t, st.Message(), "2026-02-30", "the problem is interpolated")

	info, ok := apperrgrpc.FromStatus(st)
	require.True(t, ok)
	require.Equal(t, "INVALID_WORK_CALENDAR", info.Symbol)
	require.Equal(t, 4009, info.Code)
	require.Equal(t, "core", info.Domain)
}

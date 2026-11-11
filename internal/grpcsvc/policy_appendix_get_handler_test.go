// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func (f *fakePolicyAppendixBackend) Get(_ context.Context, id uuid.UUID) (domain.PolicyAppendix, error) {
	f.calls++
	f.gotID = id
	return f.result, f.err
}

func TestGetPolicyAppendix_Found(t *testing.T) {
	f := newAppendixFixture()
	resp, err := f.h.GetPolicyAppendix(context.Background(), &corev1.GetPolicyAppendixRequest{Id: f.id.String()})
	require.NoError(t, err)
	require.Equal(t, f.id, f.b.gotID)

	a := resp.GetAppendix()
	require.Equal(t, f.id.String(), a.GetId())
	require.Equal(t, f.policy.String(), a.GetPolicyId())
	require.Equal(t, "B", a.GetLetter())
	require.Equal(t, int32(3), a.GetDraft().GetVersionNo())
	require.Equal(t, int32(2), a.GetPublished().GetVersionNo())
	require.Len(t, a.GetVersions(), 3)
	require.Len(t, a.GetPublishedVersions(), 2)
	require.Equal(t, []string{f.bob.String(), f.alice.String()}, a.GetContributorIds())
	require.Empty(t, f.events(), "a read writes no audit event")
}

func TestGetPolicyAppendix_Retired(t *testing.T) {
	f := newAppendixFixture()
	gone := time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)
	f.b.result.Letter = ""
	f.b.result.RetiredAt = &gone
	resp, err := f.h.GetPolicyAppendix(context.Background(), &corev1.GetPolicyAppendixRequest{Id: f.id.String()})
	require.NoError(t, err)
	require.Empty(t, resp.GetAppendix().GetLetter())
	require.NotNil(t, resp.GetAppendix().GetRetiredAt())
}

func TestGetPolicyAppendix_UnknownID(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = store.ErrPolicyAppendixNotFound
	_, err := f.h.GetPolicyAppendix(context.Background(), &corev1.GetPolicyAppendixRequest{Id: f.id.String()})
	require.Equal(t, codes.NotFound, status.Code(err))
}

func TestGetPolicyAppendix_BadID(t *testing.T) {
	f := newAppendixFixture()
	_, err := f.h.GetPolicyAppendix(context.Background(), &corev1.GetPolicyAppendixRequest{Id: "nope"})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, f.b.calls, "a malformed id never reaches the store")
}

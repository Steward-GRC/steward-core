// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Steward-GRC/steward-core/internal/store"
)

func TestPolicyAppendix_GetByID(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Glossary")
	b := e.create(t, "Forms")
	_, err := e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)

	got, err := e.st.Get(e.ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, a.ID, got.ID)
	require.Equal(t, e.policy, got.PolicyID)
	require.Equal(t, "A", got.Letter)
	require.NotNil(t, got.Published())
	require.Nil(t, got.Draft())
	require.Equal(t, []uuid.UUID{e.bob}, got.ContributorIDs())

	other, err := e.st.Get(e.ctx, b.ID)
	require.NoError(t, err)
	require.Equal(t, "B", other.Letter, "the letter follows the place among the policy's live appendices")
	require.NotNil(t, other.Draft())
}

func TestPolicyAppendix_GetRetiredHasNoLetter(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Glossary")
	e.create(t, "Forms")
	_, _, err := e.st.Retire(e.ctx, a.ID, e.bob)
	require.NoError(t, err)

	got, err := e.st.Get(e.ctx, a.ID)
	require.NoError(t, err)
	require.Empty(t, got.Letter)
	require.NotNil(t, got.RetiredAt)
	require.Len(t, got.Versions, 1, "history is kept")
}

func TestPolicyAppendix_GetUnknownID(t *testing.T) {
	e := newAppendixEnv(t)
	_, err := e.st.Get(e.ctx, uuid.New())
	require.ErrorIs(t, err, store.ErrPolicyAppendixNotFound)
}

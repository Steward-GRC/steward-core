// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
)

func sections(rates string) string { return `{"sections":{"rates":"` + rates + `"}}` }

// appendix-titles-contributors.test.ts "saves nothing: no draft appears and
// the stored draft is left alone", and "is unchanged against itself", over the
// real store.
func TestPolicyAppendix_LiveDiffSavesNothing(t *testing.T) {
	e := newAppendixEnv(t)
	a, err := e.st.Create(e.ctx, e.policy, e.bob, "Glossary", sections("Reviewed annually by Finance."))
	require.NoError(t, err)
	_, err = e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	h := grpcsvc.NewPolicyAppendixHandler(e.st, nil)

	resp, err := h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{
		Id: a.ID.String(), ContentJson: proto.String(sections("Typed, not saved.")),
	})
	require.NoError(t, err)
	require.Equal(t, "changed", resp.GetDiffs()[0].GetChangeType())
	got, err := e.st.Get(e.ctx, a.ID)
	require.NoError(t, err)
	require.Nil(t, got.Draft(), "no draft appears")
	require.Len(t, got.Versions, 1)

	same, err := h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{
		Id: a.ID.String(), ContentJson: proto.String(sections("Reviewed annually by Finance.")),
	})
	require.NoError(t, err)
	for _, d := range same.GetDiffs() {
		require.Equal(t, "unchanged", d.GetChangeType())
	}

	// With a draft saved, a live diff leaves its content alone and a plain
	// diff compares the draft with the published row.
	_, _, err = e.st.SaveDraft(e.ctx, a.ID, e.alice, "Glossary", sections("Reviewed every quarter."))
	require.NoError(t, err)
	_, err = h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{
		Id: a.ID.String(), ContentJson: proto.String(sections("Typed again.")),
	})
	require.NoError(t, err)
	got, err = e.st.Get(e.ctx, a.ID)
	require.NoError(t, err)
	require.JSONEq(t, sections("Reviewed every quarter."), got.Draft().ContentJSON)
	plain, err := h.DiffPolicyAppendix(context.Background(), &corev1.DiffPolicyAppendixRequest{Id: a.ID.String()})
	require.NoError(t, err)
	require.Equal(t, "changed", plain.GetDiffs()[0].GetChangeType())
}

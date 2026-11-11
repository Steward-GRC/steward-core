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

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func (f *appendixFixture) checkpoint(content string) *corev1.CheckpointAppendixDraftRequest {
	return &corev1.CheckpointAppendixDraftRequest{
		Id: f.id.String(), PolicyId: f.policy.String(), ContentJson: content, ActorUserId: f.alice.String(),
	}
}

// appendix-collab.test.ts "becomes the appendix's draft and leaves the published row alone"
// and appendix-titles-contributors.test.ts "adds whoever co-edits in the appendix room and checkpoints".
func TestPolicyAppendixHandler_CheckpointIsAcceptedAndAuditsAsADraftSave(t *testing.T) {
	f := newAppendixFixture()
	f.b.changed = true
	resp, err := f.h.CheckpointAppendixDraft(context.Background(), f.checkpoint(`{"text":"Written in the room"}`))
	require.NoError(t, err)
	require.True(t, resp.GetAccepted())
	require.Empty(t, resp.GetRejectReason())
	require.Equal(t, 1, f.b.checkpoints)
	require.Equal(t, f.id, f.b.gotID)
	require.Equal(t, f.policy, f.b.gotPolicy)
	require.Equal(t, f.alice, f.b.gotActor)
	require.Equal(t, `{"text":"Written in the room"}`, f.b.gotContent)
	require.Len(t, f.events(), 1)
	require.Equal(t, "appendix.saved", f.events()[0].Action)
	require.Equal(t, f.alice.String(), f.events()[0].ActorUserID)
	require.Equal(t, "policy_appendix:"+f.id.String(), f.events()[0].Subject)
	for k, v := range f.events()[0].Attributes {
		require.NotContains(t, k+v, "Written in the room", "no text in an audit event")
	}
}

// appendix-collab.test.ts "makes no draft when the checkpoint is what is already published (bug log 246)".
func TestPolicyAppendixHandler_CheckpointThatChangedNothingIsAcceptedWithoutAnAuditEvent(t *testing.T) {
	f := newAppendixFixture()
	f.b.changed = false
	resp, err := f.h.CheckpointAppendixDraft(context.Background(), f.checkpoint(`{}`))
	require.NoError(t, err)
	require.True(t, resp.GetAccepted())
	require.Equal(t, 1, f.b.checkpoints)
	require.Empty(t, f.events())
}

// appendix-collab.test.ts "is refused when the content is not a JSON document".
func TestPolicyAppendixHandler_CheckpointOfNonJSONIsARefusalNotAnError(t *testing.T) {
	for name, content := range map[string]string{"not json": "not json", "truncated": "{", "blank": "  "} {
		t.Run(name, func(t *testing.T) {
			f := newAppendixFixture()
			resp, err := f.h.CheckpointAppendixDraft(context.Background(), f.checkpoint(content))
			require.NoError(t, err)
			require.False(t, resp.GetAccepted())
			require.NotEmpty(t, resp.GetRejectReason())
			require.Zero(t, f.b.checkpoints, "nothing stored")
			require.Empty(t, f.events())
		})
	}
}

func TestPolicyAppendixHandler_CheckpointRefusesBadInput(t *testing.T) {
	f0 := newAppendixFixture()
	for name, req := range map[string]*corev1.CheckpointAppendixDraftRequest{
		"bad id":        {Id: "x", PolicyId: f0.policy.String(), ContentJson: "{}", ActorUserId: uuid.NewString()},
		"bad policy id": {Id: uuid.NewString(), PolicyId: "x", ContentJson: "{}", ActorUserId: uuid.NewString()},
		"no policy id":  {Id: uuid.NewString(), ContentJson: "{}", ActorUserId: uuid.NewString()},
		"no actor":      {Id: uuid.NewString(), PolicyId: f0.policy.String(), ContentJson: "{}"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAppendixFixture()
			_, err := f.h.CheckpointAppendixDraft(context.Background(), req)
			requireCode(t, err, codes.InvalidArgument)
			require.Zero(t, f.b.checkpoints)
		})
	}
}

func TestPolicyAppendixHandler_CheckpointMapsStoreErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		code codes.Code
	}{
		"unknown appendix": {store.ErrPolicyAppendixNotFound, codes.NotFound},
		"another policy":   {store.ErrAppendixPolicyMismatch, codes.InvalidArgument},
		"retired appendix": {store.ErrAppendixRetired, codes.FailedPrecondition},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAppendixFixture()
			f.b.err = tc.err
			_, err := f.h.CheckpointAppendixDraft(context.Background(), f.checkpoint(`{}`))
			requireCode(t, err, tc.code)
			require.Empty(t, f.events())
		})
	}
}

func TestPolicyAppendixHandler_CheckpointStoreFaultIsCodedAndNotLeaked(t *testing.T) {
	f := newAppendixFixture()
	f.b.err = errors.New("dial tcp core-pg:5432: connect: connection refused")
	_, err := f.h.CheckpointAppendixDraft(context.Background(), f.checkpoint(`{}`))
	requireStoreUnavailable(t, err, "checkpoint_appendix_draft")
	require.NotContains(t, err.Error(), "connection refused")
	require.Empty(t, f.events())
}

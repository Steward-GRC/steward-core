// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"

	grpcactor "github.com/Bugs5382/go-grpc-actor"

	"github.com/Steward-GRC/steward-core/internal/audit"
)

// auditEmitter is the audit sink the handlers depend on: *audit.Emitter, or
// that emitter wrapped by NewImpersonationEmitter.
type auditEmitter interface {
	Emit(ctx context.Context, ev audit.Event) error
}

// applyImpersonation credits an event to the real admin during act-as. The
// action itself ran as the target, who stays the actor everywhere else, so
// the target is kept in Attributes["impersonated_user_id"].
func applyImpersonation(ctx context.Context, ev audit.Event) audit.Event {
	a, ok := grpcactor.FromContext(ctx)
	if !ok || !a.Impersonated() {
		return ev
	}
	target := ev.ActorUserID
	ev.ActorUserID = a.Impersonator
	if ev.Attributes == nil {
		ev.Attributes = map[string]string{}
	}
	ev.Attributes["impersonated_user_id"] = target
	return ev
}

// actorFromContext returns the forwarded subject, or "" for a call that
// carries no actor. Emit sites whose request has no actor_user_id use it, so
// that during act-as applyImpersonation still has the target to keep.
func actorFromContext(ctx context.Context) string {
	a, ok := grpcactor.FromContext(ctx)
	if !ok {
		return ""
	}
	return a.Subject
}

type impersonationEmitter struct{ inner auditEmitter }

// NewImpersonationEmitter wraps inner so every event goes through
// applyImpersonation. A nil inner stays nil, so the handlers' nil checks hold.
func NewImpersonationEmitter(inner auditEmitter) auditEmitter {
	if inner == nil {
		return nil
	}
	return impersonationEmitter{inner: inner}
}

func (e impersonationEmitter) Emit(ctx context.Context, ev audit.Event) error {
	return e.inner.Emit(ctx, applyImpersonation(ctx, ev))
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	grpcactor "github.com/Bugs5382/go-grpc-actor"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
)

// serverCtxFromMD carries an actor across a real gRPC hop: the go-grpc-actor
// client interceptor writes it, and the server interceptor (trusting the
// caller) reads it into the handler context. The pairs name the subject
// ("x-fwd-user-id") and, during act-as, the admin ("x-fwd-actor").
func serverCtxFromMD(t *testing.T, pairs ...string) context.Context {
	t.Helper()
	var a grpcactor.Actor
	for i := 0; i+1 < len(pairs); i += 2 {
		switch pairs[i] {
		case "x-fwd-user-id":
			a.Subject = pairs[i+1]
		case "x-fwd-actor":
			a.Impersonator = pairs[i+1]
		}
	}
	var md metadata.MD
	capture := func(ctx context.Context, _ string, _, _ any, _ *grpc.ClientConn, _ ...grpc.CallOption) error {
		md, _ = metadata.FromOutgoingContext(ctx)
		return nil
	}
	if err := grpcactor.UnaryClientInterceptor()(grpcactor.WithActor(context.Background(), a), "/steward.core.v1.CategoryService/CreateCategory", nil, nil, nil, capture); err != nil {
		t.Fatalf("client interceptor: %v", err)
	}
	var got context.Context
	handler := func(ctx context.Context, _ any) (any, error) {
		got = ctx
		return nil, nil
	}
	trustAll := grpcactor.WithTrust(func(context.Context, string) bool { return true })
	in := metadata.NewIncomingContext(context.Background(), md)
	if _, err := grpcactor.UnaryServerInterceptor(trustAll)(in, nil, &grpc.UnaryServerInfo{FullMethod: "/steward.core.v1.CategoryService/CreateCategory"}, handler); err != nil {
		t.Fatalf("server interceptor: %v", err)
	}
	return got
}

func TestImpersonatedActionAttributesToAdmin(t *testing.T) {
	target := uuid.New().String()
	admin := uuid.New().String()

	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	ctx := serverCtxFromMD(t, "x-fwd-user-id", target, "x-fwd-actor", admin)
	if _, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{
		Name: "Ops", Slug: "ops", ActorUserId: target,
	}); err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}

	ev := findEvent(t, cap, "category.created")
	if ev.ActorUserID != admin {
		t.Fatalf("actor_user_id: want admin %q, got %q", admin, ev.ActorUserID)
	}
	if got := ev.Attributes["impersonated_user_id"]; got != target {
		t.Fatalf("impersonated_user_id: want target %q, got %q", target, got)
	}
}

func TestNonImpersonatedActionUnchanged(t *testing.T) {
	target := uuid.New().String()

	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	ctx := serverCtxFromMD(t, "x-fwd-user-id", target)
	if _, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{
		Name: "Ops", Slug: "ops", ActorUserId: target,
	}); err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}

	ev := findEvent(t, cap, "category.created")
	if ev.ActorUserID != target {
		t.Fatalf("actor_user_id: want target %q, got %q", target, ev.ActorUserID)
	}
	if _, ok := ev.Attributes["impersonated_user_id"]; ok {
		t.Fatalf("impersonated_user_id must be absent without impersonation, got %q", ev.Attributes["impersonated_user_id"])
	}
}

func findEvent(t *testing.T, cap *capturePublisher, action string) audit.Event {
	t.Helper()
	for _, c := range cap.calls {
		if c.event.Action == action {
			return c.event
		}
	}
	t.Fatalf("no %q audit event emitted (got %d events)", action, len(cap.calls))
	return audit.Event{}
}

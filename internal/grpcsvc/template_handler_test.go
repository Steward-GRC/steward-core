// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/google/uuid"
)

func TestTemplateHandlerActorFlowsToAudit(t *testing.T) {
	cap := &capturePublisher{}
	h := grpcsvc.NewTemplateHandler(newFakeTemplateStore(), audit.New(cap))
	ctx := context.Background()
	actor := uuid.New().String()
	tid := uuid.New().String()

	if _, err := h.CreateTemplate(ctx, &corev1.CreateTemplateRequest{
		Name: "Acceptable Use", ActorUserId: actor,
	}); err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	cv, err := h.CreateTemplateVersion(ctx, &corev1.CreateTemplateVersionRequest{
		TemplateId: tid, ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("CreateTemplateVersion: %v", err)
	}
	vid := cv.Version.Id

	if _, err := h.UpdateTemplateVersionSections(ctx, &corev1.UpdateTemplateVersionSectionsRequest{
		Id: vid, ActorUserId: actor,
	}); err != nil {
		t.Fatalf("UpdateTemplateVersionSections: %v", err)
	}
	if _, err := h.PublishTemplateVersion(ctx, &corev1.PublishTemplateVersionRequest{
		Id: vid, ActorUserId: actor,
	}); err != nil {
		t.Fatalf("PublishTemplateVersion: %v", err)
	}
	if _, err := h.DeleteTemplateVersion(ctx, &corev1.DeleteTemplateVersionRequest{
		Id: vid, ActorUserId: actor,
	}); err != nil {
		t.Fatalf("DeleteTemplateVersion: %v", err)
	}

	want := map[string]bool{
		"template.created":           false,
		"template_version.created":   false,
		"template_version.updated":   false,
		"template_version.published": false,
		"template_version.discarded": false,
	}
	for _, c := range cap.calls {
		if _, ok := want[c.event.Action]; !ok {
			continue
		}
		want[c.event.Action] = true
		if c.event.ActorUserID != actor {
			t.Fatalf("action %q: actor_user_id got %q want %q", c.event.Action, c.event.ActorUserID, actor)
		}
	}
	for action, seen := range want {
		if !seen {
			t.Fatalf("expected an audit event for %q", action)
		}
	}
}

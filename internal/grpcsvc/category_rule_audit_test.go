// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
)

func categoryRuleEvents(cap *capturePublisher) []audit.Event {
	var out []audit.Event
	for _, c := range cap.calls {
		switch c.event.Action {
		case "category_rule.created", "category_rule.updated", "category_rule.deleted":
			out = append(out, c.event)
		}
	}
	return out
}

func userRule(ref string, read, ack, approve, author corev1.GrantEffect) *corev1.CategoryRule {
	return &corev1.CategoryRule{
		SubjectKind: corev1.RuleSubjectKind_RULE_SUBJECT_KIND_USER,
		SubjectRef:  ref,
		Read:        read,
		Ack:         ack,
		Approve:     approve,
		Author:      author,
	}
}

func TestSetCategoryRuleset_EmitsCreatedForNewGrants(t *testing.T) {
	fs := newFakeCategoryStoreWithRules()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	actor := uuid.New().String()
	catID := uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", actor)

	if _, err := h.SetCategoryRuleset(ctx, &corev1.SetCategoryRulesetRequest{
		CategoryId: catID,
		Rules: []*corev1.CategoryRule{
			userRule("alice",
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_DENY,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			),
		},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}

	events := categoryRuleEvents(cap)
	if len(events) != 1 {
		t.Fatalf("expected 1 category_rule event, got %d", len(events))
	}
	ev := events[0]

	if ev.Action != "category_rule.created" {
		t.Errorf("Action: got %q, want category_rule.created", ev.Action)
	}
	if ev.Tier != audit.TierAudit {
		t.Errorf("Tier: got %v, want audit.TierAudit", ev.Tier)
	}
	if ev.ActorUserID == "" {
		t.Error("category_rule.created carries an empty ActorUserID: the role grant names nobody")
	}
	if ev.ActorUserID != actor {
		t.Errorf("ActorUserID: got %q, want %q", ev.ActorUserID, actor)
	}
	if ev.Subject != "category:"+catID {
		t.Errorf("Subject: got %q, want category:%s", ev.Subject, catID)
	}
	if ev.GroupID != catID {
		t.Errorf("CategoryID: got %q, want %q", ev.GroupID, catID)
	}
	if got := ev.Attributes["category_id"]; got != catID {
		t.Errorf("category_id: got %q, want %q", got, catID)
	}
	if got := ev.Attributes["subject_kind"]; got != "user" {
		t.Errorf("subject_kind: got %q, want user", got)
	}
	if got := ev.Attributes["subject_ref"]; got != "alice" {
		t.Errorf("subject_ref: got %q, want alice", got)
	}
	// An unset axis is recorded as unset, not as an empty value.
	for attr, want := range map[string]string{
		"read":    "allow",
		"ack":     "allow",
		"approve": "deny",
		"author":  "unset",
		"ordinal": "1",
	} {
		if got := ev.Attributes[attr]; got != want {
			t.Errorf("%s: got %q, want %q", attr, got, want)
		}
	}
}

func TestSetCategoryRuleset_EmitsUpdatedAndDeleted(t *testing.T) {
	fs := newFakeCategoryStoreWithRules()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	actor := uuid.New().String()
	catID := uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", actor)

	if _, err := h.SetCategoryRuleset(ctx, &corev1.SetCategoryRulesetRequest{
		CategoryId: catID,
		Rules: []*corev1.CategoryRule{
			userRule("alice",
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			),
			userRule("bob",
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			),
		},
	}); err != nil {
		t.Fatalf("seed SetCategoryRuleset: %v", err)
	}

	// Only the change is asserted, not the seed.
	cap.calls = nil

	if _, err := h.SetCategoryRuleset(ctx, &corev1.SetCategoryRulesetRequest{
		CategoryId: catID,
		Rules: []*corev1.CategoryRule{
			userRule("alice",
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			),
		},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}

	byAction := map[string]audit.Event{}
	for _, ev := range categoryRuleEvents(cap) {
		if prev, dup := byAction[ev.Action]; dup {
			t.Fatalf("duplicate %q event (first subject_ref %q)", ev.Action, prev.Attributes["subject_ref"])
		}
		byAction[ev.Action] = ev
	}
	if len(byAction) != 2 {
		t.Fatalf("expected an updated and a deleted event, got %d distinct actions: %v", len(byAction), byAction)
	}

	upd, ok := byAction["category_rule.updated"]
	if !ok {
		t.Fatal("no category_rule.updated event for the role change")
	}
	if upd.ActorUserID != actor {
		t.Errorf("updated ActorUserID: got %q, want %q", upd.ActorUserID, actor)
	}
	if got := upd.Attributes["subject_ref"]; got != "alice" {
		t.Errorf("updated subject_ref: got %q, want alice", got)
	}
	if got := upd.Attributes["approve"]; got != "allow" {
		t.Errorf("updated approve: got %q, want allow", got)
	}
	if got := upd.Attributes["prev_approve"]; got != "unset" {
		t.Errorf("updated prev_approve: got %q, want unset", got)
	}
	if got := upd.Attributes["prev_ack"]; got != "unset" {
		t.Errorf("updated prev_ack: got %q, want unset", got)
	}

	del, ok := byAction["category_rule.deleted"]
	if !ok {
		t.Fatal("no category_rule.deleted event for the revoked grant")
	}
	if del.ActorUserID != actor {
		t.Errorf("deleted ActorUserID: got %q, want %q", del.ActorUserID, actor)
	}
	if got := del.Attributes["subject_ref"]; got != "bob" {
		t.Errorf("deleted subject_ref: got %q, want bob", got)
	}
	// The revoked grants are carried so the trail records what was taken away.
	if got := del.Attributes["ack"]; got != "allow" {
		t.Errorf("deleted ack: got %q, want allow", got)
	}
}

func TestSetCategoryRuleset_NoChangeEmitsNothing(t *testing.T) {
	fs := newFakeCategoryStoreWithRules()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	catID := uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", uuid.New().String())
	req := &corev1.SetCategoryRulesetRequest{
		CategoryId: catID,
		Rules: []*corev1.CategoryRule{
			userRule("alice",
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			),
		},
	}

	if _, err := h.SetCategoryRuleset(ctx, req); err != nil {
		t.Fatalf("first SetCategoryRuleset: %v", err)
	}
	if got := len(categoryRuleEvents(cap)); got != 1 {
		t.Fatalf("first set: expected 1 event, got %d", got)
	}

	cap.calls = nil
	if _, err := h.SetCategoryRuleset(ctx, req); err != nil {
		t.Fatalf("replay SetCategoryRuleset: %v", err)
	}
	if got := categoryRuleEvents(cap); len(got) != 0 {
		t.Fatalf("replaying an identical ruleset emitted %d events, want 0: %v", len(got), got)
	}
}

func TestSetCategoryRuleset_ImpersonationAttributesToAdmin(t *testing.T) {
	fs := newFakeCategoryStoreWithRules()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	target := uuid.New().String()
	admin := uuid.New().String()
	catID := uuid.New().String()

	ctx := serverCtxFromMD(t, "x-fwd-user-id", target, "x-fwd-actor", admin)
	if _, err := h.SetCategoryRuleset(ctx, &corev1.SetCategoryRulesetRequest{
		CategoryId: catID,
		Rules: []*corev1.CategoryRule{
			userRule("alice",
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
			),
		},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}

	ev := findEvent(t, cap, "category_rule.created")
	if ev.ActorUserID != admin {
		t.Errorf("ActorUserID: want the real admin %q, got %q", admin, ev.ActorUserID)
	}
	if ev.ActorUserID == target {
		t.Error("role grant attributed to the impersonated target instead of the acting admin")
	}
	if got := ev.Attributes["impersonated_user_id"]; got != target {
		t.Errorf("impersonated_user_id: want target %q, got %q", target, got)
	}
}

func TestSetCategoryRuleset_NoImpersonationKeepsCaller(t *testing.T) {
	fs := newFakeCategoryStoreWithRules()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	actor := uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", actor)

	if _, err := h.SetCategoryRuleset(ctx, &corev1.SetCategoryRulesetRequest{
		CategoryId: uuid.New().String(),
		Rules: []*corev1.CategoryRule{
			userRule("alice",
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			),
		},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}

	ev := findEvent(t, cap, "category_rule.created")
	if ev.ActorUserID != actor {
		t.Errorf("ActorUserID: got %q, want %q", ev.ActorUserID, actor)
	}
	if _, ok := ev.Attributes["impersonated_user_id"]; ok {
		t.Error("impersonated_user_id must be absent without impersonation")
	}
}

func TestSetCategoryRuleset_NilAuditorStillWrites(t *testing.T) {
	fs := newFakeCategoryStoreWithRules()
	h := grpcsvc.NewCategoryHandler(fs, nil)

	resp, err := h.SetCategoryRuleset(context.Background(), &corev1.SetCategoryRulesetRequest{
		CategoryId: uuid.New().String(),
		Rules: []*corev1.CategoryRule{
			userRule("alice",
				corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			),
		},
	})
	if err != nil {
		t.Fatalf("SetCategoryRuleset with nil auditor: %v", err)
	}
	if len(resp.Rules) != 1 {
		t.Fatalf("expected 1 rule back, got %d", len(resp.Rules))
	}
}

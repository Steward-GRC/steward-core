// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Bugs5382/go-apperr/apperrgrpc"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// A deleted account's user rules go with it, and nothing else does.

// purgeFixture seeds a user rule to purge next to every kind of rule the
// purge must leave alone.
func purgeFixture(t *testing.T) (*fakeCategoryStoreWithRules, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	fs := newFakeCategoryStoreWithRules()
	victim, bystander := uuid.New(), uuid.New()
	catA, catB, catC := uuid.New(), uuid.New(), uuid.New()

	fs.rulesets[catA] = []domain.CategoryRule{
		{Ordinal: 1, SubjectKind: "everyone", Read: "allow"},
		{Ordinal: 2, SubjectKind: "user", SubjectRef: victim.String(), Ack: "deny"}, // the rule to purge
		{Ordinal: 3, SubjectKind: "group", SubjectRef: "finance-approvers", Approve: "allow"},
		{Ordinal: 4, SubjectKind: "user", SubjectRef: bystander.String(), Read: "allow"},
	}
	fs.rulesets[catB] = []domain.CategoryRule{
		{Ordinal: 1, SubjectKind: "user", SubjectRef: victim.String(), Author: "allow"},
	}
	// A group rule whose subject_ref equals the user's id: matching on
	// subject_ref alone would wrongly delete it.
	fs.rulesets[catC] = []domain.CategoryRule{
		{Ordinal: 1, SubjectKind: "group", SubjectRef: victim.String(), Read: "allow"},
	}
	return fs, victim, catA, catB, catC
}

func purgeEvents(cap *capturePublisher, action string) []audit.Event {
	var out []audit.Event
	for _, c := range cap.calls {
		if c.event.Action == action {
			out = append(out, c.event)
		}
	}
	return out
}

func TestPurgeUserCategoryRules_RemovesOnlyUserSubjectRules(t *testing.T) {
	fs, victim, catA, catB, catC := purgeFixture(t)
	h := grpcsvc.NewCategoryHandler(fs, nil)
	ctx := context.Background()
	actor := uuid.New().String()

	resp, err := h.PurgeUserCategoryRules(ctx, &corev1.PurgeUserCategoryRulesRequest{
		UserId:      victim.String(),
		ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("PurgeUserCategoryRules: %v", err)
	}
	if resp.GetRemovedRules() != 2 {
		t.Fatalf("removed_rules: got %d, want 2", resp.GetRemovedRules())
	}
	if len(resp.GetAffectedCategoryIds()) != 2 {
		t.Fatalf("affected_category_ids: got %v, want catA+catB only", resp.GetAffectedCategoryIds())
	}
	for _, got := range resp.GetAffectedCategoryIds() {
		if got != catA.String() && got != catB.String() {
			t.Fatalf("affected_category_ids names a category with no victim rule: %q", got)
		}
	}
	// The removed rules are returned in full, so the admin and the audit
	// trail see what was dropped.
	if len(resp.GetRules()) != 2 {
		t.Fatalf("rules: got %d, want 2", len(resp.GetRules()))
	}
	var sawAckDeny bool
	for _, r := range resp.GetRules() {
		if r.GetRule().GetSubjectKind() != corev1.RuleSubjectKind_RULE_SUBJECT_KIND_USER {
			t.Fatalf("echoed a non-user-subject rule: %+v", r)
		}
		if r.GetRule().GetSubjectRef() != victim.String() {
			t.Fatalf("echoed a rule for another subject: %+v", r)
		}
		if r.GetCategoryId() == catA.String() && r.GetRule().GetAck() == corev1.GrantEffect_GRANT_EFFECT_DENY {
			sawAckDeny = true
		}
	}
	if !sawAckDeny {
		t.Fatalf("the ack=deny grant was not reported — the orphaned-rule shape is unaccounted for: %+v", resp.GetRules())
	}

	type wantRule struct{ kind, ref string }
	for _, tc := range []struct {
		name string
		cat  uuid.UUID
		want []wantRule
	}{
		{
			name: "everyone, category and bystander rules survive in precedence order",
			cat:  catA,
			want: []wantRule{
				{"everyone", ""},
				{"group", "finance-approvers"},
				{"user", "bystander"},
			},
		},
		{name: "a category holding only the victim's rule is emptied", cat: catB, want: nil},
		{
			name: "a category-subject rule matching the victim id is NOT a user rule",
			cat:  catC,
			want: []wantRule{{"group", victim.String()}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fs.rulesets[tc.cat]
			if len(got) != len(tc.want) {
				t.Fatalf("got %d rules, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, w := range tc.want {
				if string(got[i].SubjectKind) != w.kind {
					t.Fatalf("rule %d kind: got %q, want %q", i, got[i].SubjectKind, w.kind)
				}
				if w.ref != "bystander" && got[i].SubjectRef != w.ref {
					t.Fatalf("rule %d ref: got %q, want %q", i, got[i].SubjectRef, w.ref)
				}
				if got[i].SubjectRef == victim.String() && got[i].SubjectKind == "user" {
					t.Fatalf("rule %d is still a victim user rule: %+v", i, got[i])
				}
			}
		})
	}
}

func TestPurgeUserCategoryRules_AuditsEveryRemovedGrant(t *testing.T) {
	fs, victim, catA, catB, _ := purgeFixture(t)
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))
	actor := uuid.New().String()

	if _, err := h.PurgeUserCategoryRules(context.Background(), &corev1.PurgeUserCategoryRulesRequest{
		UserId:      victim.String(),
		ActorUserId: actor,
	}); err != nil {
		t.Fatalf("PurgeUserCategoryRules: %v", err)
	}

	deleted := purgeEvents(cap, "category_rule.deleted")
	if len(deleted) != 2 {
		t.Fatalf("category_rule.deleted: got %d events, want one per removed rule (2)", len(deleted))
	}
	for _, ev := range deleted {
		if ev.Tier != audit.TierAudit {
			t.Errorf("Tier: got %v, want audit.TierAudit", ev.Tier)
		}
		if ev.ActorUserID != actor {
			t.Errorf("ActorUserID: got %q, want the deleting admin %q", ev.ActorUserID, actor)
		}
		if ev.Attributes["subject_ref"] != victim.String() {
			t.Errorf("subject_ref: got %q, want %q", ev.Attributes["subject_ref"], victim.String())
		}
		if ev.Attributes["reason"] != "user_deleted" {
			t.Errorf("reason: got %q, want user_deleted", ev.Attributes["reason"])
		}
		if ev.Attributes["category_id"] == "" {
			t.Error("category_id missing: the audit row does not say which category lost the grant")
		}
	}
	// A deny on the acknowledge axis is recorded too.
	var sawAck bool
	for _, ev := range deleted {
		if ev.Attributes["category_id"] == catA.String() && ev.Attributes["ack"] == "deny" {
			sawAck = true
		}
	}
	if !sawAck {
		t.Fatalf("no category_rule.deleted recorded the ack=deny grant: %+v", deleted)
	}

	changed := purgeEvents(cap, "category.ruleset_changed")
	if len(changed) != 2 {
		t.Fatalf("category.ruleset_changed: got %d events, want one per affected category (2)", len(changed))
	}
	seen := map[string]bool{}
	for _, ev := range changed {
		if ev.Attributes["reason"] != "user_deleted" {
			t.Errorf("reason: got %q, want user_deleted", ev.Attributes["reason"])
		}
		if ev.Attributes["user_id"] != victim.String() {
			t.Errorf("user_id: got %q, want %q", ev.Attributes["user_id"], victim.String())
		}
		seen[ev.GroupID] = true
	}
	if !seen[catA.String()] || !seen[catB.String()] {
		t.Fatalf("summary events did not cover both affected categories: %v", seen)
	}
}

func TestPurgeUserCategoryRules_DryRun(t *testing.T) {
	fs, victim, catA, catB, _ := purgeFixture(t)
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	resp, err := h.PurgeUserCategoryRules(context.Background(), &corev1.PurgeUserCategoryRulesRequest{
		UserId: victim.String(),
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("PurgeUserCategoryRules(dry run): %v", err)
	}
	if resp.GetRemovedRules() != 2 || len(resp.GetRules()) != 2 {
		t.Fatalf("dry run must still report what WOULD go: removed=%d rules=%d",
			resp.GetRemovedRules(), len(resp.GetRules()))
	}
	if len(fs.rulesets[catA]) != 4 || len(fs.rulesets[catB]) != 1 {
		t.Fatalf("dry run mutated the store: catA=%d catB=%d", len(fs.rulesets[catA]), len(fs.rulesets[catB]))
	}
	if len(cap.calls) != 0 {
		t.Fatalf("dry run emitted %d audit events; a preview must emit none", len(cap.calls))
	}
}

func TestPurgeUserCategoryRules_Idempotent(t *testing.T) {
	fs, victim, _, _, _ := purgeFixture(t)
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))
	req := &corev1.PurgeUserCategoryRulesRequest{UserId: victim.String(), ActorUserId: uuid.New().String()}

	first, err := h.PurgeUserCategoryRules(context.Background(), req)
	if err != nil {
		t.Fatalf("first purge: %v", err)
	}
	firstEvents := len(cap.calls)

	second, err := h.PurgeUserCategoryRules(context.Background(), req)
	if err != nil {
		t.Fatalf("second purge must not error: %v", err)
	}
	if first.GetRemovedRules() != 2 {
		t.Fatalf("first purge removed_rules: got %d, want 2", first.GetRemovedRules())
	}
	if second.GetRemovedRules() != 0 || len(second.GetRules()) != 0 || len(second.GetAffectedCategoryIds()) != 0 {
		t.Fatalf("second purge double-counted: removed=%d rules=%d cats=%v",
			second.GetRemovedRules(), len(second.GetRules()), second.GetAffectedCategoryIds())
	}
	if len(cap.calls) != firstEvents {
		t.Fatalf("second purge emitted %d extra audit events; a no-op must audit nothing",
			len(cap.calls)-firstEvents)
	}
}

func TestPurgeUserCategoryRules_RejectsBadInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  *corev1.PurgeUserCategoryRulesRequest
	}{
		{"empty user_id", &corev1.PurgeUserCategoryRulesRequest{}},
		{"malformed user_id", &corev1.PurgeUserCategoryRulesRequest{UserId: "not-a-uuid"}},
		{"nil user_id", &corev1.PurgeUserCategoryRulesRequest{UserId: uuid.Nil.String()}},
		{"malformed actor_user_id", &corev1.PurgeUserCategoryRulesRequest{
			UserId: uuid.New().String(), ActorUserId: "nope",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs, _, catA, _, _ := purgeFixture(t)
			h := grpcsvc.NewCategoryHandler(fs, nil)
			before := len(fs.rulesets[catA])

			_, err := h.PurgeUserCategoryRules(context.Background(), tc.req)
			if err == nil {
				t.Fatal("expected an error")
			}
			if got := status.Code(err); got != codes.InvalidArgument {
				t.Fatalf("code: got %v, want InvalidArgument", got)
			}
			if len(fs.rulesets[catA]) != before {
				t.Fatalf("a rejected request still mutated the ruleset")
			}
		})
	}
}

// purgeFailingStore fails the purge with a store fault.
type purgeFailingStore struct {
	*fakeCategoryStoreWithRules
}

func (s *purgeFailingStore) PurgeUserCategoryRules(context.Context, uuid.UUID, bool) ([]store.PurgedCategoryRule, error) {
	return nil, errors.New("dial tcp core-pg:5432: connect: connection refused")
}

func TestPurgeUserCategoryRules_StoreFaultIsCoded(t *testing.T) {
	fs, victim, _, _, _ := purgeFixture(t)
	h := grpcsvc.NewCategoryHandler(&purgeFailingStore{fs}, nil)

	_, err := h.PurgeUserCategoryRules(context.Background(), &corev1.PurgeUserCategoryRulesRequest{
		UserId: victim.String(),
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("not a gRPC status: %v", err)
	}
	if st.Code() != codes.Internal {
		t.Fatalf("code: got %v, want Internal", st.Code())
	}
	info, ok := apperrgrpc.FromStatus(st)
	if !ok {
		t.Fatal("status carries no ErrorInfo — the failure is a generic error")
	}
	if info.Symbol != "STORE_UNAVAILABLE" || info.Code != 4001 {
		t.Fatalf("ErrorInfo: got %s/%d, want STORE_UNAVAILABLE/4001", info.Symbol, info.Code)
	}
	if info.Metadata["op"] != "purge_user_category_rules" {
		t.Fatalf("op metadata: got %q, want purge_user_category_rules", info.Metadata["op"])
	}
}

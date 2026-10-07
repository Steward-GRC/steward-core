// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/lifecycle"
)

type breakGlassFixture struct {
	h        *grpcsvc.PolicyHandler
	auditCap *capturePublisher
	lifeCap  *lifecycleCapture
	policy   domain.Policy
	version  domain.PolicyVersion
	owner    uuid.UUID
}

func newBreakGlassFixture(t *testing.T) breakGlassFixture {
	t.Helper()
	ps := newFakePolicyStore()
	owner := uuid.New()
	p, err := domain.NewPolicy("Sensitive Matter", uuid.New(), domain.SensitivitySensitive, owner)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	p.Number = "POL-HR-000042"
	ps.policies[p.ID] = p
	v := domain.PolicyVersion{ID: uuid.New(), PolicyID: p.ID, VersionNo: 1, Status: domain.PolicyVersionStatusPublished}
	ps.versions[v.ID] = v

	auditCap := &capturePublisher{}
	lifeCap := &lifecycleCapture{}
	h := grpcsvc.NewPolicyHandler(ps, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{},
		grpcsvc.NewImpersonationEmitter(audit.New(auditCap))).WithLifecycleEmitter(lifecycle.New(lifeCap))
	return breakGlassFixture{h: h, auditCap: auditCap, lifeCap: lifeCap, policy: p, version: v, owner: owner}
}

func decodeBreakGlassRead(t *testing.T, c *lifecycleCapture) lifecycle.BreakGlassReadEvent {
	t.Helper()
	if len(c.calls) != 1 {
		t.Fatalf("lifecycle events = %d, want 1", len(c.calls))
	}
	if c.calls[0].routingKey != lifecycle.RoutingKeyBreakGlassRead {
		t.Fatalf("routing key = %q, want %q", c.calls[0].routingKey, lifecycle.RoutingKeyBreakGlassRead)
	}
	var evt lifecycle.BreakGlassReadEvent
	if err := json.Unmarshal(c.calls[0].body, &evt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return evt
}

func TestRecordBreakGlassReadAuditsTheReaderAndNotifies(t *testing.T) {
	f := newBreakGlassFixture(t)
	reader := uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", reader)

	if _, err := f.h.RecordBreakGlassRead(ctx, &corev1.RecordBreakGlassReadRequest{
		PolicyId: f.policy.ID.String(), PolicyVersionId: f.version.ID.String(),
	}); err != nil {
		t.Fatalf("RecordBreakGlassRead: %v", err)
	}

	if len(f.auditCap.calls) != 1 {
		t.Fatalf("audit events = %d, want 1", len(f.auditCap.calls))
	}
	ev := f.auditCap.calls[0].event
	if ev.Tier != audit.TierAudit || ev.Action != "policy.break_glass_read" || ev.ActorUserID != reader ||
		ev.Subject != "policy:"+f.policy.ID.String() || ev.GroupID != f.policy.HomeCategoryID.String() {
		t.Fatalf("audit event = %+v", ev)
	}
	if ev.Attributes["policy_number"] != "POL-HR-000042" || ev.Attributes["policy_version_id"] != f.version.ID.String() {
		t.Fatalf("audit attributes = %v", ev.Attributes)
	}
	if _, ok := ev.Attributes["impersonated_user_id"]; ok {
		t.Fatalf("no act-as, but impersonated_user_id is set: %v", ev.Attributes)
	}

	evt := decodeBreakGlassRead(t, f.lifeCap)
	if evt.EventType != lifecycle.EventTypeBreakGlassRead || evt.EventID == "" || evt.ReadAt.IsZero() ||
		evt.PolicyID != f.policy.ID.String() || evt.Number != "POL-HR-000042" || evt.Title != "Sensitive Matter" ||
		evt.OwnerUserID != f.owner.String() || evt.ReaderUserID != reader || evt.ActAsAdminUserID != "" ||
		evt.PolicyVersionID != f.version.ID.String() {
		t.Fatalf("lifecycle event = %+v", evt)
	}
}

func TestRecordBreakGlassReadDuringActAsNamesTheAdmin(t *testing.T) {
	f := newBreakGlassFixture(t)
	target, admin := uuid.New().String(), uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", target, "x-fwd-actor", admin)

	if _, err := f.h.RecordBreakGlassRead(ctx, &corev1.RecordBreakGlassReadRequest{PolicyId: f.policy.ID.String()}); err != nil {
		t.Fatalf("RecordBreakGlassRead: %v", err)
	}
	ev := f.auditCap.calls[0].event
	if ev.ActorUserID != admin || ev.Attributes["impersonated_user_id"] != target {
		t.Fatalf("audit actor=%q attrs=%v, want the admin as actor and the target kept", ev.ActorUserID, ev.Attributes)
	}
	evt := decodeBreakGlassRead(t, f.lifeCap)
	if evt.ReaderUserID != target || evt.ActAsAdminUserID != admin {
		t.Fatalf("lifecycle reader=%q admin=%q", evt.ReaderUserID, evt.ActAsAdminUserID)
	}
}

func TestRecordBreakGlassReadRefusals(t *testing.T) {
	f := newBreakGlassFixture(t)
	reader := serverCtxFromMD(t, "x-fwd-user-id", uuid.New().String())
	other := domain.PolicyVersion{ID: uuid.New(), PolicyID: uuid.New()}

	cases := []struct {
		name string
		ctx  context.Context
		req  *corev1.RecordBreakGlassReadRequest
		want codes.Code
	}{
		{"no actor", context.Background(), &corev1.RecordBreakGlassReadRequest{PolicyId: f.policy.ID.String()}, codes.Unauthenticated},
		{"bad policy id", reader, &corev1.RecordBreakGlassReadRequest{PolicyId: "nope"}, codes.InvalidArgument},
		{"bad version id", reader, &corev1.RecordBreakGlassReadRequest{PolicyId: f.policy.ID.String(), PolicyVersionId: "nope"}, codes.InvalidArgument},
		{"version of another policy", reader, &corev1.RecordBreakGlassReadRequest{PolicyId: f.policy.ID.String(), PolicyVersionId: other.ID.String()}, codes.InvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.h.RecordBreakGlassRead(tc.ctx, tc.req)
			if status.Code(err) != tc.want {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if len(f.auditCap.calls) != 0 || len(f.lifeCap.calls) != 0 {
		t.Fatalf("a refused call emitted audit=%d lifecycle=%d", len(f.auditCap.calls), len(f.lifeCap.calls))
	}
}

func TestRecordBreakGlassReadFailsClosed(t *testing.T) {
	f := newBreakGlassFixture(t)
	ctx := serverCtxFromMD(t, "x-fwd-user-id", uuid.New().String())
	req := &corev1.RecordBreakGlassReadRequest{PolicyId: f.policy.ID.String()}
	ps := newFakePolicyStore()
	ps.policies[f.policy.ID] = f.policy

	t.Run("audit publish fails", func(t *testing.T) {
		lifeCap := &lifecycleCapture{}
		h := grpcsvc.NewPolicyHandler(ps, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{},
			audit.New(failingPublisher{})).WithLifecycleEmitter(lifecycle.New(lifeCap))
		if _, err := h.RecordBreakGlassRead(ctx, req); status.Code(err) != codes.Unavailable {
			t.Fatalf("err = %v, want Unavailable", err)
		}
		if len(lifeCap.calls) != 0 {
			t.Fatal("notified although the read was not audited")
		}
	})
	t.Run("notification publish fails", func(t *testing.T) {
		h := grpcsvc.NewPolicyHandler(ps, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{},
			audit.New(&capturePublisher{})).WithLifecycleEmitter(lifecycle.New(failingPublisher{}))
		if _, err := h.RecordBreakGlassRead(ctx, req); status.Code(err) != codes.Unavailable {
			t.Fatalf("err = %v, want Unavailable", err)
		}
	})
	t.Run("no audit or jobs publisher", func(t *testing.T) {
		h := grpcsvc.NewPolicyHandler(ps, newFakeCategoryStore(), &fakeTemplateStore{}, domain.NoopValidator{}, nil)
		if _, err := h.RecordBreakGlassRead(ctx, req); status.Code(err) != codes.Unavailable {
			t.Fatalf("err = %v, want Unavailable", err)
		}
	})
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package lifecycle_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Steward-GRC/steward-core/internal/lifecycle"
)

type capturePublisher struct {
	calls []capturedCall
}

type capturedCall struct {
	routingKey string
	body       []byte
}

func (c *capturePublisher) Publish(_ context.Context, rk string, body []byte) error {
	c.calls = append(c.calls, capturedCall{routingKey: rk, body: append([]byte(nil), body...)})
	return nil
}

func TestEmitPublishedUsesPolicyPublishedRoutingKey(t *testing.T) {
	cap := &capturePublisher{}
	em := lifecycle.New(cap)

	err := em.EmitPublished(context.Background(), time.Now(), lifecycle.PolicyVersionContent{
		PolicyID:    "pol-1",
		VersionID:   "ver-2",
		VersionNo:   1,
		CategoryID:  "grp-3",
		Sensitivity: "standard",
		PolicyTitle: "Test",
	})
	if err != nil {
		t.Fatalf("EmitPublished: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(cap.calls))
	}
	if got, want := cap.calls[0].routingKey, "policy.published"; got != want {
		t.Fatalf("routing key: got %q want %q", got, want)
	}
}

func TestEmitPublishedJSONMatchesAIConsumerShape(t *testing.T) {
	cap := &capturePublisher{}
	em := lifecycle.New(cap)

	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	v := lifecycle.PolicyVersionContent{
		PolicyID:    "pol-1",
		VersionID:   "ver-2",
		VersionNo:   3,
		CategoryID:  "grp-hr",
		Sensitivity: "sensitive",
		PolicyTitle: "Test Policy",
		Sections: []lifecycle.SectionContent{
			{Key: "scope", Text: "All employees."},
		},
	}
	if err := em.EmitPublished(context.Background(), at, v); err != nil {
		t.Fatalf("EmitPublished: %v", err)
	}

	// A loose map checks the wire keys, not Go field names.
	var got map[string]any
	if err := json.Unmarshal(cap.calls[0].body, &got); err != nil {
		t.Fatalf("unmarshal published body: %v", err)
	}
	if got["event_type"] != "policy.published" {
		t.Fatalf("event_type: got %v", got["event_type"])
	}
	if got["published_at"] == nil {
		t.Fatalf("published_at missing from envelope")
	}
	version, ok := got["version"].(map[string]any)
	if !ok {
		t.Fatalf("version: not an object: %T", got["version"])
	}
	// The AI indexer's struct has no json tags, so the keys are its field
	// names.
	mustEqual(t, version, "PolicyID", "pol-1")
	mustEqual(t, version, "VersionID", "ver-2")
	mustEqual(t, version, "VersionNo", float64(3))
	mustEqual(t, version, "CategoryID", "grp-hr")
	mustEqual(t, version, "Sensitivity", "sensitive")
	mustEqual(t, version, "PolicyTitle", "Test Policy")

	sections, ok := version["Sections"].([]any)
	if !ok || len(sections) != 1 {
		t.Fatalf("Sections: %v", version["Sections"])
	}
	sec, _ := sections[0].(map[string]any)
	mustEqual(t, sec, "Key", "scope")
	mustEqual(t, sec, "Text", "All employees.")
}

func mustEqual(t *testing.T, m map[string]any, key string, want any) {
	t.Helper()
	if got, ok := m[key]; !ok || got != want {
		t.Fatalf("key %q: got %v (present=%v) want %v", key, got, ok, want)
	}
}

func TestEmitPublishedCarriesDocumentType(t *testing.T) {
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	procCap := &capturePublisher{}
	procEm := lifecycle.New(procCap)
	if err := procEm.EmitPublished(context.Background(), at, lifecycle.PolicyVersionContent{
		PolicyID: "proc-1", VersionID: "ver-1", DocumentType: "procedure",
	}); err != nil {
		t.Fatalf("EmitPublished(procedure): %v", err)
	}
	var procGot map[string]any
	if err := json.Unmarshal(procCap.calls[0].body, &procGot); err != nil {
		t.Fatalf("unmarshal procedure body: %v", err)
	}
	procVer, _ := procGot["version"].(map[string]any)
	mustEqual(t, procVer, "DocumentType", "procedure")

	// A policy leaves DocumentType out.
	polCap := &capturePublisher{}
	polEm := lifecycle.New(polCap)
	if err := polEm.EmitPublished(context.Background(), at, lifecycle.PolicyVersionContent{
		PolicyID: "pol-1", VersionID: "ver-2",
	}); err != nil {
		t.Fatalf("EmitPublished(policy): %v", err)
	}
	var polGot map[string]any
	if err := json.Unmarshal(polCap.calls[0].body, &polGot); err != nil {
		t.Fatalf("unmarshal policy body: %v", err)
	}
	polVer, _ := polGot["version"].(map[string]any)
	if _, present := polVer["DocumentType"]; present {
		t.Fatalf("policy publish must omit DocumentType, got %v", polVer["DocumentType"])
	}
}

func TestEmitPublishedNilEmitterSafe(t *testing.T) {
	var em *lifecycle.Emitter
	if err := em.EmitPublished(context.Background(), time.Now(), lifecycle.PolicyVersionContent{}); err != nil {
		t.Fatalf("nil emitter should be a no-op, got %v", err)
	}
}

func TestEmitObligationChangedRoutingAndShape(t *testing.T) {
	cap := &capturePublisher{}
	em := lifecycle.New(cap)

	if err := em.EmitObligationChanged(context.Background(), "policy-123"); err != nil {
		t.Fatalf("EmitObligationChanged: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("want 1 publish, got %d", len(cap.calls))
	}
	if cap.calls[0].routingKey != lifecycle.RoutingKeyObligationChanged {
		t.Fatalf("routing key = %q, want %q", cap.calls[0].routingKey, lifecycle.RoutingKeyObligationChanged)
	}
	var got struct {
		EventType string `json:"event_type"`
		PolicyID  string `json:"policy_id"`
	}
	if err := json.Unmarshal(cap.calls[0].body, &got); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if got.EventType != "policy.obligation_changed" || got.PolicyID != "policy-123" {
		t.Fatalf("body = %+v, want event_type=policy.obligation_changed policy_id=policy-123", got)
	}
}

func TestEmitObligationChangedNilSafe(t *testing.T) {
	var em *lifecycle.Emitter
	if err := em.EmitObligationChanged(context.Background(), "p1"); err != nil {
		t.Fatalf("nil emitter should be a no-op, got %v", err)
	}
	em2 := lifecycle.New(nil)
	if err := em2.EmitObligationChanged(context.Background(), "p1"); err != nil {
		t.Fatalf("nil publisher should be a no-op, got %v", err)
	}
}

func TestEmitProcedurePublishedRoutingAndShape(t *testing.T) {
	cap := &capturePublisher{}
	em := lifecycle.New(cap)

	at := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	if err := em.EmitProcedurePublished(context.Background(), at, lifecycle.PolicyVersionContent{
		PolicyID: "proc-1", VersionID: "ver-1", DocumentType: lifecycle.DocumentTypeProcedure,
	}); err != nil {
		t.Fatalf("EmitProcedurePublished: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(cap.calls))
	}
	if got, want := cap.calls[0].routingKey, lifecycle.RoutingKeyProcedurePublished; got != want {
		t.Fatalf("routing key: got %q want %q", got, want)
	}
	if got := cap.calls[0].routingKey; got == lifecycle.RoutingKeyPublished {
		t.Fatalf("procedure publish must NOT ride the policy.published key, got %q", got)
	}
	var evt map[string]any
	if err := json.Unmarshal(cap.calls[0].body, &evt); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if evt["event_type"] != "procedure.published" {
		t.Fatalf("event_type: got %v want procedure.published", evt["event_type"])
	}
	version, ok := evt["version"].(map[string]any)
	if !ok {
		t.Fatalf("version object missing: %+v", evt)
	}
	mustEqual(t, version, "DocumentType", "PROCEDURE")
}

func TestEmitProcedureRetiredRoutingAndShape(t *testing.T) {
	cap := &capturePublisher{}
	em := lifecycle.New(cap)

	at := time.Date(2026, time.August, 24, 12, 0, 0, 0, time.UTC)
	if err := em.EmitProcedureRetired(context.Background(), at, "proc-1", "PRC-HR-000001", "Onboarding"); err != nil {
		t.Fatalf("EmitProcedureRetired: %v", err)
	}
	if len(cap.calls) != 1 {
		t.Fatalf("expected 1 publish, got %d", len(cap.calls))
	}
	if got, want := cap.calls[0].routingKey, lifecycle.RoutingKeyProcedureRetired; got != want {
		t.Fatalf("routing key: got %q want %q", got, want)
	}
	var evt lifecycle.PolicyRetiredEvent
	if err := json.Unmarshal(cap.calls[0].body, &evt); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if evt.EventType != lifecycle.EventTypeProcedureRetired {
		t.Fatalf("event_type: got %q want %q", evt.EventType, lifecycle.EventTypeProcedureRetired)
	}
	if evt.Number != "PRC-HR-000001" || evt.Title != "Onboarding" || evt.PolicyID != "proc-1" {
		t.Fatalf("unexpected retired event fields: %+v", evt)
	}
}

func TestEmitProcedureReindexTargetsProcedureQueue(t *testing.T) {
	aiCap := &capturePublisher{}
	em := lifecycle.New(&capturePublisher{}).WithAIPublisher(aiCap)

	at := time.Now()
	if err := em.EmitProcedureReindex(context.Background(), at, lifecycle.PolicyVersionContent{
		PolicyID: "proc-1", VersionID: "ver-1", DocumentType: lifecycle.DocumentTypeProcedure,
	}); err != nil {
		t.Fatalf("EmitProcedureReindex: %v", err)
	}
	if err := em.EmitProcedureReindexRemove(context.Background(), "proc-1", "ver-0"); err != nil {
		t.Fatalf("EmitProcedureReindexRemove: %v", err)
	}
	if len(aiCap.calls) != 2 {
		t.Fatalf("expected 2 AI-direct publishes, got %d", len(aiCap.calls))
	}
	for i, c := range aiCap.calls {
		if c.routingKey != lifecycle.AIProcedureIndexQueue {
			t.Fatalf("call %d routing key: got %q want %q", i, c.routingKey, lifecycle.AIProcedureIndexQueue)
		}
	}
	var reindex map[string]any
	if err := json.Unmarshal(aiCap.calls[0].body, &reindex); err != nil {
		t.Fatalf("unmarshal reindex: %v", err)
	}
	if reindex["event_type"] != "procedure.published" {
		t.Fatalf("reindex event_type: got %v want procedure.published", reindex["event_type"])
	}
	var remove map[string]any
	if err := json.Unmarshal(aiCap.calls[1].body, &remove); err != nil {
		t.Fatalf("unmarshal remove: %v", err)
	}
	if remove["event_type"] != "procedure.unpublished" {
		t.Fatalf("remove event_type: got %v want procedure.unpublished", remove["event_type"])
	}
}

func TestEmitProcedureNilSafe(t *testing.T) {
	var em *lifecycle.Emitter
	if err := em.EmitProcedurePublished(context.Background(), time.Now(), lifecycle.PolicyVersionContent{}); err != nil {
		t.Fatalf("nil emitter EmitProcedurePublished: %v", err)
	}
	if err := em.EmitProcedureRetired(context.Background(), time.Now(), "p", "n", "t"); err != nil {
		t.Fatalf("nil emitter EmitProcedureRetired: %v", err)
	}
	em2 := lifecycle.New(nil)
	if err := em2.EmitProcedurePublished(context.Background(), time.Now(), lifecycle.PolicyVersionContent{}); err != nil {
		t.Fatalf("nil publisher EmitProcedurePublished: %v", err)
	}
	if err := em2.EmitProcedureReindex(context.Background(), time.Now(), lifecycle.PolicyVersionContent{}); err != nil {
		t.Fatalf("nil aiPub EmitProcedureReindex: %v", err)
	}
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
)

func governanceEvents(cap *capturePublisher) []audit.Event {
	var out []audit.Event
	for _, c := range cap.calls {
		if c.event.Action == "category.governance_updated" {
			out = append(out, c.event)
		}
	}
	return out
}

// seedGovernedCategory stores a category with a known governance state.
func seedGovernedCategory(t *testing.T, fs *fakeCategoryStore, owners []uuid.UUID) uuid.UUID {
	t.Helper()
	aud := []string{"grp-b", "grp-a"}
	excl := []string{}
	every := false
	review := time.Date(2027, 1, 31, 0, 0, 0, 0, time.UTC)
	c, err := fs.Create(context.Background(), domain.Category{
		ID:                uuid.New(),
		Name:              "Finance",
		Slug:              "finance",
		Owners:            owners,
		AudienceGroupIDs:  &aud,
		ExclusionGroupIDs: &excl,
		AckEveryone:       &every,
		AckTriggers:       domain.AckOnPublish,
		ReviewCadence:     domain.CadenceOnDate,
		ReviewDate:        &review,
	})
	if err != nil {
		t.Fatalf("seed category: %v", err)
	}
	return c.ID
}

func TestSetGovernance_AuditCarriesActorBeforeAndAfter(t *testing.T) {
	fs := newFakeCategoryStore()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	ownerB, ownerA, ownerNew := uuid.New(), uuid.New(), uuid.New()
	catID := seedGovernedCategory(t, fs, []uuid.UUID{ownerB, ownerA})
	actor := uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", actor)

	if _, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:                        catID.String(),
		Owners:                    []string{ownerNew.String()},
		AudienceGroupIds:          []string{"grp-c"},
		AudienceGroupIdsProvided:  true,
		ExclusionGroupIds:         []string{"grp-z", "grp-y"},
		ExclusionGroupIdsProvided: true,
		AckTriggers:               corev1.AckTrigger_ACK_TRIGGER_ON_CHANGE,
		ReviewCadence:             corev1.ReviewCadence_REVIEW_CADENCE_ANNUAL,
		AckEveryone:               true,
		AckEveryoneProvided:       true,
		ActorUserId:               actor,
	}); err != nil {
		t.Fatalf("SetGovernance: %v", err)
	}

	events := governanceEvents(cap)
	if len(events) != 1 {
		t.Fatalf("expected 1 governance event, got %d", len(events))
	}
	ev := events[0]
	if ev.ActorUserID != actor {
		t.Errorf("ActorUserID: got %q, want %q", ev.ActorUserID, actor)
	}
	if ev.Tier != audit.TierAudit {
		t.Errorf("Tier: got %v, want audit", ev.Tier)
	}
	if ev.Subject != "category:"+catID.String() || ev.GroupID != catID.String() {
		t.Errorf("Subject/GroupID: got %q/%q", ev.Subject, ev.GroupID)
	}

	sortedPair := []string{ownerA.String(), ownerB.String()}
	sort.Strings(sortedPair)
	want := map[string]string{
		"owners":                   ownerNew.String(),
		"prev_owners":              strings.Join(sortedPair, ","),
		"audience_group_ids":       "grp-c",
		"prev_audience_group_ids":  "grp-a,grp-b",
		"exclusion_group_ids":      "grp-y,grp-z",
		"prev_exclusion_group_ids": "none",
		"ack_triggers":             "on-change",
		"prev_ack_triggers":        "on-publish",
		"review_cadence":           "annual",
		"prev_review_cadence":      "on-date",
		"review_date":              "none",
		"prev_review_date":         "2027-01-31",
		"ack_everyone":             "true",
		"prev_ack_everyone":        "false",
	}
	for k, v := range want {
		if got := ev.Attributes[k]; got != v {
			t.Errorf("%s: got %q, want %q", k, got, v)
		}
	}
}

func TestSetGovernance_AuditOwnersAreIDsOnlyAndNilReadsInherit(t *testing.T) {
	fs := newFakeCategoryStore()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	owner := uuid.New()
	catID := seedGovernedCategory(t, fs, []uuid.UUID{owner})
	actor := uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", actor)

	// Lists and the flag not provided: stored as inherit.
	if _, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:          catID.String(),
		Owners:      []string{owner.String()},
		AckTriggers: corev1.AckTrigger_ACK_TRIGGER_ON_PUBLISH,
		ActorUserId: actor,
	}); err != nil {
		t.Fatalf("SetGovernance: %v", err)
	}
	ev := governanceEvents(cap)[0]
	if got := ev.Attributes["audience_group_ids"]; got != "inherit" {
		t.Errorf("audience_group_ids: got %q, want inherit", got)
	}
	if got := ev.Attributes["ack_everyone"]; got != "inherit" {
		t.Errorf("ack_everyone: got %q, want inherit", got)
	}
	for _, k := range []string{"owners", "prev_owners"} {
		for _, part := range strings.Split(ev.Attributes[k], ",") {
			if _, err := uuid.Parse(part); err != nil {
				t.Errorf("%s holds %q, which is not a user uuid", k, part)
			}
		}
	}
}

func TestSetGovernance_NoChangeEmitsNoEvent(t *testing.T) {
	fs := newFakeCategoryStore()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fs, grpcsvc.NewImpersonationEmitter(audit.New(cap)))

	owner := uuid.New()
	catID := seedGovernedCategory(t, fs, []uuid.UUID{owner})
	actor := uuid.New().String()
	ctx := serverCtxFromMD(t, "x-fwd-user-id", actor)

	// The same governance, owners sent in another order.
	if _, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:                        catID.String(),
		Owners:                    []string{owner.String()},
		AudienceGroupIds:          []string{"grp-a", "grp-b"},
		AudienceGroupIdsProvided:  true,
		ExclusionGroupIds:         []string{},
		ExclusionGroupIdsProvided: true,
		AckTriggers:               corev1.AckTrigger_ACK_TRIGGER_ON_PUBLISH,
		ReviewCadence:             corev1.ReviewCadence_REVIEW_CADENCE_ON_DATE,
		ReviewDate:                "2027-01-31",
		AckEveryone:               false,
		AckEveryoneProvided:       true,
		ActorUserId:               actor,
	}); err != nil {
		t.Fatalf("SetGovernance: %v", err)
	}
	if n := len(governanceEvents(cap)); n != 0 {
		t.Fatalf("a no-op governance call emitted %d events, want 0", n)
	}
}

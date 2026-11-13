// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

func TestResolveChangeDefaultsToTheSuggestion(t *testing.T) {
	got, err := domain.ResolveChange(domain.ChangeState{}, domain.ChangeTypeMinor, "", "", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Suggested != domain.ChangeTypeMinor || got.Type != domain.ChangeTypeMinor || got.Requested != "" || got.Explicit() {
		t.Fatalf("state = %+v", got)
	}
}

func TestResolveChangeRaiseNeedsNoReason(t *testing.T) {
	actor := uuid.New()
	got, err := domain.ResolveChange(domain.ChangeState{}, domain.ChangeTypeFix, domain.ChangeTypeMajor, "", actor)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != domain.ChangeTypeMajor || got.Requested != "" || got.SetBy != actor || !got.Explicit() {
		t.Fatalf("state = %+v", got)
	}
}

func TestResolveChangeLoweringNeedsAReasonAndWaitsForAgreement(t *testing.T) {
	actor := uuid.New()
	if _, err := domain.ResolveChange(domain.ChangeState{}, domain.ChangeTypeMajor, domain.ChangeTypeMinor, "  ", actor); !errors.Is(err, domain.ErrChangeReasonRequired) {
		t.Fatalf("err = %v, want ErrChangeReasonRequired", err)
	}
	got, err := domain.ResolveChange(domain.ChangeState{}, domain.ChangeTypeMajor, domain.ChangeTypeMinor, "typo only", actor)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != domain.ChangeTypeMajor || got.Requested != domain.ChangeTypeMinor || got.Reason != "typo only" || got.SetBy != actor {
		t.Fatalf("a lowering must leave the effective type at the suggestion: %+v", got)
	}
}

func TestResolveChangeAgreementAppliesTheRequest(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	pending := domain.ChangeState{Suggested: domain.ChangeTypeMajor, Type: domain.ChangeTypeMajor, Requested: domain.ChangeTypeMinor, Reason: "r", SetBy: uuid.New()}
	if _, err := domain.RecordAgreement(pending, nil); !errors.Is(err, domain.ErrAgreementNeedsApprovers) {
		t.Fatalf("err = %v", err)
	}
	if _, err := domain.RecordAgreement(domain.ChangeState{Type: domain.ChangeTypeMajor}, []uuid.UUID{a}); !errors.Is(err, domain.ErrNoPendingLowering) {
		t.Fatalf("err = %v", err)
	}
	got, err := domain.RecordAgreement(pending, []uuid.UUID{a, b})
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != domain.ChangeTypeMinor || got.Requested != "" || len(got.AgreedBy) != 2 {
		t.Fatalf("state = %+v", got)
	}
}

func TestResolveChangeResubmitKeepsAnAgreementWhileTheSuggestionStands(t *testing.T) {
	actor, a := uuid.New(), uuid.New()
	agreed := domain.ChangeState{Suggested: domain.ChangeTypeMajor, Type: domain.ChangeTypeMinor, Reason: "r", SetBy: actor, AgreedBy: []uuid.UUID{a}}
	same, err := domain.ResolveChange(agreed, domain.ChangeTypeMajor, "", "", actor)
	if err != nil {
		t.Fatal(err)
	}
	if same.Type != domain.ChangeTypeMinor || len(same.AgreedBy) != 1 {
		t.Fatalf("agreement should stand: %+v", same)
	}
	// The content changed and the suggestion moved: the agreement no longer covers it.
	moved, err := domain.ResolveChange(agreed, domain.ChangeTypeMinor, "", "", actor)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.AgreedBy) != 0 || moved.Type != domain.ChangeTypeMinor {
		t.Fatalf("state = %+v", moved)
	}
}

func TestFinalizeChange(t *testing.T) {
	actor, a := uuid.New(), uuid.New()
	cases := []struct {
		name  string
		state domain.ChangeState
		now   domain.ChangeType
		want  domain.ChangeType
	}{
		{"never picked: the fresh suggestion wins", domain.ChangeState{Suggested: domain.ChangeTypeMajor, Type: domain.ChangeTypeMajor}, domain.ChangeTypeFix, domain.ChangeTypeFix},
		{"a raise stands over a lower suggestion", domain.ChangeState{Suggested: domain.ChangeTypeFix, Type: domain.ChangeTypeMajor, SetBy: actor}, domain.ChangeTypeFix, domain.ChangeTypeMajor},
		{"an edit after submit can only raise it", domain.ChangeState{Suggested: domain.ChangeTypeFix, Type: domain.ChangeTypeFix, SetBy: actor}, domain.ChangeTypeMajor, domain.ChangeTypeMajor},
		{"an unagreed request publishes at the suggestion", domain.ChangeState{Suggested: domain.ChangeTypeMajor, Type: domain.ChangeTypeMajor, Requested: domain.ChangeTypeMinor, SetBy: actor}, domain.ChangeTypeMajor, domain.ChangeTypeMajor},
		{"an agreed lowering publishes lowered", domain.ChangeState{Suggested: domain.ChangeTypeMajor, Type: domain.ChangeTypeMinor, SetBy: actor, AgreedBy: []uuid.UUID{a}}, domain.ChangeTypeMajor, domain.ChangeTypeMinor},
		{"an agreed lowering does not cover a bigger change made after", domain.ChangeState{Suggested: domain.ChangeTypeMinor, Type: domain.ChangeTypeFix, SetBy: actor, AgreedBy: []uuid.UUID{a}}, domain.ChangeTypeMajor, domain.ChangeTypeMajor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := domain.FinalizeChange(c.state, c.now); got != c.want {
				t.Fatalf("FinalizeChange = %q, want %q", got, c.want)
			}
		})
	}
}

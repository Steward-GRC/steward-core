// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"strings"

	"github.com/google/uuid"
)

var (
	// ErrChangeReasonRequired is returned when a change type is set below what core suggested and
	// no reason is given.
	ErrChangeReasonRequired = errors.New("a reason is required to set the change type below the suggested one")
	// ErrNoPendingLowering is returned when an agreement arrives and no lowering is waiting for one.
	ErrNoPendingLowering = errors.New("no lowering of the change type is waiting for agreement")
	// ErrAgreementNeedsApprovers is returned when an agreement names nobody.
	ErrAgreementNeedsApprovers = errors.New("an agreement must name the approvers who agreed")
)

// ChangeState is the change-type record of one policy version.
//
//   - Suggested is what core classified at submit.
//   - Type is the type the version will publish as. It starts at the suggestion, rises when the
//     author raises it, and falls below the suggestion only after the approvers agree.
//   - Requested is a lowering the author asked for that is still waiting for that agreement.
//   - SetBy is the user who picked a type; unset means nobody did and the suggestion rules.
//   - AgreedBy are the approvers who agreed to a lowering (written by workflow).
type ChangeState struct {
	Suggested ChangeType
	Type      ChangeType
	Requested ChangeType
	Reason    string
	SetBy     uuid.UUID
	AgreedBy  []uuid.UUID
}

// Explicit reports whether a user picked the type, as against the suggestion standing as it is.
func (s ChangeState) Explicit() bool { return s.SetBy != uuid.Nil }

// ResolveChange returns the state after a submit or a pick against a fresh suggestion. pick is
// the author's choice, or empty to leave things as they were: a pending request, or a standing
// pick, is carried over, and with neither the suggestion applies. A pick at or above the
// suggestion takes effect at once. A pick below it needs a reason and is kept as a request; the
// type stays at the suggestion until RecordAgreement. An agreement already recorded for the same
// pick survives while the suggestion is unchanged, since the approvers agreed to that.
func ResolveChange(prev ChangeState, suggested, pick ChangeType, reason string, actor uuid.UUID) (ChangeState, error) {
	reason = strings.TrimSpace(reason)
	if pick == "" {
		switch {
		case prev.Requested != "":
			pick = prev.Requested
		case prev.Explicit():
			pick = prev.Type
		default:
			return ChangeState{Suggested: suggested, Type: suggested}, nil
		}
		actor = prev.SetBy
		if reason == "" {
			reason = prev.Reason
		}
	}
	if pick.Rank() >= suggested.Rank() {
		return ChangeState{Suggested: suggested, Type: pick, SetBy: actor, Reason: reason}, nil
	}
	if reason == "" {
		return ChangeState{}, ErrChangeReasonRequired
	}
	if len(prev.AgreedBy) > 0 && prev.Type == pick && prev.Suggested == suggested {
		return ChangeState{Suggested: suggested, Type: pick, SetBy: prev.SetBy, Reason: prev.Reason, AgreedBy: prev.AgreedBy}, nil
	}
	return ChangeState{Suggested: suggested, Type: suggested, Requested: pick, Reason: reason, SetBy: actor}, nil
}

// RecordAgreement applies the pending lowering: the approvers agreed, so the type becomes the
// one requested.
func RecordAgreement(s ChangeState, agreedBy []uuid.UUID) (ChangeState, error) {
	if s.Requested == "" {
		return ChangeState{}, ErrNoPendingLowering
	}
	if len(agreedBy) == 0 {
		return ChangeState{}, ErrAgreementNeedsApprovers
	}
	s.Type = s.Requested
	s.Requested = ""
	s.AgreedBy = agreedBy
	return s, nil
}

// FinalizeChange returns the type a version publishes as, given a fresh classification of its
// content, which may have been edited since submit. Nobody picked: the fresh suggestion. A pick
// stands but can only be raised by the fresh suggestion, never lowered by it. A recorded
// agreement keeps its lowered type only while the content is no bigger a change than what the
// approvers agreed to. A request still waiting for agreement does not count.
func FinalizeChange(s ChangeState, fresh ChangeType) ChangeType {
	if !s.Explicit() || !s.Type.Valid() {
		return fresh
	}
	if len(s.AgreedBy) > 0 && fresh.Rank() <= s.Suggested.Rank() {
		return s.Type
	}
	return MaxChangeType(s.Type, fresh)
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/lifecycle"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func (f *fakePolicyStore) SetVersionChangeType(_ context.Context, id uuid.UUID, ct domain.ChangeType, _ string, _ uuid.UUID) (domain.PolicyVersion, error) {
	f.changeCalls = append(f.changeCalls, "set:"+string(ct))
	if f.changeErr != nil {
		return domain.PolicyVersion{}, f.changeErr
	}
	pv := f.versions[id]
	pv.Change.Type = ct
	f.versions[id] = pv
	return pv, nil
}

func (f *fakePolicyStore) RecordChangeTypeAgreement(_ context.Context, id uuid.UUID, by []uuid.UUID) (domain.PolicyVersion, error) {
	f.changeCalls = append(f.changeCalls, "agree")
	if f.changeErr != nil {
		return domain.PolicyVersion{}, f.changeErr
	}
	pv := f.versions[id]
	pv.Change.AgreedBy = by
	f.versions[id] = pv
	return pv, nil
}

func semverHandler(t *testing.T, st domain.PolicyVersionStatus) (*grpcsvc.PolicyHandler, *fakePolicyStore, *capturePublisher, uuid.UUID) {
	t.Helper()
	cap := &capturePublisher{}
	fakePS := newFakePolicyStore()
	policyID, verID := uuid.New(), uuid.New()
	fakePS.policies[policyID] = domain.Policy{ID: policyID}
	fakePS.versions[verID] = domain.PolicyVersion{ID: verID, PolicyID: policyID, Status: st, ContentJSON: textDoc("x")}
	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, audit.New(cap))
	return h, fakePS, cap, verID
}

func TestSetVersionStatusSubmitCarriesTheChangePick(t *testing.T) {
	h, fakePS, _, id := semverHandler(t, domain.PolicyVersionStatusDraft)
	req := setStatusReq(id, "in_review")
	req.ActorUserId = uuid.NewString()
	req.ChangeType = corev1.ChangeType_CHANGE_TYPE_MINOR
	req.ChangeTypeReason = "wording"
	if _, err := h.SetVersionStatus(context.Background(), req); err != nil {
		t.Fatalf("SetVersionStatus: %v", err)
	}
	if len(fakePS.statusOpts) != 1 {
		t.Fatalf("the pick must reach the store, got %d options", len(fakePS.statusOpts))
	}

	h2, fake2, _, id2 := semverHandler(t, domain.PolicyVersionStatusDraft)
	if _, err := h2.SetVersionStatus(context.Background(), setStatusReq(id2, "in_review")); err != nil {
		t.Fatalf("SetVersionStatus without a pick: %v", err)
	}
	if len(fake2.statusOpts) != 0 {
		t.Fatalf("no pick means no option, got %d", len(fake2.statusOpts))
	}
}

func TestSetVersionStatusPickNeedsARealActor(t *testing.T) {
	h, _, _, id := semverHandler(t, domain.PolicyVersionStatusDraft)
	req := setStatusReq(id, "in_review") // actor "system"
	req.ChangeType = corev1.ChangeType_CHANGE_TYPE_MAJOR
	_, err := h.SetVersionStatus(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v (%v), want InvalidArgument", status.Code(err), err)
	}
}

func TestSetVersionStatusLoweringWithoutReasonIsInvalidArgument(t *testing.T) {
	h, fakePS, _, id := semverHandler(t, domain.PolicyVersionStatusDraft)
	fakePS.statusErr = domain.ErrChangeReasonRequired
	req := setStatusReq(id, "in_review")
	req.ActorUserId = uuid.NewString()
	req.ChangeType = corev1.ChangeType_CHANGE_TYPE_FIX
	_, err := h.SetVersionStatus(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "reason") {
		t.Fatalf("err = %v, want InvalidArgument naming the reason", err)
	}
}

func TestSetVersionChangeType(t *testing.T) {
	actor := uuid.NewString()
	t.Run("validates", func(t *testing.T) {
		h, _, _, id := semverHandler(t, domain.PolicyVersionStatusDraft)
		for name, req := range map[string]*corev1.SetVersionChangeTypeRequest{
			"bad version id": {PolicyVersionId: "x", ChangeType: corev1.ChangeType_CHANGE_TYPE_MAJOR, ActorUserId: actor},
			"no type":        {PolicyVersionId: id.String(), ActorUserId: actor},
			"no actor":       {PolicyVersionId: id.String(), ChangeType: corev1.ChangeType_CHANGE_TYPE_MAJOR},
			"system actor":   {PolicyVersionId: id.String(), ChangeType: corev1.ChangeType_CHANGE_TYPE_MAJOR, ActorUserId: "system"},
		} {
			if _, err := h.SetVersionChangeType(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: code = %v (%v), want InvalidArgument", name, status.Code(err), err)
			}
		}
	})
	t.Run("maps the store's refusals", func(t *testing.T) {
		for want, errIn := range map[codes.Code]error{
			codes.InvalidArgument:    domain.ErrChangeReasonRequired,
			codes.FailedPrecondition: store.ErrChangeTypeLocked,
		} {
			h, fakePS, _, id := semverHandler(t, domain.PolicyVersionStatusDraft)
			fakePS.changeErr = errIn
			_, err := h.SetVersionChangeType(context.Background(), &corev1.SetVersionChangeTypeRequest{
				PolicyVersionId: id.String(), ChangeType: corev1.ChangeType_CHANGE_TYPE_MINOR, ActorUserId: actor})
			if status.Code(err) != want {
				t.Fatalf("%v: code = %v (%v), want %v", errIn, status.Code(err), err, want)
			}
		}
		h, fakePS, _, id := semverHandler(t, domain.PolicyVersionStatusDraft)
		fakePS.changeErr = store.ErrFirstVersion
		_, err := h.SetVersionChangeType(context.Background(), &corev1.SetVersionChangeTypeRequest{
			PolicyVersionId: id.String(), ChangeType: corev1.ChangeType_CHANGE_TYPE_MINOR, ActorUserId: actor})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("first version: code = %v", status.Code(err))
		}
	})
	t.Run("sets and audits", func(t *testing.T) {
		h, _, cap, id := semverHandler(t, domain.PolicyVersionStatusDraft)
		resp, err := h.SetVersionChangeType(context.Background(), &corev1.SetVersionChangeTypeRequest{
			PolicyVersionId: id.String(), ChangeType: corev1.ChangeType_CHANGE_TYPE_MAJOR, ActorUserId: actor})
		if err != nil {
			t.Fatalf("SetVersionChangeType: %v", err)
		}
		if resp.GetVersion().GetChangeType() != corev1.ChangeType_CHANGE_TYPE_MAJOR {
			t.Fatalf("change_type = %v", resp.GetVersion().GetChangeType())
		}
		if len(cap.calls) != 1 || cap.calls[0].event.Action != "policy.change_type_set" || cap.calls[0].event.ActorUserID != actor {
			t.Fatalf("audit = %+v", cap.calls)
		}
	})
}

func TestRecordChangeTypeAgreement(t *testing.T) {
	a, b := uuid.NewString(), uuid.NewString()
	t.Run("validates", func(t *testing.T) {
		h, _, _, id := semverHandler(t, domain.PolicyVersionStatusInReview)
		for name, req := range map[string]*corev1.RecordChangeTypeAgreementRequest{
			"bad version id": {PolicyVersionId: "x", AgreedByUserIds: []string{a}, ActorUserId: a},
			"no approvers":   {PolicyVersionId: id.String(), ActorUserId: a},
			"bad approver":   {PolicyVersionId: id.String(), AgreedByUserIds: []string{"nope"}, ActorUserId: a},
		} {
			if _, err := h.RecordChangeTypeAgreement(context.Background(), req); status.Code(err) != codes.InvalidArgument {
				t.Fatalf("%s: code = %v (%v), want InvalidArgument", name, status.Code(err), err)
			}
		}
	})
	t.Run("nothing pending is a precondition failure", func(t *testing.T) {
		h, fakePS, _, id := semverHandler(t, domain.PolicyVersionStatusInReview)
		fakePS.changeErr = domain.ErrNoPendingLowering
		_, err := h.RecordChangeTypeAgreement(context.Background(), &corev1.RecordChangeTypeAgreementRequest{
			PolicyVersionId: id.String(), AgreedByUserIds: []string{a}, ActorUserId: a})
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("code = %v (%v)", status.Code(err), err)
		}
	})
	t.Run("records and audits who agreed", func(t *testing.T) {
		h, fakePS, cap, id := semverHandler(t, domain.PolicyVersionStatusInReview)
		resp, err := h.RecordChangeTypeAgreement(context.Background(), &corev1.RecordChangeTypeAgreementRequest{
			PolicyVersionId: id.String(), AgreedByUserIds: []string{a, b}, ActorUserId: b})
		if err != nil {
			t.Fatalf("RecordChangeTypeAgreement: %v", err)
		}
		if got := resp.GetVersion().GetChangeTypeAgreedBy(); len(got) != 2 || got[0] != a || got[1] != b {
			t.Fatalf("agreed_by = %v", got)
		}
		if len(fakePS.changeCalls) != 1 {
			t.Fatalf("calls = %v", fakePS.changeCalls)
		}
		if len(cap.calls) != 1 || cap.calls[0].event.Action != "policy.change_type_agreed" ||
			!strings.Contains(cap.calls[0].event.Attributes["agreed_by"], a) {
			t.Fatalf("audit = %+v", cap.calls)
		}
	})
}

func TestVersionReadsCarryTheSemanticVersionAndChange(t *testing.T) {
	h, fakePS, _, id := semverHandler(t, domain.PolicyVersionStatusPublished)
	setBy, agreed := uuid.New(), uuid.New()
	pv := fakePS.versions[id]
	pv.Semver = domain.SemVer{Major: 2, Minor: 1}
	pv.Change = domain.ChangeState{Suggested: domain.ChangeTypeMajor, Type: domain.ChangeTypeMinor, Reason: "r", SetBy: setBy, AgreedBy: []uuid.UUID{agreed}}
	fakePS.versions[id] = pv

	resp, err := h.GetPolicyVersion(context.Background(), &corev1.GetPolicyVersionRequest{Id: id.String()})
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	v := resp.GetVersion()
	if v.GetSemanticVersion().GetText() != "2.1.0" || v.GetSemanticVersion().GetMajor() != 2 || v.GetSemanticVersion().GetMinor() != 1 {
		t.Fatalf("semantic_version = %v", v.GetSemanticVersion())
	}
	if v.GetSuggestedChangeType() != corev1.ChangeType_CHANGE_TYPE_MAJOR || v.GetChangeType() != corev1.ChangeType_CHANGE_TYPE_MINOR ||
		v.GetChangeTypeReason() != "r" || v.GetChangeTypeSetBy() != setBy.String() || len(v.GetChangeTypeAgreedBy()) != 1 {
		t.Fatalf("change fields = %v", v)
	}
	if v.GetRequestedChangeType() != corev1.ChangeType_CHANGE_TYPE_UNSPECIFIED {
		t.Fatalf("requested_change_type = %v", v.GetRequestedChangeType())
	}

	// A version with no number leaves the field unset rather than sending 0.0.0.
	id2 := uuid.New()
	fakePS.versions[id2] = domain.PolicyVersion{ID: id2, PolicyID: pv.PolicyID, Status: domain.PolicyVersionStatusDraft}
	resp, _ = h.GetPolicyVersion(context.Background(), &corev1.GetPolicyVersionRequest{Id: id2.String()})
	if resp.GetVersion().GetSemanticVersion() != nil {
		t.Fatalf("an unnumbered version must leave semantic_version unset, got %v", resp.GetVersion().GetSemanticVersion())
	}
}

func TestObligatingPolicyCarriesTheChangeType(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeGS := newFakeCategoryStore()
	_, _, _, publishedVersionID := seedObligationEnv(t, fakePS, fakeGS)
	pv := fakePS.versions[publishedVersionID]
	pv.Semver = domain.SemVer{Major: 3}
	pv.Change.Type = domain.ChangeTypeMajor
	fakePS.versions[publishedVersionID] = pv

	h := grpcsvc.NewPolicyHandler(fakePS, fakeGS, newFakeTemplateStore(), domain.NoopValidator{}, nil)
	resp, err := h.ListObligatingPolicies(context.Background(), &corev1.ListObligatingPoliciesRequest{})
	if err != nil || len(resp.Policies) != 1 {
		t.Fatalf("ListObligatingPolicies = %v, %v", resp, err)
	}
	op := resp.Policies[0]
	if op.GetChangeType() != corev1.ChangeType_CHANGE_TYPE_MAJOR || op.GetSemanticVersion().GetText() != "3.0.0" {
		t.Fatalf("obligating policy = %v", op)
	}
}

func TestPublishedLifecycleEventCarriesTheNumberAndChangeType(t *testing.T) {
	lifeCap := &lifecycleCapture{}
	fakePS := newFakePolicyStore()
	policyID, verID := uuid.New(), uuid.New()
	fakePS.policies[policyID] = domain.Policy{ID: policyID, Number: "POL-IT-000001", Title: "T"}
	fakePS.versions[verID] = domain.PolicyVersion{
		ID: verID, PolicyID: policyID, Status: domain.PolicyVersionStatusInReview, ContentJSON: textDoc("x"),
		Semver: domain.SemVer{Major: 2, Minor: 1}, Change: domain.ChangeState{Type: domain.ChangeTypeMinor},
	}
	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil).
		WithLifecycleEmitter(lifecycle.New(lifeCap))
	if _, err := h.SetVersionStatus(context.Background(), setStatusReq(verID, "published")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(lifeCap.calls) != 1 {
		t.Fatalf("lifecycle events = %d, want 1", len(lifeCap.calls))
	}
	var env struct {
		Version lifecycle.PolicyVersionContent `json:"version"`
	}
	if err := json.Unmarshal(lifeCap.calls[0].body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := env.Version
	if got.Semver != "2.1.0" || got.ChangeType != "minor" {
		t.Fatalf("event carries %q / %q, want 2.1.0 / minor", got.Semver, got.ChangeType)
	}
}

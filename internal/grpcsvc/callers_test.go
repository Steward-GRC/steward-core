// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"testing"

	log "github.com/Bugs5382/go-log"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	workloadidentity "github.com/Bugs5382/go-workload-identity"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
)

var coreServices = []grpc.ServiceDesc{
	corev1.CategoryService_ServiceDesc, corev1.TemplateService_ServiceDesc, corev1.PolicyService_ServiceDesc,
	corev1.SettingsService_ServiceDesc, corev1.AppendixService_ServiceDesc, corev1.RelationService_ServiceDesc,
	corev1.ContactService_ServiceDesc, corev1.ReferenceService_ServiceDesc, corev1.DefinitionLibraryService_ServiceDesc,
	corev1.EmailServiceSecretService_ServiceDesc, corev1.AssetService_ServiceDesc, corev1.ReviewService_ServiceDesc,
	corev1.ReviewSnapshotService_ServiceDesc, corev1.PolicyAppendixService_ServiceDesc, corev1.VersionLinkService_ServiceDesc,
}

func allMethods() []string {
	var out []string
	for _, sd := range coreServices {
		for _, md := range sd.Methods {
			out = append(out, "/"+sd.ServiceName+"/"+md.MethodName)
		}
	}
	return out
}

// requireCallerMethods checks caller is listed with access on exactly want.
func requireCallerMethods(t *testing.T, caller string, want map[string]workloadidentity.Access) {
	t.Helper()
	p := CallerPolicy()
	got := map[string]workloadidentity.Access{}
	for _, m := range allMethods() {
		if a, ok := p.Lookup(m, caller); ok {
			got[m] = a
		}
	}
	require.Equal(t, want, got)
}

func TestCallerPolicyCoversEveryMethodAndNothingElse(t *testing.T) {
	p := CallerPolicy()
	methods := map[string]bool{}
	for _, m := range allMethods() {
		methods[m] = true
		require.NotEmpty(t, p[m], "%s has no caller", m)
	}
	for m := range p {
		require.True(t, methods[m], "the policy lists %s, which core doesn't serve", m)
	}
}

func TestCallerPolicyGateway(t *testing.T) {
	want := map[string]workloadidentity.Access{}
	for _, m := range allMethods() {
		// The secret never reaches a browser, and the approvers' agreement to a lowered change
		// type is workflow's to write: a client must not be able to claim it.
		if m != corev1.EmailServiceSecretService_GetEmailServiceSecret_FullMethodName &&
			m != corev1.PolicyService_RecordChangeTypeAgreement_FullMethodName {
			want[m] = workloadidentity.OnBehalf
		}
	}
	requireCallerMethods(t, CallerGateway, want)
}

func TestCallerPolicyDelivery(t *testing.T) {
	requireCallerMethods(t, CallerDelivery, map[string]workloadidentity.Access{
		corev1.PolicyService_GetPolicy_FullMethodName:        workloadidentity.Self,
		corev1.PolicyService_GetPolicyVersion_FullMethodName: workloadidentity.Self,
		corev1.PolicyService_DiffVersions_FullMethodName:     workloadidentity.Self,
		corev1.AppendixService_ListAppendices_FullMethodName: workloadidentity.Self,
	})
}

func TestCallerPolicyWorkflow(t *testing.T) {
	requireCallerMethods(t, CallerWorkflow, map[string]workloadidentity.Access{
		corev1.CategoryService_GetCategory_FullMethodName:         workloadidentity.Self,
		corev1.CategoryService_GetEffectiveRuleset_FullMethodName: workloadidentity.Self,
		corev1.PolicyService_GetPolicy_FullMethodName:             workloadidentity.Self,
		corev1.PolicyService_GetPolicyVersion_FullMethodName:      workloadidentity.Self,
		corev1.PolicyService_SetVersionStatus_FullMethodName:      workloadidentity.Self,
		// core#28: workflow records who agreed to lower a change type.
		corev1.PolicyService_RecordChangeTypeAgreement_FullMethodName: workloadidentity.Self,
		// core#114: workflow reads the site work calendar when a document is submitted.
		corev1.SettingsService_GetGlobalSettings_FullMethodName: workloadidentity.Self,
	})
}

// workflow reads the global settings but never writes them or touches the email-service config, and no other
// internal caller gains the read.
func TestCallerPolicyGetGlobalSettings(t *testing.T) {
	p := CallerPolicy()
	got, ok := p.Lookup(corev1.SettingsService_GetGlobalSettings_FullMethodName, CallerWorkflow)
	require.True(t, ok, "workflow must be allowed")
	require.Equal(t, workloadidentity.Self, got)
	for _, m := range []string{
		corev1.SettingsService_SetGlobalSettings_FullMethodName,
		corev1.SettingsService_SetEmailServiceConfig_FullMethodName,
		corev1.SettingsService_EmailServiceConfigStatus_FullMethodName,
	} {
		_, ok = p.Lookup(m, CallerWorkflow)
		require.False(t, ok, "workflow must be refused %s", m)
	}
	for _, caller := range []string{CallerDelivery, CallerObligations, CallerCollab, CallerIdentity, "ai"} {
		_, ok = p.Lookup(corev1.SettingsService_GetGlobalSettings_FullMethodName, caller)
		require.False(t, ok, "%s must be refused", caller)
	}
}

func TestCallerPolicyObligations(t *testing.T) {
	requireCallerMethods(t, CallerObligations, map[string]workloadidentity.Access{
		corev1.CategoryService_GetCategory_FullMethodName:                     workloadidentity.Self,
		corev1.CategoryService_GetCategoryRuleset_FullMethodName:              workloadidentity.Self,
		corev1.CategoryService_GetEffectiveRuleset_FullMethodName:             workloadidentity.Self,
		corev1.PolicyService_GetPolicy_FullMethodName:                         workloadidentity.Self,
		corev1.PolicyService_GetPolicyVersion_FullMethodName:                  workloadidentity.Self,
		corev1.PolicyService_ListPolicyVersions_FullMethodName:                workloadidentity.Self,
		corev1.PolicyService_ListObligatingPolicies_FullMethodName:            workloadidentity.Self,
		corev1.PolicyService_ResolvePolicyObligation_FullMethodName:           workloadidentity.Self,
		corev1.EmailServiceSecretService_GetEmailServiceSecret_FullMethodName: workloadidentity.Self,
	})
}

func TestCallerPolicyCollab(t *testing.T) {
	requireCallerMethods(t, CallerCollab, map[string]workloadidentity.Access{
		corev1.CategoryService_GetCategory_FullMethodName:         workloadidentity.Self,
		corev1.CategoryService_GetEffectiveRuleset_FullMethodName: workloadidentity.Self,
		corev1.PolicyService_GetPolicy_FullMethodName:             workloadidentity.Self,
		corev1.PolicyService_UpdateDraftContent_FullMethodName:    workloadidentity.OnBehalf,
		// core#87: the appendix room resolves an appendix's policy to issue a room token.
		corev1.PolicyAppendixService_GetPolicyAppendix_FullMethodName: workloadidentity.Self,
		// core#87: the appendix room checkpoints on behalf of the editing user.
		corev1.PolicyAppendixService_CheckpointAppendixDraft_FullMethodName: workloadidentity.OnBehalf,
	})
}

// Identity's account merge and delete checks and previews call these,
// passing the admin who runs them.
func TestCallerPolicyIdentity(t *testing.T) {
	requireCallerMethods(t, CallerIdentity, map[string]workloadidentity.Access{
		corev1.CategoryService_PurgeUserCategoryRules_FullMethodName: workloadidentity.OnBehalf,
		corev1.PolicyService_ListPoliciesByOwner_FullMethodName:      workloadidentity.OnBehalf,
		corev1.PolicyService_ReassignUserPolicies_FullMethodName:     workloadidentity.OnBehalf,
		corev1.PolicyService_GetPolicy_FullMethodName:                workloadidentity.OnBehalf,
		corev1.PolicyService_GetPolicyVersion_FullMethodName:         workloadidentity.OnBehalf,
	})
}

// GetEffectiveRuleset is the chain read the access engine's callers share:
// the gateway on behalf of a user, workflow, obligations and collab as
// themselves. delivery, identity and an unlisted caller are refused.
func TestCallerPolicyGetEffectiveRuleset(t *testing.T) {
	p := CallerPolicy()
	m := corev1.CategoryService_GetEffectiveRuleset_FullMethodName
	for caller, want := range map[string]workloadidentity.Access{
		CallerGateway: workloadidentity.OnBehalf, CallerWorkflow: workloadidentity.Self,
		CallerObligations: workloadidentity.Self, CallerCollab: workloadidentity.Self,
	} {
		got, ok := p.Lookup(m, caller)
		require.True(t, ok, "%s must be allowed", caller)
		require.Equal(t, want, got, caller)
	}
	for _, caller := range []string{CallerDelivery, CallerIdentity, "ai", "reporting"} {
		_, ok := p.Lookup(m, caller)
		require.False(t, ok, "%s must be refused", caller)
	}
}

func TestCallerPolicyListsNoOtherCaller(t *testing.T) {
	known := map[string]bool{CallerGateway: true, CallerDelivery: true, CallerWorkflow: true, CallerObligations: true, CallerCollab: true, CallerIdentity: true}
	for m, callers := range CallerPolicy() {
		for c := range callers {
			require.True(t, known[c], "%s lists unknown caller %q", m, c)
		}
	}
	requireCallerMethods(t, "ai", map[string]workloadidentity.Access{})
}

type recordingEmitter struct{ evs []audit.Event }

func (r *recordingEmitter) Emit(_ context.Context, ev audit.Event) error {
	r.evs = append(r.evs, ev)
	return nil
}

func TestAuditDenialRecordsTheCallerNotAClaimedUser(t *testing.T) {
	rec := &recordingEmitter{}
	hook := AuditDenial(rec, log.Nop())
	hook(context.Background(), workloadidentity.Denial{
		Method: corev1.PolicyService_GetPolicy_FullMethodName, Code: codes.PermissionDenied, Reason: workloadidentity.ReasonMethodNotAllowed,
		Caller: workloadidentity.Caller{Name: "reporting", ServiceAccount: "steward/steward-reporting"},
	})
	hook(context.Background(), workloadidentity.Denial{Method: "/m", Code: codes.Unauthenticated, Reason: workloadidentity.ReasonNoToken})
	require.Len(t, rec.evs, 2)
	require.Equal(t, audit.TierAudit, rec.evs[0].Tier)
	require.Equal(t, "rpc.denied", rec.evs[0].Action)
	require.Equal(t, "service:reporting", rec.evs[0].ActorUserID)
	require.Equal(t, corev1.PolicyService_GetPolicy_FullMethodName, rec.evs[0].Subject)
	require.Equal(t, map[string]string{
		"method": corev1.PolicyService_GetPolicy_FullMethodName, "caller": "reporting", "service_account": "steward/steward-reporting",
		"code": "PermissionDenied", "reason": workloadidentity.ReasonMethodNotAllowed,
	}, rec.evs[0].Attributes)
	require.Equal(t, "service:unauthenticated", rec.evs[1].ActorUserID)
}

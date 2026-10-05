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

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/workloadauth"
)

var coreServices = []grpc.ServiceDesc{
	corev1.CategoryService_ServiceDesc, corev1.TemplateService_ServiceDesc, corev1.PolicyService_ServiceDesc,
	corev1.SettingsService_ServiceDesc, corev1.AppendixService_ServiceDesc, corev1.RelationService_ServiceDesc,
	corev1.ContactService_ServiceDesc, corev1.ReferenceService_ServiceDesc, corev1.DefinitionLibraryService_ServiceDesc,
	corev1.EmailServiceSecretService_ServiceDesc, corev1.AssetService_ServiceDesc,
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
func requireCallerMethods(t *testing.T, caller string, want map[string]workloadauth.Access) {
	t.Helper()
	p := CallerPolicy()
	got := map[string]workloadauth.Access{}
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
	want := map[string]workloadauth.Access{}
	for _, m := range allMethods() {
		if m != corev1.EmailServiceSecretService_GetEmailServiceSecret_FullMethodName {
			want[m] = workloadauth.OnBehalf
		}
	}
	requireCallerMethods(t, CallerGateway, want)
}

func TestCallerPolicyDelivery(t *testing.T) {
	requireCallerMethods(t, CallerDelivery, map[string]workloadauth.Access{
		corev1.PolicyService_GetPolicy_FullMethodName:        workloadauth.Self,
		corev1.PolicyService_GetPolicyVersion_FullMethodName: workloadauth.Self,
		corev1.AppendixService_ListAppendices_FullMethodName: workloadauth.Self,
	})
}

func TestCallerPolicyWorkflow(t *testing.T) {
	requireCallerMethods(t, CallerWorkflow, map[string]workloadauth.Access{
		corev1.CategoryService_GetCategory_FullMethodName:    workloadauth.Self,
		corev1.PolicyService_GetPolicy_FullMethodName:        workloadauth.Self,
		corev1.PolicyService_GetPolicyVersion_FullMethodName: workloadauth.Self,
		corev1.PolicyService_SetVersionStatus_FullMethodName: workloadauth.Self,
	})
}

func TestCallerPolicyObligations(t *testing.T) {
	requireCallerMethods(t, CallerObligations, map[string]workloadauth.Access{
		corev1.CategoryService_GetCategory_FullMethodName:                     workloadauth.Self,
		corev1.CategoryService_GetCategoryRuleset_FullMethodName:              workloadauth.Self,
		corev1.PolicyService_GetPolicy_FullMethodName:                         workloadauth.Self,
		corev1.PolicyService_GetPolicyVersion_FullMethodName:                  workloadauth.Self,
		corev1.PolicyService_ListPolicyVersions_FullMethodName:                workloadauth.Self,
		corev1.PolicyService_ListObligatingPolicies_FullMethodName:            workloadauth.Self,
		corev1.PolicyService_ResolvePolicyObligation_FullMethodName:           workloadauth.Self,
		corev1.EmailServiceSecretService_GetEmailServiceSecret_FullMethodName: workloadauth.Self,
	})
}

func TestCallerPolicyCollab(t *testing.T) {
	requireCallerMethods(t, CallerCollab, map[string]workloadauth.Access{
		corev1.CategoryService_GetCategory_FullMethodName:      workloadauth.Self,
		corev1.PolicyService_GetPolicy_FullMethodName:          workloadauth.Self,
		corev1.PolicyService_UpdateDraftContent_FullMethodName: workloadauth.OnBehalf,
	})
}

func TestCallerPolicyListsNoOtherCaller(t *testing.T) {
	known := map[string]bool{CallerGateway: true, CallerDelivery: true, CallerWorkflow: true, CallerObligations: true, CallerCollab: true}
	for m, callers := range CallerPolicy() {
		for c := range callers {
			require.True(t, known[c], "%s lists unknown caller %q", m, c)
		}
	}
	requireCallerMethods(t, "ai", map[string]workloadauth.Access{})
}

type recordingEmitter struct{ evs []audit.Event }

func (r *recordingEmitter) Emit(_ context.Context, ev audit.Event) error {
	r.evs = append(r.evs, ev)
	return nil
}

func TestAuditDenialRecordsTheCallerNotAClaimedUser(t *testing.T) {
	rec := &recordingEmitter{}
	hook := AuditDenial(rec, log.Nop())
	hook(context.Background(), workloadauth.Denial{
		Method: corev1.PolicyService_GetPolicy_FullMethodName, Code: codes.PermissionDenied, Reason: workloadauth.ReasonMethodNotAllowed,
		Caller: workloadauth.Caller{Name: "reporting", ServiceAccount: "steward/steward-reporting"},
	})
	hook(context.Background(), workloadauth.Denial{Method: "/m", Code: codes.Unauthenticated, Reason: workloadauth.ReasonNoToken})
	require.Len(t, rec.evs, 2)
	require.Equal(t, audit.TierAudit, rec.evs[0].Tier)
	require.Equal(t, "rpc.denied", rec.evs[0].Action)
	require.Equal(t, "service:reporting", rec.evs[0].ActorUserID)
	require.Equal(t, corev1.PolicyService_GetPolicy_FullMethodName, rec.evs[0].Subject)
	require.Equal(t, map[string]string{
		"method": corev1.PolicyService_GetPolicy_FullMethodName, "caller": "reporting", "service_account": "steward/steward-reporting",
		"code": "PermissionDenied", "reason": workloadauth.ReasonMethodNotAllowed,
	}, rec.evs[0].Attributes)
	require.Equal(t, "service:unauthenticated", rec.evs[1].ActorUserID)
}

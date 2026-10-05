// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"

	log "github.com/Bugs5382/go-log"
	"google.golang.org/grpc"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/workloadauth"
)

// Caller names, from the service accounts steward-<name>.
const (
	CallerGateway     = "gateway"
	CallerDelivery    = "delivery"
	CallerWorkflow    = "workflow"
	CallerObligations = "obligations"
	CallerCollab      = "collab"
)

var services = []grpc.ServiceDesc{
	corev1.CategoryService_ServiceDesc, corev1.TemplateService_ServiceDesc, corev1.PolicyService_ServiceDesc,
	corev1.SettingsService_ServiceDesc, corev1.AppendixService_ServiceDesc, corev1.RelationService_ServiceDesc,
	corev1.ContactService_ServiceDesc, corev1.ReferenceService_ServiceDesc, corev1.DefinitionLibraryService_ServiceDesc,
	corev1.EmailServiceSecretService_ServiceDesc, corev1.AssetService_ServiceDesc,
}

// The internal callers act as themselves: each reads what its own job needs
// and names any user in the request's actor_user_id, never by forwarding an
// actor.
var (
	deliveryMethods = []string{
		corev1.PolicyService_GetPolicy_FullMethodName,
		corev1.PolicyService_GetPolicyVersion_FullMethodName,
		corev1.PolicyService_DiffVersions_FullMethodName,
		corev1.AppendixService_ListAppendices_FullMethodName,
	}
	workflowMethods = []string{
		corev1.CategoryService_GetCategory_FullMethodName,
		corev1.PolicyService_GetPolicy_FullMethodName,
		corev1.PolicyService_GetPolicyVersion_FullMethodName,
		corev1.PolicyService_SetVersionStatus_FullMethodName,
	}
	obligationsMethods = []string{
		corev1.CategoryService_GetCategory_FullMethodName,
		corev1.CategoryService_GetCategoryRuleset_FullMethodName,
		corev1.PolicyService_GetPolicy_FullMethodName,
		corev1.PolicyService_GetPolicyVersion_FullMethodName,
		corev1.PolicyService_ListPolicyVersions_FullMethodName,
		corev1.PolicyService_ListObligatingPolicies_FullMethodName,
		corev1.PolicyService_ResolvePolicyObligation_FullMethodName,
		corev1.EmailServiceSecretService_GetEmailServiceSecret_FullMethodName,
	}
	collabMethods = []string{
		corev1.CategoryService_GetCategory_FullMethodName,
		corev1.PolicyService_GetPolicy_FullMethodName,
	}
)

// CallerPolicy is core's per-method allow-list. The gateway passes the
// signed-in user's actor on every method except the email-service secret,
// which never reaches a browser. collab saves a draft on behalf of the editing
// user. delivery, workflow and obligations act only as themselves. Anything
// else is refused.
func CallerPolicy() workloadauth.Policy {
	p := workloadauth.Policy{}
	for _, sd := range services {
		for _, md := range sd.Methods {
			p["/"+sd.ServiceName+"/"+md.MethodName] = map[string]workloadauth.Access{CallerGateway: workloadauth.OnBehalf}
		}
	}
	p[corev1.EmailServiceSecretService_GetEmailServiceSecret_FullMethodName] = map[string]workloadauth.Access{}
	for caller, methods := range map[string][]string{
		CallerDelivery: deliveryMethods, CallerWorkflow: workflowMethods,
		CallerObligations: obligationsMethods, CallerCollab: collabMethods,
	} {
		for _, m := range methods {
			p[m][caller] = workloadauth.Self
		}
	}
	p[corev1.PolicyService_UpdateDraftContent_FullMethodName][CallerCollab] = workloadauth.OnBehalf
	return p
}

// AuditDenial records a call the workload-auth interceptor refused, as
// rpc.denied in the audit tier. The actor is the authenticated caller (or
// "unauthenticated"), never a user the call claimed.
func AuditDenial(emitter auditEmitter, lg log.Logger) workloadauth.DenyHook {
	return func(ctx context.Context, d workloadauth.Denial) {
		caller := d.Caller.Name
		if caller == "" {
			caller = "unauthenticated"
		}
		err := emitter.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "rpc.denied", ActorUserID: "service:" + caller, Subject: d.Method,
			Attributes: map[string]string{
				"method": d.Method, "caller": d.Caller.Name, "service_account": d.Caller.ServiceAccount,
				"code": d.Code.String(), "reason": d.Reason,
			},
		})
		if err != nil {
			lg.Ctx(ctx).Error(err, "audit of a refused call failed", log.F("method", d.Method), log.F("caller", caller))
		}
	}
}

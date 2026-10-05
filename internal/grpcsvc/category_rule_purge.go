// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"strconv"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// purgeReasonUserDeleted tells a purge apart from ReassignUserPolicies'
// reason=user_reassign in the audit trail.
const purgeReasonUserDeleted = "user_deleted"

// PurgeUserCategoryRules handles CategoryService.PurgeUserCategoryRules: it
// removes every user rule naming the user, across all categories, in one
// audited transaction.
//
// A deleted account has no target to move records to, so without this a
// deleted user's rules would stay behind as grants naming nobody. Only user
// rules go: group and everyone rules stay even when their subject_ref equals
// the id, and records of what the user owned or did keep naming them.
//
// Dropping a permission is itself a permission change, so it is audited per
// rule and per category, both with reason=user_deleted. A dry run writes
// nothing and emits nothing, so a preview never looks like a revoke.
func (h *CategoryHandler) PurgeUserCategoryRules(ctx context.Context, req *corev1.PurgeUserCategoryRulesRequest) (*corev1.PurgeUserCategoryRulesResponse, error) {
	// A bad id must not reach a query that matches nothing: it would report
	// "removed 0 rules" and the caller would believe the purge happened.
	userID, err := uuid.Parse(req.GetUserId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid user_id: %v", err)
	}
	if userID == uuid.Nil {
		return nil, status.Error(codes.InvalidArgument, "user_id must not be the nil UUID")
	}
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}

	removed, err := h.store.PurgeUserCategoryRules(ctx, userID, req.GetDryRun())
	if err != nil {
		return nil, storeUnavailable(ctx, "purge_user_category_rules", err)
	}

	// Affected categories in first-seen order — the store returns rows sorted by
	// (category_id, ordinal), so this is stable across calls.
	affected := make([]string, 0, len(removed))
	seen := make(map[uuid.UUID]struct{}, len(removed))
	for _, r := range removed {
		if _, ok := seen[r.CategoryID]; ok {
			continue
		}
		seen[r.CategoryID] = struct{}{}
		affected = append(affected, r.CategoryID.String())
	}

	if !req.GetDryRun() {
		h.emitCategoryRulePurgeAudit(ctx, req.GetActorUserId(), userID, removed)
	}

	out := make([]*corev1.RemovedCategoryRule, 0, len(removed))
	for _, r := range removed {
		out = append(out, &corev1.RemovedCategoryRule{
			CategoryId: r.CategoryID.String(),
			Rule:       categoryRuleToProto(r.Rule),
		})
	}
	return &corev1.PurgeUserCategoryRulesResponse{
		RemovedRules:        toInt32(len(removed)),
		AffectedCategoryIds: affected,
		Rules:               out,
	}, nil
}

// emitCategoryRulePurgeAudit records each removed rule's grants with the same
// attributes SetCategoryRuleset uses, so a revoke reads the same however it
// happened, plus one summary per category. Nothing removed, nothing emitted.
// Without an actor_user_id it falls back to the forwarded actor.
func (h *CategoryHandler) emitCategoryRulePurgeAudit(ctx context.Context, actorUserID string, userID uuid.UUID, removed []store.PurgedCategoryRule) {
	if h.auditor == nil || len(removed) == 0 {
		return
	}
	actor := actorUserID
	if actor == "" {
		actor = actorFromContext(ctx)
	}

	perCategory := make(map[uuid.UUID]int, len(removed))
	order := make([]uuid.UUID, 0, len(removed))
	for _, r := range removed {
		if _, ok := perCategory[r.CategoryID]; !ok {
			order = append(order, r.CategoryID)
		}
		perCategory[r.CategoryID]++

		attrs := categoryRuleAttrs(r.CategoryID, r.Rule)
		attrs["reason"] = purgeReasonUserDeleted
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier:        audit.TierAudit,
			Action:      "category_rule.deleted",
			ActorUserID: actor,
			Subject:     "category:" + r.CategoryID.String(),
			GroupID:     r.CategoryID.String(),
			Attributes:  attrs,
		})
	}

	for _, catID := range order {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier:        audit.TierAudit,
			Action:      "category.ruleset_changed",
			ActorUserID: actor,
			Subject:     "category:" + catID.String(),
			GroupID:     catID.String(),
			Attributes: map[string]string{
				"category_id": catID.String(),
				"user_id":     userID.String(),
				"count":       strconv.Itoa(perCategory[catID]),
				"reason":      purgeReasonUserDeleted,
			},
		})
	}
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/errcodes"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// WithMoveTypeGate sets the check the bulk move uses to ask whether a category holds a document
// type. nil, the default, means every category holds both.
func (h *PolicyHandler) WithMoveTypeGate(accepts func(domain.Category, domain.DocumentType) bool) *PolicyHandler {
	h.moveTypeGate = accepts
	return h
}

// movePlan is what a bulk move would do, one entry per requested id, in order.
type movePlan struct {
	target domain.Category
	docs   []domain.Policy
	items  []*corev1.MovePoliciesItem
	// refusal is the first document's refusal, nil when every document can move.
	refusal *domain.MoveRefusal
	// refusedNumber is the number of the document that carries refusal.
	refusedNumber string
	// moving[i] is true when document i changes category.
	moving []bool
}

// planMove reads the target and every document and works out, for each, its refusal or the number
// it would get. It changes nothing. Ids and the target are validated here, so both RPCs refuse
// the same way.
func (h *PolicyHandler) planMove(ctx context.Context, policyIDs []string, homeCategoryID string) (movePlan, error) {
	var plan movePlan
	dst, err := uuid.Parse(homeCategoryID)
	if err != nil {
		return plan, status.Errorf(codes.InvalidArgument, "invalid home_category_id: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(policyIDs))
	for _, raw := range policyIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return plan, status.Errorf(codes.InvalidArgument, "invalid policy_id %q: %v", raw, err)
		}
		ids = append(ids, id)
	}
	plan.target, err = h.categoryStore.Get(ctx, dst)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return plan, status.Error(codes.NotFound, "target category not found")
		}
		logger.Ctx(ctx).Error(err, "bulk move: get target category failed", log.F("category_id", dst.String()))
		return plan, status.Error(codes.Internal, "get target category failed")
	}

	var renumberTypes []domain.DocumentType
	renumberAt := map[uuid.UUID]int{} // document id -> index into renumberTypes
	slot := make([]int, len(ids))     // item index -> index into renumberTypes, or -1
	for i, id := range ids {
		p, err := h.store.GetPolicy(ctx, id)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return plan, status.Error(codes.NotFound, "policy not found")
			}
			return plan, storeUnavailable(ctx, "get_policy", err)
		}
		item := &corev1.MovePoliciesItem{PolicyId: p.ID.String(), Title: p.Title, Number: p.Number}
		slot[i] = -1
		moving := false
		if r := domain.RefuseMove(p, plan.target, h.moveTypeGate); r != nil {
			item.RefusalCode, item.Refusal = r.Code, r.Message
			if plan.refusal == nil {
				plan.refusal, plan.refusedNumber = r, p.Number
			}
			logger.Ctx(ctx).Debug("bulk move: document refused",
				log.F("policy_id", p.ID.String()), log.F("code", r.Code), log.F("category_id", dst.String()))
		} else if p.HomeCategoryID == dst {
			item.NewNumber = p.Number
			logger.Ctx(ctx).Debug("bulk move: document already in the target", log.F("policy_id", p.ID.String()))
		} else {
			moving = true
			// A document listed twice takes one number, as it moves once.
			at, seen := renumberAt[p.ID]
			if !seen {
				at = len(renumberTypes)
				renumberAt[p.ID] = at
				dt := p.DocumentType
				if dt == "" {
					dt = domain.DocumentTypePolicy
				}
				renumberTypes = append(renumberTypes, dt)
			}
			slot[i] = at
		}
		plan.docs = append(plan.docs, p)
		plan.items = append(plan.items, item)
		plan.moving = append(plan.moving, moving)
	}

	if len(renumberTypes) > 0 {
		numbers, err := h.store.PreviewMoveNumbers(ctx, dst, renumberTypes)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return plan, status.Error(codes.NotFound, "target category not found")
			}
			return plan, storeUnavailable(ctx, "preview_move_numbers", err)
		}
		for i, at := range slot {
			if at >= 0 {
				plan.items[i].NewNumber = numbers[at]
			}
		}
	}
	logger.Ctx(ctx).Debug("bulk move: planned",
		log.F("category_id", dst.String()), log.F("documents", len(ids)), log.F("renumbered", len(renumberTypes)),
		log.F("refused", plan.refusal != nil))
	return plan, nil
}

// PreviewMovePolicies handles PolicyService.PreviewMovePolicies: the dry run of MovePolicies. A
// document that cannot move comes back with a refusal and no new number; nothing is changed.
func (h *PolicyHandler) PreviewMovePolicies(ctx context.Context, req *corev1.PreviewMovePoliciesRequest) (*corev1.PreviewMovePoliciesResponse, error) {
	logger.Ctx(ctx).Debug("PreviewMovePolicies: start",
		log.F("documents", len(req.GetPolicyIds())), log.F("category_id", req.GetHomeCategoryId()))
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	plan, err := h.planMove(ctx, req.GetPolicyIds(), req.GetHomeCategoryId())
	if err != nil {
		return nil, err
	}
	logger.Ctx(ctx).Debug("PreviewMovePolicies: done", log.F("items", len(plan.items)))
	return &corev1.PreviewMovePoliciesResponse{Items: plan.items}, nil
}

// MovePolicies handles PolicyService.MovePolicies. It is all or nothing: if any document would be
// refused the whole batch is refused with that document's reason and nothing moves. Otherwise the
// documents move in order in one transaction, each renumbered from the target's sequence, with a
// policy.moved entry for each document that changed category and one policy.bulk_moved for the
// batch. A document already in the target stays as it is.
func (h *PolicyHandler) MovePolicies(ctx context.Context, req *corev1.MovePoliciesRequest) (*corev1.MovePoliciesResponse, error) {
	logger.Ctx(ctx).Info("MovePolicies: start",
		log.F("documents", len(req.GetPolicyIds())), log.F("category_id", req.GetHomeCategoryId()))
	if _, err := parseOptionalUUID(req.GetActorUserId()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid actor_user_id: %v", err)
	}
	if len(req.GetPolicyIds()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "pick at least one document to move")
	}
	plan, err := h.planMove(ctx, req.GetPolicyIds(), req.GetHomeCategoryId())
	if err != nil {
		return nil, err
	}
	if plan.refusal != nil {
		logger.Ctx(ctx).Info("MovePolicies: batch refused, nothing moved",
			log.F("code", plan.refusal.Code), log.F("category_id", plan.target.ID.String()))
		return nil, errcodes.Error(ctx, errcodes.MoveRefused(plan.refusedNumber, plan.refusal.Code, plan.refusal.Message))
	}

	ids := make([]uuid.UUID, 0, len(plan.docs))
	for _, d := range plan.docs {
		ids = append(ids, d.ID)
	}
	after, err := h.store.MovePolicies(ctx, plan.target.ID, ids)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrPolicyRetired):
			// Retired between the plan and the move: the transaction rolled back.
			logger.Ctx(ctx).Warn("MovePolicies: a document was retired during the move, nothing moved",
				log.F("category_id", plan.target.ID.String()))
			return nil, errcodes.Error(ctx, errcodes.MoveRefused("", domain.MoveRefusalRetired,
				"RETIRED: a retired document cannot be moved."))
		case errors.Is(err, pgx.ErrNoRows):
			return nil, status.Error(codes.NotFound, "policy not found")
		}
		logger.Ctx(ctx).Error(err, "MovePolicies: store move failed", log.F("category_id", plan.target.ID.String()))
		return nil, storeUnavailable(ctx, "move_policies", err)
	}

	actor := req.GetActorUserId()
	moved := make([]*corev1.MovePoliciesItem, 0, len(after))
	audited := map[uuid.UUID]bool{}
	var fromGroups []string
	count := 0
	for i, p := range after {
		moved = append(moved, &corev1.MovePoliciesItem{
			PolicyId: p.ID.String(), Title: p.Title, Number: plan.docs[i].Number, NewNumber: p.Number,
		})
		if !plan.moving[i] || audited[p.ID] {
			continue
		}
		audited[p.ID] = true
		count++
		from := plan.docs[i].HomeCategoryID.String()
		if !slices.Contains(fromGroups, from) {
			fromGroups = append(fromGroups, from)
		}
		if h.auditor != nil {
			_ = h.auditor.Emit(ctx, audit.Event{
				Tier: audit.TierAudit, Action: "policy.moved",
				ActorUserID: actor,
				Subject:     "policy:" + p.ID.String(),
				GroupID:     p.HomeCategoryID.String(),
				Attributes: map[string]string{
					"from_group": from,
					"to_group":   p.HomeCategoryID.String(),
					"old_number": plan.docs[i].Number,
					"new_number": p.Number,
				},
			})
		}
		// A new home category means new inherited governance, which can change the ack audience.
		h.emitObligationChanged(ctx, p.ID.String())
		logger.Ctx(ctx).Debug("MovePolicies: document moved",
			log.F("policy_id", p.ID.String()), log.F("from_group", from), log.F("to_group", p.HomeCategoryID.String()))
	}
	slices.Sort(fromGroups)
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.bulk_moved",
			ActorUserID: actor,
			Subject:     "category:" + plan.target.ID.String(),
			GroupID:     plan.target.ID.String(),
			Attributes: map[string]string{
				"count":       strconv.Itoa(count),
				"to_group":    plan.target.ID.String(),
				"from_groups": strings.Join(fromGroups, ","),
			},
		})
	}
	logger.Ctx(ctx).Info("MovePolicies: done",
		log.F("category_id", plan.target.ID.String()), log.F("requested", len(ids)), log.F("moved", count))
	return &corev1.MovePoliciesResponse{Moved: moved}, nil
}

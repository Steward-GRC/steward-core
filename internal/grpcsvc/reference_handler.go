// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// referenceBackend is the part of *store.ReferenceStore the handler uses.
type referenceBackend interface {
	ListReferences(ctx context.Context, includeArchived bool) ([]domain.Reference, error)
	CreateReference(ctx context.Context, r domain.Reference) (domain.Reference, error)
	UpdateReference(ctx context.Context, id uuid.UUID, r domain.Reference) (domain.Reference, error)
	DeleteReference(ctx context.Context, id uuid.UUID) error
	SetReferenceArchived(ctx context.Context, id uuid.UUID, archived bool) (domain.Reference, error)
	ListPolicyReferences(ctx context.Context, policyID uuid.UUID) ([]domain.Reference, error)
	SetPolicyReferences(ctx context.Context, policyID uuid.UUID, referenceIDs []uuid.UUID) ([]domain.Reference, error)
}

// ReferenceHandler implements corev1.ReferenceServiceServer.
type ReferenceHandler struct {
	corev1.UnimplementedReferenceServiceServer
	store   referenceBackend
	auditor auditEmitter
}

// NewReferenceHandler returns a handler over s. auditor may be nil.
func NewReferenceHandler(s referenceBackend, auditor auditEmitter) *ReferenceHandler {
	return &ReferenceHandler{store: s, auditor: auditor}
}

func refKindFromProto(k corev1.ReferenceKind) domain.ReferenceKind {
	switch k {
	case corev1.ReferenceKind_REFERENCE_KIND_STANDARD:
		return domain.ReferenceKindStandard
	case corev1.ReferenceKind_REFERENCE_KIND_TEXT:
		return domain.ReferenceKindText
	case corev1.ReferenceKind_REFERENCE_KIND_LINK:
		return domain.ReferenceKindLink
	default:
		return ""
	}
}

func refKindToProto(k domain.ReferenceKind) corev1.ReferenceKind {
	switch k {
	case domain.ReferenceKindStandard:
		return corev1.ReferenceKind_REFERENCE_KIND_STANDARD
	case domain.ReferenceKindText:
		return corev1.ReferenceKind_REFERENCE_KIND_TEXT
	case domain.ReferenceKindLink:
		return corev1.ReferenceKind_REFERENCE_KIND_LINK
	default:
		return corev1.ReferenceKind_REFERENCE_KIND_UNSPECIFIED
	}
}

func referenceToProto(r domain.Reference) *corev1.Reference {
	createdAt := ""
	if !r.CreatedAt.IsZero() {
		createdAt = r.CreatedAt.UTC().Format(time.RFC3339)
	}
	return &corev1.Reference{
		Id: r.ID.String(), Label: r.Label, Kind: refKindToProto(r.Kind),
		Clause: r.Clause, Body: r.Body, Url: r.URL, Archived: r.Archived,
		CreatedByUserId: r.CreatedByUserID, CreatedAt: createdAt,
		UsedByCount: toInt32(r.UsedByCount),
	}
}

func referenceFromInput(in *corev1.ReferenceInput) domain.Reference {
	return domain.Reference{
		Label: in.GetLabel(), Kind: refKindFromProto(in.GetKind()),
		Clause: in.GetClause(), Body: in.GetBody(), URL: in.GetUrl(),
	}
}

func (h *ReferenceHandler) ListReferences(ctx context.Context, req *corev1.ListReferencesRequest) (*corev1.ListReferencesResponse, error) {
	list, err := h.store.ListReferences(ctx, req.GetIncludeArchived())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list references: %v", err)
	}
	out := make([]*corev1.Reference, len(list))
	for i, r := range list {
		out[i] = referenceToProto(r)
	}
	return &corev1.ListReferencesResponse{References: out}, nil
}

func (h *ReferenceHandler) CreateReference(ctx context.Context, req *corev1.CreateReferenceRequest) (*corev1.CreateReferenceResponse, error) {
	in := req.GetInput()
	r := referenceFromInput(in)
	if err := domain.ValidateReference(r.Label, r.Kind, r.Body, r.URL); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	r.CreatedByUserID = req.GetActorUserId()
	saved, err := h.store.CreateReference(ctx, r)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create reference: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "reference.created",
			Subject:     "reference:" + saved.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.CreateReferenceResponse{Reference: referenceToProto(saved)}, nil
}

func (h *ReferenceHandler) UpdateReference(ctx context.Context, req *corev1.UpdateReferenceRequest) (*corev1.UpdateReferenceResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	r := referenceFromInput(req.GetInput())
	if err := domain.ValidateReference(r.Label, r.Kind, r.Body, r.URL); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	saved, err := h.store.UpdateReference(ctx, id, r)
	if err != nil {
		if errors.Is(err, store.ErrReferenceNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "update reference: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "reference.updated",
			Subject:     "reference:" + saved.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.UpdateReferenceResponse{Reference: referenceToProto(saved)}, nil
}

func (h *ReferenceHandler) DeleteReference(ctx context.Context, req *corev1.DeleteReferenceRequest) (*corev1.DeleteReferenceResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if err := h.store.DeleteReference(ctx, id); err != nil {
		switch {
		case errors.Is(err, store.ErrReferenceNotFound):
			return nil, status.Errorf(codes.NotFound, "%v", err)
		case errors.Is(err, store.ErrReferenceInUse):
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		default:
			return nil, status.Errorf(codes.Internal, "delete reference: %v", err)
		}
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "reference.deleted",
			Subject:     "reference:" + id.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.DeleteReferenceResponse{}, nil
}

// SetReferenceArchived archives (true) or restores (false) a library entry.
func (h *ReferenceHandler) SetReferenceArchived(ctx context.Context, req *corev1.SetReferenceArchivedRequest) (*corev1.SetReferenceArchivedResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	r, err := h.store.SetReferenceArchived(ctx, id, req.GetArchived())
	if err != nil {
		if errors.Is(err, store.ErrReferenceNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "set reference archived: %v", err)
	}
	if h.auditor != nil {
		archived := "false"
		if req.GetArchived() {
			archived = "true"
		}
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "reference.archived",
			Subject:     "reference:" + r.ID.String(),
			ActorUserID: req.GetActorUserId(),
			Attributes:  map[string]string{"archived": archived},
		})
	}
	return &corev1.SetReferenceArchivedResponse{Reference: referenceToProto(r)}, nil
}

func (h *ReferenceHandler) ListPolicyReferences(ctx context.Context, req *corev1.ListPolicyReferencesRequest) (*corev1.ListPolicyReferencesResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	list, err := h.store.ListPolicyReferences(ctx, pid)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list policy references: %v", err)
	}
	out := make([]*corev1.Reference, len(list))
	for i, r := range list {
		out[i] = referenceToProto(r)
	}
	return &corev1.ListPolicyReferencesResponse{References: out}, nil
}

func (h *ReferenceHandler) SetPolicyReferences(ctx context.Context, req *corev1.SetPolicyReferencesRequest) (*corev1.SetPolicyReferencesResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(req.GetReferenceIds()))
	for i, s := range req.GetReferenceIds() {
		rid, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "reference_ids[%d]: %v", i, err)
		}
		ids = append(ids, rid)
	}
	saved, err := h.store.SetPolicyReferences(ctx, pid, ids)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set policy references: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.references_changed",
			Subject:     "policy:" + pid.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	out := make([]*corev1.Reference, len(saved))
	for i, r := range saved {
		out[i] = referenceToProto(r)
	}
	return &corev1.SetPolicyReferencesResponse{References: out}, nil
}

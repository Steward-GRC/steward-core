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

// definitionLibraryBackend is the part of *store.DefinitionLibraryStore the handler uses.
type definitionLibraryBackend interface {
	ListDefinitionEntries(ctx context.Context, categoryID *uuid.UUID, includeArchived bool) ([]domain.DefinitionEntry, error)
	ListPolicyDefinitionCandidates(ctx context.Context, policyID uuid.UUID, includeArchived bool) ([]domain.DefinitionEntry, error)
	CreateDefinitionEntry(ctx context.Context, d domain.DefinitionEntry) (domain.DefinitionEntry, error)
	UpdateDefinitionEntry(ctx context.Context, id uuid.UUID, d domain.DefinitionEntry) (domain.DefinitionEntry, error)
	DeleteDefinitionEntry(ctx context.Context, id uuid.UUID) error
	SetDefinitionEntryArchived(ctx context.Context, id uuid.UUID, archived bool) (domain.DefinitionEntry, error)
	ListPolicyDefinitionEntries(ctx context.Context, policyID uuid.UUID) ([]domain.DefinitionEntry, error)
	SetPolicyDefinitionEntries(ctx context.Context, policyID uuid.UUID, definitionIDs []uuid.UUID) ([]domain.DefinitionEntry, error)
}

// DefinitionLibraryHandler implements corev1.DefinitionLibraryServiceServer.
type DefinitionLibraryHandler struct {
	corev1.UnimplementedDefinitionLibraryServiceServer
	store   definitionLibraryBackend
	auditor auditEmitter
}

// NewDefinitionLibraryHandler returns a handler over s. auditor may be nil.
func NewDefinitionLibraryHandler(s definitionLibraryBackend, auditor auditEmitter) *DefinitionLibraryHandler {
	return &DefinitionLibraryHandler{store: s, auditor: auditor}
}

func definitionEntryToProto(d domain.DefinitionEntry) *corev1.DefinitionEntry {
	createdAt := ""
	if !d.CreatedAt.IsZero() {
		createdAt = d.CreatedAt.UTC().Format(time.RFC3339)
	}
	return &corev1.DefinitionEntry{
		Id: d.ID.String(), CategoryId: d.CategoryID.String(), Term: d.Term,
		Definition: d.Definition, Archived: d.Archived,
		CreatedByUserId: d.CreatedByUserID, CreatedAt: createdAt,
		UsedByCount: toInt32(d.UsedByCount),
	}
}

func definitionEntriesToProto(list []domain.DefinitionEntry) []*corev1.DefinitionEntry {
	out := make([]*corev1.DefinitionEntry, len(list))
	for i, d := range list {
		out[i] = definitionEntryToProto(d)
	}
	return out
}

func (h *DefinitionLibraryHandler) ListDefinitionEntries(ctx context.Context, req *corev1.ListDefinitionEntriesRequest) (*corev1.ListDefinitionEntriesResponse, error) {
	var categoryID *uuid.UUID
	if s := req.GetCategoryId(); s != "" {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "category_id: %v", err)
		}
		categoryID = &id
	}
	list, err := h.store.ListDefinitionEntries(ctx, categoryID, req.GetIncludeArchived())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list definitions: %v", err)
	}
	return &corev1.ListDefinitionEntriesResponse{Definitions: definitionEntriesToProto(list)}, nil
}

func (h *DefinitionLibraryHandler) ListPolicyDefinitionCandidates(ctx context.Context, req *corev1.ListPolicyDefinitionCandidatesRequest) (*corev1.ListPolicyDefinitionCandidatesResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	list, err := h.store.ListPolicyDefinitionCandidates(ctx, pid, req.GetIncludeArchived())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list definition candidates: %v", err)
	}
	return &corev1.ListPolicyDefinitionCandidatesResponse{Definitions: definitionEntriesToProto(list)}, nil
}

func (h *DefinitionLibraryHandler) CreateDefinitionEntry(ctx context.Context, req *corev1.CreateDefinitionEntryRequest) (*corev1.CreateDefinitionEntryResponse, error) {
	in := req.GetInput()
	if err := domain.ValidateDefinitionEntry(in.GetTerm(), in.GetDefinition()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	categoryID, err := uuid.Parse(in.GetCategoryId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "category_id: %v", err)
	}
	d := domain.DefinitionEntry{
		CategoryID: categoryID, Term: in.GetTerm(), Definition: in.GetDefinition(),
		CreatedByUserID: req.GetActorUserId(),
	}
	saved, err := h.store.CreateDefinitionEntry(ctx, d)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create definition: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "definition.created",
			Subject:     "definition:" + saved.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.CreateDefinitionEntryResponse{Definition: definitionEntryToProto(saved)}, nil
}

func (h *DefinitionLibraryHandler) UpdateDefinitionEntry(ctx context.Context, req *corev1.UpdateDefinitionEntryRequest) (*corev1.UpdateDefinitionEntryResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	in := req.GetInput()
	if err := domain.ValidateDefinitionEntry(in.GetTerm(), in.GetDefinition()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	saved, err := h.store.UpdateDefinitionEntry(ctx, id, domain.DefinitionEntry{Term: in.GetTerm(), Definition: in.GetDefinition()})
	if err != nil {
		if errors.Is(err, store.ErrDefinitionEntryNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "update definition: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "definition.updated",
			Subject:     "definition:" + saved.ID.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.UpdateDefinitionEntryResponse{Definition: definitionEntryToProto(saved)}, nil
}

func (h *DefinitionLibraryHandler) DeleteDefinitionEntry(ctx context.Context, req *corev1.DeleteDefinitionEntryRequest) (*corev1.DeleteDefinitionEntryResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if err := h.store.DeleteDefinitionEntry(ctx, id); err != nil {
		switch {
		case errors.Is(err, store.ErrDefinitionEntryNotFound):
			return nil, status.Errorf(codes.NotFound, "%v", err)
		case errors.Is(err, store.ErrDefinitionEntryInUse):
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		default:
			return nil, status.Errorf(codes.Internal, "delete definition: %v", err)
		}
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "definition.deleted",
			Subject:     "definition:" + id.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.DeleteDefinitionEntryResponse{}, nil
}

// SetDefinitionEntryArchived archives (true) or restores (false) a library entry.
func (h *DefinitionLibraryHandler) SetDefinitionEntryArchived(ctx context.Context, req *corev1.SetDefinitionEntryArchivedRequest) (*corev1.SetDefinitionEntryArchivedResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	d, err := h.store.SetDefinitionEntryArchived(ctx, id, req.GetArchived())
	if err != nil {
		if errors.Is(err, store.ErrDefinitionEntryNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "set definition archived: %v", err)
	}
	if h.auditor != nil {
		archived := "false"
		if req.GetArchived() {
			archived = "true"
		}
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "definition.archived",
			Subject:     "definition:" + d.ID.String(),
			ActorUserID: req.GetActorUserId(),
			Attributes:  map[string]string{"archived": archived},
		})
	}
	return &corev1.SetDefinitionEntryArchivedResponse{Definition: definitionEntryToProto(d)}, nil
}

func (h *DefinitionLibraryHandler) ListPolicyDefinitionEntries(ctx context.Context, req *corev1.ListPolicyDefinitionEntriesRequest) (*corev1.ListPolicyDefinitionEntriesResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	list, err := h.store.ListPolicyDefinitionEntries(ctx, pid)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list policy definitions: %v", err)
	}
	return &corev1.ListPolicyDefinitionEntriesResponse{Definitions: definitionEntriesToProto(list)}, nil
}

func (h *DefinitionLibraryHandler) SetPolicyDefinitionEntries(ctx context.Context, req *corev1.SetPolicyDefinitionEntriesRequest) (*corev1.SetPolicyDefinitionEntriesResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(req.GetDefinitionIds()))
	for i, s := range req.GetDefinitionIds() {
		did, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "definition_ids[%d]: %v", i, err)
		}
		ids = append(ids, did)
	}
	saved, err := h.store.SetPolicyDefinitionEntries(ctx, pid, ids)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set policy definitions: %v", err)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "policy.definitions_changed",
			Subject:     "policy:" + pid.String(),
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.SetPolicyDefinitionEntriesResponse{Definitions: definitionEntriesToProto(saved)}, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// contactBackend is the part of *store.ContactStore the handler uses.
type contactBackend interface {
	ListBlocks(ctx context.Context, includeArchived bool) ([]domain.ContactBlock, error)
	CreateBlock(ctx context.Context, b domain.ContactBlock) (domain.ContactBlock, error)
	UpdateBlock(ctx context.Context, id uuid.UUID, b domain.ContactBlock) (domain.ContactBlock, error)
	DeleteBlock(ctx context.Context, id uuid.UUID) error
	SetArchived(ctx context.Context, id uuid.UUID, archived bool) (domain.ContactBlock, error)
	ListByPolicy(ctx context.Context, policyID uuid.UUID) ([]domain.ContactBlock, error)
	SetForPolicy(ctx context.Context, policyID uuid.UUID, blockIDs []uuid.UUID) ([]domain.ContactBlock, error)
}

// ContactHandler implements corev1.ContactServiceServer.
type ContactHandler struct {
	corev1.UnimplementedContactServiceServer
	store contactBackend
}

// NewContactHandler returns a handler over the given backend.
func NewContactHandler(s contactBackend) *ContactHandler { return &ContactHandler{store: s} }

func contactToProto(b domain.ContactBlock) *corev1.ContactBlock {
	return &corev1.ContactBlock{
		Id: b.ID.String(), Label: b.Label, Name: b.Name, Role: b.Role, Department: b.Department,
		Email: b.Email, Phone: b.Phone, Hours: b.Hours, Notes: b.Notes,
		Archived: b.Archived, UsedByCount: toInt32(b.UsedByCount),
	}
}

func contactFromInput(in *corev1.ContactBlockInput) domain.ContactBlock {
	return domain.ContactBlock{
		Label: in.GetLabel(), Name: in.GetName(), Role: in.GetRole(), Department: in.GetDepartment(),
		Email: in.GetEmail(), Phone: in.GetPhone(), Hours: in.GetHours(), Notes: in.GetNotes(),
	}
}

func (h *ContactHandler) ListContactBlocks(ctx context.Context, req *corev1.ListContactBlocksRequest) (*corev1.ListContactBlocksResponse, error) {
	list, err := h.store.ListBlocks(ctx, req.GetIncludeArchived())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list contact blocks: %v", err)
	}
	out := make([]*corev1.ContactBlock, len(list))
	for i, b := range list {
		out[i] = contactToProto(b)
	}
	return &corev1.ListContactBlocksResponse{Blocks: out}, nil
}

func (h *ContactHandler) CreateContactBlock(ctx context.Context, req *corev1.CreateContactBlockRequest) (*corev1.CreateContactBlockResponse, error) {
	in := req.GetBlock()
	if err := domain.ValidateContactBlock(in.GetLabel()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	saved, err := h.store.CreateBlock(ctx, contactFromInput(in))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create contact block: %v", err)
	}
	return &corev1.CreateContactBlockResponse{Block: contactToProto(saved)}, nil
}

func (h *ContactHandler) UpdateContactBlock(ctx context.Context, req *corev1.UpdateContactBlockRequest) (*corev1.UpdateContactBlockResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	in := req.GetBlock()
	if err := domain.ValidateContactBlock(in.GetLabel()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	saved, err := h.store.UpdateBlock(ctx, id, contactFromInput(in))
	if err != nil {
		if errors.Is(err, store.ErrContactBlockNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "update contact block: %v", err)
	}
	return &corev1.UpdateContactBlockResponse{Block: contactToProto(saved)}, nil
}

func (h *ContactHandler) DeleteContactBlock(ctx context.Context, req *corev1.DeleteContactBlockRequest) (*corev1.DeleteContactBlockResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if err := h.store.DeleteBlock(ctx, id); err != nil {
		if errors.Is(err, store.ErrContactBlockNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "delete contact block: %v", err)
	}
	return &corev1.DeleteContactBlockResponse{}, nil
}

// SetContactBlockArchived archives (true) or restores (false) a library block.
func (h *ContactHandler) SetContactBlockArchived(ctx context.Context, req *corev1.SetContactBlockArchivedRequest) (*corev1.SetContactBlockArchivedResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	b, err := h.store.SetArchived(ctx, id, req.GetArchived())
	if err != nil {
		if errors.Is(err, store.ErrContactBlockNotFound) {
			return nil, status.Errorf(codes.NotFound, "%v", err)
		}
		return nil, status.Errorf(codes.Internal, "set contact block archived: %v", err)
	}
	return &corev1.SetContactBlockArchivedResponse{Block: contactToProto(b)}, nil
}

func (h *ContactHandler) ListPolicyContactBlocks(ctx context.Context, req *corev1.ListPolicyContactBlocksRequest) (*corev1.ListPolicyContactBlocksResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	list, err := h.store.ListByPolicy(ctx, pid)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list policy contact blocks: %v", err)
	}
	out := make([]*corev1.ContactBlock, len(list))
	for i, b := range list {
		out[i] = contactToProto(b)
	}
	return &corev1.ListPolicyContactBlocksResponse{Blocks: out}, nil
}

func (h *ContactHandler) SetPolicyContactBlocks(ctx context.Context, req *corev1.SetPolicyContactBlocksRequest) (*corev1.SetPolicyContactBlocksResponse, error) {
	pid, err := uuid.Parse(req.GetPolicyId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_id: %v", err)
	}
	ids := make([]uuid.UUID, 0, len(req.GetContactBlockIds()))
	for i, s := range req.GetContactBlockIds() {
		bid, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "contact_block_ids[%d]: %v", i, err)
		}
		ids = append(ids, bid)
	}
	saved, err := h.store.SetForPolicy(ctx, pid, ids)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "set policy contact blocks: %v", err)
	}
	out := make([]*corev1.ContactBlock, len(saved))
	for i, b := range saved {
		out[i] = contactToProto(b)
	}
	return &corev1.SetPolicyContactBlocksResponse{Blocks: out}, nil
}

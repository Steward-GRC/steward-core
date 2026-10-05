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

// appendixBackend is the part of *store.AppendixStore the handler uses.
type appendixBackend interface {
	ListByVersion(ctx context.Context, versionID uuid.UUID) ([]domain.Appendix, error)
	Add(ctx context.Context, a domain.Appendix) (domain.Appendix, error)
	Update(ctx context.Context, id uuid.UUID, title, contentJSON string) (domain.Appendix, error)
	Delete(ctx context.Context, id uuid.UUID) error
	Reorder(ctx context.Context, versionID uuid.UUID, orderedIDs []uuid.UUID) ([]domain.Appendix, error)
}

// AppendixHandler implements corev1.AppendixServiceServer.
type AppendixHandler struct {
	corev1.UnimplementedAppendixServiceServer
	store appendixBackend
}

// NewAppendixHandler returns a handler over the given backend.
func NewAppendixHandler(s appendixBackend) *AppendixHandler { return &AppendixHandler{store: s} }

func appendixToProto(a domain.Appendix) *corev1.Appendix {
	return &corev1.Appendix{
		Id:              a.ID.String(),
		PolicyVersionId: a.PolicyVersionID.String(),
		Title:           a.Title,
		ContentJson:     a.ContentJSON,
		OrderIndex:      toInt32(a.OrderIndex),
	}
}

// mapWriteErr maps store write errors to gRPC codes.
func mapWriteErr(err error) error {
	if errors.Is(err, store.ErrVersionNotDraft) {
		return status.Error(codes.FailedPrecondition, "published versions are immutable")
	}
	return status.Errorf(codes.Internal, "%v", err)
}

func (h *AppendixHandler) ListAppendices(ctx context.Context, req *corev1.ListAppendicesRequest) (*corev1.ListAppendicesResponse, error) {
	vid, err := uuid.Parse(req.GetPolicyVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_version_id: %v", err)
	}
	list, err := h.store.ListByVersion(ctx, vid)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list appendices: %v", err)
	}
	out := make([]*corev1.Appendix, len(list))
	for i, a := range list {
		out[i] = appendixToProto(a)
	}
	return &corev1.ListAppendicesResponse{Appendices: out}, nil
}

func (h *AppendixHandler) AddAppendix(ctx context.Context, req *corev1.AddAppendixRequest) (*corev1.AddAppendixResponse, error) {
	vid, err := uuid.Parse(req.GetPolicyVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_version_id: %v", err)
	}
	a, err := domain.NewAppendix(vid, req.GetTitle(), req.GetContentJson())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	saved, err := h.store.Add(ctx, a)
	if err != nil {
		return nil, mapWriteErr(err)
	}
	return &corev1.AddAppendixResponse{Appendix: appendixToProto(saved)}, nil
}

func (h *AppendixHandler) UpdateAppendix(ctx context.Context, req *corev1.UpdateAppendixRequest) (*corev1.UpdateAppendixResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if req.GetTitle() == "" {
		return nil, status.Error(codes.InvalidArgument, "title must not be empty")
	}
	saved, err := h.store.Update(ctx, id, req.GetTitle(), req.GetContentJson())
	if err != nil {
		return nil, mapWriteErr(err)
	}
	return &corev1.UpdateAppendixResponse{Appendix: appendixToProto(saved)}, nil
}

func (h *AppendixHandler) ReorderAppendices(ctx context.Context, req *corev1.ReorderAppendicesRequest) (*corev1.ReorderAppendicesResponse, error) {
	vid, err := uuid.Parse(req.GetPolicyVersionId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "policy_version_id: %v", err)
	}
	ids := make([]uuid.UUID, len(req.GetOrderedIds()))
	for i, s := range req.GetOrderedIds() {
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "ordered_ids[%d]: %v", i, err)
		}
		ids[i] = id
	}
	list, err := h.store.Reorder(ctx, vid, ids)
	if err != nil {
		return nil, mapWriteErr(err)
	}
	out := make([]*corev1.Appendix, len(list))
	for i, a := range list {
		out[i] = appendixToProto(a)
	}
	return &corev1.ReorderAppendicesResponse{Appendices: out}, nil
}

func (h *AppendixHandler) DeleteAppendix(ctx context.Context, req *corev1.DeleteAppendixRequest) (*corev1.DeleteAppendixResponse, error) {
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "id: %v", err)
	}
	if err := h.store.Delete(ctx, id); err != nil {
		return nil, mapWriteErr(err)
	}
	return &corev1.DeleteAppendixResponse{}, nil
}

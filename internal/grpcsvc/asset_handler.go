// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	objectstore "github.com/Bugs5382/go-objectstore"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// assetBackend is the part of *store.AssetStore the handler uses.
type assetBackend interface {
	Create(ctx context.Context, a domain.Asset) (domain.Asset, error)
	Get(ctx context.Context, id uuid.UUID) (domain.Asset, error)
}

// objectStore is the part of go-objectstore's Store the handler uses. A nil
// store means object storage isn't configured: calls fail Unavailable.
type objectStore interface {
	Put(ctx context.Context, key string, body io.Reader, opts objectstore.PutOptions) (objectstore.Info, error)
	Get(ctx context.Context, key string, opts objectstore.GetOptions) (*objectstore.Object, error)
}

// AssetHandler implements corev1.AssetServiceServer. Access is enforced at
// the gateway; created_by_user_id is recorded as sent, like actor_user_id
// elsewhere.
type AssetHandler struct {
	corev1.UnimplementedAssetServiceServer
	store   assetBackend
	objects objectStore
	auditor auditEmitter
}

// NewAssetHandler returns a handler. objects may be nil when object storage
// isn't configured, and auditor may be nil.
func NewAssetHandler(s assetBackend, objects objectStore, auditor auditEmitter) *AssetHandler {
	return &AssetHandler{store: s, objects: objects, auditor: auditor}
}

func assetToProto(a domain.Asset) *corev1.Asset {
	createdAt := ""
	if !a.CreatedAt.IsZero() {
		createdAt = a.CreatedAt.UTC().Format(time.RFC3339)
	}
	return &corev1.Asset{
		Id:              a.ID.String(),
		Url:             a.URL(),
		ContentType:     a.ContentType,
		SizeBytes:       a.SizeBytes,
		Filename:        a.Filename,
		CreatedByUserId: a.CreatedByUserID,
		CreatedAt:       createdAt,
	}
}

// UploadAsset handles AssetService.UploadAsset. The stored type is sniffed
// from the bytes, never taken from the client.
func (h *AssetHandler) UploadAsset(ctx context.Context, req *corev1.UploadAssetRequest) (*corev1.UploadAssetResponse, error) {
	if h.objects == nil {
		return nil, status.Error(codes.Unavailable, "asset store is not configured")
	}

	actor := req.GetCreatedByUserId()
	if actor == "" {
		return nil, status.Error(codes.InvalidArgument, "created_by_user_id is required")
	}

	data := req.GetData()
	if err := domain.ValidateAssetSize(len(data)); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	contentType, err := domain.DetectAssetContentType(data)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}

	id := uuid.New()
	key := domain.StorageKeyFor(id)

	if _, err := h.objects.Put(ctx, key, bytes.NewReader(data), objectstore.PutOptions{
		ContentType: contentType, Size: int64(len(data)), MaxSize: domain.MaxAssetBytes, IfNotExists: true,
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "store asset object: %v", err)
	}

	saved, err := h.store.Create(ctx, domain.Asset{
		ID:              id,
		StorageKey:      key,
		ContentType:     contentType,
		SizeBytes:       int64(len(data)),
		Filename:        domain.SanitizeFilename(req.GetFilename()),
		CreatedByUserID: actor,
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "record asset metadata: %v", err)
	}

	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "asset.uploaded",
			Subject:     "asset:" + saved.ID.String(),
			ActorUserID: actor,
		})
	}

	return &corev1.UploadAssetResponse{Asset: assetToProto(saved)}, nil
}

// GetAsset handles AssetService.GetAsset.
func (h *AssetHandler) GetAsset(ctx context.Context, req *corev1.GetAssetRequest) (*corev1.GetAssetResponse, error) {
	if h.objects == nil {
		return nil, status.Error(codes.Unavailable, "asset store is not configured")
	}

	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid asset id")
	}

	meta, err := h.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrAssetNotFound) {
			return nil, status.Error(codes.NotFound, "asset not found")
		}
		return nil, status.Errorf(codes.Internal, "get asset metadata: %v", err)
	}

	obj, err := h.objects.Get(ctx, meta.StorageKey, objectstore.GetOptions{})
	if err != nil {
		if errors.Is(err, objectstore.ErrNotFound) {
			return nil, status.Error(codes.NotFound, "asset object not found")
		}
		return nil, status.Errorf(codes.Internal, "read asset object: %v", err)
	}
	defer func() { _ = obj.Body.Close() }()

	data, err := io.ReadAll(io.LimitReader(obj.Body, domain.MaxAssetBytes+1))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read asset bytes: %v", err)
	}

	// The metadata row's type wins; the object's is only a fallback.
	contentType := meta.ContentType
	if contentType == "" {
		contentType = obj.ContentType
	}

	return &corev1.GetAssetResponse{
		Data:        data,
		ContentType: contentType,
		SizeBytes:   int64(len(data)),
		Filename:    meta.Filename,
	}, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	log "github.com/Bugs5382/go-log"
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

// assetSettingsBackend is the part of *store.AssetSettingsStore the handler
// uses: the admin setting for which kinds of file are accepted.
type assetSettingsBackend interface {
	Get(ctx context.Context) (domain.AssetSettings, error)
	Set(ctx context.Context, kinds []domain.AssetKind, actor string) (domain.AssetSettings, error)
}

// assetLinksBackend is the part of *store.AssetStore the admin list and delete
// use: files with the documents that link to them.
type assetLinksBackend interface {
	ListWithLinks(ctx context.Context, unlinkedOnly bool) ([]domain.LinkedAsset, error)
	DeleteWithLinks(ctx context.Context, id uuid.UUID) (domain.LinkedAsset, error)
}

// objectStore is the part of go-objectstore's Store the handler uses. A nil
// store means object storage isn't configured: calls fail Unavailable.
type objectStore interface {
	Put(ctx context.Context, key string, body io.Reader, opts objectstore.PutOptions) (objectstore.Info, error)
	Get(ctx context.Context, key string, opts objectstore.GetOptions) (*objectstore.Object, error)
	Delete(ctx context.Context, key string) error
}

// AssetHandler implements corev1.AssetServiceServer. Access is enforced at
// the gateway; created_by_user_id is recorded as sent, like actor_user_id
// elsewhere.
type AssetHandler struct {
	corev1.UnimplementedAssetServiceServer
	store   assetBackend
	objects objectStore
	auditor auditEmitter
	// settings is the admin setting of allowed kinds. Nil means every kind is
	// allowed and the settings RPCs answer FailedPrecondition.
	settings assetSettingsBackend
	// links serves ListAssets and DeleteAsset; nil answers FailedPrecondition.
	links assetLinksBackend
}

// NewAssetHandler returns a handler. objects may be nil when object storage
// isn't configured, and auditor may be nil.
func NewAssetHandler(s assetBackend, objects objectStore, auditor auditEmitter) *AssetHandler {
	return &AssetHandler{store: s, objects: objects, auditor: auditor}
}

// WithSettings sets the store of the allowed-kinds setting.
func (h *AssetHandler) WithSettings(b assetSettingsBackend) *AssetHandler {
	h.settings = b
	return h
}

// WithLinks sets the backend of the admin list and delete.
func (h *AssetHandler) WithLinks(b assetLinksBackend) *AssetHandler {
	h.links = b
	return h
}

// allowedKinds is the set of kinds the admin setting has on.
func (h *AssetHandler) allowedKinds(ctx context.Context) ([]domain.AssetKind, error) {
	if h.settings == nil {
		return domain.AllAssetKinds(), nil
	}
	s, err := h.settings.Get(ctx)
	if err != nil {
		return nil, err
	}
	return s.AllowedKinds, nil
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
		ServeAs:         a.Class(),
		Kind:            string(domain.KindOfContentType(a.ContentType)),
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

	lg := logger.Ctx(ctx)
	data := req.GetData()
	lg.Debug("upload asset: received", log.F("size_bytes", len(data)))
	if err := domain.ValidateAssetSize(len(data)); err != nil {
		lg.Debug("upload asset: refused on size", log.F("size_bytes", len(data)), log.F("class", refusalClass(err)))
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	allowed, err := h.allowedKinds(ctx)
	if err != nil {
		lg.Error(err, "upload asset: read allowed kinds")
		return nil, status.Errorf(codes.Internal, "read asset settings: %v", err)
	}
	detected, err := domain.DetectAsset(data, req.GetFilename(), req.GetContentType(), allowed)
	if err != nil {
		lg.Info("upload asset: refused", log.F("size_bytes", len(data)), log.F("class", refusalClass(err)))
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	if err := domain.ValidateKindSize(detected.Kind, len(data)); err != nil {
		lg.Info("upload asset: refused on kind cap", log.F("kind", string(detected.Kind)), log.F("size_bytes", len(data)))
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	contentType := detected.ContentType

	id := uuid.New()
	key := domain.StorageKeyFor(id)

	if _, err := h.objects.Put(ctx, key, bytes.NewReader(data), objectstore.PutOptions{
		ContentType: contentType, Size: int64(len(data)), MaxSize: domain.MaxBytesFor(detected.Kind), IfNotExists: true,
	}); err != nil {
		lg.Error(err, "upload asset: store object", log.F("asset_id", id.String()), log.F("size_bytes", len(data)))
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
		lg.Error(err, "upload asset: record metadata", log.F("asset_id", id.String()))
		return nil, status.Errorf(codes.Internal, "record asset metadata: %v", err)
	}
	lg.Info("upload asset: stored", log.F("asset_id", id.String()), log.F("kind", string(detected.Kind)),
		log.F("content_type", contentType), log.F("size_bytes", len(data)))

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

	data, err := io.ReadAll(io.LimitReader(obj.Body, domain.MaxFileBytes+1))
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
		ServeAs:     meta.Class(),
		Kind:        string(domain.KindOfContentType(contentType)),
	}, nil
}

// refusalClass names why an upload was refused without the filename that the
// error text may quote, so it is safe to log.
func refusalClass(err error) string {
	switch {
	case errors.Is(err, domain.ErrAssetEmpty):
		return "empty"
	case errors.Is(err, domain.ErrAssetTooLarge):
		return "too_large"
	case errors.Is(err, domain.ErrAssetMismatch):
		return "mismatch"
	case errors.Is(err, domain.ErrAssetKindDisabled):
		return "kind_off"
	case errors.Is(err, domain.ErrAssetUnsupportedType):
		return "not_allowed"
	}
	return "other"
}

func assetSettingsToProto(s domain.AssetSettings) *corev1.AssetSettings {
	out := &corev1.AssetSettings{
		RefusedExtensions: domain.RefusedAssetExtensions(),
		MaxFileBytes:      domain.MaxFileBytes,
		UpdatedBy:         s.UpdatedBy,
	}
	if !s.UpdatedAt.IsZero() {
		out.UpdatedAt = s.UpdatedAt.UTC().Format(time.RFC3339)
	}
	for _, k := range domain.AssetKinds() {
		out.Kinds = append(out.Kinds, &corev1.AssetKindSetting{
			Kind: string(k.Kind), Label: k.Label, Extensions: k.Extensions,
			Enabled: s.Allows(k.Kind), MaxBytes: domain.MaxBytesFor(k.Kind),
		})
	}
	return out
}

// GetAssetSettings handles AssetService.GetAssetSettings: every kind with its
// switch. Any caller with a session may read it, so the editor can offer only
// the allowed extensions.
func (h *AssetHandler) GetAssetSettings(ctx context.Context, _ *corev1.GetAssetSettingsRequest) (*corev1.GetAssetSettingsResponse, error) {
	if h.settings == nil {
		return nil, status.Error(codes.FailedPrecondition, "asset settings are not configured")
	}
	s, err := h.settings.Get(ctx)
	if err != nil {
		logger.Ctx(ctx).Error(err, "get asset settings")
		return nil, status.Errorf(codes.Internal, "get asset settings: %v", err)
	}
	return &corev1.GetAssetSettingsResponse{Settings: assetSettingsToProto(s)}, nil
}

// SetAssetSettings handles AssetService.SetAssetSettings: replaces the set of
// allowed kinds. Only an admin may call it; the gateway enforces that.
func (h *AssetHandler) SetAssetSettings(ctx context.Context, req *corev1.SetAssetSettingsRequest) (*corev1.SetAssetSettingsResponse, error) {
	if h.settings == nil {
		return nil, status.Error(codes.FailedPrecondition, "asset settings are not configured")
	}
	actor := req.GetActorUserId()
	if actor == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_user_id is required")
	}
	kinds, err := domain.ParseAssetKinds(req.GetAllowedKinds())
	if err != nil {
		logger.Ctx(ctx).Info("set asset settings: refused", log.F("reason", err.Error()))
		return nil, status.Errorf(codes.InvalidArgument, "%v", err)
	}
	saved, err := h.settings.Set(ctx, kinds, actor)
	if err != nil {
		logger.Ctx(ctx).Error(err, "set asset settings")
		return nil, status.Errorf(codes.Internal, "set asset settings: %v", err)
	}
	names := make([]string, len(saved.AllowedKinds))
	for i, k := range saved.AllowedKinds {
		names[i] = string(k)
	}
	logger.Ctx(ctx).Info("set asset settings: updated", log.F("allowed_kinds", strings.Join(names, ",")))
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "asset.settings.updated",
			Subject:     "asset_settings",
			ActorUserID: actor,
			Attributes:  map[string]string{"allowed_kinds": strings.Join(names, ",")},
		})
	}
	return &corev1.SetAssetSettingsResponse{Settings: assetSettingsToProto(saved)}, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	log "github.com/Bugs5382/go-log"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func linksToProto(in []domain.AssetDocLink) []*corev1.AssetDocumentLink {
	out := make([]*corev1.AssetDocumentLink, 0, len(in))
	for _, l := range in {
		out = append(out, &corev1.AssetDocumentLink{
			PolicyId: l.PolicyID.String(), Title: l.Title, Location: l.Location,
		})
	}
	return out
}

// ListAssets handles AssetService.ListAssets: the stored editor files with
// where each is used, optionally only those no document links to. Reporting
// attachments live in another store and are not here. Only an admin may call
// it; the gateway enforces that.
func (h *AssetHandler) ListAssets(ctx context.Context, req *corev1.ListAssetsRequest) (*corev1.ListAssetsResponse, error) {
	if h.links == nil {
		return nil, status.Error(codes.FailedPrecondition, "asset listing is not configured")
	}
	lg := logger.Ctx(ctx)
	lg.Debug("list assets: start", log.F("unlinked_only", req.GetUnlinkedOnly()))
	files, err := h.links.ListWithLinks(ctx, req.GetUnlinkedOnly())
	if err != nil {
		lg.Error(err, "list assets")
		return nil, status.Errorf(codes.Internal, "list assets: %v", err)
	}
	out := &corev1.ListAssetsResponse{Assets: make([]*corev1.StoredAsset, 0, len(files))}
	for _, f := range files {
		out.Assets = append(out.Assets, &corev1.StoredAsset{
			Asset: assetToProto(f.Asset), Linked: f.Linked(), Documents: linksToProto(f.Links),
		})
	}
	lg.Debug("list assets: done", log.F("count", len(out.Assets)), log.F("unlinked_only", req.GetUnlinkedOnly()))
	return out, nil
}

// DeleteAsset handles AssetService.DeleteAsset: removes the bytes and the
// record, and takes the card out of every document that links to the file,
// at once, with no change, approval or version. It works for a linked file.
// The only record is the asset.deleted audit entry: who, when, and the file's
// id, name and kind with the documents it left, never the caller's network
// details. Only an admin may call it; the gateway enforces that.
func (h *AssetHandler) DeleteAsset(ctx context.Context, req *corev1.DeleteAssetRequest) (*corev1.DeleteAssetResponse, error) {
	if h.links == nil || h.objects == nil {
		if h.objects == nil {
			return nil, status.Error(codes.Unavailable, "asset store is not configured")
		}
		return nil, status.Error(codes.FailedPrecondition, "asset deletion is not configured")
	}
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid asset id")
	}
	actor := req.GetActorUserId()
	if actor == "" {
		return nil, status.Error(codes.InvalidArgument, "actor_user_id is required")
	}

	lg := logger.Ctx(ctx)
	lg.Info("delete asset: start", log.F("asset_id", id.String()), log.F("actor_user_id", actor))

	meta, err := h.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrAssetNotFound) {
			lg.Info("delete asset: not found", log.F("asset_id", id.String()))
			return nil, status.Error(codes.NotFound, "asset not found")
		}
		lg.Error(err, "delete asset: read metadata", log.F("asset_id", id.String()))
		return nil, status.Errorf(codes.Internal, "get asset metadata: %v", err)
	}

	// The bytes go first. A failure here changes nothing and can be retried;
	// a failure after it leaves a record with no bytes that a retry finishes
	// (deleting a missing object succeeds).
	if err := h.objects.Delete(ctx, meta.StorageKey); err != nil {
		lg.Error(err, "delete asset: remove object", log.F("asset_id", id.String()))
		return nil, status.Errorf(codes.Internal, "delete asset object: %v", err)
	}
	lg.Debug("delete asset: object removed", log.F("asset_id", id.String()))

	removed, err := h.links.DeleteWithLinks(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrAssetNotFound) {
			return nil, status.Error(codes.NotFound, "asset not found")
		}
		lg.Error(err, "delete asset: remove record and links", log.F("asset_id", id.String()))
		return nil, status.Errorf(codes.Internal, "delete asset record: %v", err)
	}

	kind := string(domain.KindOfContentType(removed.Asset.ContentType))
	docIDs := make([]string, 0, len(removed.Links))
	seen := map[uuid.UUID]bool{}
	for _, l := range removed.Links {
		if !seen[l.PolicyID] {
			seen[l.PolicyID] = true
			docIDs = append(docIDs, l.PolicyID.String())
		}
	}
	lg.Info("delete asset: deleted", log.F("asset_id", id.String()), log.F("kind", kind),
		log.F("documents", len(docIDs)), log.F("actor_user_id", actor))

	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "asset.deleted",
			Subject:     "asset:" + id.String(),
			ActorUserID: actor,
			Attributes: map[string]string{
				"name":      removed.Asset.Filename,
				"kind":      kind,
				"documents": strings.Join(docIDs, ","),
			},
		})
	}

	return &corev1.DeleteAssetResponse{
		Id: id.String(), Filename: removed.Asset.Filename, Kind: kind, RemovedFrom: linksToProto(removed.Links),
	}, nil
}

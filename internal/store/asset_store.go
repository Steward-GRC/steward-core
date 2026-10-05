// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// ErrAssetNotFound is returned when an asset lookup targets a missing id.
// Callers map it to codes.NotFound.
var ErrAssetNotFound = errors.New("asset not found")

// AssetStore persists editor-image metadata. The bytes live in object storage
// under StorageKey.
type AssetStore struct{ db *postgres.DB }

// NewAssetStore returns an AssetStore on db.
func NewAssetStore(db *postgres.DB) *AssetStore { return &AssetStore{db: db} }

const assetCols = `id, storage_key, content_type, size_bytes, filename, created_by_user_id, created_at`

func scanAsset(row pgx.Row) (domain.Asset, error) {
	var a domain.Asset
	if err := row.Scan(&a.ID, &a.StorageKey, &a.ContentType, &a.SizeBytes, &a.Filename, &a.CreatedByUserID, &a.CreatedAt); err != nil {
		return domain.Asset{}, err
	}
	return a, nil
}

// Create inserts an asset row. The caller picks the id, which StorageKey is
// built from, so the object can be written before the row exists.
func (s *AssetStore) Create(ctx context.Context, a domain.Asset) (domain.Asset, error) {
	created, err := scanAsset(s.db.Querier().QueryRow(ctx, `
		INSERT INTO editor_assets (id, storage_key, content_type, size_bytes, filename, created_by_user_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING `+assetCols,
		a.ID, a.StorageKey, a.ContentType, a.SizeBytes, a.Filename, a.CreatedByUserID))
	if err != nil {
		return domain.Asset{}, fmt.Errorf("insert editor_asset: %w", err)
	}
	return created, nil
}

// Get loads an asset metadata row by id, returning ErrAssetNotFound when absent.
func (s *AssetStore) Get(ctx context.Context, id uuid.UUID) (domain.Asset, error) {
	a, err := scanAsset(s.db.Querier().QueryRow(ctx, `SELECT `+assetCols+` FROM editor_assets WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Asset{}, ErrAssetNotFound
		}
		return domain.Asset{}, fmt.Errorf("get editor_asset: %w", err)
	}
	return a, nil
}

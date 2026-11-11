// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// AssetSettingsStore persists the asset_settings singleton: which kinds of
// file the editor's File card accepts.
type AssetSettingsStore struct{ db *postgres.DB }

// NewAssetSettingsStore returns an AssetSettingsStore on db.
func NewAssetSettingsStore(db *postgres.DB) *AssetSettingsStore { return &AssetSettingsStore{db: db} }

func scanAssetSettings(row pgx.Row) (domain.AssetSettings, error) {
	var kinds []string
	var s domain.AssetSettings
	if err := row.Scan(&kinds, &s.UpdatedBy, &s.UpdatedAt); err != nil {
		return domain.AssetSettings{}, err
	}
	// Names the code no longer knows are dropped, so a retired kind can't
	// turn into an error on read.
	parsed, err := domain.ParseAssetKinds(kinds)
	if err != nil {
		parsed = nil
		for _, k := range kinds {
			if one, perr := domain.ParseAssetKinds([]string{k}); perr == nil {
				parsed = append(parsed, one...)
			}
		}
	}
	s.AllowedKinds = parsed
	return s, nil
}

// Get returns the settings, falling back to every kind on when the row is
// absent.
func (s *AssetSettingsStore) Get(ctx context.Context) (domain.AssetSettings, error) {
	got, err := scanAssetSettings(s.db.Querier().QueryRow(ctx,
		`SELECT allowed_kinds, updated_by, updated_at FROM asset_settings WHERE id`))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.DefaultAssetSettings(), nil
		}
		return domain.AssetSettings{}, fmt.Errorf("get asset_settings: %w", err)
	}
	return got, nil
}

// Set replaces the allowed kinds. An empty set turns every kind off.
func (s *AssetSettingsStore) Set(ctx context.Context, kinds []domain.AssetKind, actor string) (domain.AssetSettings, error) {
	names := make([]string, len(kinds))
	for i, k := range kinds {
		names[i] = string(k)
	}
	got, err := scanAssetSettings(s.db.Querier().QueryRow(ctx, `
		INSERT INTO asset_settings (id, allowed_kinds, updated_by, updated_at)
		VALUES (true, $1, $2, now())
		ON CONFLICT (id) DO UPDATE
		   SET allowed_kinds = EXCLUDED.allowed_kinds, updated_by = EXCLUDED.updated_by, updated_at = now()
		RETURNING allowed_kinds, updated_by, updated_at`, names, actor))
	if err != nil {
		return domain.AssetSettings{}, fmt.Errorf("set asset_settings: %w", err)
	}
	return got, nil
}

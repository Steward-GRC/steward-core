// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/jackc/pgx/v5"
)

// GlobalSettings is the stored settings row: the announcement and
// maintenance banners.
type GlobalSettings struct {
	AnnouncementEnabled bool
	AnnouncementLevel   string
	AnnouncementMessage string
	MaintenanceEnabled  bool
	MaintenanceMessage  string
}

// defaultGlobalSettings is returned if the seeded row is missing.
func defaultGlobalSettings() GlobalSettings {
	return GlobalSettings{AnnouncementLevel: "info"}
}

// SettingsStore persists the global_settings singleton.
type SettingsStore struct{ db *postgres.DB }

// NewSettingsStore returns a SettingsStore on db.
func NewSettingsStore(db *postgres.DB) *SettingsStore {
	return &SettingsStore{db: db}
}

// GetGlobalSettings returns the singleton row, falling back to defaults when the
// row is absent.
func (s *SettingsStore) GetGlobalSettings(ctx context.Context) (GlobalSettings, error) {
	var gs GlobalSettings
	err := s.db.Querier().QueryRow(ctx,
		`SELECT announcement_enabled, announcement_level, announcement_message,
		        maintenance_enabled, maintenance_message
		   FROM global_settings WHERE id = true`,
	).Scan(
		&gs.AnnouncementEnabled, &gs.AnnouncementLevel, &gs.AnnouncementMessage,
		&gs.MaintenanceEnabled, &gs.MaintenanceMessage,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return defaultGlobalSettings(), nil
		}
		return GlobalSettings{}, fmt.Errorf("SettingsStore.GetGlobalSettings: %w", err)
	}
	return gs, nil
}

// SetGlobalSettings upserts the singleton row and returns the stored value.
func (s *SettingsStore) SetGlobalSettings(ctx context.Context, gs GlobalSettings) (GlobalSettings, error) {
	var out GlobalSettings
	err := s.db.Querier().QueryRow(ctx,
		`INSERT INTO global_settings (
		     id, announcement_enabled, announcement_level, announcement_message,
		     maintenance_enabled, maintenance_message)
		 VALUES (true, $1, $2, $3, $4, $5)
		 ON CONFLICT (id) DO UPDATE SET
		     announcement_enabled = EXCLUDED.announcement_enabled,
		     announcement_level   = EXCLUDED.announcement_level,
		     announcement_message = EXCLUDED.announcement_message,
		     maintenance_enabled  = EXCLUDED.maintenance_enabled,
		     maintenance_message  = EXCLUDED.maintenance_message
		 RETURNING announcement_enabled, announcement_level, announcement_message,
		           maintenance_enabled, maintenance_message`,
		gs.AnnouncementEnabled, gs.AnnouncementLevel, gs.AnnouncementMessage,
		gs.MaintenanceEnabled, gs.MaintenanceMessage,
	).Scan(
		&out.AnnouncementEnabled, &out.AnnouncementLevel, &out.AnnouncementMessage,
		&out.MaintenanceEnabled, &out.MaintenanceMessage,
	)
	if err != nil {
		return GlobalSettings{}, fmt.Errorf("SettingsStore.SetGlobalSettings: %w", err)
	}
	return out, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/workcal"
)

// GlobalSettings is the stored settings row: the announcement and
// maintenance banners and the site work calendar. A read always carries the
// effective calendar. The calendar is not written through this field: see
// SetGlobalSettings.
type GlobalSettings struct {
	AnnouncementEnabled bool
	AnnouncementLevel   string
	AnnouncementMessage string
	MaintenanceEnabled  bool
	MaintenanceMessage  string
	WorkCalendar        workcal.Calendar
}

// defaultGlobalSettings is returned if the seeded row is missing.
func defaultGlobalSettings() GlobalSettings {
	return GlobalSettings{AnnouncementLevel: "info", WorkCalendar: workcal.Default()}
}

// workCalendarJSON is the stored shape of the work_calendar column.
type workCalendarJSON struct {
	Weekdays []int         `json:"weekdays"`
	Timezone string        `json:"timezone"`
	Holidays []holidayJSON `json:"holidays"`
}

type holidayJSON struct {
	Date  string `json:"date"`
	Label string `json:"label"`
}

func encodeWorkCalendar(c workcal.Calendar) ([]byte, error) {
	c = c.Normalize()
	j := workCalendarJSON{Weekdays: c.Weekdays, Timezone: c.Timezone, Holidays: make([]holidayJSON, 0, len(c.Holidays))}
	for _, h := range c.Holidays {
		j.Holidays = append(j.Holidays, holidayJSON{Date: h.Date, Label: h.Label})
	}
	return json.Marshal(j)
}

func decodeWorkCalendar(raw []byte) (workcal.Calendar, error) {
	var j workCalendarJSON
	if err := json.Unmarshal(raw, &j); err != nil {
		return workcal.Calendar{}, err
	}
	c := workcal.Calendar{Weekdays: j.Weekdays, Timezone: j.Timezone}
	for _, h := range j.Holidays {
		c.Holidays = append(c.Holidays, workcal.Holiday{Date: h.Date, Label: h.Label})
	}
	return c.Normalize(), nil
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
	var cal []byte
	err := s.db.Querier().QueryRow(ctx,
		`SELECT announcement_enabled, announcement_level, announcement_message,
		        maintenance_enabled, maintenance_message, work_calendar
		   FROM global_settings WHERE id = true`,
	).Scan(
		&gs.AnnouncementEnabled, &gs.AnnouncementLevel, &gs.AnnouncementMessage,
		&gs.MaintenanceEnabled, &gs.MaintenanceMessage, &cal,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return defaultGlobalSettings(), nil
		}
		return GlobalSettings{}, fmt.Errorf("SettingsStore.GetGlobalSettings: %w", err)
	}
	if gs.WorkCalendar, err = decodeWorkCalendar(cal); err != nil {
		return GlobalSettings{}, fmt.Errorf("SettingsStore.GetGlobalSettings: decode work calendar: %w", err)
	}
	return gs, nil
}

// SetGlobalSettings upserts the singleton row and returns the stored value.
// The banners in gs replace the stored ones. The calendar is the separate
// argument: nil keeps the stored calendar (the write is a single statement, so
// a concurrent calendar change is not lost), non-nil replaces it. The caller
// validates it; gs.WorkCalendar is ignored.
func (s *SettingsStore) SetGlobalSettings(ctx context.Context, gs GlobalSettings, cal *workcal.Calendar) (GlobalSettings, error) {
	var calArg *string
	if cal != nil {
		b, err := encodeWorkCalendar(*cal)
		if err != nil {
			return GlobalSettings{}, fmt.Errorf("SettingsStore.SetGlobalSettings: encode work calendar: %w", err)
		}
		str := string(b)
		calArg = &str
	}
	var out GlobalSettings
	var outCal []byte
	err := s.db.Querier().QueryRow(ctx,
		`INSERT INTO global_settings (
		     id, announcement_enabled, announcement_level, announcement_message,
		     maintenance_enabled, maintenance_message, work_calendar)
		 VALUES (true, $1, $2, $3, $4, $5,
		     COALESCE($6::jsonb, '{"weekdays":[1,2,3,4,5],"timezone":"UTC","holidays":[]}'::jsonb))
		 ON CONFLICT (id) DO UPDATE SET
		     announcement_enabled = EXCLUDED.announcement_enabled,
		     announcement_level   = EXCLUDED.announcement_level,
		     announcement_message = EXCLUDED.announcement_message,
		     maintenance_enabled  = EXCLUDED.maintenance_enabled,
		     maintenance_message  = EXCLUDED.maintenance_message,
		     work_calendar        = COALESCE($6::jsonb, global_settings.work_calendar)
		 RETURNING announcement_enabled, announcement_level, announcement_message,
		           maintenance_enabled, maintenance_message, work_calendar`,
		gs.AnnouncementEnabled, gs.AnnouncementLevel, gs.AnnouncementMessage,
		gs.MaintenanceEnabled, gs.MaintenanceMessage, calArg,
	).Scan(
		&out.AnnouncementEnabled, &out.AnnouncementLevel, &out.AnnouncementMessage,
		&out.MaintenanceEnabled, &out.MaintenanceMessage, &outCal,
	)
	if err != nil {
		return GlobalSettings{}, fmt.Errorf("SettingsStore.SetGlobalSettings: %w", err)
	}
	if out.WorkCalendar, err = decodeWorkCalendar(outCal); err != nil {
		return GlobalSettings{}, fmt.Errorf("SettingsStore.SetGlobalSettings: decode work calendar: %w", err)
	}
	return out, nil
}

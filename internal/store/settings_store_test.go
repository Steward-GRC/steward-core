// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/Steward-GRC/steward-core/internal/workcal"
)

// The row 0001 seeded exists before the work-calendar migration runs, so this
// reads the column itself: the Go fallback for a missing row must not hide a
// column with no default.
func TestSettingsStore_WorkCalendar_existingRowGetsTheDefaults(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()

	var raw string
	var rows int
	if err := pool.Querier().QueryRow(ctx,
		`SELECT count(*), coalesce(max(work_calendar::text), '') FROM global_settings`).Scan(&rows, &raw); err != nil {
		t.Fatalf("read the column: %v", err)
	}
	if rows != 1 || raw == "" {
		t.Fatalf("want the seeded row with a calendar, got rows=%d calendar=%q", rows, raw)
	}

	gs, err := store.NewSettingsStore(pool).GetGlobalSettings(ctx)
	if err != nil {
		t.Fatalf("GetGlobalSettings: %v", err)
	}
	if !reflect.DeepEqual(gs.WorkCalendar, workcal.Default().Normalize()) {
		t.Fatalf("calendar = %+v, want the default", gs.WorkCalendar)
	}
}

func TestSettingsStore_WorkCalendar_roundTrip(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewSettingsStore(pool)
	ctx := context.Background()

	cal := workcal.Calendar{
		Weekdays: []int{7, 1, 2, 3, 4}, Timezone: "Asia/Dubai",
		Holidays: []workcal.Holiday{{Date: "2026-12-02", Label: "National Day – é"}, {Date: "2026-01-01", Label: "New Year"}},
	}.Normalize()
	out, err := s.SetGlobalSettings(ctx, store.GlobalSettings{AnnouncementLevel: "info"}, &cal)
	if err != nil {
		t.Fatalf("SetGlobalSettings: %v", err)
	}
	if !reflect.DeepEqual(out.WorkCalendar, cal) {
		t.Fatalf("returned calendar = %+v, want %+v", out.WorkCalendar, cal)
	}
	got, err := s.GetGlobalSettings(ctx)
	if err != nil {
		t.Fatalf("GetGlobalSettings: %v", err)
	}
	if !reflect.DeepEqual(got.WorkCalendar, cal) {
		t.Fatalf("stored calendar = %+v, want %+v", got.WorkCalendar, cal)
	}
}

func TestSettingsStore_WorkCalendar_nilKeepsTheStoredCalendar(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewSettingsStore(pool)
	ctx := context.Background()

	cal := workcal.Calendar{Weekdays: []int{2, 3}, Timezone: "Europe/London",
		Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: "Christmas"}}}.Normalize()
	if _, err := s.SetGlobalSettings(ctx, store.GlobalSettings{AnnouncementLevel: "info"}, &cal); err != nil {
		t.Fatalf("seed calendar: %v", err)
	}

	out, err := s.SetGlobalSettings(ctx, store.GlobalSettings{
		AnnouncementEnabled: true, AnnouncementLevel: "warning", AnnouncementMessage: "banner",
	}, nil)
	if err != nil {
		t.Fatalf("banner-only write: %v", err)
	}
	if !reflect.DeepEqual(out.WorkCalendar, cal) {
		t.Fatalf("returned calendar = %+v, want it kept as %+v", out.WorkCalendar, cal)
	}
	got, _ := s.GetGlobalSettings(ctx)
	if !reflect.DeepEqual(got.WorkCalendar, cal) || got.AnnouncementMessage != "banner" {
		t.Fatalf("after a banner-only write: %+v", got)
	}
}

func TestSettingsStore_WorkCalendar_clearingHolidaysStoresAnEmptyList(t *testing.T) {
	pool := newTestDB(t)
	s := store.NewSettingsStore(pool)
	ctx := context.Background()

	withHol := workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: "x"}}}.Normalize()
	if _, err := s.SetGlobalSettings(ctx, store.GlobalSettings{AnnouncementLevel: "info"}, &withHol); err != nil {
		t.Fatal(err)
	}
	none := workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC"}.Normalize()
	if _, err := s.SetGlobalSettings(ctx, store.GlobalSettings{AnnouncementLevel: "info"}, &none); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := pool.Querier().QueryRow(ctx, `SELECT work_calendar->'holidays' FROM global_settings`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != "[]" {
		t.Fatalf("holidays stored as %s, want []", raw)
	}
}

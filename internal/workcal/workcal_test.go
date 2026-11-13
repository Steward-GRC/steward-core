// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package workcal_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Steward-GRC/steward-core/internal/workcal"
)

func TestDefaultIsMondayToFridayUTCNoHolidays(t *testing.T) {
	d := workcal.Default()
	if !reflect.DeepEqual(d.Weekdays, []int{1, 2, 3, 4, 5}) || d.Timezone != "UTC" || len(d.Holidays) != 0 {
		t.Fatalf("default = %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("default must be valid: %v", err)
	}
}

func TestValidateAccepts(t *testing.T) {
	cases := map[string]workcal.Calendar{
		"single weekday":       {Weekdays: []int{3}, Timezone: "UTC"},
		"all seven":            {Weekdays: []int{1, 2, 3, 4, 5, 6, 7}, Timezone: "Europe/London"},
		"sun to thu":           {Weekdays: []int{7, 1, 2, 3, 4}, Timezone: "Asia/Dubai"},
		"half hour zone":       {Weekdays: []int{1}, Timezone: "Asia/Kolkata"},
		"holiday on a weekend": {Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-11-14", Label: "Saturday"}}},
		"leap day":             {Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2028-02-29", Label: "Leap"}}},
		"empty label":          {Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25"}}},
		"blank label":          {Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: "   "}}},
		"80 multibyte chars":   {Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: strings.Repeat("\u00e9", 80)}}},
		"label of 80 runes":    {Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: strings.Repeat("e", 80)}}},
	}
	for name, c := range cases {
		if err := c.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestValidateRejectsEachRule(t *testing.T) {
	hol := func(date string) []workcal.Holiday { return []workcal.Holiday{{Date: date, Label: "x"}} }
	tooMany := make([]workcal.Holiday, 0, workcal.MaxHolidays+1)
	for i := 0; i <= workcal.MaxHolidays; i++ {
		tooMany = append(tooMany, workcal.Holiday{Date: time.Date(2026, 1, 1+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly), Label: "h"})
	}
	atLimit := tooMany[:workcal.MaxHolidays]
	if err := (workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: atLimit}).Validate(); err != nil {
		t.Fatalf("exactly %d holidays must be accepted: %v", workcal.MaxHolidays, err)
	}

	cases := []struct {
		name string
		cal  workcal.Calendar
		want error
	}{
		{"no weekday", workcal.Calendar{Timezone: "UTC"}, workcal.ErrNoWorkingWeekday},
		{"weekday zero", workcal.Calendar{Weekdays: []int{0}, Timezone: "UTC"}, workcal.ErrInvalidWeekday},
		{"weekday eight", workcal.Calendar{Weekdays: []int{8}, Timezone: "UTC"}, workcal.ErrInvalidWeekday},
		{"weekday negative", workcal.Calendar{Weekdays: []int{-1}, Timezone: "UTC"}, workcal.ErrInvalidWeekday},
		{"weekday repeated", workcal.Calendar{Weekdays: []int{1, 2, 1}, Timezone: "UTC"}, workcal.ErrInvalidWeekday},
		{"empty zone", workcal.Calendar{Weekdays: []int{1}, Timezone: ""}, workcal.ErrInvalidTimezone},
		{"Local zone", workcal.Calendar{Weekdays: []int{1}, Timezone: "Local"}, workcal.ErrInvalidTimezone},
		{"unknown zone", workcal.Calendar{Weekdays: []int{1}, Timezone: "Nowhere/Land"}, workcal.ErrInvalidTimezone},
		{"zone with spaces", workcal.Calendar{Weekdays: []int{1}, Timezone: " UTC "}, workcal.ErrInvalidTimezone},
		{"holiday impossible date", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("2026-02-30")}, workcal.ErrInvalidHoliday},
		{"holiday not a leap day", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("2027-02-29")}, workcal.ErrInvalidHoliday},
		{"holiday month 13", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("2026-13-01")}, workcal.ErrInvalidHoliday},
		{"holiday unpadded", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("2026-1-5")}, workcal.ErrInvalidHoliday},
		{"holiday slashes", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("2026/12/25")}, workcal.ErrInvalidHoliday},
		{"holiday with time", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("2026-12-25T00:00:00Z")}, workcal.ErrInvalidHoliday},
		{"holiday padded", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol(" 2026-12-25")}, workcal.ErrInvalidHoliday},
		{"holiday empty", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("")}, workcal.ErrInvalidHoliday},
		{"holiday year zero", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("0000-01-01")}, workcal.ErrInvalidHoliday},
		{"holiday non digit", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: hol("2026-1a-05")}, workcal.ErrInvalidHoliday},
		{"holiday duplicated", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: "a"}, {Date: "2026-12-25", Label: "b"}}}, workcal.ErrDuplicateHoliday},
		{"too many holidays", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: tooMany}, workcal.ErrTooManyHolidays},
		{"label 81 multibyte chars", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: strings.Repeat("\u00e9", 81)}}}, workcal.ErrInvalidLabel},
		{"label 81 runes", workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: strings.Repeat("e", 81)}}}, workcal.ErrInvalidLabel},
	}
	for _, tc := range cases {
		err := tc.cal.Validate()
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestValidateErrorNamesTheProblemNotThePackage(t *testing.T) {
	err := (workcal.Calendar{Weekdays: []int{9}, Timezone: "UTC"}).Validate()
	if err == nil || strings.Contains(err.Error(), "workcal") || !strings.Contains(err.Error(), "9") {
		t.Fatalf("message must be user-facing and name the value: %v", err)
	}
}

func TestNormalizeSortsAndNeverReturnsNil(t *testing.T) {
	got := workcal.Calendar{
		Weekdays: []int{5, 1, 3}, Timezone: "UTC",
		Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: "b"}, {Date: "2026-01-01", Label: "a"}},
	}.Normalize()
	want := workcal.Calendar{
		Weekdays: []int{1, 3, 5}, Timezone: "UTC",
		Holidays: []workcal.Holiday{{Date: "2026-01-01", Label: "a"}, {Date: "2026-12-25", Label: "b"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalize = %+v, want %+v", got, want)
	}
	empty := workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC"}.Normalize()
	if empty.Holidays == nil {
		t.Fatal("holidays must be an empty slice, not nil, so JSON stays []")
	}
}

func TestNormalizeDoesNotMutateTheInput(t *testing.T) {
	in := workcal.Calendar{Weekdays: []int{5, 1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: "b"}, {Date: "2026-01-01", Label: "a"}}}
	_ = in.Normalize()
	if in.Weekdays[0] != 5 || in.Holidays[0].Date != "2026-12-25" {
		t.Fatalf("input changed: %+v", in)
	}
}

func TestEqualIgnoresOrder(t *testing.T) {
	a := workcal.Calendar{Weekdays: []int{1, 2}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-01-01", Label: "a"}, {Date: "2026-12-25", Label: "b"}}}
	b := workcal.Calendar{Weekdays: []int{2, 1}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: "b"}, {Date: "2026-01-01", Label: "a"}}}
	if !a.Equal(b) {
		t.Fatal("same calendar in a different order must be equal")
	}
	for name, c := range map[string]workcal.Calendar{
		"weekday":  {Weekdays: []int{1, 3}, Timezone: "UTC", Holidays: a.Holidays},
		"timezone": {Weekdays: []int{1, 2}, Timezone: "Europe/London", Holidays: a.Holidays},
		"date":     {Weekdays: []int{1, 2}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-01-02", Label: "a"}, {Date: "2026-12-25", Label: "b"}}},
		"label":    {Weekdays: []int{1, 2}, Timezone: "UTC", Holidays: []workcal.Holiday{{Date: "2026-01-01", Label: "A"}, {Date: "2026-12-25", Label: "b"}}},
		"count":    {Weekdays: []int{1, 2}, Timezone: "UTC"},
	} {
		if a.Equal(c) {
			t.Errorf("%s change must not be equal", name)
		}
	}
}

func TestDiffHolidays(t *testing.T) {
	before := workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{
		{Date: "2026-01-01", Label: "New Year"}, {Date: "2026-05-01", Label: "Labour"}, {Date: "2026-12-25", Label: "Xmas"}}}
	after := workcal.Calendar{Weekdays: []int{1}, Timezone: "UTC", Holidays: []workcal.Holiday{
		{Date: "2026-01-01", Label: "New Year's Day"}, {Date: "2026-07-04", Label: "Fourth"}, {Date: "2026-12-25", Label: "Xmas"}}}
	added, removed, relabeled := workcal.DiffHolidays(before, after)
	if fmt.Sprint(added) != "[2026-07-04]" || fmt.Sprint(removed) != "[2026-05-01]" || fmt.Sprint(relabeled) != "[2026-01-01]" {
		t.Fatalf("added=%v removed=%v relabeled=%v", added, removed, relabeled)
	}
}

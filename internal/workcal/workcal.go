// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package workcal holds the site work calendar: which weekdays are working
// days, which civil dates are holidays, and the time zone those dates are read
// in. It validates the calendar the settings accept; it does not count days
// (the workflow service does). The rules match the workflow service's calendar
// package, so a calendar stored here is one that service accepts.
//
// Whole days only. The package uses the standard library alone and holds no
// state.
package workcal

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	// The zone database ships inside the binary so a minimal image with no
	// system zoneinfo still loads the site time zone.
	_ "time/tzdata"
)

const (
	// MaxHolidays is the most holidays a calendar may list.
	MaxHolidays = 366
	// MaxLabelRunes is the longest holiday label.
	MaxLabelRunes = 80
	// DefaultTimezone is the zone of the default calendar.
	DefaultTimezone = "UTC"
)

// The validation errors. Each wraps detail naming the value, so the message is
// fit to show an admin.
var (
	// ErrNoWorkingWeekday means the calendar has no working weekday.
	ErrNoWorkingWeekday = errors.New("at least one working weekday is required")
	// ErrInvalidWeekday means a weekday is outside 1 (Monday) to 7 (Sunday) or
	// is listed twice.
	ErrInvalidWeekday = errors.New("a weekday must be 1 (Monday) to 7 (Sunday), listed once")
	// ErrInvalidTimezone means the zone is not a loadable IANA name.
	ErrInvalidTimezone = errors.New("the time zone must be an IANA name such as Europe/London")
	// ErrInvalidHoliday means a holiday is not a real YYYY-MM-DD date.
	ErrInvalidHoliday = errors.New("a holiday date must be a real date as YYYY-MM-DD")
	// ErrDuplicateHoliday means a date is listed twice.
	ErrDuplicateHoliday = errors.New("a holiday date can be listed once")
	// ErrTooManyHolidays means the list is longer than MaxHolidays.
	ErrTooManyHolidays = errors.New("too many holidays")
	// ErrInvalidLabel means a holiday label is longer than MaxLabelRunes.
	ErrInvalidLabel = errors.New("a holiday label can be at most 80 characters")
)

// Holiday is one day off. Date is a strict YYYY-MM-DD civil date.
type Holiday struct {
	Date  string
	Label string
}

// Calendar is the site work calendar. Weekdays are ISO numbers (1 Monday to 7
// Sunday).
type Calendar struct {
	Weekdays []int
	Timezone string
	Holidays []Holiday
}

// Default is Monday to Friday, UTC, no holidays.
func Default() Calendar {
	return Calendar{Weekdays: []int{1, 2, 3, 4, 5}, Timezone: DefaultTimezone, Holidays: []Holiday{}}
}

// Validate checks every rule and returns the first break.
func (c Calendar) Validate() error {
	if len(c.Weekdays) == 0 {
		return ErrNoWorkingWeekday
	}
	var seen [8]bool
	for _, n := range c.Weekdays {
		if n < 1 || n > 7 {
			return fmt.Errorf("%w: got %d", ErrInvalidWeekday, n)
		}
		if seen[n] {
			return fmt.Errorf("%w: %d is repeated", ErrInvalidWeekday, n)
		}
		seen[n] = true
	}
	// The empty string and "Local" would depend on the host, so they are
	// refused before LoadLocation, which accepts both.
	if c.Timezone == "" || c.Timezone == "Local" {
		return fmt.Errorf("%w: got %q", ErrInvalidTimezone, c.Timezone)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("%w: got %q", ErrInvalidTimezone, c.Timezone)
	}
	if len(c.Holidays) > MaxHolidays {
		return fmt.Errorf("%w: %d (at most %d)", ErrTooManyHolidays, len(c.Holidays), MaxHolidays)
	}
	dates := make(map[string]struct{}, len(c.Holidays))
	for _, h := range c.Holidays {
		if !validDate(h.Date) {
			return fmt.Errorf("%w: got %q", ErrInvalidHoliday, h.Date)
		}
		if _, dup := dates[h.Date]; dup {
			return fmt.Errorf("%w: %s is repeated", ErrDuplicateHoliday, h.Date)
		}
		dates[h.Date] = struct{}{}
		if n := utf8.RuneCountInString(h.Label); n > MaxLabelRunes {
			return fmt.Errorf("%w: the label for %s has %d characters", ErrInvalidLabel, h.Date, n)
		}
	}
	return nil
}

// validDate reports whether s is a strict YYYY-MM-DD civil date: ten bytes,
// digits and two dashes, a real day, a year from 1.
func validDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if i != 4 && i != 7 && (s[i] < '0' || s[i] > '9') {
			return false
		}
	}
	t, err := time.Parse(time.DateOnly, s)
	return err == nil && t.Year() >= 1
}

// Normalize returns a copy with the weekdays ascending, the holidays by date
// and no nil slice, so two equal calendars store and compare the same. It does
// not validate.
func (c Calendar) Normalize() Calendar {
	out := Calendar{
		Weekdays: slices.Clone(c.Weekdays),
		Timezone: c.Timezone,
		Holidays: slices.Clone(c.Holidays),
	}
	if out.Weekdays == nil {
		out.Weekdays = []int{}
	}
	if out.Holidays == nil {
		out.Holidays = []Holiday{}
	}
	slices.Sort(out.Weekdays)
	slices.SortStableFunc(out.Holidays, func(a, b Holiday) int { return strings.Compare(a.Date, b.Date) })
	return out
}

// Equal reports whether a and b are the same calendar, whatever the order of
// their weekdays and holidays.
func (c Calendar) Equal(o Calendar) bool {
	a, b := c.Normalize(), o.Normalize()
	return a.Timezone == b.Timezone && slices.Equal(a.Weekdays, b.Weekdays) && slices.Equal(a.Holidays, b.Holidays)
}

// DiffHolidays lists the holiday dates only after has (added), only before has
// (removed) and in both with another label (relabeled), each ascending. Labels
// are not returned: they are free text.
func DiffHolidays(before, after Calendar) (added, removed, relabeled []string) {
	was := make(map[string]string, len(before.Holidays))
	for _, h := range before.Holidays {
		was[h.Date] = h.Label
	}
	now := make(map[string]string, len(after.Holidays))
	for _, h := range after.Holidays {
		now[h.Date] = h.Label
		old, ok := was[h.Date]
		switch {
		case !ok:
			added = append(added, h.Date)
		case old != h.Label:
			relabeled = append(relabeled, h.Date)
		}
	}
	for d := range was {
		if _, ok := now[d]; !ok {
			removed = append(removed, d)
		}
	}
	slices.Sort(added)
	slices.Sort(removed)
	slices.Sort(relabeled)
	return added, removed, relabeled
}

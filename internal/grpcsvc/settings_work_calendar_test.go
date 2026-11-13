// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Bugs5382/go-apperr/apperrgrpc"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/Steward-GRC/steward-core/internal/workcal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// calSettingsStore is an in-memory settings store that keeps the nil-means-keep
// rule of the real one, so the handler is tested against the same contract.
type calSettingsStore struct {
	gs         store.GlobalSettings
	reads      int
	writes     int
	getErr     error
	setErr     error
	setCalCall []*workcal.Calendar
}

func newCalSettingsStore() *calSettingsStore {
	return &calSettingsStore{gs: store.GlobalSettings{AnnouncementLevel: "info", WorkCalendar: workcal.Default().Normalize()}}
}

func (f *calSettingsStore) GetGlobalSettings(context.Context) (store.GlobalSettings, error) {
	f.reads++
	return f.gs, f.getErr
}

func (f *calSettingsStore) SetGlobalSettings(_ context.Context, gs store.GlobalSettings, cal *workcal.Calendar) (store.GlobalSettings, error) {
	if f.setErr != nil {
		return store.GlobalSettings{}, f.setErr
	}
	f.writes++
	f.setCalCall = append(f.setCalCall, cal)
	keep := f.gs.WorkCalendar
	f.gs = gs
	f.gs.WorkCalendar = keep
	if cal != nil {
		f.gs.WorkCalendar = *cal
	}
	return f.gs, nil
}

func protoCal(weekdays []int32, tz string, hol ...*corev1.Holiday) *corev1.WorkCalendar {
	return &corev1.WorkCalendar{Weekdays: weekdays, Timezone: tz, Holidays: hol}
}

func calActions(pub *capturePublisher) []string {
	var out []string
	for _, c := range pub.calls {
		out = append(out, c.event.Action)
	}
	return out
}

func TestGetGlobalSettingsAlwaysReturnsTheEffectiveCalendar(t *testing.T) {
	h := grpcsvc.NewSettingsHandler(newCalSettingsStore(), nil)
	resp, err := h.GetGlobalSettings(context.Background(), &corev1.GetGlobalSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	wc := resp.GetSettings().GetWorkCalendar()
	if wc == nil {
		t.Fatal("work_calendar must be present even when never set")
	}
	if !reflect.DeepEqual(wc.GetWeekdays(), []int32{1, 2, 3, 4, 5}) || wc.GetTimezone() != "UTC" || len(wc.GetHolidays()) != 0 {
		t.Fatalf("calendar = %v, want Mon-Fri UTC with no holidays", wc)
	}
}

func TestSetGlobalSettingsAbsentCalendarMeansUnchanged(t *testing.T) {
	fs := newCalSettingsStore()
	fs.gs.WorkCalendar = workcal.Calendar{Weekdays: []int{2, 3}, Timezone: "Europe/London",
		Holidays: []workcal.Holiday{{Date: "2026-12-25", Label: "Christmas"}}}
	pub := &capturePublisher{}
	h := grpcsvc.NewSettingsHandler(fs, audit.New(pub))

	resp, err := h.SetGlobalSettings(context.Background(), &corev1.SetGlobalSettingsRequest{
		Settings:    &corev1.GlobalSettings{Announcement: &corev1.Announcement{Enabled: true, Level: "info", Message: "hi"}}, // old client
		ActorUserId: "admin-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fs.setCalCall) != 1 || fs.setCalCall[0] != nil {
		t.Fatalf("store must be told to keep the calendar (nil), got %v", fs.setCalCall)
	}
	got := resp.GetSettings().GetWorkCalendar()
	if got.GetTimezone() != "Europe/London" || !reflect.DeepEqual(got.GetWeekdays(), []int32{2, 3}) || len(got.GetHolidays()) != 1 {
		t.Fatalf("response must carry the kept calendar, got %v", got)
	}
	if acts := calActions(pub); !reflect.DeepEqual(acts, []string{"global_settings.updated"}) {
		t.Fatalf("a banner-only write must not emit the calendar event, got %v", acts)
	}
}

func TestSetGlobalSettingsNilSettingsKeepsTheCalendarToo(t *testing.T) {
	fs := newCalSettingsStore()
	h := grpcsvc.NewSettingsHandler(fs, nil)
	if _, err := h.SetGlobalSettings(context.Background(), &corev1.SetGlobalSettingsRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fs.setCalCall) != 1 || fs.setCalCall[0] != nil {
		t.Fatalf("want nil calendar passed on, got %v", fs.setCalCall)
	}
}

func TestSetGlobalSettingsStoresANormalizedCalendar(t *testing.T) {
	fs := newCalSettingsStore()
	h := grpcsvc.NewSettingsHandler(fs, nil)
	resp, err := h.SetGlobalSettings(context.Background(), &corev1.SetGlobalSettingsRequest{
		Settings: &corev1.GlobalSettings{WorkCalendar: protoCal([]int32{5, 1, 3}, "Asia/Kolkata",
			&corev1.Holiday{Date: "2026-12-25", Label: "b"}, &corev1.Holiday{Date: "2026-01-26", Label: "a"})},
	})
	if err != nil {
		t.Fatal(err)
	}
	wc := resp.GetSettings().GetWorkCalendar()
	if !reflect.DeepEqual(wc.GetWeekdays(), []int32{1, 3, 5}) || wc.GetTimezone() != "Asia/Kolkata" ||
		wc.GetHolidays()[0].GetDate() != "2026-01-26" || wc.GetHolidays()[1].GetLabel() != "b" {
		t.Fatalf("calendar = %v", wc)
	}
}

func TestSetGlobalSettingsRejectsAnInvalidCalendarWithoutWriting(t *testing.T) {
	h0 := func() (*grpcsvc.SettingsHandler, *calSettingsStore, *capturePublisher, *fakeCache) {
		fs := newCalSettingsStore()
		pub := &capturePublisher{}
		c := newFakeCache()
		return grpcsvc.NewSettingsHandler(fs, audit.New(pub)).WithCache(c), fs, pub, c
	}
	bad := map[string]*corev1.WorkCalendar{
		"no weekday":     protoCal(nil, "UTC"),
		"weekday 9":      protoCal([]int32{9}, "UTC"),
		"repeat weekday": protoCal([]int32{1, 1}, "UTC"),
		"empty zone":     protoCal([]int32{1}, ""),
		"Local zone":     protoCal([]int32{1}, "Local"),
		"unknown zone":   protoCal([]int32{1}, "Nowhere/Land"),
		"bad date":       protoCal([]int32{1}, "UTC", &corev1.Holiday{Date: "2026-02-30", Label: "x"}),
		"duplicate date": protoCal([]int32{1}, "UTC", &corev1.Holiday{Date: "2026-12-25", Label: "a"}, &corev1.Holiday{Date: "2026-12-25", Label: "b"}),
		"empty label":    protoCal([]int32{1}, "UTC", &corev1.Holiday{Date: "2026-12-25"}),
	}
	for name, wc := range bad {
		h, fs, pub, c := h0()
		_, err := h.SetGlobalSettings(context.Background(), &corev1.SetGlobalSettingsRequest{
			Settings: &corev1.GlobalSettings{WorkCalendar: wc}, ActorUserId: "admin-1"})
		st := status.Convert(err)
		if st.Code() != codes.InvalidArgument {
			t.Errorf("%s: code = %v (%v), want InvalidArgument", name, st.Code(), err)
			continue
		}
		info, ok := apperrgrpc.FromStatus(st)
		if !ok || info.Symbol != "INVALID_WORK_CALENDAR" || info.Code != 4009 {
			t.Errorf("%s: want coded INVALID_WORK_CALENDAR 4009, got %+v", name, info)
		}
		if fs.writes != 0 || len(pub.calls) != 0 || len(c.dels) != 0 {
			t.Errorf("%s: a refused calendar must write, emit and invalidate nothing (writes=%d events=%d dels=%d)",
				name, fs.writes, len(pub.calls), len(c.dels))
		}
		if !strings.Contains(st.Message(), "work calendar") {
			t.Errorf("%s: message %q should say what is wrong", name, st.Message())
		}
	}
}

func TestSetGlobalSettingsEmitsTheCalendarEventOnlyOnChange(t *testing.T) {
	fs := newCalSettingsStore()
	pub := &capturePublisher{}
	h := grpcsvc.NewSettingsHandler(fs, audit.New(pub))
	ctx := context.Background()

	change := &corev1.SetGlobalSettingsRequest{ActorUserId: "admin-1", Settings: &corev1.GlobalSettings{
		WorkCalendar: protoCal([]int32{1, 2, 3, 4}, "Europe/London",
			&corev1.Holiday{Date: "2026-12-25", Label: "Christmas"}, &corev1.Holiday{Date: "2026-12-26", Label: "Boxing"})}}
	if _, err := h.SetGlobalSettings(ctx, change); err != nil {
		t.Fatal(err)
	}
	if acts := calActions(pub); !reflect.DeepEqual(acts, []string{"global_settings.updated", "global_settings.work_calendar_updated"}) {
		t.Fatalf("first write events = %v", acts)
	}
	ev := pub.calls[1].event
	if ev.Tier != audit.TierAudit || ev.Subject != "global_settings" || ev.ActorUserID != "admin-1" {
		t.Fatalf("event = %+v", ev)
	}
	want := map[string]string{
		"weekdays_before": "1,2,3,4,5", "weekdays_after": "1,2,3,4",
		"timezone_before": "UTC", "timezone_after": "Europe/London",
		"holidays_before": "0", "holidays_after": "2",
		"holidays_added": "2026-12-25,2026-12-26",
	}
	if !reflect.DeepEqual(ev.Attributes, want) {
		t.Fatalf("attributes = %v, want %v", ev.Attributes, want)
	}
	for _, v := range ev.Attributes {
		if strings.Contains(v, "Christmas") || strings.Contains(v, "Boxing") {
			t.Fatalf("labels are free text and stay out of the event: %v", ev.Attributes)
		}
	}

	// The same calendar again, in another order: no change, no calendar event.
	pub.calls = nil
	same := &corev1.SetGlobalSettingsRequest{ActorUserId: "admin-1", Settings: &corev1.GlobalSettings{
		WorkCalendar: protoCal([]int32{4, 3, 2, 1}, "Europe/London",
			&corev1.Holiday{Date: "2026-12-26", Label: "Boxing"}, &corev1.Holiday{Date: "2026-12-25", Label: "Christmas"})}}
	if _, err := h.SetGlobalSettings(ctx, same); err != nil {
		t.Fatal(err)
	}
	if acts := calActions(pub); !reflect.DeepEqual(acts, []string{"global_settings.updated"}) {
		t.Fatalf("an identical calendar must not emit the calendar event, got %v", acts)
	}

	// A label edit alone is a change, reported by date.
	pub.calls = nil
	relabel := &corev1.SetGlobalSettingsRequest{ActorUserId: "admin-1", Settings: &corev1.GlobalSettings{
		WorkCalendar: protoCal([]int32{1, 2, 3, 4}, "Europe/London",
			&corev1.Holiday{Date: "2026-12-25", Label: "Christmas Day"}, &corev1.Holiday{Date: "2026-12-26", Label: "Boxing"})}}
	if _, err := h.SetGlobalSettings(ctx, relabel); err != nil {
		t.Fatal(err)
	}
	if len(pub.calls) != 2 || pub.calls[1].event.Attributes["holidays_relabeled"] != "2026-12-25" {
		t.Fatalf("relabel events = %v", calActions(pub))
	}
}

func TestSetGlobalSettingsInvalidatesTheCacheOnACalendarWrite(t *testing.T) {
	fs := newCalSettingsStore()
	c := newFakeCache()
	h := grpcsvc.NewSettingsHandler(fs, nil).WithCache(c)
	ctx := context.Background()

	if _, err := h.GetGlobalSettings(ctx, &corev1.GetGlobalSettingsRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.SetGlobalSettings(ctx, &corev1.SetGlobalSettingsRequest{Settings: &corev1.GlobalSettings{
		WorkCalendar: protoCal([]int32{1}, "UTC")}}); err != nil {
		t.Fatal(err)
	}
	if !c.deleted("polsettings:global") {
		t.Fatalf("a calendar write must bust the cache, dels=%v", c.dels)
	}
	resp, err := h.GetGlobalSettings(ctx, &corev1.GetGlobalSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(resp.GetSettings().GetWorkCalendar().GetWeekdays(), []int32{1}) {
		t.Fatalf("read after write must see the new calendar, got %v", resp.GetSettings().GetWorkCalendar())
	}
}

// A response cached before this field existed has no calendar. It is read as a
// miss, so a reader never sees "no calendar" during a rolling upgrade.
func TestGetGlobalSettingsTreatsACachedResponseWithoutACalendarAsAMiss(t *testing.T) {
	fs := newCalSettingsStore()
	c := newFakeCache()
	old, _ := proto.Marshal(&corev1.GetGlobalSettingsResponse{Settings: &corev1.GlobalSettings{
		Announcement: &corev1.Announcement{Level: "info"}}})
	c.data["polsettings:global"] = old
	h := grpcsvc.NewSettingsHandler(fs, nil).WithCache(c)

	resp, err := h.GetGlobalSettings(context.Background(), &corev1.GetGlobalSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetSettings().GetWorkCalendar() == nil || fs.reads != 1 {
		t.Fatalf("want a store read and a calendar, got reads=%d cal=%v", fs.reads, resp.GetSettings().GetWorkCalendar())
	}
}

func TestSetGlobalSettingsStoreFailuresAreCodedNotLeaked(t *testing.T) {
	fs := newCalSettingsStore()
	fs.getErr = errors.New("dial tcp 192.0.2.10:5432: connection refused")
	h := grpcsvc.NewSettingsHandler(fs, nil)
	_, err := h.SetGlobalSettings(context.Background(), &corev1.SetGlobalSettingsRequest{Settings: &corev1.GlobalSettings{
		WorkCalendar: protoCal([]int32{1}, "UTC")}})
	st := status.Convert(err)
	if st.Code() != codes.Internal || strings.Contains(st.Message(), "192.0.2.10") {
		t.Fatalf("status = %v %q", st.Code(), st.Message())
	}
}

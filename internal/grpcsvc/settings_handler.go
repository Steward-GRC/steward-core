// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"strconv"
	"strings"

	log "github.com/Bugs5382/go-log"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/errcodes"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/Steward-GRC/steward-core/internal/workcal"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// globalSettingsCacheKey holds the encoded GetGlobalSettingsResponse. The
// settings are one row, so one key is enough; every write clears it.
const globalSettingsCacheKey = "polsettings:global"

// settingsCache is the optional read cache, *cache.Redis in production.
type settingsCache interface {
	GetBytes(ctx context.Context, key string) ([]byte, bool)
	SetBytes(ctx context.Context, key string, val []byte)
	Del(ctx context.Context, key string)
}

// SettingsStorer is the store interface the SettingsHandler depends on.
type SettingsStorer interface {
	GetGlobalSettings(ctx context.Context) (store.GlobalSettings, error)
	SetGlobalSettings(ctx context.Context, gs store.GlobalSettings, cal *workcal.Calendar) (store.GlobalSettings, error)
}

// EmailServiceStorer is the keyless part of the email-service store. It has
// no method that returns the key: that read is EmailServiceSecretStorer's.
type EmailServiceStorer interface {
	SetEmailServiceConfig(ctx context.Context, in store.SetEmailServiceConfigInput) error
	EmailServiceConfigStatus(ctx context.Context) (store.EmailServiceStatus, error)
}

// SettingsHandler implements corev1.SettingsServiceServer.
type SettingsHandler struct {
	corev1.UnimplementedSettingsServiceServer
	store        SettingsStorer
	emailService EmailServiceStorer
	auditor      auditEmitter
	// Nil means no caching.
	cache settingsCache
}

// NewSettingsHandler returns a handler over s. auditor may be nil.
func NewSettingsHandler(s SettingsStorer, auditor auditEmitter) *SettingsHandler {
	return &SettingsHandler{store: s, auditor: auditor}
}

// WithEmailServiceStore sets the email-service store. Without it the
// email-service RPCs answer FailedPrecondition.
func (h *SettingsHandler) WithEmailServiceStore(m EmailServiceStorer) *SettingsHandler {
	h.emailService = m
	return h
}

// WithCache sets the optional read cache for GetGlobalSettings.
func (h *SettingsHandler) WithCache(c settingsCache) *SettingsHandler {
	h.cache = c
	return h
}

func settingsToProto(gs store.GlobalSettings) *corev1.GlobalSettings {
	return &corev1.GlobalSettings{
		Announcement: &corev1.Announcement{
			Enabled: gs.AnnouncementEnabled,
			Level:   gs.AnnouncementLevel,
			Message: gs.AnnouncementMessage,
		},
		Maintenance: &corev1.Maintenance{
			Enabled: gs.MaintenanceEnabled,
			Message: gs.MaintenanceMessage,
		},
		WorkCalendar: workCalendarToProto(gs.WorkCalendar),
	}
}

func workCalendarToProto(c workcal.Calendar) *corev1.WorkCalendar {
	out := &corev1.WorkCalendar{Weekdays: make([]int32, 0, len(c.Weekdays)), Timezone: c.Timezone}
	for _, d := range c.Weekdays {
		out.Weekdays = append(out.Weekdays, int32(d)) //nolint:gosec // ISO weekdays are 1..7
	}
	for _, h := range c.Holidays {
		out.Holidays = append(out.Holidays, &corev1.Holiday{Date: h.Date, Label: h.Label})
	}
	return out
}

func workCalendarFromProto(in *corev1.WorkCalendar) workcal.Calendar {
	c := workcal.Calendar{Timezone: in.GetTimezone()}
	for _, d := range in.GetWeekdays() {
		c.Weekdays = append(c.Weekdays, int(d))
	}
	for _, h := range in.GetHolidays() {
		c.Holidays = append(c.Holidays, workcal.Holiday{Date: h.GetDate(), Label: h.GetLabel()})
	}
	return c.Normalize()
}

func settingsFromProto(in *corev1.GlobalSettings) store.GlobalSettings {
	gs := store.GlobalSettings{AnnouncementLevel: "info"}
	if in == nil {
		return gs
	}
	if a := in.GetAnnouncement(); a != nil {
		gs.AnnouncementEnabled = a.GetEnabled()
		if a.GetLevel() != "" {
			gs.AnnouncementLevel = a.GetLevel()
		}
		gs.AnnouncementMessage = a.GetMessage()
	}
	if m := in.GetMaintenance(); m != nil {
		gs.MaintenanceEnabled = m.GetEnabled()
		gs.MaintenanceMessage = m.GetMessage()
	}
	return gs
}

// GetGlobalSettings handles SettingsService.GetGlobalSettings.
func (h *SettingsHandler) GetGlobalSettings(ctx context.Context, _ *corev1.GetGlobalSettingsRequest) (*corev1.GetGlobalSettingsResponse, error) {
	if h.cache != nil {
		l := logger.Ctx(ctx)
		if b, ok := h.cache.GetBytes(ctx, globalSettingsCacheKey); ok {
			var resp corev1.GetGlobalSettingsResponse
			// An entry with no calendar was cached before the field existed:
			// read it as a miss so a reader always gets the effective calendar.
			if proto.Unmarshal(b, &resp) == nil && resp.GetSettings().GetWorkCalendar() != nil {
				l.Debug("global-settings cache hit", log.F("cache_key", globalSettingsCacheKey))
				return &resp, nil
			}
		}
		l.Debug("global-settings cache miss", log.F("cache_key", globalSettingsCacheKey))
	}
	gs, err := h.store.GetGlobalSettings(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get global settings: %v", err)
	}
	resp := &corev1.GetGlobalSettingsResponse{Settings: settingsToProto(gs)}
	if h.cache != nil {
		if b, mErr := proto.Marshal(resp); mErr == nil {
			h.cache.SetBytes(ctx, globalSettingsCacheKey, b)
		}
	}
	return resp, nil
}

// SetGlobalSettings handles SettingsService.SetGlobalSettings. The banners
// replace the stored ones. The work calendar is replaced only when the request
// carries one; an absent calendar leaves it as stored, so a client that
// predates the field cannot wipe it.
func (h *SettingsHandler) SetGlobalSettings(ctx context.Context, req *corev1.SetGlobalSettingsRequest) (*corev1.SetGlobalSettingsResponse, error) {
	l := logger.Ctx(ctx)
	l.Debug("set global settings", log.F("calendar_present", req.GetSettings().GetWorkCalendar() != nil))

	var newCal *workcal.Calendar
	var before workcal.Calendar
	if in := req.GetSettings().GetWorkCalendar(); in != nil {
		c := workCalendarFromProto(in)
		if err := c.Validate(); err != nil {
			// Refused before anything is read or written.
			l.Debug("work calendar refused", log.F("problem", err.Error()))
			return nil, errcodes.Error(ctx, errcodes.InvalidWorkCalendar(err.Error()))
		}
		cur, err := h.store.GetGlobalSettings(ctx)
		if err != nil {
			return nil, storeUnavailable(ctx, "get_global_settings", err)
		}
		before = cur.WorkCalendar
		newCal = &c
	}

	gs, err := h.store.SetGlobalSettings(ctx, settingsFromProto(req.GetSettings()), newCal)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set global settings: %v", err)
	}
	// Clear the cache so the next read sees this write; the TTL is only a
	// backstop.
	if h.cache != nil {
		h.cache.Del(ctx, globalSettingsCacheKey)
		l.Debug("global-settings cache cleared", log.F("cache_key", globalSettingsCacheKey))
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "global_settings.updated",
			Subject:     "global_settings",
			ActorUserID: req.GetActorUserId(),
		})
	}
	if newCal != nil && !before.Equal(*newCal) {
		l.Info("work calendar changed", log.F("holidays", len(newCal.Holidays)), log.F("weekdays", len(newCal.Weekdays)))
		h.emitWorkCalendarUpdated(ctx, req.GetActorUserId(), before, *newCal)
	} else if newCal != nil {
		l.Debug("work calendar unchanged, no calendar event")
	}
	return &corev1.SetGlobalSettingsResponse{Settings: settingsToProto(gs)}, nil
}

// emitWorkCalendarUpdated records a calendar change: the weekdays and zone
// before and after, the holiday counts and the holiday dates added, removed or
// relabeled. Labels are free text and stay out of the event.
func (h *SettingsHandler) emitWorkCalendarUpdated(ctx context.Context, actor string, before, after workcal.Calendar) {
	if h.auditor == nil {
		return
	}
	attrs := map[string]string{
		"weekdays_before": joinWeekdays(before.Weekdays),
		"weekdays_after":  joinWeekdays(after.Weekdays),
		"timezone_before": before.Timezone,
		"timezone_after":  after.Timezone,
		"holidays_before": strconv.Itoa(len(before.Holidays)),
		"holidays_after":  strconv.Itoa(len(after.Holidays)),
	}
	added, removed, relabeled := workcal.DiffHolidays(before, after)
	for k, v := range map[string][]string{"holidays_added": added, "holidays_removed": removed, "holidays_relabeled": relabeled} {
		if len(v) > 0 {
			attrs[k] = strings.Join(v, ",")
		}
	}
	_ = h.auditor.Emit(ctx, audit.Event{
		Tier: audit.TierAudit, Action: "global_settings.work_calendar_updated",
		Subject:     "global_settings",
		ActorUserID: actor,
		Attributes:  attrs,
	})
}

func joinWeekdays(days []int) string {
	parts := make([]string, len(days))
	for i, d := range days {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, ",")
}

func emailServiceStatusToProto(st store.EmailServiceStatus) *corev1.EmailServiceStatus {
	return &corev1.EmailServiceStatus{
		ApiKeySet:   st.APIKeySet,
		Domain:      st.Domain,
		Region:      st.Region,
		FromAddress: st.FromAddress,
		Enabled:     st.Enabled,
		UpdatedBy:   st.UpdatedBy,
		Provider:    st.Provider,
	}
}

// SetEmailServiceConfig handles SettingsService.SetEmailServiceConfig. The
// key is never logged and never echoed: the response is the keyless status.
func (h *SettingsHandler) SetEmailServiceConfig(ctx context.Context, req *corev1.SetEmailServiceConfigRequest) (*corev1.SetEmailServiceConfigResponse, error) {
	if h.emailService == nil {
		return nil, status.Error(codes.FailedPrecondition, "email service config store not configured")
	}
	// The pointer is passed on as it is: nil keeps the stored key, "" clears
	// it. Never dereference it into a log line.
	in := store.SetEmailServiceConfigInput{
		APIKey:      req.ApiKey,
		Domain:      req.GetDomain(),
		Region:      req.GetRegion(),
		FromAddress: req.GetFromAddress(),
		Enabled:     req.GetEnabled(),
		UpdatedBy:   req.GetActorUserId(),
		Provider:    req.GetProvider(),
	}
	if err := h.emailService.SetEmailServiceConfig(ctx, in); err != nil {
		return nil, status.Errorf(codes.Internal, "set email service config: %v", err)
	}
	st, err := h.emailService.EmailServiceConfigStatus(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read email service status: %v", err)
	}
	if h.auditor != nil {
		// The event carries no secret: only the action, subject and actor.
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "email_service_config.updated",
			Subject:     "email_service_config",
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.SetEmailServiceConfigResponse{Status: emailServiceStatusToProto(st)}, nil
}

// EmailServiceConfigStatus handles SettingsService.EmailServiceConfigStatus.
func (h *SettingsHandler) EmailServiceConfigStatus(ctx context.Context, _ *corev1.EmailServiceConfigStatusRequest) (*corev1.EmailServiceConfigStatusResponse, error) {
	if h.emailService == nil {
		return nil, status.Error(codes.FailedPrecondition, "email service config store not configured")
	}
	st, err := h.emailService.EmailServiceConfigStatus(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "email service config status: %v", err)
	}
	return &corev1.EmailServiceConfigStatusResponse{Status: emailServiceStatusToProto(st)}, nil
}

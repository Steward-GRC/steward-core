// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"

	log "github.com/Bugs5382/go-log"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/store"
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
	SetGlobalSettings(ctx context.Context, gs store.GlobalSettings) (store.GlobalSettings, error)
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
	}
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
			if proto.Unmarshal(b, &resp) == nil {
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

// SetGlobalSettings handles SettingsService.SetGlobalSettings.
func (h *SettingsHandler) SetGlobalSettings(ctx context.Context, req *corev1.SetGlobalSettingsRequest) (*corev1.SetGlobalSettingsResponse, error) {
	gs, err := h.store.SetGlobalSettings(ctx, settingsFromProto(req.GetSettings()))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "set global settings: %v", err)
	}
	// Clear the cache so the next read sees this write; the TTL is only a
	// backstop.
	if h.cache != nil {
		h.cache.Del(ctx, globalSettingsCacheKey)
	}
	if h.auditor != nil {
		_ = h.auditor.Emit(ctx, audit.Event{
			Tier: audit.TierAudit, Action: "global_settings.updated",
			Subject:     "global_settings",
			ActorUserID: req.GetActorUserId(),
		})
	}
	return &corev1.SetGlobalSettingsResponse{Settings: settingsToProto(gs)}, nil
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

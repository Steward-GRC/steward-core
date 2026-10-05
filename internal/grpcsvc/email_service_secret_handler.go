// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// EmailServiceSecretStorer is the one store read that returns the key.
type EmailServiceSecretStorer interface {
	GetEmailServiceConfig(ctx context.Context) (store.EmailServiceConfig, error)
}

// EmailServiceSecretHandler implements corev1.EmailServiceSecretServiceServer
// for the service that sends mail. It is a separate service from
// SettingsHandler so the gateway, which serves only SettingsService, can never
// reach the key. Never log its response.
type EmailServiceSecretHandler struct {
	corev1.UnimplementedEmailServiceSecretServiceServer
	store EmailServiceSecretStorer
}

// NewEmailServiceSecretHandler returns a handler over s.
func NewEmailServiceSecretHandler(s EmailServiceSecretStorer) *EmailServiceSecretHandler {
	return &EmailServiceSecretHandler{store: s}
}

// GetEmailServiceSecret handles EmailServiceSecretService.GetEmailServiceSecret.
func (h *EmailServiceSecretHandler) GetEmailServiceSecret(ctx context.Context, _ *corev1.GetEmailServiceSecretRequest) (*corev1.GetEmailServiceSecretResponse, error) {
	cfg, err := h.store.GetEmailServiceConfig(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "get email service secret: %v", err)
	}
	return &corev1.GetEmailServiceSecretResponse{
		Config: &corev1.EmailServiceConfig{
			ApiKey:      cfg.APIKey,
			Domain:      cfg.Domain,
			Region:      cfg.Region,
			FromAddress: cfg.FromAddress,
			Enabled:     cfg.Enabled,
			UpdatedBy:   cfg.UpdatedBy,
			Provider:    cfg.Provider,
		},
	}, nil
}

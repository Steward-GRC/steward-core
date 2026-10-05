// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeEmailServiceStore serves both the keyless and the secret read.
type fakeEmailServiceStore struct {
	setCalls  int
	lastSet   store.SetEmailServiceConfigInput
	setErr    error
	status    store.EmailServiceStatus
	statusErr error
	full      store.EmailServiceConfig
	getErr    error
}

func (f *fakeEmailServiceStore) SetEmailServiceConfig(_ context.Context, in store.SetEmailServiceConfigInput) error {
	f.setCalls++
	f.lastSet = in
	return f.setErr
}

func (f *fakeEmailServiceStore) EmailServiceConfigStatus(_ context.Context) (store.EmailServiceStatus, error) {
	return f.status, f.statusErr
}

func (f *fakeEmailServiceStore) GetEmailServiceConfig(_ context.Context) (store.EmailServiceConfig, error) {
	return f.full, f.getErr
}

func TestSetEmailServiceConfigPersistsAndReturnsKeylessStatus(t *testing.T) {
	fake := &fakeEmailServiceStore{
		status: store.EmailServiceStatus{
			APIKeySet: true, Domain: "mg.example.org", Region: "us",
			FromAddress: "noreply@example.org", Enabled: true, UpdatedBy: "admin-1",
		},
	}
	pub := &capturePublisher{}
	h := grpcsvc.NewSettingsHandler(nil, audit.New(pub)).WithEmailServiceStore(fake)

	const secret = "key-super-secret-value"
	resp, err := h.SetEmailServiceConfig(context.Background(), &corev1.SetEmailServiceConfigRequest{
		ApiKey:      new(secret),
		Domain:      "mg.example.org",
		Region:      "us",
		FromAddress: "noreply@example.org",
		Enabled:     true,
		ActorUserId: "admin-1",
	})
	if err != nil {
		t.Fatalf("SetEmailServiceConfig: %v", err)
	}

	if fake.setCalls != 1 {
		t.Fatalf("SetEmailServiceConfig store calls = %d, want 1", fake.setCalls)
	}
	if fake.lastSet.APIKey == nil || *fake.lastSet.APIKey != secret {
		t.Fatalf("persisted api key = %v, want %q", fake.lastSet.APIKey, secret)
	}
	if fake.lastSet.Domain != "mg.example.org" || fake.lastSet.Region != "us" ||
		fake.lastSet.FromAddress != "noreply@example.org" || !fake.lastSet.Enabled {
		t.Fatalf("persisted non-secret fields wrong: %+v", fake.lastSet)
	}
	if fake.lastSet.UpdatedBy != "admin-1" {
		t.Fatalf("persisted updated_by = %q, want admin-1", fake.lastSet.UpdatedBy)
	}

	if resp.GetStatus() == nil || !resp.GetStatus().GetApiKeySet() {
		t.Fatalf("response status missing or api_key_set=false: %+v", resp.GetStatus())
	}
	if resp.GetStatus().GetDomain() != "mg.example.org" {
		t.Fatalf("response domain = %q", resp.GetStatus().GetDomain())
	}

	// The audit event never carries the key.
	if len(pub.calls) != 1 {
		t.Fatalf("audit events = %d, want 1", len(pub.calls))
	}
	ev := pub.calls[0].event
	if ev.Action != "email_service_config.updated" || ev.Subject != "email_service_config" || ev.ActorUserID != "admin-1" {
		t.Fatalf("unexpected audit event: %+v", ev)
	}
	raw, _ := json.Marshal(ev)
	if strings.Contains(string(raw), secret) {
		t.Fatalf("api key leaked into audit event: %s", raw)
	}
}

func TestSetEmailServiceConfigForwardsThreeStateKey(t *testing.T) {
	cases := []struct {
		name    string
		reqKey  *string
		wantNil bool
		wantVal string
	}{
		{name: "absent leaves key unchanged", reqKey: nil, wantNil: true},
		{name: "empty clears key", reqKey: new(""), wantNil: false, wantVal: ""},
		{name: "value sets key", reqKey: new("key-new-secret"), wantNil: false, wantVal: "key-new-secret"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeEmailServiceStore{
				status: store.EmailServiceStatus{
					APIKeySet: true, Domain: "mg.example.org", Region: "us",
					FromAddress: "noreply@example.org", Enabled: true, UpdatedBy: "admin-1",
				},
			}
			h := grpcsvc.NewSettingsHandler(nil, nil).WithEmailServiceStore(fake)

			resp, err := h.SetEmailServiceConfig(context.Background(), &corev1.SetEmailServiceConfigRequest{
				ApiKey:      tc.reqKey,
				Domain:      "mg.example.org",
				Region:      "us",
				FromAddress: "noreply@example.org",
				Enabled:     true,
				ActorUserId: "admin-1",
			})
			if err != nil {
				t.Fatalf("SetEmailServiceConfig: %v", err)
			}

			if tc.wantNil {
				if fake.lastSet.APIKey != nil {
					t.Fatalf("expected nil api key forwarded, got %q", *fake.lastSet.APIKey)
				}
			} else {
				if fake.lastSet.APIKey == nil {
					t.Fatal("expected non-nil api key forwarded, got nil")
				}
				if *fake.lastSet.APIKey != tc.wantVal {
					t.Fatalf("forwarded api key = %q, want %q", *fake.lastSet.APIKey, tc.wantVal)
				}
			}

			if resp.GetStatus() == nil {
				t.Fatal("response status missing")
			}
			rt := reflect.TypeFor[corev1.EmailServiceStatus]()
			for f := range rt.Fields() {
				if strings.Contains(strings.ToLower(f.Name), "key") &&
					(f.Name != "ApiKeySet" || f.Type.Kind() != reflect.Bool) {
					t.Fatalf("response status exposes key-bearing field: %s %s", f.Name, f.Type)
				}
			}
		})
	}
}

func TestEmailServiceConfigStatusReturnsPresenceNotKey(t *testing.T) {
	fake := &fakeEmailServiceStore{
		status: store.EmailServiceStatus{
			APIKeySet: true, Domain: "mg.example.org", Region: "eu",
			FromAddress: "noreply@example.org", Enabled: false, UpdatedBy: "admin-2",
		},
	}
	h := grpcsvc.NewSettingsHandler(nil, nil).WithEmailServiceStore(fake)

	resp, err := h.EmailServiceConfigStatus(context.Background(), &corev1.EmailServiceConfigStatusRequest{})
	if err != nil {
		t.Fatalf("EmailServiceConfigStatus: %v", err)
	}
	st := resp.GetStatus()
	if st == nil || !st.GetApiKeySet() || st.GetRegion() != "eu" || st.GetUpdatedBy() != "admin-2" {
		t.Fatalf("unexpected status: %+v", st)
	}

	// The gateway-facing status type may only say whether a key is set; no
	// field can hold the key itself.
	rt := reflect.TypeFor[corev1.EmailServiceStatus]()
	for f := range rt.Fields() {
		if strings.Contains(strings.ToLower(f.Name), "key") {
			if f.Name != "ApiKeySet" || f.Type.Kind() != reflect.Bool {
				t.Fatalf("EmailServiceStatus exposes a key-bearing field: %s %s", f.Name, f.Type)
			}
		}
	}
}

func TestGetEmailServiceSecretReturnsFullConfigIncludingKey(t *testing.T) {
	const secret = "key-super-secret-value"
	fake := &fakeEmailServiceStore{
		full: store.EmailServiceConfig{
			APIKey: secret, Domain: "mg.example.org", Region: "us",
			FromAddress: "noreply@example.org", Enabled: true, UpdatedBy: "admin-1",
		},
	}
	h := grpcsvc.NewEmailServiceSecretHandler(fake)

	resp, err := h.GetEmailServiceSecret(context.Background(), &corev1.GetEmailServiceSecretRequest{})
	if err != nil {
		t.Fatalf("GetEmailServiceSecret: %v", err)
	}
	if resp.GetConfig().GetApiKey() != secret {
		t.Fatalf("GetEmailServiceSecret api key = %q, want %q", resp.GetConfig().GetApiKey(), secret)
	}
	if resp.GetConfig().GetDomain() != "mg.example.org" {
		t.Fatalf("GetEmailServiceSecret domain = %q", resp.GetConfig().GetDomain())
	}
}

func TestGetEmailServiceSecretIsOffTheGatewayFacingService(t *testing.T) {
	for _, m := range corev1.SettingsService_ServiceDesc.Methods {
		if strings.Contains(m.MethodName, "Secret") || m.MethodName == "GetEmailServiceSecret" {
			t.Fatalf("gateway-facing SettingsService exposes secret RPC %q", m.MethodName)
		}
	}

	found := false
	for _, m := range corev1.EmailServiceSecretService_ServiceDesc.Methods {
		if m.MethodName == "GetEmailServiceSecret" {
			found = true
		}
	}
	if !found {
		t.Fatal("GetEmailServiceSecret is not registered on EmailServiceSecretService")
	}

	// SettingsHandler can't satisfy the secret service's interface, so it can
	// never be registered to serve the key.
	if _, ok := any((*grpcsvc.SettingsHandler)(nil)).(corev1.EmailServiceSecretServiceServer); ok {
		t.Fatal("SettingsHandler must not implement EmailServiceSecretServiceServer")
	}
}

func TestEmailServiceRPCsFailedPreconditionWithoutStore(t *testing.T) {
	h := grpcsvc.NewSettingsHandler(nil, nil) // no WithEmailServiceStore
	// An unwired store is FailedPrecondition, not Unimplemented: the RPC guard
	// treats Unimplemented as a missing handler.
	_, err := h.EmailServiceConfigStatus(context.Background(), &corev1.EmailServiceConfigStatusRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("EmailServiceConfigStatus without store: want FailedPrecondition, got %v", err)
	}
	_, err = h.SetEmailServiceConfig(context.Background(), &corev1.SetEmailServiceConfigRequest{})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("SetEmailServiceConfig without store: want FailedPrecondition, got %v", err)
	}
}

func TestEmailServiceProviderIsCarriedEveryWay(t *testing.T) {
	fake := &fakeEmailServiceStore{
		status: store.EmailServiceStatus{Provider: "smtp"},
		full:   store.EmailServiceConfig{Provider: "smtp"},
	}
	h := grpcsvc.NewSettingsHandler(nil, nil).WithEmailServiceStore(fake)
	resp, err := h.SetEmailServiceConfig(context.Background(), &corev1.SetEmailServiceConfigRequest{Provider: "smtp"})
	if err != nil {
		t.Fatalf("SetEmailServiceConfig: %v", err)
	}
	if fake.lastSet.Provider != "smtp" {
		t.Fatalf("provider written = %q, want smtp", fake.lastSet.Provider)
	}
	if resp.GetStatus().GetProvider() != "smtp" {
		t.Fatalf("status provider = %q, want smtp", resp.GetStatus().GetProvider())
	}
	sec, err := grpcsvc.NewEmailServiceSecretHandler(fake).GetEmailServiceSecret(context.Background(), &corev1.GetEmailServiceSecretRequest{})
	if err != nil || sec.GetConfig().GetProvider() != "smtp" {
		t.Fatalf("secret provider = %q, %v", sec.GetConfig().GetProvider(), err)
	}
}

func TestEmailServiceKeyNeverLeavesThroughErrorsOrAudit(t *testing.T) {
	const secret = "key-must-not-leak-7f3a"
	cap := &capturePublisher{}
	ok := &fakeEmailServiceStore{status: store.EmailServiceStatus{APIKeySet: true}}
	h := grpcsvc.NewSettingsHandler(nil, audit.New(cap)).WithEmailServiceStore(ok)
	resp, err := h.SetEmailServiceConfig(context.Background(), &corev1.SetEmailServiceConfigRequest{ApiKey: new(secret)})
	if err != nil {
		t.Fatalf("SetEmailServiceConfig: %v", err)
	}
	if strings.Contains(resp.String(), secret) {
		t.Fatal("the response must not carry the key")
	}
	for _, c := range cap.calls {
		if strings.Contains(fmt.Sprint(c.event), secret) {
			t.Fatal("the audit event must not carry the key")
		}
	}
	failing := &fakeEmailServiceStore{setErr: errors.New("connection refused")}
	_, err = grpcsvc.NewSettingsHandler(nil, nil).WithEmailServiceStore(failing).
		SetEmailServiceConfig(context.Background(), &corev1.SetEmailServiceConfigRequest{ApiKey: new(secret)})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("a failed save must not echo the key: %v", err)
	}
}

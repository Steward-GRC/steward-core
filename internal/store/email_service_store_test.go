// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"testing"

	postgres "github.com/Bugs5382/go-postgres"

	"github.com/Steward-GRC/steward-core/internal/fixture"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// A fresh database has the row seeded with no key and every setting empty:
// the adopter picks the provider and region.
func TestEmailServiceStore_Unset_statusReportsNotSet(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()

	st, err := repo.EmailServiceConfigStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.APIKeySet {
		t.Fatal("expected APIKeySet=false on an unset config")
	}
	if st.Region != "" || st.Provider != "" {
		t.Fatalf("expected an empty region and provider by default, got %q and %q", st.Region, st.Provider)
	}
	if st.Domain != "" || st.FromAddress != "" || st.Enabled {
		t.Fatalf("expected empty/disabled non-secret fields, got %+v", st)
	}

	// The internal read likewise returns no key and an empty region.
	cfg, err := repo.GetEmailServiceConfig(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if cfg.APIKey != "" {
		t.Fatalf("expected empty api key on an unset config, got %q", cfg.APIKey)
	}
	if cfg.Region != "" {
		t.Fatalf("expected an empty region by default, got %q", cfg.Region)
	}
}

// The internal read returns the whole config, decrypted key included, for the
// service that sends mail.
func TestEmailServiceStore_SetGet_roundTripsFullConfigIncludingKey(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()

	want := store.SetEmailServiceConfigInput{
		APIKey:      new("key-test-secret"),
		Domain:      "mg.example.org",
		Region:      "eu",
		FromAddress: "no-reply@example.org",
		Enabled:     true,
		UpdatedBy:   fixture.AliceEmail,
	}
	if err := repo.SetEmailServiceConfig(ctx, want); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := repo.GetEmailServiceConfig(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.APIKey != *want.APIKey {
		t.Fatalf("expected api key to round-trip, got %q", got.APIKey)
	}
	if got.Domain != want.Domain || got.Region != want.Region ||
		got.FromAddress != want.FromAddress || got.Enabled != want.Enabled ||
		got.UpdatedBy != want.UpdatedBy {
		t.Fatalf("expected non-secret fields to round-trip, got %+v", got)
	}
}

// The gateway's status view reports whether a key is set and the other
// fields, never the key.
func TestEmailServiceStore_Status_reportsSetWithoutKey(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()

	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{
		APIKey:      new("key-should-not-leak"),
		Domain:      "mg.example.org",
		Region:      "us",
		FromAddress: "ops@example.org",
		Enabled:     true,
		UpdatedBy:   fixture.AliceEmail,
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	st, err := repo.EmailServiceConfigStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.APIKeySet {
		t.Fatal("expected APIKeySet=true after a key was stored")
	}
	if st.Domain != "mg.example.org" || st.Region != "us" ||
		st.FromAddress != "ops@example.org" || !st.Enabled {
		t.Fatalf("expected non-secret fields on the status view, got %+v", st)
	}
}

// An empty region stays empty: there is no provider-specific default.
func TestEmailServiceStore_RegionStaysEmpty(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()

	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{
		APIKey: new("key-1"),
		Domain: "mg.example.org",
		Region: "", // caller left it blank
	}); err != nil {
		t.Fatalf("set: %v", err)
	}

	got, err := repo.GetEmailServiceConfig(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Region != "" {
		t.Fatalf("expected an empty region to stay empty, got %q", got.Region)
	}
}

// A second write overwrites the singleton in place rather than adding a row —
// the boolean primary key enforces exactly one row.
func TestEmailServiceStore_Update_overwritesAndStaysSingleRow(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()

	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{
		APIKey: new("key-first"), Domain: "first.example.org", Region: "us", Enabled: false,
	}); err != nil {
		t.Fatalf("set (first): %v", err)
	}
	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{
		APIKey: new("key-second"), Domain: "second.example.org", Region: "eu", Enabled: true,
	}); err != nil {
		t.Fatalf("set (second): %v", err)
	}

	got, err := repo.GetEmailServiceConfig(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.APIKey != "key-second" || got.Domain != "second.example.org" ||
		got.Region != "eu" || !got.Enabled {
		t.Fatalf("expected the second write to overwrite, got %+v", got)
	}

	var count int
	if err := pool.Pool().QueryRow(ctx, `SELECT count(*) FROM email_service_config`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row in email_service_config, got %d", count)
	}
}

// Three-state: saving with the api key ABSENT (APIKey == nil) writes the
// non-secret fields but leaves the stored key untouched — the presence flag
// stays true and the internal read still returns the original secret.
func TestEmailServiceStore_SetWithKeyAbsent_preservesStoredKey(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()

	// Seed a key plus non-secret fields.
	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{
		APIKey:      new("key-original"),
		Domain:      "old.example.org",
		Region:      "us",
		FromAddress: "old@example.org",
		Enabled:     false,
		UpdatedBy:   fixture.AliceEmail,
	}); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	// Save again with the key ABSENT (nil) but changed non-secret fields.
	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{
		APIKey:      nil, // caller did not send a key
		Domain:      "new.example.org",
		Region:      "eu",
		FromAddress: "new@example.org",
		Enabled:     true,
		UpdatedBy:   fixture.AliceEmail,
	}); err != nil {
		t.Fatalf("update set: %v", err)
	}

	st, err := repo.EmailServiceConfigStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !st.APIKeySet {
		t.Fatal("expected APIKeySet to stay true when the key is absent on save")
	}
	if st.Domain != "new.example.org" || st.Region != "eu" ||
		st.FromAddress != "new@example.org" || !st.Enabled {
		t.Fatalf("expected non-secret fields to update, got %+v", st)
	}

	// The original secret must be intact.
	got, err := repo.GetEmailServiceConfig(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.APIKey != "key-original" {
		t.Fatalf("expected stored key to be preserved, got %q", got.APIKey)
	}
}

// Three-state: saving with the api key set to the empty string CLEARS the
// stored key — presence flips to false — while the non-secret fields persist.
func TestEmailServiceStore_SetWithEmptyKey_clearsStoredKey(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()

	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{
		APIKey:      new("key-to-clear"),
		Domain:      "mg.example.org",
		Region:      "us",
		FromAddress: "ops@example.org",
		Enabled:     true,
		UpdatedBy:   fixture.AliceEmail,
	}); err != nil {
		t.Fatalf("seed set: %v", err)
	}

	// Explicitly clear the key.
	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{
		APIKey:      new(""), // clear
		Domain:      "mg.example.org",
		Region:      "us",
		FromAddress: "ops@example.org",
		Enabled:     true,
		UpdatedBy:   fixture.AliceEmail,
	}); err != nil {
		t.Fatalf("clear set: %v", err)
	}

	st, err := repo.EmailServiceConfigStatus(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if st.APIKeySet {
		t.Fatal("expected APIKeySet=false after clearing the key with \"\"")
	}

	got, err := repo.GetEmailServiceConfig(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.APIKey != "" {
		t.Fatalf("expected stored key to be cleared, got %q", got.APIKey)
	}
}

func newEmailServiceStore(t *testing.T, db *postgres.DB) *store.EmailServiceStore {
	t.Helper()
	box, err := store.NewSecretBox(testSettingsKey)
	if err != nil {
		t.Fatalf("NewSecretBox: %v", err)
	}
	return store.NewEmailServiceStore(db, box)
}

var testSettingsKey = []byte("0123456789abcdef0123456789abcdef")

func TestEmailServiceStore_KeyIsEncryptedAtRest(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()
	const secret = "key-never-stored-in-plain-text"
	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{APIKey: new(secret)}); err != nil {
		t.Fatalf("set: %v", err)
	}
	var raw []byte
	if err := pool.Pool().QueryRow(ctx, `SELECT api_key_ciphertext FROM email_service_config`).Scan(&raw); err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if len(raw) == 0 || bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("the stored key must be ciphertext, got %d bytes", len(raw))
	}
	got, err := repo.GetEmailServiceConfig(ctx)
	if err != nil || got.APIKey != secret {
		t.Fatalf("expected the key to decrypt, got %v", err)
	}
}

func TestEmailServiceStore_OtherSettingsKeyCannotRead(t *testing.T) {
	pool := newTestDB(t)
	ctx := context.Background()
	if err := newEmailServiceStore(t, pool).SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{APIKey: new("key-a")}); err != nil {
		t.Fatalf("set: %v", err)
	}
	other, err := store.NewSecretBox([]byte("fedcba9876543210fedcba9876543210"))
	if err != nil {
		t.Fatalf("NewSecretBox: %v", err)
	}
	if _, err := store.NewEmailServiceStore(pool, other).GetEmailServiceConfig(ctx); err == nil {
		t.Fatal("a different settings key must not decrypt the stored key")
	}
}

func TestEmailServiceStore_ProviderRoundTrips(t *testing.T) {
	pool := newTestDB(t)
	repo := newEmailServiceStore(t, pool)
	ctx := context.Background()
	if err := repo.SetEmailServiceConfig(ctx, store.SetEmailServiceConfigInput{Provider: "smtp", FromAddress: fixture.BobEmail}); err != nil {
		t.Fatalf("set: %v", err)
	}
	st, err := repo.EmailServiceConfigStatus(ctx)
	if err != nil || st.Provider != "smtp" {
		t.Fatalf("status provider = %q, %v", st.Provider, err)
	}
	cfg, err := repo.GetEmailServiceConfig(ctx)
	if err != nil || cfg.Provider != "smtp" {
		t.Fatalf("config provider = %q, %v", cfg.Provider, err)
	}
}

func TestNewSecretBoxNeedsA32ByteKey(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33} {
		if _, err := store.NewSecretBox(make([]byte, n)); err == nil {
			t.Errorf("a %d-byte key must be refused", n)
		}
	}
}

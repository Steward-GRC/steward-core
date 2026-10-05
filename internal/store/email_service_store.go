// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/jackc/pgx/v5"
)

const emailServiceKeyName = "email_service_config.api_key"

// EmailServiceConfig is the outbound email-service configuration with the
// decrypted API key. Only GetEmailServiceConfig fills it, for the internal
// secret RPC; never log it.
type EmailServiceConfig struct {
	APIKey      string
	Domain      string
	Region      string
	FromAddress string
	Enabled     bool
	UpdatedBy   string
	Provider    string
}

// EmailServiceStatus is the keyless view for the gateway. It has no field for
// the key, so the key can't leak through it.
type EmailServiceStatus struct {
	APIKeySet   bool
	Domain      string
	Region      string
	FromAddress string
	Enabled     bool
	UpdatedBy   string
	Provider    string
}

// SetEmailServiceConfigInput is a write. APIKey has three states: nil keeps
// the stored key, "" clears it, anything else replaces it.
type SetEmailServiceConfigInput struct {
	APIKey      *string
	Domain      string
	Region      string
	FromAddress string
	Enabled     bool
	UpdatedBy   string
	Provider    string
}

// EmailServiceStore reads and writes the single email_service_config row.
type EmailServiceStore struct {
	db  *postgres.DB
	box *SecretBox
}

// NewEmailServiceStore returns a store that encrypts the key with box.
func NewEmailServiceStore(db *postgres.DB, box *SecretBox) *EmailServiceStore {
	return &EmailServiceStore{db: db, box: box}
}

// SetEmailServiceConfig writes the configuration.
func (s *EmailServiceStore) SetEmailServiceConfig(ctx context.Context, in SetEmailServiceConfigInput) error {
	keyProvided := in.APIKey != nil
	sealed := []byte{}
	if keyProvided {
		var err error
		if sealed, err = s.box.seal(emailServiceKeyName, *in.APIKey); err != nil {
			return fmt.Errorf("store: encrypt email-service key: %w", err)
		}
	}
	const q = `
		INSERT INTO email_service_config (id, api_key_ciphertext, domain, region, from_address, enabled, updated_by, provider, updated_at)
		VALUES (true, $1, $2, $3, $4, $5, $6, $7, now())
		ON CONFLICT (id) DO UPDATE SET
			api_key_ciphertext = CASE WHEN $8 THEN EXCLUDED.api_key_ciphertext ELSE email_service_config.api_key_ciphertext END,
			domain       = EXCLUDED.domain,
			region       = EXCLUDED.region,
			from_address = EXCLUDED.from_address,
			enabled      = EXCLUDED.enabled,
			updated_by   = EXCLUDED.updated_by,
			provider     = EXCLUDED.provider,
			updated_at   = now()`
	if _, err := s.db.Querier().Exec(ctx, q,
		sealed, in.Domain, in.Region, in.FromAddress, in.Enabled, in.UpdatedBy, in.Provider, keyProvided,
	); err != nil {
		return fmt.Errorf("store: set email service config: %w", err)
	}
	return nil
}

// GetEmailServiceConfig returns the configuration with the decrypted key.
func (s *EmailServiceStore) GetEmailServiceConfig(ctx context.Context) (EmailServiceConfig, error) {
	var c EmailServiceConfig
	var sealed []byte
	const q = `SELECT api_key_ciphertext, domain, region, from_address, enabled, updated_by, provider
	             FROM email_service_config WHERE id = true`
	err := s.db.Querier().QueryRow(ctx, q).Scan(
		&sealed, &c.Domain, &c.Region, &c.FromAddress, &c.Enabled, &c.UpdatedBy, &c.Provider,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return EmailServiceConfig{}, nil
	}
	if err != nil {
		return EmailServiceConfig{}, fmt.Errorf("store: get email service config: %w", err)
	}
	if c.APIKey, err = s.box.open(emailServiceKeyName, sealed); err != nil {
		return EmailServiceConfig{}, err
	}
	return c, nil
}

// EmailServiceConfigStatus returns the keyless view. The key is never read
// out of Postgres here.
func (s *EmailServiceStore) EmailServiceConfigStatus(ctx context.Context) (EmailServiceStatus, error) {
	var st EmailServiceStatus
	const q = `SELECT length(api_key_ciphertext) > 0, domain, region, from_address, enabled, updated_by, provider
	             FROM email_service_config WHERE id = true`
	err := s.db.Querier().QueryRow(ctx, q).Scan(
		&st.APIKeySet, &st.Domain, &st.Region, &st.FromAddress, &st.Enabled, &st.UpdatedBy, &st.Provider,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return EmailServiceStatus{}, nil
	}
	if err != nil {
		return EmailServiceStatus{}, fmt.Errorf("store: email service config status: %w", err)
	}
	return st, nil
}

// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

// Package config reads the core service's settings from the environment.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// SettingsKeySize is the length of the decoded CORE_SETTINGS_KEY.
const SettingsKeySize = 32

// S3 is the object store for editor images. An empty Endpoint and Bucket
// turn editor images off.
type S3 struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	PathStyle bool
}

// TLS is the server certificate and the CA client certificates must chain
// to. Empty serves plain gRPC.
type TLS struct {
	CertFile     string
	KeyFile      string
	ClientCAFile string
}

// Config is every setting the service runs with.
type Config struct {
	DatabaseDSN   string
	MigrateDSN    string // a direct connection for migrations; defaults to DatabaseDSN
	MigrationsDir string
	RabbitURL     string
	GRPCPort      string
	OTLPEndpoint  string
	// SettingsKey encrypts the secrets core stores, such as the email-service
	// key.
	SettingsKey []byte

	RedisAddr     string
	RedisPassword string
	CacheTTL      time.Duration

	S3  S3
	TLS TLS
	// TrustedCallers are the SPIFFE IDs whose forwarded actor is believed.
	// They need TLS with client certificates.
	TrustedCallers []string
}

// Load reads the settings through getenv (os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	or := func(k, d string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return d
	}
	c := Config{
		DatabaseDSN:   getenv("DATABASE_DSN"),
		MigrationsDir: or("MIGRATIONS_DIR", "migrations"),
		RabbitURL:     getenv("RABBITMQ_URL"),
		GRPCPort:      or("GRPC_PORT", "9090"),
		OTLPEndpoint:  or("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4317"),
		RedisAddr:     getenv("REDIS_ADDR"),
		RedisPassword: getenv("REDIS_PASSWORD"),
		S3: S3{
			Endpoint: getenv("S3_ENDPOINT"), Bucket: getenv("S3_BUCKET"), Region: or("S3_REGION", "us-east-1"),
			AccessKey: getenv("S3_ACCESS_KEY"), SecretKey: getenv("S3_SECRET_KEY"),
		},
		TLS: TLS{CertFile: getenv("GRPC_TLS_CERT_FILE"), KeyFile: getenv("GRPC_TLS_KEY_FILE"), ClientCAFile: getenv("GRPC_TLS_CLIENT_CA_FILE")},
	}
	c.MigrateDSN = or("MIGRATE_DSN", c.DatabaseDSN)
	for _, id := range strings.Split(getenv("CORE_TRUSTED_CALLERS"), ",") {
		if id = strings.TrimSpace(id); id != "" {
			c.TrustedCallers = append(c.TrustedCallers, id)
		}
	}

	var errs []error
	if c.DatabaseDSN == "" {
		errs = append(errs, errors.New("DATABASE_DSN is required"))
	}
	if c.RabbitURL == "" {
		errs = append(errs, errors.New("RABBITMQ_URL is required"))
	}
	key, err := base64.StdEncoding.DecodeString(getenv("CORE_SETTINGS_KEY"))
	switch {
	case getenv("CORE_SETTINGS_KEY") == "":
		errs = append(errs, errors.New("CORE_SETTINGS_KEY is required"))
	case err != nil || len(key) != SettingsKeySize:
		errs = append(errs, fmt.Errorf("CORE_SETTINGS_KEY must be %d bytes, base64-encoded", SettingsKeySize))
	default:
		c.SettingsKey = key
	}
	if c.CacheTTL, err = time.ParseDuration(or("CACHE_TTL", "10m")); err != nil || c.CacheTTL <= 0 {
		errs = append(errs, errors.New("CACHE_TTL must be a positive duration"))
	}
	if c.S3.PathStyle, err = strconv.ParseBool(or("S3_FORCE_PATH_STYLE", "true")); err != nil {
		errs = append(errs, errors.New("S3_FORCE_PATH_STYLE must be true or false"))
	}
	if (c.S3.Endpoint == "") != (c.S3.Bucket == "") {
		errs = append(errs, errors.New("S3_ENDPOINT and S3_BUCKET are set together or not at all"))
	}
	tlsSet := c.TLS.CertFile != "" || c.TLS.KeyFile != "" || c.TLS.ClientCAFile != ""
	if tlsSet && (c.TLS.CertFile == "" || c.TLS.KeyFile == "" || c.TLS.ClientCAFile == "") {
		errs = append(errs, errors.New("GRPC_TLS_CERT_FILE, GRPC_TLS_KEY_FILE and GRPC_TLS_CLIENT_CA_FILE are set together"))
	}
	if len(c.TrustedCallers) > 0 && !tlsSet {
		errs = append(errs, errors.New("CORE_TRUSTED_CALLERS needs GRPC_TLS_* with client certificates"))
	}
	return c, errors.Join(errs...)
}

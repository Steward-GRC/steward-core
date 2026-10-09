// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	workloadidentity "github.com/Bugs5382/go-workload-identity"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

const dsn = "postgres://core@db.example.org/core"

var settingsKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func base() map[string]string {
	return map[string]string{
		"DATABASE_DSN": dsn, "RABBITMQ_URL": "amqp://mq.example.org", "CORE_SETTINGS_KEY": settingsKey,
		"WORKLOAD_OIDC_ISSUER": "https://issuer.example.org", "WORKLOAD_ALLOWED_SERVICEACCOUNTS": "steward/steward-gateway",
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(base()))
	require.NoError(t, err)
	require.Equal(t, "9090", c.GRPCPort)
	require.Equal(t, "8080", c.ProbePort)
	require.Equal(t, "localhost:4317", c.OTLPEndpoint)
	require.Equal(t, "migrations", c.MigrationsDir)
	require.Equal(t, dsn, c.MigrateDSN)
	require.Equal(t, []byte("0123456789abcdef0123456789abcdef"), c.SettingsKey)
	require.Empty(t, c.RedisAddr, "the cache is off unless configured")
	require.Equal(t, 10*time.Minute, c.CacheTTL)
	require.Empty(t, c.S3.Endpoint, "editor images are off unless configured")
	require.Equal(t, "us-east-1", c.S3.Region)
	require.True(t, c.S3.PathStyle)
	require.True(t, c.WorkloadAuthEnabled)
	require.Equal(t, "steward", c.WorkloadAuth.Audience)
}

func TestLoadReadsEverySetting(t *testing.T) {
	m := base()
	for k, v := range map[string]string{
		"MIGRATE_DSN": "postgres://migrate@db.example.org/core", "MIGRATIONS_DIR": "/migrations", "GRPC_PORT": "9443", "PROBE_PORT": "8081",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "otel.example.org:4317",
		"REDIS_ADDR":                  "cache.example.org:6379", "REDIS_PASSWORD": "pw", "CACHE_TTL": "1m",
		"S3_ENDPOINT": "http://objects.example.org:9000", "S3_BUCKET": "steward", "S3_REGION": "eu-west-1",
		"S3_ACCESS_KEY": "ak", "S3_SECRET_KEY": "sk", "S3_FORCE_PATH_STYLE": "false",
		"GRPC_TLS_CERT_FILE": "/tls/tls.crt", "GRPC_TLS_KEY_FILE": "/tls/tls.key", "GRPC_TLS_CLIENT_CA_FILE": "/tls/ca.crt",
		"WORKLOAD_OIDC_JWKS_URL": "https://issuer.example.org/openid/v1/jwks", "WORKLOAD_OIDC_CA_FILE": "/oidc/ca.crt",
		"WORKLOAD_OIDC_BEARER_FILE": "/oidc/token", "WORKLOAD_AUDIENCE": "steward",
		"WORKLOAD_ALLOWED_SERVICEACCOUNTS": "steward/steward-gateway, steward/steward-delivery",
	} {
		m[k] = v
	}
	c, err := Load(env(m))
	require.NoError(t, err)
	require.Equal(t, "postgres://migrate@db.example.org/core", c.MigrateDSN)
	require.Equal(t, "/migrations", c.MigrationsDir)
	require.Equal(t, "9443", c.GRPCPort)
	require.Equal(t, "8081", c.ProbePort)
	require.Equal(t, "cache.example.org:6379", c.RedisAddr)
	require.Equal(t, "pw", c.RedisPassword)
	require.Equal(t, time.Minute, c.CacheTTL)
	require.Equal(t, S3{Endpoint: "http://objects.example.org:9000", Bucket: "steward", Region: "eu-west-1", AccessKey: "ak", SecretKey: "sk"}, c.S3)
	require.Equal(t, TLS{CertFile: "/tls/tls.crt", KeyFile: "/tls/tls.key", ClientCAFile: "/tls/ca.crt"}, c.TLS)
	require.Equal(t, workloadidentity.Config{
		Issuer: "https://issuer.example.org", JWKSURL: "https://issuer.example.org/openid/v1/jwks", CAFile: "/oidc/ca.crt",
		BearerFile: "/oidc/token", Audience: "steward", ServiceAccountPrefix: WorkloadServiceAccountPrefix,
		AllowedServiceAccounts: []string{"steward/steward-gateway", "steward/steward-delivery"},
	}, c.WorkloadAuth)
}

func TestLoadFailsClosedWithoutWorkloadAuth(t *testing.T) {
	m := base()
	delete(m, "WORKLOAD_OIDC_ISSUER")
	_, err := Load(env(m))
	require.ErrorIs(t, err, workloadidentity.ErrNotConfigured, "no issuer and no explicit off switch stops the boot")
}

func TestLoadTurnsWorkloadAuthOffOnlyWhenDisabled(t *testing.T) {
	m := base()
	delete(m, "WORKLOAD_OIDC_ISSUER")
	m["WORKLOAD_AUTH"] = "disabled"
	c, err := Load(env(m))
	require.NoError(t, err)
	require.False(t, c.WorkloadAuthEnabled)
}

// A bad value stops the boot instead of quietly falling back to a default.
func TestLoadRejectsBadSettings(t *testing.T) {
	for name, kv := range map[string][2]string{
		"no dsn":                 {"DATABASE_DSN", ""},
		"no broker":              {"RABBITMQ_URL", ""},
		"no settings key":        {"CORE_SETTINGS_KEY", ""},
		"settings key not b64":   {"CORE_SETTINGS_KEY", "not base64!"},
		"settings key too short": {"CORE_SETTINGS_KEY", base64.StdEncoding.EncodeToString([]byte("short"))},
		"bad ttl":                {"CACHE_TTL", "soon"},
		"zero ttl":               {"CACHE_TTL", "0s"},
		"bad path style":         {"S3_FORCE_PATH_STYLE", "maybe"},
		"bucket without host":    {"S3_BUCKET", "steward"},
		"half tls":               {"GRPC_TLS_CERT_FILE", "/tls/tls.crt"},
		"auth mode typo":         {"WORKLOAD_AUTH", "off"},
		"plain http issuer":      {"WORKLOAD_OIDC_ISSUER", "http://issuer.example.org"},
		"bad allow-list entry":   {"WORKLOAD_ALLOWED_SERVICEACCOUNTS", "steward-gateway"},
	} {
		m := base()
		m[kv[0]] = kv[1]
		_, err := Load(env(m))
		require.Error(t, err, name)
		require.True(t, strings.Contains(err.Error(), kv[0]), "%s: the error names %s: %v", name, kv[0], err)
	}
}

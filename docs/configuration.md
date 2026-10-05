# Configuration

Every setting is an environment variable, read once at start-up. A missing required setting or a
value that doesn't parse stops the service with every problem listed; nothing falls back quietly.
`.env.example` has the local defaults.

| Variable | Default | Meaning |
| --- | --- | --- |
| `DATABASE_DSN` | required | Postgres connection for the service. |
| `MIGRATE_DSN` | `DATABASE_DSN` | A direct connection for migrations, when `DATABASE_DSN` goes through a transaction-pooling proxy. |
| `MIGRATIONS_DIR` | `migrations` | Where the SQL migrations are. The image sets `/migrations`. |
| `RABBITMQ_URL` | required | The broker lifecycle and audit events are published to. |
| `CORE_SETTINGS_KEY` | required | 32 random bytes, base64. Encrypts the secrets core stores (the email-service key). |
| `GRPC_PORT` | `9090` | The gRPC listen port. |
| `GRPC_TLS_CERT_FILE`, `GRPC_TLS_KEY_FILE`, `GRPC_TLS_CLIENT_CA_FILE` | empty | Serve mTLS: the server certificate and key, and the CA client certificates must chain to. All three or none. |
| `CORE_TRUSTED_CALLERS` | empty | Comma-separated SPIFFE IDs whose forwarded actor (go-grpc-actor) is believed. Needs mTLS. Empty ignores every forwarded actor. |
| `REDIS_ADDR`, `REDIS_PASSWORD` | empty | The read cache (Redis or Valkey). Off when `REDIS_ADDR` is empty. |
| `CACHE_TTL` | `10m` | How long cached published versions and settings are kept. |
| `S3_ENDPOINT`, `S3_BUCKET` | empty | The object store for editor images (RustFS or any S3 API). Set both or neither; off when empty. |
| `S3_REGION` | `us-east-1` | The region the object store expects. |
| `S3_ACCESS_KEY`, `S3_SECRET_KEY` | empty | Static object-store credentials. |
| `S3_FORCE_PATH_STYLE` | `true` | Path-style bucket addressing, which most self-hosted stores need. |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | `localhost:4317` | The OTLP collector for traces and metrics. |
| `LOG_LEVEL`, `LOG_FORMAT` | go-log's defaults | `trace` to `error`; `console` locally, `json` in every cluster. |

## The settings key

`CORE_SETTINGS_KEY` encrypts the email-service API key in Postgres (AES-256-GCM). Generate it once
with `openssl rand -base64 32`, keep it in your secret store, and back it up with the database: a
restored database is unreadable without it. Changing it means saving the email-service key again.

## Act-as and trusted callers

During act-as, audit events name the admin at the keyboard and keep the target in
`impersonated_user_id`. Core learns both from the actor the calling service forwards, and it only
believes a caller whose verified client certificate carries a SPIFFE ID in `CORE_TRUSTED_CALLERS`.
Without mTLS and that list, the forwarded actor is ignored and events name only the `actor_user_id`
of the request.

## Adopter settings

Nothing about an organisation is built in. The announcement, the maintenance notice and the
email-service configuration (provider, domain, region, sender, key) start empty and are set through
`SettingsService`.

## Migrations

The schema is one baseline, `migrations/0001_baseline.up.sql`, applied with go-postgres at start-up,
before the service connects for work. Installs of the original service come over through
`steward-migrate`.

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
| `PROBE_PORT` | `8080` | Plain HTTP for `/livez` and `/readyz`. |
| `GRPC_TLS_CERT_FILE`, `GRPC_TLS_KEY_FILE`, `GRPC_TLS_CLIENT_CA_FILE` | empty | Serve mTLS: the server certificate and key, and the CA client certificates must chain to. All three or none. |
| `WORKLOAD_OIDC_ISSUER` | required | The cluster's service-account token issuer, an `https` URL that must equal the token's `iss`. |
| `WORKLOAD_OIDC_JWKS_URL` | discovered | The issuer's JWKS, when it isn't at the `jwks_uri` of `<issuer>/.well-known/openid-configuration`. `https` only. |
| `WORKLOAD_OIDC_CA_FILE` | system roots | Extra PEM CA trusted for the discovery and JWKS fetch (the cluster CA). |
| `WORKLOAD_OIDC_BEARER_FILE` | empty | A token sent on the discovery and JWKS fetch, re-read on every fetch (the pod's API token). |
| `WORKLOAD_AUDIENCE` | `steward` | The audience a caller's token must carry. |
| `WORKLOAD_ALLOWED_SERVICEACCOUNTS` | required | Comma list of `<namespace>/<serviceaccount>` that may call core at all, for example `steward/steward-gateway,steward/steward-delivery`. |
| `WORKLOAD_AUTH` | enabled | `disabled` turns caller authentication off, for local runs only. Nothing else turns it off, and it can't be combined with `WORKLOAD_OIDC_ISSUER`. |
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

## Service-to-service authentication

Every call to core carries the calling service's projected Kubernetes service-account token, with
audience `steward`, as `authorization: Bearer <token>`. Core verifies it against the issuer's JWKS
and maps the service account `<namespace>/steward-<name>` to the caller `<name>`. The service
account must be in `WORKLOAD_ALLOWED_SERVICEACCOUNTS`, and the caller must be listed for the method
in core's allow-list (`internal/grpcsvc/callers.go`):

| Caller | Methods | Access |
| --- | --- | --- |
| `gateway` | every method except `EmailServiceSecretService/GetEmailServiceSecret` | on behalf of the signed-in user |
| `delivery` | `PolicyService/GetPolicy`, `GetPolicyVersion`; `AppendixService/ListAppendices` | as itself |
| `workflow` | `CategoryService/GetCategory`; `PolicyService/GetPolicy`, `GetPolicyVersion`, `SetVersionStatus` | as itself |
| `obligations` | `CategoryService/GetCategory`, `GetCategoryRuleset`; `PolicyService/GetPolicy`, `GetPolicyVersion`, `ListPolicyVersions`, `ListObligatingPolicies`, `ResolvePolicyObligation`; `EmailServiceSecretService/GetEmailServiceSecret` | as itself |
| `collab` | `CategoryService/GetCategory`, `PolicyService/GetPolicy` as itself; `PolicyService/UpdateDraftContent` on behalf of the editing user | |

`ai` isn't listed yet: its calls aren't known until it's ported, so it's refused until then.

- **Refusals:** a missing or rejected token is `Unauthenticated`, a caller the method doesn't list
  is `PermissionDenied`, and a verifier that hasn't loaded a key set yet answers `Unavailable`.
  Every refusal is logged and audited as `rpc.denied`, with the caller (or `unauthenticated`) as
  the actor.
- **Open methods:** `grpc.health.v1` and server reflection need no token.
- **Off switch:** `WORKLOAD_AUTH=disabled` is the only way to run without authentication, for
  local runs. Core logs a warning at start-up and every 5 minutes, and readiness reports
  `workloadauth` degraded. With neither `WORKLOAD_OIDC_ISSUER` nor `WORKLOAD_AUTH=disabled`, core
  doesn't start.

## Act-as and forwarded actors

During act-as, audit events name the admin at the keyboard and keep the target in
`impersonated_user_id`. Core learns both from the actor the calling service forwards (go-grpc-actor),
and it only believes it from a verified caller with on-behalf access to the method (the gateway,
and collab saving a draft). Any other caller acts only as itself: its forwarded actor is ignored,
and events name the `actor_user_id` of the request. With authentication disabled, no forwarded
actor is believed.

## Adopter settings

Nothing about an organisation is built in. The announcement, the maintenance notice and the
email-service configuration (provider, domain, region, sender, key) start empty and are set through
`SettingsService`.

## Migrations

The schema is one baseline, `migrations/0001_baseline.up.sql`, applied with go-postgres at start-up,
before the service connects for work. Installs of the original service come over through
`steward-migrate`.

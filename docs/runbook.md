# Runbook

## Start-up

The service applies the baseline migration, connects to Postgres and RabbitMQ, and serves gRPC. A
bad setting stops it with every problem listed. The cache and editor images are optional: when
Redis is unreachable the service logs a warning and reads from Postgres; without `S3_ENDPOINT`,
`AssetService` answers `Unavailable` and everything else works.

## Probes

Readiness follows go-buildinfo's dependency checker. Each check has a 2-second timeout, and a result
is reused for 5 seconds.

| Dependency | Required | When it's down |
| --- | --- | --- |
| `postgres` | yes | Not ready: nothing can be read or written. |
| `rabbitmq` | yes | Not ready: writes can't be audited. |
| `valkey` | no, reported when `REDIS_ADDR` is set | Degraded, still ready: reads go to Postgres. If Valkey was unreachable at start-up, the cache stays off and reports degraded until a restart. |
| `objectstore` | no, reported when `S3_ENDPOINT` is set | Degraded, still ready: only editor images fail. |
| `jwks` | yes, while service-to-service authentication is on | Not ready: no caller can be verified. A good fetch keeps it up for a minute; a failure is retried on the next probe. The verifier keeps its last good key set either way. |
| `workloadauth` | no, reported only with `WORKLOAD_AUTH=disabled` | Always degraded: every caller that reaches the port is served. Never run like this outside local development. |

- **HTTP on `PROBE_PORT` (8080):** `GET /livez` is 200 while the process is up and never checks a
  dependency. `GET /readyz` is 200 while ready and 503 while a required dependency is down; its JSON
  body lists every dependency with its state and error class. There's no plain `/health`.
- **gRPC on `GRPC_PORT`:** `grpc.health.v1` with the service name `liveness` reports the process
  only. The empty name and `readiness` follow readiness. Every `Health/Check` answer carries
  `steward-version`, `steward-commit`, `steward-dep-postgres` (the server version) and
  `steward-depstate-<name>` (`ok`, `degraded` or `down`).
- Never point liveness at a dependency: a database outage would restart every replica.
- Readiness recovers on its own once the dependency is back.

## Common problems

| Symptom | Look at |
| --- | --- |
| `Code 4001: Internal Error` from any RPC | A store call failed. The log line with the same trace id names the `op`. |
| `CATEGORY_NOT_DELETABLE` | The category or a subcategory still holds documents: move or delete them first. |
| `INVALID_CATEGORY_RULE` | The rule named in the message has no subject, a subject where none belongs, or an unknown grant. |
| The email-service key can't be read | `CORE_SETTINGS_KEY` changed since the key was saved. Restore the old key, or save the email-service key again. |
| Act-as events name the target, not the admin | The call didn't come from a caller with on-behalf access: check the gateway's token and that `steward/steward-gateway` is in `WORKLOAD_ALLOWED_SERVICEACCOUNTS`. |
| `Unauthenticated: no workload token` | The caller sent no `authorization` metadata: check its `WORKLOAD_TOKEN_FILE` and the projected token mount (audience `steward`). |
| `Unauthenticated: workload token rejected` | The log line `caller token rejected` gives the reason: wrong `iss` or `aud`, expired, or a service account missing from `WORKLOAD_ALLOWED_SERVICEACCOUNTS`. |
| `PermissionDenied: caller not allowed on this method` | The caller is verified but core's allow-list doesn't list it for the method. The `rpc.denied` audit event names the caller and method. |
| `Unavailable: workload verifier unavailable` | No JWKS has loaded since start: `steward-depstate-jwks`, then the `JWKS refresh failed` log line (CA file, bearer file, issuer URL). |
| No events reach audit | `steward-depstate-rabbitmq` or `/readyz`, then the `audit` exchange and its binding to audit's queue. |

## Backups

Back up Postgres and `CORE_SETTINGS_KEY` together. Editor images live in the object store bucket;
back it up with the database, since `editor_assets` rows point at its keys.

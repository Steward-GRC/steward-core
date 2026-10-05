# Runbook

## Start-up

The service applies the baseline migration, connects to Postgres and RabbitMQ, and serves gRPC. A
bad setting stops it with every problem listed. The cache and editor images are optional: when
Redis is unreachable the service logs a warning and reads from Postgres; without `S3_ENDPOINT`,
`AssetService` answers `Unavailable` and everything else works.

## Probes

- **Liveness:** `grpc.health.v1` with the empty service name. It reports the process only; never
  point liveness at a dependency.
- **Readiness:** `grpc.health.v1` with the service name `readiness`. It turns `NOT_SERVING` while
  Postgres or RabbitMQ is unreachable and recovers on its own, checked every 10 seconds.

## Common problems

| Symptom | Look at |
| --- | --- |
| `Code 4001: Internal Error` from any RPC | A store call failed. The log line with the same trace id names the `op`. |
| `CATEGORY_NOT_DELETABLE` | The category or a subcategory still holds documents: move or delete them first. |
| `INVALID_CATEGORY_RULE` | The rule named in the message has no subject, a subject where none belongs, or an unknown grant. |
| The email-service key can't be read | `CORE_SETTINGS_KEY` changed since the key was saved. Restore the old key, or save the email-service key again. |
| Act-as events name the target, not the admin | The gateway isn't a trusted caller: check mTLS and `CORE_TRUSTED_CALLERS`. |
| No events reach audit | RabbitMQ readiness, then the `audit` exchange and its binding to audit's queue. |

## Backups

Back up Postgres and `CORE_SETTINGS_KEY` together. Editor images live in the object store bucket;
back it up with the database, since `editor_assets` rows point at its keys.

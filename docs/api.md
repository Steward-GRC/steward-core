# API

The API is `steward.core.v1`, in [`proto/steward/core/v1`](../proto/steward/core/v1), with the Go
stubs committed in `gen/go`. Access to every RPC is enforced at the gateway; core records the
`actor_user_id` a request names. Errors carry an `ErrorInfo` ([error codes](error-codes.md)).

| Service | Holds |
| --- | --- |
| `CategoryService` | The category tree, governance and its inheritance, and the ordered access rules. `SetCategoryRuleset` validates rules with steward-authz; `PurgeUserCategoryRules` removes a deleted user's rules. |
| `PolicyService` | Policies and procedures: drafts, publish, versions, diffs, templates, sensitivity, rename, move, owners, acknowledgement settings and reindexing. |
| `TemplateService` | Templates and their versions. |
| `AppendixService` | Appendices of a draft version. |
| `ContactService`, `DefinitionLibraryService`, `ReferenceService` | The shared libraries and each document's attachments. |
| `RelationService` | Related links: a flat, one-way list per document. |
| `SettingsService` | The global settings and the keyless email-service status. |
| `EmailServiceSecretService` | Internal only: the email-service configuration with its key, for the service that sends mail. The gateway never serves it. |
| `AssetService` | Editor images. |

The server also serves `grpc.health.v1` and reflection. On `grpc.health.v1`, `liveness` reports the
process only, and the empty name and `readiness` fail while Postgres or RabbitMQ is down. Every
`Health/Check` answer carries the `steward-version`, `steward-commit`, `steward-dep-<name>` and
`steward-depstate-<name>` headers. `/livez` and `/readyz` serve the same over HTTP on
`PROBE_PORT`; see the [runbook](runbook.md#probes).

## Events out

**Audit.** Every change publishes a `steward.audit.v1.AuditEvent` (steward-audit's contract) to the
`audit` topic exchange, as protobuf binary with the content type
`application/protobuf; proto=steward.audit.v1.AuditEvent` and the routing key `audit.audit`.
Actions are dotted verbs such as `category.created`, `category_rule.updated`, `policy.published`
and `email_service_config.updated`. No event carries a secret or document content.

**Lifecycle.** JSON events on the `jobs` topic exchange, for the services that act on documents:

| Routing key | When |
| --- | --- |
| `policy.published`, `procedure.published` | A version is published, with its section text for indexing. |
| `policy.retired`, `procedure.retired` | A document is retired. |
| `policy.obligation_changed` | A change that may alter who must acknowledge a policy. |

Reindexing sends the published content straight to the AI indexer's queues (`ai.policy.publish`,
`ai.procedure.publish`) through the default exchange, so other `jobs` consumers don't see it again.

## Calling other services

Core calls no other service. It uses one other service's contract: steward-audit's `AuditEvent`,
pinned by commit in [`proto-refs.env`](../proto-refs.env). `scripts/proto-generate.sh` fetches that
proto into the git-ignored `.protos/` and generates `gen/go/thirdparty/audit/v1`; only the stubs are
committed. To try an unmerged audit change, set `STEWARD_AUDIT_PROTO_DIR` to its `proto/` directory.

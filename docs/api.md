# API

The API is `steward.core.v1`, in [`proto/steward/core/v1`](../proto/steward/core/v1), with the Go
stubs committed in `gen/go`. Access to every RPC is enforced at the gateway; core records the
`actor_user_id` a request names. Errors carry an `ErrorInfo` ([error codes](error-codes.md)).

| Service | Holds |
| --- | --- |
| `CategoryService` | The category tree, governance and its inheritance, and the ordered access rules. `SetCategoryRuleset` validates rules with steward-authz; `PurgeUserCategoryRules` removes a deleted user's rules. Siblings have a stored order (`order_index`, from zero under each parent): a new category and a moved one go last, `ReorderCategories` sets the order of one parent's children (empty `parent_id` for the roots) from a list that holds every child exactly once, and `ListCategoryChildren` returns them in that order. A category says which document types it takes (`document_types`: both, the default, policies, procedures, or none for a container of child categories); it is the category's own setting and a child does not inherit it. `CreatePolicy` and `MovePolicy` refuse a type the category does not take with `CATEGORY_REFUSES_TYPE`, `SetCategoryDocumentTypes` refuses a narrowing while live documents of a removed type are in the category with `CATEGORY_HOLDS_DOCUMENTS` (the total and per-type counts are in the error metadata), and `CountCategoryDocuments` counts a category's live policies and procedures. |
| `PolicyService` | Policies and procedures: drafts, publish, versions, diffs, templates, sensitivity, rename, move, owners, acknowledgement settings and reindexing. |
| `TemplateService` | Templates and their versions. |
| `AppendixService` | Appendices of a draft version. |
| `PolicyAppendixService` | The appendices a policy or procedure owns, which belong to the document and not to one of its versions. Each has a version table of its own: at most one `DRAFT` beside the `PUBLISHED` history, and readers see the latest published row. `ListPolicyAppendices` returns the live appendices in order (the retired ones after them with `include_retired`), each with `published`, `draft`, `versions` (oldest first), `published_versions` (newest first, no draft) and `contributor_ids` (everyone who saved or published, each once, in first-save order); the letter (A, B, C) follows the order of the live appendices and is empty while retired. `GetPolicyAppendix` returns one appendix by `id` in the same shape, a retired one included (its letter is empty); an unknown id is `NotFound` and a malformed one `InvalidArgument`, and it is a read that writes no audit event. `CreatePolicyAppendix` starts a draft v1 last in the order, with a blank title becoming `Appendix <letter>` and skipping any title the policy's appendices (retired included) already use. `SaveAppendixDraft` updates the open draft or starts draft vN+1 beside the published row, and a save equal to the published title and content with no draft open makes none. `PublishAppendix` promotes the draft to the next published version. `RetireAppendix` hides an appendix and keeps its history, `RestoreAppendix` puts it back last in the order; neither makes a draft. `ReorderPolicyAppendices(policy_id, ordered_ids)` sets the order of the live appendices and returns them in the new order with their letters: `ordered_ids` must hold every live (not retired) appendix of the policy exactly once, and a missing, repeated, foreign, retired or unknown id is `InvalidArgument` and changes nothing (an unknown policy is `NotFound`). Retired appendices keep no letter and stay out of the order, and a reorder makes no draft and touches no version row. `CheckpointAppendixDraft(id, policy_id, content_json, actor_user_id)` is how the collab room saves an appendix's text on behalf of the person editing, and returns `{accepted, reject_reason}`: it takes no title (an open draft keeps its own, a new draft takes the published row's), updates the open draft or starts draft vN+1, makes no draft when the content equals the published row with none open (accepted, nothing written), records the actor as a contributor once and writes the same `appendix.saved` event as a draft save. Content that is not a JSON document is a refusal (`accepted=false` with a `reject_reason`, nothing stored), a `policy_id` that is not the appendix's is `InvalidArgument`, an unknown `id` is `NotFound`, and a retired appendix is `FailedPrecondition`, as `UpdateDraftContent` answers a version that is no longer the working copy. The collab service may call it on behalf of the editing user, and may call `GetPolicyAppendix` as itself (to resolve an appendix's policy when it issues an appendix room token); it may call no other `PolicyAppendixService` method. Saving or publishing a retired appendix is `FailedPrecondition`, as is publishing with no draft. Core applies no role or reader filtering: it returns the published row and the draft side by side and the gateway decides who sees the draft. Every write names its `actor_user_id`. `AppendixService` and its tables are unchanged. |
| `VersionLinkService` | The library links of a policy version: definitions, references, contact blocks and related policies. The library items are not versioned; the links are, so they go through approval with the version, stay as approved once it is published and are copied into a new draft by `PolicyService.SaveDraft` (and by the draft a rename stages) from the version the appendices copy from. `GetVersionLinks` returns definitions alphabetically, references and contact blocks in the order they were set (each with `used_by_count`), and `related`: the version's own links (`OUTGOING`) followed by the links other policies make to this one (`INCOMING`, read as the inverse type), taken from the published version of the naming policy, so a link in someone's draft shows on the other side only once that draft is published. `editable` is status only (the version is a draft or in review); the caller's permission is the gateway's to check. The four `SetVersion*` calls each replace one kind of link and return the version's links: `FailedPrecondition` for a version that is published, superseded, rejected or archived (linking never creates a draft), `InvalidArgument` for an id named twice, an unknown relationship type, a link to the version's own policy or a repeated policy and type, `NotFound` for a version, library item or policy that does not exist. `ListRelationshipTypes` is a fixed list of five pairs, each with its inverse. A definition title is unique across categories ignoring case and surrounding spaces: `CreateDefinitionEntry` and `UpdateDefinitionEntry` return `AlreadyExists` for a taken title and `InvalidArgument` for an empty one. `RelationService` and the per-document attachment calls are unchanged until the gateway moves. |
| `RelationService` | Related links: a flat, one-way list per document. |
| `ReviewService` | Inline review threads on a policy version, each with one approver, and suggested changes with counters and outcomes on a thread; the gateway passes `actor_user_id` and `actor_side`. |
| `SettingsService` | The global settings and the keyless email-service status. |
| `EmailServiceSecretService` | Internal only: the email-service configuration with its key, for the service that sends mail. The gateway never serves it. |
| `AssetService` | Editor images and files: upload, get, the admin setting of which kinds of file are accepted, and the admin list of stored files (with the documents that link to each, or only the unlinked ones) and delete. A delete removes the bytes, the record and the file card from every document that links to it, with no new version; the only record is the `asset.deleted` audit entry. |

The server also serves `grpc.health.v1` and reflection. On `grpc.health.v1`, `liveness` reports the
process only, and the empty name and `readiness` fail while Postgres or RabbitMQ is down. Every
`Health/Check` answer carries the `steward-version`, `steward-commit`, `steward-dep-<name>` and
`steward-depstate-<name>` headers. `/livez` and `/readyz` serve the same over HTTP on
`PROBE_PORT`; see the [runbook](runbook.md#probes).

## Events out

**Audit.** Every change publishes a `steward.audit.v1.AuditEvent` (steward-audit's contract) to the
`audit` topic exchange, as protobuf binary with the content type
`application/protobuf; proto=steward.audit.v1.AuditEvent` and the routing key `audit.audit`.
Actions are dotted verbs such as `category.created`, `category_rule.updated`, `policy.published`,
`appendix.created`, `appendix.saved`, `appendix.published`, `appendix.retired`, `appendix.restored`, `appendix.reordered`
and `email_service_config.updated`. No event carries a secret or document content.

**Lifecycle.** JSON events on the `jobs` topic exchange, for the services that act on documents:

| Routing key | When |
| --- | --- |
| `policy.published`, `procedure.published` | A version is published, with its section text for indexing. |
| `policy.retired`, `procedure.retired` | A document is retired. |
| `policy.obligation_changed` | A change that may alter who must acknowledge a policy. |
| `policy.break_glass_read` | A document (policy or procedure) was read under a break-glass grant. Obligations tells its owner and the compliance admins. Unique `event_id` per read. |

Reindexing sends the published content straight to the AI indexer's queues (`ai.policy.publish`,
`ai.procedure.publish`) through the default exchange, so other `jobs` consumers don't see it again.

## Break-glass reads

A site admin who would see a document obfuscated can ask identity for a time-boxed break-glass grant
on that one document (the gateway's `breakGlassReveal`). The gateway honours an active grant for that
document only, and before it serves the content it calls `PolicyService.RecordBreakGlassRead` with
the policy id and, for a single version, the version id. The reader is the forwarded actor.

Core then:

- records `policy.break_glass_read` in the audit tier, with the policy number and version id. During
  act-as the actor is the real admin and `impersonated_user_id` names the user acted as, as for every
  other event;
- publishes `policy.break_glass_read` on `jobs` with the document, its owner, the reader and, during
  act-as, the admin.

Both must succeed. If either publish fails, or the service runs without the audit or jobs
publisher, the call is `Unavailable` and the gateway serves nothing: a break-glass read is never
served unrecorded. A version id that belongs to another policy is `InvalidArgument`, and a call
with no forwarded actor is `Unauthenticated`.

## Calling other services

Core calls no other service. It uses one other service's contract: steward-audit's `AuditEvent`,
pinned by commit in [`proto-refs.env`](../proto-refs.env). `scripts/proto-generate.sh` fetches that
proto into the git-ignored `.protos/` and generates `gen/go/thirdparty/audit/v1`; only the stubs are
committed. To try an unmerged audit change, set `STEWARD_AUDIT_PROTO_DIR` to its `proto/` directory.

# Error codes

Every coded gRPC error from the core service carries an `ErrorInfo` with the symbol as its
reason, the domain `core` and the code in `codeNum`. Only user-safe messages reach the
caller; every other code is sent as `Code N: Internal Error`.

| Code | Symbol | Area | Cause | User-safe |
| --- | --- | --- | --- | --- |
| 4000 | `INTERNAL` | core | an uncoded failure inside the core service | no |
| 4001 | `STORE_UNAVAILABLE` | core store | a core Postgres read or write failed; the op metadata names it, the cause is only logged | no |
| 4002 | `CATEGORY_NOT_DELETABLE` | delete category | the category or one of its subcategories still holds documents | yes |
| 4003 | `INVALID_CATEGORY_RULE` | category rules | a category ruleset failed validation | yes |
| 4004 | `CATEGORY_TEMPLATE_LOCKED` | category template lock | a document was created or changed to a template other than the one its category locks | yes |
| 4005 | `TEMPLATE_LOCK_NEEDS_DEFAULT` | category template lock | a lock was set on a category that names no default template | yes |
| 4006 | `MOVE_REFUSED` | move documents | a bulk move was refused whole because a document cannot move: it is retired or the target category does not hold its type; the reason metadata names which | yes |
| 4007 | `CATEGORY_REFUSES_TYPE` | category document types | a document was created in, or moved into, a category that does not accept its type | yes |
| 4008 | `CATEGORY_HOLDS_DOCUMENTS` | category document types | narrowing the accepted document types would strand documents already in the category | yes |

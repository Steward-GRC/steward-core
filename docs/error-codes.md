# Error codes

Every coded gRPC error from the core service carries an `ErrorInfo` with the symbol as its
reason, the domain `core` and the code in `codeNum`. Only user-safe messages reach the
caller; every other code is sent as `Code N: Internal Error`.

| Code | Symbol | Area | Cause | User-safe |
| --- | --- | --- | --- | --- |
| 4000 | `INTERNAL` | core | an uncoded failure inside the core service | no |
| 4001 | `STORE_UNAVAILABLE` | core store | a core Postgres read or write failed; the op metadata names it, the cause is only logged | no |
| 4002 | `CATEGORY_NOT_DELETABLE` | delete category | the category or one of its subcategories still holds documents | yes |
| 4003 | `INVALID_CATEGORY_RULE` | category rules | a category ruleset failed the steward-authz validation | yes |

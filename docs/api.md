# The generated HTTP API

> **Not written yet.** This page will document the full generated surface:
> filtering, sorting, pagination, PATCH semantics, the trash and restore
> endpoints, snapshots, and every error body.
>
> Until it exists, [tutorial.md](tutorial.md#8-use-it) exercises most of it with
> real requests and responses, and [examples/todo](../examples/todo) is a
> working application you can curl.

The shape, in brief. For a table `todo` under `base_path: /api/v1`:

```
GET     /api/v1/todos                    list
POST    /api/v1/todos                    create
QUERY   /api/v1/todos                    search
POST    /api/v1/todos/_search            search (alias, see rig-yaml.md)
GET     /api/v1/todos/{id}               get
PATCH   /api/v1/todos/{id}               update
DELETE  /api/v1/todos/{id}               delete
```

Soft-deletable tables also get:

```
GET     /api/v1/todos/_deleted           the trash
POST    /api/v1/todos/{id}/_restore      restore
```

## Pagination

Every list and search response carries the page's position in the full result
set:

```json
{"data": [...], "pagination": {"offset": 0, "limit": 50, "total": 1}}
```

A limit is always applied: 50 by default, 500 at most. There is no way to ask
for an unbounded read — see [design.md](design.md).

## PATCH semantics

A field left out of the body is left alone. `null` clears it. That distinction
is what makes a partial update expressible at all, and it is why update input
types are wrapped rather than plain pointers.

## Errors

Every failure carries a machine-readable code from a closed set of eight:

| Code | Status | |
|---|---|---|
| `BadRequest` | 400 | Malformed, or a parameter could not be parsed |
| `Unauthorized` | 401 | No valid session or API key was presented |
| `Forbidden` | 403 | The caller is known but not permitted |
| `NotFound` | 404 | No such resource, or it belongs to another tenant |
| `Conflict` | 409 | Conflicts with the current state of the resource |
| `UnprocessableEntity` | 422 | Well-formed but failed validation |
| `RateLimited` | 429 | Retry after the interval in `Retry-After` |
| `TooLarge` | 413 | The body is larger than this endpoint accepts |
| `UnsupportedMediaType` | 415 | The body's content type is not one this endpoint takes |
| `UpgradeRequired` | 426 | Built against an API revision this server no longer serves |
| `Internal` | 500 | Something went wrong on the server |

```json
{
  "code": "UnprocessableEntity",
  "message": "todo is not valid: title CannotBeEmpty: cannot be empty",
  "requestId": "…",
  "fields": {
    "title": {"code": "CannotBeEmpty", "message": "cannot be empty"}
  }
}
```

`fields` is present when the failure was validation, and is shaped like the
request body that failed — one member per field — so a client can put each
message beside the control it belongs to.

Note that a row belonging to another tenant is a **404**, not a 403. A 403 would
confirm the row exists.

## See also

- [tables.md](tables.md) — choosing which operations exist, and adding your own
- [rig-yaml.md](rig-yaml.md#api) — `base_path`, `permissions`, `search_method`
- [auth.md](auth.md) — the authentication endpoints, which are documented in full

## The inbox

A project with a [`notifications:`](rig-yaml.md#notifications) block also serves
five routes rig writes rather than generates, because the tables behind them are
rig's own and are the same in every project:

```
GET    /notifications                 the caller's inbox, newest first
GET    /notifications/_unread-count    the badge, one number
POST   /notifications/{id}/_read       mark one read
POST   /notifications/_read-all        mark what the caller can currently see
DELETE /notifications/{id}             remove one from the inbox
```

They sit outside `api.base_path`, beside the authentication routes, and every one
of them narrows to the caller's own account. None takes a `?scope=` parameter:
there is no widening for an inbox. See [notifications.md](notifications.md).

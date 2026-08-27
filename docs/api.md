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

Tables that keep their previous versions also get:

```
GET     /api/v1/todos/{id}/_versions     the row's history
POST    /api/v1/todos/{id}/_revert       put a previous version back
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

An `Internal` body never says what happened: the message is always "something
went wrong", because the real one names your tables. The cause is written to the
server's log with the same `requestId` that went out in the body — see
[observability.md](observability.md#why-a-500-happened).

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
message beside the control it belongs to. A generated Go client reads it back
already decoded, with one function per call; see
[clients.md](clients.md#when-a-call-is-refused).

Note that a row belonging to another tenant is a **404**, not a 403. A 403 would
confirm the row exists.

## Sending a write twice

A write may carry an `Idempotency-Key`. A server that has seen the key before
answers with what it answered the first time — same status, same bytes — and
adds `Idempotency-Replayed: true`, rather than doing the work again.

The record and the write commit together, so there is no moment where one exists
without the other: a write that failed leaves no record, and its key is free for
the corrected request that follows.

- **The same key with a different body is a 422**, not a replay. A key names one
  request; answering a different one with a stored response would hand a client a
  success describing something it never asked for. For a multipart write the body
  compared is the fields and the path, not the file bytes.
- **The same key against a request still in flight waits, then a 409.** The
  second request blocks until the first commits or rolls back — usually it then
  replays — and gives up after five seconds rather than holding a connection for
  as long as the first one takes.
- **A key is remembered until it is pruned**, which is a day by default. The
  subcommand is already wired: `api.Tasks(…)` carries `prune-idempotency` in
  every project, because every project has the table.

  ```go
  Tasks: api.Tasks(map[string]serve.Task{ /* yours */ }),
  ```

  **Nothing schedules it for you** — it is a subcommand of your binary, and what
  runs it hourly is your deployment's business. Without that cron entry
  `rig_idempotency` keeps every record ever written, and a key reused a month
  later replays instead of writing — which is not what a key is for, since a
  request arriving a day later is not a retry.

- **An upload route is not recorded.** `POST /todos/{id}/cover-file` and its kind
  take the key and ignore it: the body is still arriving when the handler runs,
  and a record would mean holding a transaction open for the whole transfer. A
  create that *carries* a file is recorded — its parts are stored before the
  write begins.

Keys are scoped to the tenant and to the route, so two tenants choosing the same
string is not a collision and one identifier reused across two endpoints is two
records rather than a replay of the wrong one.

A generated Go client does all of this for you — it names every write it might
have to send again; see [clients.md](clients.md#when-the-server-says-not-now).

## How many calls a caller may make

Off unless [`throttle:`](rig-yaml.md#throttle) is in your `rig.yaml`. With it,
every generated route counts the call against whoever made it — the API key, the
account, the tenant, or, for a caller who is not signed in, the address.

Past the limit the answer is `429` with `Retry-After`. Under it, every response
still carries `RateLimit-Limit` and `RateLimit-Remaining`, so a client can slow
down before it is refused rather than after. The Go SDK already reads all three
and retries for you, and hands you the numbers through one callback so you can
slow down first — see [clients.md](clients.md#seeing-the-limit-before-you-hit-it).

```
HTTP/1.1 429 Too Many Requests
Retry-After: 34
RateLimit-Limit: 1000
RateLimit-Remaining: 0
```

Two things worth knowing before you rely on it.

**It is fair-use limiting, not DDoS protection.** A request that gets here has
already cost a connection, a handshake and a goroutine — under a real flood an
application-level limiter is more load on the part that fails first. Put a CDN or
an L7 proxy in front for that. What this stops is a client stuck in a retry loop,
one tenant crowding out the others, scraping, and runaway cost on an API that
calls something metered.

**The count is approximate, and it fails open.** Each replica counts locally and
reconciles with the database periodically, tightening as a caller approaches
their limit — so several replicas can collectively allow a little more than the
configured number. And if the counters cannot be reached at all, requests are
served rather than refused. Both are deliberate; the reasoning, and the knob for
the first, are in [rig-yaml.md](rig-yaml.md#throttle).

**The counters want a cron entry.** `rig_throttle` gains a row per caller per
window; `api.Tasks(…)` carries a `sweep-throttle` subcommand that deletes the
dead ones, the way it carries `prune-idempotency` for keys, but nothing schedules
it. See [rig-yaml.md](rig-yaml.md#throttle).

The [auth endpoints](auth.md) have limits of their own, counted differently —
exactly, out of the audit log — because a login limiter that guessed high or
failed open would be a way in rather than a nuisance.

## The OpenAPI document

The [`openapi`](generators.md) generator writes this whole surface out as an
OpenAPI 3.1 document, from the same compiled document the router is built from —
so it cannot describe an endpoint that does not exist, or spell a path
differently from the one the server answers on.

```yaml
- name: openapi
  out_dir: docs
  options:
    formats: [json, yaml]
```

`docs/openapi.gen.json` and `docs/openapi.gen.yaml`, rewritten on every run.
Every resource endpoint is there, including the trash, the snapshots, the file
routes and your own custom endpoints; so are the live-sync shape routes, the
error responses, and the permission each operation requires.

Two things about it are worth knowing before you publish it.

**Search is documented as its POST alias.** OpenAPI 3.1 has no way to describe an
operation on the QUERY method, so `QUERY /api/v1/todos` appears in the document
as `POST /api/v1/todos/_search`, with the operation's description saying which is
the primary form. One handler serves both, and they answer identically. If you
turned the alias off with `api.search_method: query`, that resource's search is
absent from the document rather than misdescribed, and its tag says so.

**A shape route's 200 is two answers.** A live-sync route answers with a chunk
of the shape or, when the sync service could not be reached and the shape has a
[fallback](electric.md#when-the-sync-service-is-down), with a snapshot in the
same format. The document describes `X-Rig-Sync-Fallback` because that header is
the only thing distinguishing them, and a `503` beside the `502` because a
subscriber already holding a snapshot is asked to wait rather than told the
request failed.

**The `/auth/*` and `/notifications/*` routes are not in it.** They are the same
routes in every project, so rig serves them from a hand-written module rather
than generating them — which means they reach nothing the document is written
from. `info.description` says as much, so a reader can tell the omission is
deliberate. [auth.md](auth.md) documents them in full.

## See also

- [tables.md](tables.md) — choosing which operations exist, and adding your own
- [rig-yaml.md](rig-yaml.md#api) — `base_path`, `permissions`, `search_method`
- [rig-yaml.md](rig-yaml.md#throttle) — how many calls a caller may make
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

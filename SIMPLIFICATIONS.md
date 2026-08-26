# Simplification backlog

Findings from a full-repo review (2026-08-26) hunting for duplication, needless
complexity, and dead code. Each task is written to be workable on its own
branch; the **Files** list is the collision surface — two tasks that name the
same files should not run in parallel. Verification for anything under
`internal/gen/` is mechanical: golden tests plus `make examples` prove the
generated output did not change.

Status: `[ ]` open · `[x]` done

---

## A. Generators (`internal/gen`)

### A1. Unify the copy-pasted IR-selection half of `goclient` / `tsclient` `[ ]`

The language-independent half of the two SDK generators — which resources,
objects, and enums to emit, and what to name them — is copy-pasted.
`queryTypeName` and `fieldsTypeName` are byte-identical, comments included
(`internal/gen/goclient/types.go:117` vs `internal/gen/tsclient/types.go:401`);
so are `searchFilterField`, `routePath`, `exposed()`, `filterObjects()`.
`unclaimedObjects` differs by one map entry. The two `Generate` bodies
(`goclient/goclient.go:95-160`, `tsclient/tsclient.go:115-170`) are the same
60-line loop.

- **Fix:** move the shared helpers into `internal/gen/genutil/surface.go`
  (which already exists for exactly this reason — see `BodyShapeName`'s doc
  comment). Give `unclaimedObjects` a `claimed map[string]bool` seed parameter.
- **Files:** `internal/gen/goclient/{goclient,client,types,methods}.go`,
  `internal/gen/tsclient/{tsclient,types,methods}.go`, `internal/gen/genutil/surface.go`
- **Size:** ~150-200 lines removed. **Risk:** low — golden output unchanged.
- Closes the bug class where the Go SDK and TS SDK disagree about a type name.

### A2. One `Reach` instead of three `reach()`s `[ ]`

The type-reachability walk is written three times with an identical 12-line
`follow`/`visit` core: `goclient/goclient.go:180-229`,
`tsclient/tsclient.go:321-374`, `openapigen/openapigen.go:231-283`. Only the
seeds differ (openapigen adds `enumErrorCode` and skips routeless endpoints;
tsclient adds streamed row columns).

- **Fix:** `genutil.Reach(doc, seeds []string, include func(*ir.Endpoint) bool,
  extra func(visit func([]ir.Field)))`. Three ~50-line functions become three
  ~8-line call sites.
- **Files:** same three generators + `genutil`. **Overlaps with A1 and A3 —
  do A1-A3 together or sequentially.**
- **Size:** ~120 lines. **Risk:** low.
- Closes "an enum reachable in the SDK but missing from the OpenAPI spec".

### A3. Use the IR's indexed lookups instead of hand-rolled scans `[ ]`

`goclient/goclient.go:244-252`, `tsclient/tsclient.go:428-436`, and
`openapigen/openapigen.go:198-215` each hand-roll a linear `object()`/`enum()`
scan. `pkg/ir/document.go:135-148` already provides index-backed
`Document.Object(name)` / `Document.Enum(name)`, and `servicego`, `persistgo`,
`modelgo` already use them.

- **Fix:** one-line delegation, matching `servicego/servicego.go:145`.
- **Files:** same three generators. **Overlaps with A1/A2.**
- **Size:** ~40 lines. **Risk:** minimal.

### A4. Stop emitting the static row cache; move it to `runtime/cache` `[ ]`

`persistgo.cacheHelpers` (`internal/gen/persistgo/cache.go:75-187`) is ~112
lines of `b.L` calls that emit a fixed generic `rowCache[V]` — zero dependence
on the document — into every generated store (~98 lines each, see
`testdata/rowcache/store.gen.go:281-378`). `runtime/cache` already exports
`Bus`, `Topic`, `Map[V]`, `Forgetter`; the missing type belongs beside them.

- **Fix:** add `cache.RowCache[V]` to `runtime/cache`; the generator emits
  `*cache.RowCache[*model.Lesson]` / `cache.NewRowCache[...]` call sites only.
- **Files:** `internal/gen/persistgo/cache.go`, `runtime/cache/`, regenerated
  goldens + `make update-examples`.
- **Size:** ~112 generator lines + ~98 lines per generated project.
- **Risk:** medium — it's the trickiest concurrency code in rig, but the move
  makes it unit-testable as ordinary Go instead of golden-file-only.

### A5. Shared HTTP parameter parsers for `servergo` / `electricgo` `[ ]`

Both generators emit the same static parsers (bool/int/UUID/RFC-3339 time),
down to the wording of the 400 message: `servergo/server.go:607-678` and
`electricgo/base.go:237-297`. `electricgo`'s `fail` (`base.go:219-232`) also
re-emits error mapping `servergo` emits elsewhere.

- **Fix:** a small `runtime/httparg` (or additions to `runtime/serve`) with
  `PathUUID/PathInt/QueryBool/QueryTime/...`; generators emit call sites.
- **Files:** `internal/gen/servergo/server.go`, `internal/gen/electricgo/base.go`,
  new runtime package, goldens, examples.
- **Size:** ~130 emit lines + ~140 lines per generated project. **Risk:** low-medium.

### A6. `persistgo` repository emit cleanups `[ ]`

- The `writableFields → columns/values` loop is emitted verbatim in
  `updateMethod` and `restoreMethod` (`repository.go:984-1004` vs `1508-1519`).
- `deleteMethod` emits the hard-delete chain twice (`1247-1263` and
  `1285-1300`).
- `repository.go:1264-1268` emits `var _ = time.Now` etc. to paper over
  eagerly-registered imports; `gobuf.Import` is lazy by design — move the
  `b.Import` calls into the branches that use them and delete the blanks.
- **Files:** `internal/gen/persistgo/repository.go`, goldens, examples.
- **Size:** ~40 lines + 3 lines of noise per generated repository. **Risk:** low.

### A7. Small generator dedups `[ ]`

- `wrap` is character-identical in `gobuf/gobuf.go:186-205` and
  `tsbuf/tsbuf.go:164-183` — export from `gobuf`, import in `tsbuf`.
- `tsbuf/tsbuf.go:285-292` hand-rolls `sortedKeys`; use
  `slices.Sorted(maps.Keys(...))` like `gobuf.go:265` does.
- `expand(tmpl, res)` identical in `servicego/stub.go:319-327` and
  `electricgo/stub.go:135-143`.
- `jsonTag` identical in `goclient/goclient.go:316-322` and
  `servicego/base.go:354-360`; `modelgo/entity.go:116-129` is a superset.
- `pointerTo` in `goclient/inputs.go:143-148` vs `compile/filter.go:369-377`
  differ only in empty-string handling.
- **Size:** ~60 lines total. **Risk:** minimal.

---

## B. Service modules (`auth`, `notify`, `presence`, `files`, `observe`, `migrate`)

### B1. Extract the outbox: `auth/account` mail vs `notify` delivery `[ ]`

`auth/account/{outbox,dispatch}.go` is a line-by-line twin of `notify`'s
delivery machinery — states, error classification
(`PermanentMailError`/`RetryMailAfter` vs `Permanent`/`RetryAfter`), lease
bookkeeping, backoff identical down to the comments, which say so themselves:
"This is notify.Permanent's twin" (`outbox.go:263`), "notify's nextAttemptAt,
to the line" (`dispatch.go:261`).

- **Fix:** a `runtime/outbox` package holding retry classification, lease
  bookkeeping (`hold`/`forget`/`release`/`ReleaseClaims`), `nextAttemptAt`,
  `budgetFor`, and the claim→send→mark pass behind small `Store`/`Sender`
  interfaces. Callers keep their SQL and their per-row "what to send".
- **Files:** `auth/account/outbox.go`, `auth/account/dispatch.go`,
  `notify/{failure,channel,dispatch,engine}.go`, new `runtime/outbox`.
- **Size:** 400-500 lines. **Risk:** highest on this list — concurrency
  semantics, and it adds a runtime dependency both modules must share. The
  twin-ness may be deliberate module independence; decide that first.

### B2. One HTTP scaffold; fix the error-envelope drift `[ ]`

`notifyhttp` and `presencehttp` are copy-paste (`Handler`, `Options`, `New`,
`with()` claims middleware, `writeJSON`, `writeError`, `decode`). Worse, their
fallback error writer emits a **nested** `{"error":{code,message}}` envelope
(`notifyhttp.go:264`, `presencehttp.go:339`) while `authhttp.go:495` and the
generated server's `DefaultErrorMapper` emit a **flat** `{code,message}` —
a client-visible inconsistency. `authhttp.fail` also re-implements
`DefaultErrorMapper` verbatim, redaction and Retry-After included.
Content-Type differs too (`application/json` vs `; charset=utf-8`).

- **Fix:** `WriteJSON`, `WriteError` (one envelope, flat), `Decode`
  (DisallowUnknownFields + limit reader), `ClaimsMiddleware` in a shared
  runtime package (`runtime/rigerr` or a new `runtime/httpx`); delete the
  copies. Decide the canonical envelope first — flat is what the generated
  server ships.
- **Files:** `notify/notifyhttp/`, `presence/presencehttp/`,
  `auth/authhttp/authhttp.go`, `observe/page.go:702-706`, runtime package.
- **Size:** ~200 lines. **Risk:** medium — the envelope change is
  client-visible for anyone parsing the nested shape.

### B3. `cache.Keyed[V]`: four near-identical cache wrappers in `auth` `[ ]`

`session.TokenCache` (`auth/session/cache.go:34-160`), `apikey.KeyCache` and
`apikey.FailureCache` (`auth/apikey/cache.go`), and `GrantsCache`
(`auth/grantscache.go:66-256`) all wrap `cache.Map` + `cache.Topic` the same
way: nil-for-nil-bus constructor, miss sentinel to avoid caching negatives,
clone-on-way-out, `forget` via `topic.Forget`, nil-safe `drop`.

- **Fix:** `cache.Keyed[V]` in `runtime/cache` with
  `{Bus, Topic, TTL, MaxEntries, Now, Clone}` and
  `Load/Forget/Clear/Drop` + nil-receiver safety; the four become thin aliases.
- **Files:** the four cache files + `runtime/cache/`.
- **Size:** ~350 lines. **Risk:** low-medium (concurrent, but well-tested shape).

### B4. Decide the fate of the in-memory auth stores (~1,140 lines) `[ ]`

`account.Store` is a 30-method interface with one production implementation
(`auth/authpg/account.go`) and a 663-line hand-written fake
(`auth/account/memory.go`) whose only callers are tests. Same pattern smaller
in `auth/session/memory.go` (224), `auth/apikey/memory.go` (116),
`auth/authlog/memory.go` (137). Sibling modules (`notify`, `presence`,
`files`) have no store interface at all and test against real Postgres via the
docker harness.

- **Fix:** move the flow tests onto the docker harness (`internal/authtest`
  already exists) and delete the fakes; optionally collapse the interface to
  the concrete `authpg` stores like the siblings.
- **Files:** `auth/account/`, `auth/session/`, `auth/apikey/`, `auth/authlog/`,
  `auth/authhttp/authhttp_test.go`, `internal/authtest/`.
- **Size:** 1,100+ lines deleted. **Risk:** it's a test-strategy decision —
  docker tests are slower than in-memory ones; decide deliberately.

### B5. Shared Postgres plumbing in `runtime/dbx` `[ ]`

Re-declared per module:

- `type DB interface { dbx.Conn; dbx.Beginner }` with the identical doc
  comment — `notify/store.go:18`, `presence/store.go:21`, `files/store.go:18`.
- `conn(ctx)` tx-or-pool helper ×5 — `notify/store.go:60`, `files/store.go:47`,
  `auth/authpg/authpg.go:55`, `auth/apikey/apikey.go:583,708`,
  `auth/session/session.go:630`.
- `scanner` interface ×3, `nullString`/`nullable`/`deref` ×5.

- **Fix:** add `dbx.Pool` (Conn+Beginner), `dbx.ConnFor(ctx, fallback)`,
  `dbx.Scanner`, `dbx.NullString` to `runtime/dbx`; alias or delete the copies.
- **Also:** `presence` never calls `Begin` — `presence.DB` should be plain
  `dbx.Conn` (this also simplifies `presence/stub_test.go`).
- **Size:** ~80 lines, plus the "which copy did I fix" question. **Risk:** low.

### B6. `migrate`: singular API as one-liners over the plural `[x]`

`Apply` (`migrate/migrate.go:378-406`) and `ApplyAll` (`:211-227`) had the
same body modulo `Up` vs `UpAll`; `Require`/`RequireAll` likewise, identical
error string included. Both halves have callers, so both stayed — the singular
forms now delegate:
`Apply(fsys, opt) → ApplyAll([]Source{{FS: fsys, Dir: opt.Dir, Table: opt.Table}}, opt)`.

- **The message changed, deliberately.** `PendingAll` runs every path through
  `label` (`:270-279`), which prefixes it with `Source.who()` — the literal word
  `migrations` for a set with no name. So `Require`'s refusal now reads
  `...starting with migrations 00001_create.sql` and `Apply`'s failure gains a
  `migrations: ` wrapper. Accepted rather than worked around: it is the plural
  form's behaviour arriving in the singular, and contorting `label` to drop the
  prefix would leak into `UpAll`/`PendingAll` for every unnamed source. Pinned by
  `migrate/migrate_docker_test.go`'s `TestRequireRefusesUntilApplied`.
- **Correction:** `migrate.Version` did **not** have zero callers — it had two,
  both tests (`migrate/options_test.go:55`, `migrate/migrate_docker_test.go:71`).
  Deleted with them. `make test` would not have caught it, since the second is
  behind `//go:build docker`; `make lint` would, because `.golangci.yml` sets
  `build-tags: [docker]`.
- **Also worth knowing:** `make test-docker` skips this module entirely.
  `migrate/migrate_docker_test.go` starts no container of its own — it reads
  `DATABASE_URL` and falls back to port 55440, which under `RIG_DB_ISOLATE` is
  not this checkout's todo database. The suite must be run by hand with a
  `DATABASE_URL` from this checkout's own `rig db url`, and a green
  `make test-docker` says nothing about it.
- **Size:** ~40 lines, not the 90 estimated; `Version` was 13 of them.

### B7. One safe ticker lifecycle for `presence.Sweeper` / `notify.Engine` `[ ]`

Two hand-rolled ticker-goroutine lifecycles with divergent safety:
`presence/sweep.go:55-180` is idempotent and close-safe; `notify/engine.go`'s
`Start` spawns a second goroutine if called twice, and `Close` hangs until ctx
expiry if `Start` was never called.

- **Fix:** `serve.Ticker{Interval, Nudge, Pass}` with safe
  `Start`/`Nudge`/`Close(ctx)` in `runtime/serve`, used by both.
- **Size:** ~120 → ~50 lines, and the `notify.Engine` hazards go away.
- **Risk:** low-medium.

### B8. `observe` small items `[ ]`

- `ReadLogs` (`observe/logread.go:24-45`) and `ReadSpans`
  (`observe/spanread.go:58-73`) are the same tail-decode loop; a generic
  `decodeLines[T](path, max)` removes one.
- **Doc bug:** `observe/logfile.go:219` says "oldest first" but the function
  delegates to `ReadLogs`, which is newest-first. Fix the comment.

---

## C. Runtime & Go client (`runtime`, `rigclient`)

### C1. `throttle.Limiter`: `Allow` and `Take` are one function `[ ]`

The fold loop is byte-identical apart from calling `evaluate` vs `spend`
(`runtime/throttle/throttle.go:137-164` vs `:208-236`); `evaluate`/`spend`
themselves are near-twins.

- **Fix:** unexported `fold(ctx, checks, per func(...) (Decision, error))`;
  `Allow`/`Take` become nil-guard + one call. Optionally merge
  `evaluate`/`spend` behind an `(n, resetAt, allowed)` builder.
- **Size:** ~40 lines. **Risk:** low.

### C2. `rigclient/auth.go`: twelve inlined copies of four helpers `[ ]`

- "call, then install the session" ×4 (`:68-79`, `:124-144`, `:240-260`,
  `:288-309`) — `adopt` (`:540-549`) already does the pair version.
- `append([]CallOption{Anonymous()}, opts...)` ×7 and
  `append([]CallOption{withBearer(...)}, opts...)` ×5.
- API-key mount guard ×3 (`:502-529`) with the same hint sentence.
- **Fix:** `install(res)`, `anon(opts)`, `asIdentity(token, opts)`,
  `needsAPIKeys(route)` (mirroring the existing `needsIdentity`).
- **Size:** ~60 lines. **Risk:** low.

### C3. `rigclient/transport.go` call loop: extract the retry tail `[ ]`

Two verbatim five-statement retry tails (`:209-216`, `:297-304`);
`op.Multipart.rewind(marks)` ×4, `readError(res, rt.Now())` ×4, inside a
150-line loop that is the hardest function in the module to review.

- **Fix:** extract `backoff(ctx, op, marks, wait, cause) error` and hoist the
  status branches (`fallback`, `reauthorize`, `refusal`) into methods.
- **Size:** loop body ~150 → ~40 lines. **Risk:** medium — this is the retry
  engine; the mirror change in TS is D1.

### C4. `electric.Proxy` cleanups `[ ]`

- `answer` (`runtime/electric/proxy.go:451-504`) repeats the
  snapshot-refusal arm twice; extract `trySnapshot(w, r, s) (Snapshot, bool)`.
- `Serve` (`:297-401`) does four jobs; split the "ask upstream under the
  initial deadline" half so the `answered()`/`Stop()` trick is legible.
- `isHopHeader` (`:595-602`) → `slices.ContainsFunc` or a canonical-key map.
- `splitTable` written twice in `fallback.go` (`:71-74`, `:122-125`).
- **Size:** ~60 lines. **Risk:** low-medium.

### C5. Zero-default boilerplate `[ ]`

`if x == 0 { x = Default }` runs ×4 in `electric/proxy.go:241-256`, ×3 in
`throttle/local.go:120-129`, ×2 each in `cache/cache.go` + `cache/bus.go`, ×9
in `rigclient/client.go:242-283` (plus a 3-arm switch that is "first non-empty
of three").

- **Fix:** `Or[T comparable](v, fallback T)` and `First[T comparable](vs ...T)`
  in a tiny shared spot; keep `serve.orDefault` (real negative-means-zero
  semantics) as the special case.
- **Size:** ~50 lines. **Risk:** minimal.

### C6. Dead runtime surface `[ ]`

- `ir.Resource.IsPublic` (`pkg/ir/api.go:660`) — no non-test caller; delete or
  make the generators use it.
- `electric.Where.NotEq` (`runtime/electric/where.go:26-29`) — exported, doc'd,
  never emitted by `electricgo`, never called.
- `tenancy.RequireScope` (`runtime/tenancy/scope.go:73-81`) — keep the doc,
  collapse the body to `if s != ScopeAll { return nil }; return Require(c, wide)`.
- `query.qualified()` written twice (`runtime/query/query.go:107-112`,
  `:317-322`) — one package-level `qualify(table, column)`.
- `serve.bounded` (`serve.go:601-614`) vs `App.run` (`app.go:163-180`) — same
  select-on-ctx pattern; one `await(ctx, f)` helper.

---

## D. TypeScript workspace (`ts/`)

### D1. `client/src/transport.ts`: extract the duplicated retry ladder `[ ]`

The transport-throw path (`:219-237`) and bad-status path (`:276-306`) run the
identical check-repeatable → check-attempts → delay → budget → wait → increment
ladder. Extract `backoffOrThrow(...)` returning the next attempt or throwing;
hoist `attemptsOf(retry)` out of the loop. Mirror of C3.

- **Size:** ~25 lines. **Risk:** low-medium (retry engine).

### D2. Trim `@rig/client`'s public surface `[ ]`

`ts/packages/client/src/index.ts` exports ~15 symbols nothing outside the
package imports and no doc mentions: `asPost`, `isIdempotent`, `writes`,
`isBindable`, `readError`, `parseRetryAfter`, `formatParam`, `retryable`,
`retryDelayMs`, the `DEFAULT_*`/`MAX_*` constants, the `RATE_LIMIT_*` header
names. Exporting plumbing freezes it as SemVer surface. Keep what the
generator emits (verified consumed set includes `METHOD_QUERY`, `send*`,
`pathValue`, `setParam(s)`, `multipart`, `Session`, error predicates).

- **Size:** ~25 export lines. **Risk:** low — verify against
  `internal/gen/tsclient` emit sites before deleting each.

### D3. `electric/src/params.ts`: latent cache-key collision `[ ]`

`paramsCacheKey` hand-rolls `join("&")`, so `{a: "b&c=d"}` and
`{a: "b", c: "d"}` produce the same key. Rebuild on `serializeParams` +
`URLSearchParams` (`q.sort(); q.toString()`), which percent-encodes and
removes the duplication with `serializeParams`. **This one is a bug fix, not
just cleanup.**

- **Size:** ~26 → ~10 lines. **Risk:** low (cache keys change shape once).

### D4. Presence applies the credential twice per heartbeat `[ ]`

`send()` awaits `credential.apply(headers)`; `presence.ts:245-255` then calls
`authorizationOf(runtime)` which builds a second `Headers` and awaits `apply`
again just to read back the same value — with a `Session` credential that can
run the whole stale/exchange path twice per beat.

- **Fix:** have `beat` return `{ answer, authorization }`; delete
  `authorizationOf` (`presence/src/transport.ts:113-117`).
- **Size:** ~20 lines. **Risk:** low.

### D5. TS small items `[ ]`

- `errors.ts:222-225` — the lowercase `x-request-id` fallback is unreachable
  (`Headers.get` is case-insensitive), and both lines hardcode the name past
  `Runtime.requestIdHeader`; pass the configured name in from `transport.ts`.
- `retry.ts:98-104` — replace the doubling loop with
  `Math.min(base * 2 ** (attempt - 2), cap)`.
- `credential.ts:62-83` — `apiKey` is a byte-identical copy of `staticToken`;
  make it `export const apiKey = staticToken;` (or keep both bodies only if
  the types should diverge).
- `presence.ts:320-324` — `others()` chains five array allocations on the
  `useSyncExternalStore` hot path; one loop + sort.

---

## E. CLI & project config (`internal/cli`, `internal/project`)

### E1. Foundation checks: three copies of one control flow `[ ]`

`checkFilesFoundation` / `checkNotificationsFoundation` /
`checkPresenceFoundation` (`internal/cli/load.go:280-395`, plus the
`checkFoundationPresent` variant at `:407-427`) share the whole
enabled → managed-tables → expose-warnings flow; only the anchor path and one
message differ.

- **Fix:** one `checkFoundationBlock(p, set, spec)` driven by a slice of
  `{enabled, expose, anchor, tables, exposeReason}` specs. Makes it impossible
  for a fourth foundation block to forget the expose half.
- **Size:** ~60 lines. **Risk:** low.

### E2. Small CLI/config dedups `[ ]`

- `Configured()` is `!reflect.DeepEqual(x, bare)` in six `internal/project`
  blocks — one generic `configured[T](v, bare T)`.
- `mustProject()` + `!p.UsesContainer()` + the "database.url is set, so rig
  does not manage this database" refusal repeats at
  `internal/cli/db.go:39,95,129,185`.

---

## F. Dead code sweep (verify each before deleting) `[ ]`

Zero references found in code, tests, docs, or generated templates:

| Symbol | Location | Note |
|---|---|---|
| ~~`migrate.Version`~~ | `migrate/migrate.go:358` | Done under B6 — and it had two test callers, not zero. |
| `ir.Resource.IsPublic` | `pkg/ir/api.go:660` | or make generators use it |
| `electric.Where.NotEq` | `runtime/electric/where.go:26` | |
| `password.AtLeast` | `auth/password/password.go` | |
| `account.DefaultSlug` | `auth/account/tenant.go:264` | |
| `(*account.Service).HasPassword` | `auth/account/service.go:885` | |
| `(*files.File).Uploaded` | `files/files.go:102` | |
| `(*blob.Memory).SetClock` | `files/blob/memory.go:43` | |
| `(*apikey.Key).Allows` | `auth/apikey/apikey.go:98` | ⚠ CIDR check — may be a **missing call**, not dead code. Decide, don't just delete. |
| `var _ = dbx.IsNoRows` | `notify/dispatch.go:524` | plus the now-unused import |

---

## Suggested parallel batches

Tasks in one batch touch disjoint files and can run as separate workspaces:

1. **A1+A2+A3** (SDK generator unification — one workspace, they overlap)
2. **B2** (HTTP scaffold + envelope) · **B3** (cache.Keyed) · **B6** (migrate) · **C1** (throttle) · **D1-D5** (all of ts/) · **E1+E2**
3. **A4** and **A5** each touch goldens + examples broadly — run them after
   batch 1 lands, not alongside.
4. **B1** (outbox) and **B4** (memory stores) are decisions before they are
   refactors — do them last, one at a time.
5. **F** (dead code) conflicts with almost everything by nature of touching
   many modules lightly — do it first or last, not in the middle.

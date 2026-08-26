# Simplification backlog

Findings from a full-repo review (2026-08-26) hunting for duplication, needless
complexity, and dead code. Each task is written to be workable on its own
branch; the **Files** list is the collision surface — two tasks that name the
same files should not run in parallel. Verification for anything under
`internal/gen/` is mechanical: golden tests plus `make examples` prove the
generated output did not change.

Status: `[ ]` open · `[x]` done · `[-]` decided against — the entry says why

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

### B2. One HTTP scaffold; fix the error-envelope drift `[x]`

`notifyhttp` and `presencehttp` were copy-paste, and their fallback error writer
emitted a **nested** `{"error":{code,message}}` while `authhttp` and the
generated server emitted a **flat** `{code,message}`. All three now write
`httpx.Error` from the new `runtime/httpx`.

- **The canonical envelope is flat, camelCase, `application/json; charset=utf-8`.**
  Nothing read the nested shape: `rigclient/error.go` and
  `ts/packages/client/src/errors.ts` both decode a flat struct, against which a
  nested body comes out all-zero — so every error predicate answered false and
  the caller saw a status and nothing else.
- **`runtime/httpx` had to be a new package, not additions to `runtime/rigerr`.**
  The error writer needs `throttle.RefusalOf` for the Retry-After, and
  `runtime/throttle` already imports `rigerr` (`go list -deps ./throttle`). In
  `rigerr` it would be a cycle. Said so in the package doc so nobody folds it
  back.
- **The generated `DefaultErrorMapper` stays emitted and does not delegate.** Its
  field names go through the project's `api.json_case`, so a `json_case: snake`
  project answers `request_id`; `httpx.Error` is fixed camelCase, because these
  routes are identical in every project and the browser packages are compiled
  against them once. One struct cannot be both. `httpx.AnswerFor` exists as the
  seam if the *classification* half is ever worth sharing — the encoding half is
  correctly two implementations.

**Three bugs fixed, each demonstrated before the fix:**

1. **`notify.ErrNotFound` answered 500 in every real application.** It was a bare
   `errors.New`, so `rigerr.CodeOf` read `CodeInternal`; the 404 lived only in
   `notifyhttp`'s fallback writer, which every generated project replaces by
   supplying `Fail`. So `POST /notifications/{id}/_read` and `DELETE
   /notifications/{id}` on a missing or foreign row answered
   `500 something went wrong`. It is now `rigerr.NotFound`, so it carries its own
   status wherever it surfaces and there is no mapping left for a caller to be
   missing. (`files` solved the same problem differently, with a `notFound()`
   translator at the service edge — `files/service.go:347`. Either works; the
   sentinel carrying the code is the one that cannot be bypassed.)
2. **`presencehttp.decode` had no limit reader** — the only route in rig that
   would read an unbounded request body. Now `httpx.Decode(r, 1<<16, …)`.
3. **`authhttp.fail` dropped `fields`.** It wrote a `map[string]string`, so
   per-field validation detail had nowhere to go. Latent rather than live —
   nothing in `auth` produces a field-carrying error today — but it is the kind of
   gap only somebody's client finds. Pinned by `runtime/httpx`'s own tests.

- **`observe/page.go` was dropped from scope.** `observe/go.mod` has no rig
  dependency by design, and its `writeJSON` is already the correct form with no
  error writer at all. A new module edge for a five-line function is a worse trade
  than the duplicate.
- **`authhttp` took `WriteJSON`/`Decode`/`WriteError` only, not `Caller`.** Twelve
  handlers call `h.Claims(r)` inline and it has no middleware to replace;
  retrofitting one is a separate change. `presencehttp` kept its
  `(w, r, claims)` handler signature — `presence.Service` takes claims
  explicitly — but now reads them back through `tenancy.FromContext`, which
  *validates*: claims that are well-formed with no tenant are refused here rather
  than written into a row.
- **The test that stops this coming back is `internal/httpxtest`.** It is in the
  root module because that is the only one that can import `rig/auth`,
  `rig/notify` and `rig/presence` at once — there was nowhere a test could stand
  and see all three, which is most of why they drifted. It mounts all three,
  provokes an unestablished caller, and asserts one status, one Content-Type, one
  key set and one code.
- **Files:** new `runtime/httpx/` (+ tests), new `internal/httpxtest/`,
  `notify/notify.go`, `notify/notifyhttp/` (+ its first test file),
  `presence/presencehttp/`, `auth/authhttp/{authhttp,handlers,invite,identity,provision}.go`.
- **Follow-up found, unrelated to this change:**
  `examples/auth/auth_docker_test.go`'s `TestAServiceAccountCannotSignIn` logs in
  as the fixed address `nobody-at-all@example.com`, so the failed-attempt counter
  accumulates in the database across runs. After enough runs against one database
  it flips from `Unauthorized` to `RateLimited` and the test fails, having proved
  nothing about the property it names. Randomize the address or reset the counter.

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

### B4. The in-memory auth stores stay `[-]`

Decided, not deferred. The reasoning is here so nobody reopens it on the line
count alone.

**What was proposed.** Delete `auth/account/memory.go` (663 lines),
`auth/session/memory.go` (224), `auth/apikey/memory.go` (116) and
`auth/authlog/memory.go` (137) — 1,140 exactly — move the tests they back onto
the Docker harness at `internal/authtest`, and optionally collapse
`account.Store` to the concrete `authpg` stores the way `notify`, `presence` and
`files` have no store interface at all.

**Two corrections to the entry as filed.** `account.Store` is **22** methods,
not 30. And the claim that the fakes' "only callers are tests" is true but
undersells one thing and oversells another: nothing outside a `_test.go` file
references any of the four, and no document mentions them — so the "it is
published API somebody depends on" argument is weaker than it looks. They are
exported and doc-commented, and `make godoc-check` covers `./auth`, so they are
surface a project *may* build on; nothing here proves one does.

**The argument that actually holds: what deleting them costs.** The `auth`
module has **237 tests, not one of them behind a build tag, and the whole suite
finishes in under three seconds**. That is what the doubles buy, and it is the
thing a Docker harness cannot give back. `internal/authtest` does not skip when
Docker is absent — it fails, which is the right shape for a suite whose whole
job is the database — so moving 237 tests behind it makes a daemon a
precondition for running the auth tests at all.

**And the division of labour is already written down, correctly.**
`auth/account/dispatch_test.go:15-17`: *"What is proved here is the rules; that
two dispatchers racing over one row behave is a question for a database, and the
Docker suite asks it."* Both halves exist and both are used. The proposal
collapses a split that is doing its job.

**The interface is not the fakes' fault.** 22 methods because the flows are 22
reads and writes, in a vocabulary deliberately not SQL's — `Store.InTx`, and the
`(bool, error)` returns on `RevokeVerification` (`auth/account/store.go:338`)
and `ConsumeVerification` (`:354`), exist so the service can express "a no-op on
a link already used, and say so" without knowing there is a database.
Collapsing to the concrete stores gives `auth/account` a pgx dependency it does
not have. The siblings are not a precedent: `notify`, `presence` and `files` are
each one table and a handful of statements, and none of them has a flow with a
redemption race in it.

**And the Docker half can pass by not running.** Every example suite carries the
same `t.Skipf("no database at %s: %v — run \`rig db up\` first")` pair, and
`make examples` still exits 0 when they all skip. A net that can be absent
without saying so is weaker than one that cannot be.

**The cost of keeping them, honestly.** Two implementations of a 22-method
contract with no conformance harness: nothing runs one suite against both, so
`MemoryStore` can disagree with `authpg.AccountStore` and only `internal/authtest`
would notice, and only on the paths it happens to cover. That has already
happened once — the `accountOrder` slice at `auth/account/memory.go:18-29` exists
because ranging a map made "the tenant they joined first" a coin flip, and its
comment says the quiet part: *"A double that disagrees with the real store about
the property under test is worse than no double."* The 1,140 lines are also
1,140 lines that move whenever `Store` grows a method, with the compiler as the
only thing that says so.

**What would be worth doing instead.** A conformance suite: one table of cases
run against `MemoryStore` in `make test` and against the `authpg` stores under
`-tags docker`. It closes the drift without deleting anything, and it is a
smaller change than either half of what B4 proposed. Not scheduled here;
recorded so the next reader has somewhere to go.

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

### B7. One safe ticker lifecycle for `presence.Sweeper` / `notify.Engine` `[x]`

Two hand-rolled ticker-goroutine lifecycles with divergent safety.
`presence.Sweeper` was idempotent and close-safe; `notify.Engine` was neither.
Both are now `serve.Ticker` (`runtime/serve/ticker.go`), where the four
properties are asserted once.

- **Three hazards in `notify.Engine`, all demonstrated before they were fixed.**
  Two Starts panicked with `close of closed channel` — both goroutines
  `defer close(e.done)`, so `Close` woke both and the second one to return took
  the process down from a shutdown path. `Close` with no prior `Start` waited on
  `e.done` until the context expired, which with the documented
  `app.CloseWithin("notifications", 15*time.Second, …)` is a fifteen-second stall
  and a reported shutdown failure on every deploy for anyone who left dispatching
  to the cron task. And `Close`'s `select`/`default`/`close(e.stop)` was an
  unlocked check-then-act, so two concurrent Closes both took the branch. All
  three are now `notify/engine_lifecycle_test.go`, a file that did not exist —
  `notify` had no lifecycle tests at all.
- **`PassTimeout` defaults to unbounded, and that is the load-bearing decision.**
  Presence bounds its pass by the interval, which reads as simply better and is
  not: `Engine.Dispatch` bounds its own pass by `claimTTL`, five minutes against
  a one-minute interval, so an interval-bounded pass context would cancel every
  dispatch at sixty seconds — cutting sends mid-flight and cutting the
  `ReleaseClaims` after them. Presence passes `PassTimeout: interval` explicitly;
  notify leaves it zero. `TestAZeroPassTimeoutIsUnbounded` is the guard.
- **The `claiming` gate stayed in notify**, not in the Ticker: `StopClaiming` is a
  separate `serve.Drain` step, and a ticker that knew about readiness would be a
  ticker with an opinion about what it is ticking.
- **Follow-up found, not fixed:** the two modules disagree about a non-positive
  `Interval`. `presence` reads a negative as "the cron job owns this" and starts
  nothing; `notify` resolves zero *and* negative to `DefaultInterval`
  (`engine.go:135`), so an operator who turned notify's goroutine off by writing
  `-1` got a dispatcher running every minute. `serve.Ticker` supports "never", so
  the setting is one line away — but adding it would turn a working configuration
  into a silent no-op for anyone relying on the current answer. Left as it is and
  pinned by `TestANonPositiveIntervalIsTheDefaultAndNotNever`, so the asymmetry
  is at least written down.
- **`presence.Sweeper`'s three lifecycle tests pass unchanged**, which was the
  acceptance criterion; they are also ported onto `serve.Ticker` so the
  properties are asserted at the source as well as through the wrapper.
- **Files:** new `runtime/serve/ticker.go` + `ticker_test.go`,
  `presence/sweep.go`, `notify/engine.go`, new
  `notify/engine_lifecycle_test.go`.
- **Cost worth naming:** `notify` and `presence` now depend on `runtime/serve`,
  which imports `pgxpool`, so both gained `puddle/v2` and `golang.org/x/sync` as
  indirects. Two service modules depending on the server-framework package for a
  sixty-line ticker. If that ever bites, the fix is a leaf `runtime/tick` plus
  `type Ticker = tick.Ticker` in `serve` — a zero-cost alias that keeps the name.

### B8. `observe` small items `[x]`

- **Doc bug, and the valuable half.** `observe/logfile.go:218` — not `:219` —
  said `Logs.Read` was "oldest first"; it delegates to `ReadLogs`, which ends in
  `slices.Reverse` and whose own doc says newest first. Every other comment in
  the package agreed with newest-first, so this was the single outlier. The
  behaviour was already pinned sideways by two tests that index `recs[1]` for the
  line written first, but neither was *named* for it — so `Logs.Read` now has
  `TestLogsReadIsNewestFirst`, which is what makes the next reader who notices
  the disagreement fix the comment rather than the code.
- `ReadLogs` and `ReadSpans` were the same tail-decode loop.
  `decodeLines[T](path, max)` now lives beside `tailLines` in
  `observe/spanread.go`; `ReadSpans` is a one-liner and `ReadLogs` is the
  generic plus its TraceID backfill and the reverse. `observe` stays
  dependency-free — nothing here needed a rig import.
- **Size:** ~10 lines. The generic is the smaller half.

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

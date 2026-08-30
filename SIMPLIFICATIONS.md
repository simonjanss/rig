# Simplification backlog

> **Closed, and kept for the reasoning.** Every item here is done or decided
> against; the one deliberate residue is named in A6. New todos go to
> [GitHub issues](https://github.com/simonjanss/rig/issues), not to this file.

Findings from a full-repo review (2026-08-26) hunting for duplication, needless
complexity, and dead code. Each task is written to be workable on its own
branch; the **Files** list is the collision surface — two tasks that name the
same files should not run in parallel. Verification for anything under
`internal/gen/` is mechanical: golden tests plus `make examples` prove the
generated output did not change.

Status: `[ ]` open · `[x]` done · `[-]` decided against — the entry says why

---

## A. Generators (`internal/gen`)

### A1. Unify the copy-pasted IR-selection half of `goclient` / `tsclient` `[x]`

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
- **Done**, except the two `Generate` bodies. They are the same shape but the
  shared loop's body calls six different per-generator file methods, and TS
  appends `electricFile` + `indexFile` where Go appends `authFile`; a
  callback-driven shared `Generate` costs more indirection than the duplication
  did. The selection half — `QueryTypeName`, `FieldsTypeName`,
  `SearchFilterField`, `RoutePath`, `Exposed`, `FilterObjects`,
  `UnclaimedObjects` — is now `genutil`'s, with `claimed` as a seed parameter.

### A2. One `Reach` instead of three `reach()`s `[x]`

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
- **Done** as `genutil.Walk` (`Follow`/`Fields`/`Endpoint`/`Seen`) rather than
  one `Reach` with flags: the recursion is shared, the seeding stays with each
  generator, which is what `openapigen.reach`'s comment was actually defending.
  `tsclient` now walks endpoint headers like the other two — no compiled
  document fills them yet, so no golden moved, and an enum-typed header will
  now reach every output at once.

### A3. Use the IR's indexed lookups instead of hand-rolled scans `[x]`

`goclient/goclient.go:244-252`, `tsclient/tsclient.go:428-436`, and
`openapigen/openapigen.go:198-215` each hand-roll a linear `object()`/`enum()`
scan. `pkg/ir/document.go:135-148` already provides index-backed
`Document.Object(name)` / `Document.Enum(name)`, and `servicego`, `persistgo`,
`modelgo` already use them.

- **Fix:** one-line delegation, matching `servicego/servicego.go:145`.
- **Files:** same three generators. **Overlaps with A1/A2.**
- **Size:** ~40 lines. **Risk:** minimal.
- **Done.** `openapigen`'s `object` turned out to have no caller left once A2
  landed and is deleted rather than delegated.

### A4. Stop emitting the static row cache; move it to `runtime/cache` `[x]`

`persistgo.cacheHelpers` emitted a fixed generic `rowCache[V]` with zero
dependence on the document into every generated store. It is
`cache.RowCache[V]` now, and the generator emits two call sites.

- **The reason it was emitted had already been removed, by B3.**
  `cacheHelpers`'s own doc comment said it could not live in `runtime/cache`
  because "what it adds is not about caching: it is the deferred attachment. A
  `cache.Map` wants its Live function at construction and the bus does not exist
  yet… rig/auth has the same shape in `auth.GrantsCache`, for the same reason and
  with the same three states." B3 took exactly that three-state deferred
  attachment out of `GrantsCache` and made it `cache.Keyed`'s — so the argument
  had been true when it was written and was not true any more, and nothing said
  so. That is the case for reading these comments as claims rather than as
  settled facts.
- **`RowCache` is a `Keyed` and the two places it departs from one, each written
  against Keyed's opposite choice.** They are not incidental:
  - **A write is dropped locally as well as published.** `Keyed.Forget` publishes
    and drops nothing, "because the publisher hears its own notification". The
    row cache does both, because a notification travels out through Postgres and
    back in on the listener's connection, and those moments belong to the caller
    who just wrote — somebody shown the old value after saving has been told
    their write did not happen. The two types are now the only place in rig
    where that trade is made both ways, and each says why.
  - **There is no serving it locally.** `Keyed.ServeLocally` is "attached to no
    channel and holding anyway", which is a real posture for a session cache. It
    is not one for the application's own rows, so `RowCache.Serve(nil)` leaves it
    dead. Reaching that state for a test needs `ServeLocallyForTest`, which is in
    an internal test file, is a function rather than a method, and says why.
- **Six tests where there were none.** The old ones were golden comparisons —
  they prove the text did not move and say nothing about what it does. The
  properties now asserted: an unserved cache holds nothing, a nil bus is the same
  as unserved, a nil receiver reads through and forgets nothing, `Forget` with no
  transaction still drops locally, forgetting an unheld key is fine, a zero TTL
  holds nothing. What still cannot be asserted in `make test` is the publication
  and its atomicity with the writing transaction; that is `internal/authtest`'s,
  through `Keyed`.
- **`cache_test.go`'s `TestAnUncachedTableIsUntouched` was re-aimed, not
  deleted.** It asserts `"runtime/cache"` is absent from a project that caches
  nothing, and that assertion is now more load-bearing than before: the import
  is the whole of what a cached project adds.
- **Size:** 123 generator lines and 102 per generated store, against 114 lines
  of `runtime/cache/rowcache.go` — most of it the prose above — and 130 of test.
  **Risk:** medium, as filed, and the risk was concurrency code. What removes it
  is that the concurrency is `Keyed`'s, which B3 already tested.

### A5. Shared HTTP parameter parsers for `servergo` / `electricgo` `[x]`

`runtime/httpx/param.go`, and both generators emit call sites. Additions to the
package B2 created rather than a new `runtime/httparg`, as filed.

- **What the two generators actually shared, and it was not the functions.**
  `servergo` emitted eight readers that take a `*http.Request`;
  `electricgo` emitted two readers and six parsers that take `(name, raw)`. Only
  four bodies were textually identical. What *was* shared, entirely, is the
  conversion and the sentence the 400 carries — and that sentence is a wire
  contract: it is what a client reads when it sends `limit=soon`. Two copies of a
  promise is one promise that can come to be made two ways. So the split in
  `httpx` follows that: `ParseInt`/`ParseBool`/`ParseUUID`/… take `(name, raw)`
  because the two generators disagree about where `raw` comes from, and the
  `Path*`/`Query*` readers on top are the shapes they need.
- **A reader only one generator needs is still shared**, because the refusal it
  writes is the same refusal.
- **`electricgo`'s `fail` was a third bug of B2's kind, in the place B2 did not
  look.** It re-emitted the classification *and* wrote it with `http.Error` —
  **`text/plain`** — while every other route in the same binary answers the flat
  JSON envelope. Live rather than latent: the fallback runs whenever `OnError` is
  nil, and `examples/linearlite/main.go:443` constructs
  `genelectric.Server{Proxy, GetClaims}` with no `OnError`, which is the only
  real application in the repository. `@rig-ts/client`'s `readError` decodes a flat
  JSON body, against which text is an empty code — so `isUnauthorized()` answered
  false about a 401 from a stream route, which is exactly the failure B2 found in
  the nested envelope. It is `httpx.Fail` now, and `docs/electric.md` says so.
- **One eager-import trap on the way in, of A6's kind.** The first version took
  `b.Import(".../httpx")` at the top of `parseParams`, which put the import into
  every shape file — including the ones with no declared parameters, where
  nothing used it. `gobuf.Import` registers the moment it is called; the import
  moved inside the loop.
- **Size:** 72 emit lines out of `servergo`, 76 out of `electricgo`, ~190 lines
  per generated project. `runtime/httpx/param.go` is 178 including its prose, and
  it has 150 lines of test where the emitted copies had a golden file.

### A6. `persistgo` repository emit cleanups `[x]`

- The `writableFields → columns/values` loop is `writableAssignments`, called by
  `updateMethod` and `restoreMethod`. What differs between them — the restore
  also clears the soft-delete columns and re-stamps the audit ones — stays at the
  call sites. There is a third `writableFields(res, ir.FieldOpUpdate)` loop at
  `repository.go:1654` and it is **not** this shape: it builds an `UpdateInput`
  out of a snapshot row, with `snapshot_ignore` and array handling that have no
  counterpart here.
- **The duplicated hard-delete chain was hiding a bug.** It is `hardDelete` now,
  and only one of the two copies removed the snapshots first. Nothing joins the
  two flags — `ResourceStorage.IsSnapshotable` and `IsSoftDeletable` are
  independent in the IR and the compiler has no rule pairing them, which
  `pkg/ir/accessors_test.go:149` builds a case for — so a table with versions and
  no `deleted_at` emitted a bare `DELETE` against a row its snapshot rows still
  reference, and answered whatever a foreign-key violation reads as. No fixture
  has that shape, which is why no golden moved and why nothing had noticed.
- **The three `var _ =` blanks are gone**, with the eager imports that made them
  necessary. `deleteMethod` named seven packages in one block; only the
  soft-delete path uses `time` and `fmt`, and **nothing at all used `rigerr`** —
  it was imported to be blanked. The generated diff is exactly those three lines
  per hard-delete-only repository, which is what the entry predicted.
- **B5's deferred sixth copy of `conn(ctx)` was not folded in after all.**
  `persistgo/store.go`'s emitted `(*Store).connFor` is a method on the generated
  `Store` over its own pool, not a free function over `dbx.Conn`; replacing it
  with `dbx.ConnFor` at ~200 call sites is a rename with no shared body to
  remove, and it is the wrong company for a branch that also moves the row cache.
  Left open, and this is the record of why.
- **Size:** 40 generator lines and 3 per hard-delete repository, as filed.

### A7. Small generator dedups `[x]`

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
- **Done**, with one exception: `compile/filter.go`'s `pointerTo` stays. The
  compiler must not import a generator package, and its empty-type arm is a
  different contract. `genutil.PointerTo` says so where a reader will look.

---

## B. Service modules (`auth`, `notify`, `presence`, `files`, `observe`, `migrate`)

### B1. Extract the outbox: `auth/account` mail vs `notify` delivery `[x]`

`auth/account/{outbox,dispatch}.go` was a line-by-line twin of notify's delivery
machinery, and both files said so — "this is notify.Permanent's twin", "notify's
nextAttemptAt, to the line". Those comments are now a compiler-checked fact:
`runtime/outbox`.

**Narrowed on the way in, and the narrowing is the whole design.** Only the
DB-free part moved — error classification, the backoff ladder, the pass budget,
and the lease map. Roughly 120 lines, and every one of them had no room to differ
and did not differ.

The claim→send→mark pass **stayed duplicated on purpose**. notify groups a digest
and marks N deliveries per message; auth reads three stores and rotates a token
per row; and their release statements are scoped differently — notify releases the
ids it tracked, auth releases everything the process ever claimed. A shared pass
would be one shape with two bodies and a parameter list longer than either.

- **The entry's risk statement was wrong.** "It adds a runtime dependency both
  modules must share" — both already require `rig/runtime` with a `replace`, so
  there is no new edge. What is genuinely new is *type identity across modules*:
  the two queues now speak one vocabulary, so `notify.IsPermanent` answers true
  for an error `account.PermanentMailError` wrapped, and the reverse. Both used to
  answer false. Nothing depends on either answer and true is arguably the more
  correct one, but it is a semantic change to two published modules and it is
  documented in both.
- **The constants and the three config refusals were deliberately not extracted.**
  The names are published API on both modules, so nothing would be deleted —
  extracting only changes what the right-hand side reads. And the messages differ
  for a reason: notify names the rig.yaml keys (`claim_ttl`, `backoff_cap`), auth
  names the Go fields (`Mail.ClaimTTL`), because one is read by somebody editing
  yaml and the other by somebody editing Go. `notify.NewEngine` panics where
  `account.resolveMail` returns, also on purpose. What the extraction was really
  trying to buy is now `internal/outboxtest`, in the root module because it is the
  only one that can import both: it asserts the six defaults are the same numbers
  and that both schedules span eight hours and three minutes.
- **A real bug fixed on the way, in notify only.** Both `ReleaseClaims`
  implementations did read-ids → `Exec` → clear-the-map. notify's statement is
  scoped by id (`WHERE id = ANY($1)`), so clearing wiped a concurrent pass's
  claims without releasing them — and two passes genuinely can run at once, since
  the in-process goroutine and the cron `serve.Task` both call `Dispatch`. Those
  leases were then left until a TTL meant for crashes ran out. It is now
  `Leases.Drop(held...)`. auth's statement is scoped by claimant, so it really did
  release everything and `Clear` is correct there — the backlog's claim that both
  were affected was wrong. Both are commented so the asymmetry reads as deliberate.
- **`var _ = dbx.IsNoRows` went too** (`notify/dispatch.go`), along with the `dbx`
  import it was papering over. Struck from section F.
- **Bonus dedup:** `abandon` built the same `[]uuid.UUID` the lease map needs, so
  it is `idsOf(abandoned)` now. `mark`'s loop stays, because it also accumulates
  the attempt count.
- **Size:** ~120 lines shared, not the 400-500 estimated, and net roughly zero —
  the doc comments moved with the code and gained a paragraph each. The win is not
  the line count; it is that the two implementations cannot drift, and that
  `runtime/outbox` has tests where the twinned copies had a comment.
- **Verification:** all 16 tests in `examples/auth/{dispatch,mail}_docker_test.go`
  pass with no skips — including `TestACleanShutdownGivesTheWorkBack` and
  `TestAPassHandsBackWhatItCannotSendInsideTheLease`, which are what the lease
  change touches. **Gap worth naming:** nothing exercises two concurrent passes
  end to end, which is precisely the case the `Drop` fix is about. The mechanism is
  covered by `runtime/outbox`'s `Leases` tests; the integration is not.

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
- **The generated `DefaultErrorMapper` keeps its own envelope and shares the
  classification.** Its field names go through the project's `api.json_case`, so a
  `json_case: snake` project answers `request_id`; `httpx.Error` is fixed
  camelCase, because these routes are identical in every project and the browser
  packages are compiled against them once. One struct cannot be both, so the
  *encoding* stays two implementations — but the emitted mapper is now four lines
  over `httpx.AnswerFor` rather than a second copy of `CodeOf`, the `errors.As`
  redaction, `throttle.RefusalOf` and `FieldsOf`. That copy was the one every real
  application actually runs, so leaving it emitted would have meant the drift this
  section is about surviving in the one place it matters most. `errors` and
  `throttle` drop out of the generated imports with it.
  `servergo.TestInternalErrorsAreNotDetailed` now asserts the delegation; the
  redaction itself is asserted against behaviour in `runtime/httpx`.

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

### B3. `cache.Keyed[V]`: four near-identical cache wrappers in `auth` `[x]`

`session.TokenCache`, `apikey.KeyCache`, `apikey.FailureCache` and
`auth.GrantsCache` all wrapped `cache.Map` + `cache.Topic` the same way. They are
now four thin wrappers over `cache.Keyed[V]` (`runtime/cache/keyed.go`).

- **Four decisions, spelled once instead of four times.** Nil-receiver safety
  (`Load` calls through, everything else is a no-op, so no call site withdrawing a
  value needs a condition); never hold a failure (the seam a miss borrows — the
  sentinel stays caller-side, because only the caller knows what to translate it
  back into); clone on the way out (deliberately contradicting `Map.Load`'s own
  recommendation, for the reason `session` already wrote down); and hold nothing
  when the cache cannot withdraw. That last one is where four copies would have
  drifted, and it is the one whose failure mode is silent.
- **`Keyed` took `GrantsCache`'s deferred `Serve`, not the other three's
  nil-for-nil-bus.** The three-state "not attached yet / attached to a dropped
  channel / live" was never a grants quirk — it is the general answer, and
  nil-for-nil-bus is the special case. `NewKeyed` serves immediately when given a
  bus and stays unattached otherwise. `GrantsCache` lost `servedOn`, `live()` and
  its `atomic.Pointer` entirely.
- **`ServeLocally` earns its place.** `auth/grantscache_internal_test.go` used to
  reach into the unexported `served` field to get a cache attached to no channel —
  testing an invariant by writing the field the invariant is about. The state is
  real (one process, no replicas, staleness is your problem) so it now has a name,
  and all three internal test helpers use it.
- **Naming that state found a bug in it.** A locally-served cache is *live*, so it
  holds — but its `Topic` is nil, and `Topic.Forget` on a nil receiver is a working
  no-op. So `Forget`, `Clear` and `ForgetOrDrop`-with-a-transaction published
  nowhere *and* dropped nothing: the one arrangement where a withdrawal silently
  does not happen. Harmless while the state was test-only, not harmless once
  `GrantsCache.ServeLocally` is exported and documented as a single-instance
  posture, where it would have left a revoked role in the map for a full TTL. Both
  now fall back to the local map when there is no bus, and
  `TestForgetOnALocalCacheDropsLocally` asserts the drop rather than only the
  absence of a panic — which is what the first version of that test did, and why it
  passed.
- **The line count went the other way, and the entry's "~350 lines" was wrong in
  both magnitude and sign.** `auth` lost 62 lines; `runtime/cache/keyed.go` is 241,
  most of it the prose that explains the four decisions. Call it **+179 lines of
  production code**, plus 289 lines of tests where the shared shape had none. The
  four wrappers were 704 lines and the majority was always prose about the
  specific value being cached — why a `Token` is cloned and a `struct{}` is not,
  why only a zero failure count is held — and none of that dedups. What was
  actually bought is that the concurrency decisions are in one place with tests
  against them, and that `golangci-lint` then found four methods
  (`TokenCache.forget`, `TokenCache.drop`, `KeyCache.forget`,
  `FailureCache.forget`) that `ForgetOrDrop` had made dead.
- **Three sites moved here from B5**, where the backlog filed them as `conn(ctx)`
  helpers: `auth/apikey/apikey.go:582,703` and `auth/session/session.go:626`. They
  never produce a connection to query on — they choose between publishing an
  invalidation on the ambient transaction and dropping it from a local map, which
  is inseparable from the cache it is about. `dbx.ConnFor` could not absorb them;
  `Keyed.ForgetOrDrop` did. The `apikey.go:703` site keeps its
  `store.InTx(dbx.WithoutTx(ctx), …)` wrapper, which is the whole point of the
  comment above it: a *fresh* transaction, not the caller's.
- **No breaking change.** `TokenCache`, `KeyCache` and `FailureCache` have only
  unexported methods, so the type and its constructor were the whole surface.
  `GrantsCache` keeps all five exported methods and gains `ServeLocally`.
- **Acceptance criterion met:** all eight of `internal/authtest/cache_docker_test.go`
  pass unchanged, which is the only thing that proves a revocation still issues its
  `NOTIFY` on the transaction that revoked and still reaches a second replica. No
  in-memory test can ask that.

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

### B5. Shared Postgres plumbing in `runtime/dbx` `[x]`

Five additions to `runtime/dbx`, and the copies deleted:

```go
type Pool interface { Conn; Beginner }
type Scanner interface{ Scan(dest ...any) error }
func ConnFor(ctx context.Context, fallback Conn) Conn
func Null[T comparable](v T) *T   // nil at T's zero value
func Deref[T any](p *T) T
```

- **`DB` stayed as a type alias in each module** — `type DB = dbx.Pool` in
  `notify` and `files`, `type DB = dbx.Conn` in `presence`. All three generators
  emit `<pkg>.DB`, so an alias means zero generator, golden or example churn. The
  triplicated two-sentence doc comment now lives once, on `dbx.Pool`.
- **`presence.DB` narrowed to `dbx.Conn`.** Nothing in presence opens a
  transaction — every statement goes straight through `s.cfg.DB`, and
  `presence/service.go` already said so in prose. Narrowing a parameter-position
  interface is source-compatible for every implementor, so this breaks nobody: a
  pool still satisfies it. Two test stubs lost a `Begin` they only had to satisfy
  the wider interface.
- **`auth/authpg.conn` was deleted outright**, not wrapped: it already had
  `dbx.ConnFor`'s exact signature, so its 50 call sites just point at the shared
  one. `notify` and `files` kept their `conn(ctx)` *methods* as one-liners over
  it, because there are 36 call sites in `notify` alone and seven of them are in
  `dispatch.go` — B1's file. The wrapper was the collision boundary that let the
  two land in one branch.
- **The `any` vs `*string` split in the null helpers was not real.** All seven
  call sites feed a pgx variadic `...any`, so both spellings erase to `any` at the
  call. One generic pair on the `*T` shape, which is the form that already ran
  against real Postgres in `internal/authtest`.
- **The one real risk, and its proof.** `dbx.Null(b.Target.ID)` puts a
  `*uuid.UUID` on the wire where an untyped nil went before.
  `internal/presencetest`'s `TestTheTargetNarrows` is a table over `Target{}`,
  `{Table}`, `{Table,ID}` and `{Table,ID,Field}` that round-trips each one; all
  five sub-cases pass, so pgx handles the pointer transparently. `internal/authtest`
  covers the `authpg` side over `user_agent`, `time_zone`, `email_address` and
  `api_key_ref`.
- **Two counts in the entry as filed were wrong.** `conn(ctx)` was ×3, not ×5 —
  the three sites at `auth/apikey/apikey.go:582,703` and
  `auth/session/session.go:626` are not tx-or-pool at all. They never produce a
  connection to query on; they choose between publishing an invalidation on the
  ambient transaction and dropping it from a local map, which is inseparable from
  the cache it is about. They belong to **B3**, as `Keyed.ForgetOrDrop`. And there
  were five `scanner` sites, not three: two of them were inline anonymous
  interfaces in `notify/store.go` and `notify/inbox.go`.
- **`Scanner` was kept rather than naming `pgx.Row`**, which is literally the same
  interface. `pgx` is `// indirect` in both `files/go.mod` and `notify/go.mod`, and
  naming it makes it direct — one four-line declaration holds pgx out of two
  modules' direct requirements. Honest sizing: this sub-item saves about four
  lines and is the weakest part of B5.
- **A sixth copy exists and was deliberately left alone.**
  `internal/gen/persistgo/store.go:246-260` emits `(*Store).connFor` into every
  generated project. Changing it drags goldens and `make update-examples` into a
  branch that touches neither — which is the property that made all of section B
  fit in one branch. Folded into A6.
- **Coverage gap worth knowing:** there is no `internal/filestest`, though
  `internal/dockerdb/ports.go:47-49` reserves a port and describes the suite. So
  `files/store.go` has no Postgres coverage at all. Mitigated here by the fact
  that files' two helpers were already `*string`, making this a pure rename — but
  nothing else should go into that file until the suite exists.
- **Size:** ~90 lines removed, plus the "which copy did I fix" question.

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
Both are now `tick.Ticker` (`runtime/tick/tick.go`), where the four
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
  `-1` got a dispatcher running every minute. `tick.Ticker` supports "never", so
  the setting is one line away — but adding it would turn a working configuration
  into a silent no-op for anyone relying on the current answer. Left as it is and
  pinned by `TestANonPositiveIntervalIsTheDefaultAndNotNever`, so the asymmetry
  is at least written down.
- **`presence.Sweeper`'s three lifecycle tests pass unchanged**, which was the
  acceptance criterion; they are also ported onto `tick.Ticker` so the
  properties are asserted at the source as well as through the wrapper.
- **Files:** new `runtime/tick/tick.go` + `tick_test.go`, `presence/sweep.go`,
  `notify/engine.go`, new `notify/engine_lifecycle_test.go`.
- **It is a leaf package, and that was not optional.** The first version of this
  was `runtime/serve/ticker.go`, which put `pgxpool` and `puddle` into `notify`'s
  and `presence`'s module graphs — `serve` builds the pool, so it imports
  pgxpool — for the sake of sixty lines of `time.Ticker`. Both go.mod files gained
  `puddle/v2` and `golang.org/x/sync` as indirects, and `dbx.Pool`'s promise that a
  module taking one "never imports pgxpool" stopped being true for exactly the two
  modules it names. `runtime/tick` is a leaf over `context`, `sync` and `time`, the
  same argument `runtime/outbox` makes for itself, and both go.mod files are back
  to what they were. No alias was left behind in `serve`: it would have had no
  caller, and an export with no caller is what this document is for.

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

All of C landed on one branch, in this order: the pins, `ir.Resource.IsPublic`,
C1, C4 with C5's electric half, C6's remainder, C2, C3's two helpers, C5's
rigclient half. Nothing in it reached a generator, so `make examples` reported the
generated code unchanged throughout — the same thing that made B one branch.
`tenancy.RequireScope` keeps its signature for exactly that reason.

Four bullets below are struck rather than done, and one is corrected. The
paragraph under each says why. A review is not a specification, and four of these
were wrong about the code.

### C1. `throttle.Limiter`: `Allow` and `Take` are one function `[x]`

The fold loop is byte-identical apart from calling `evaluate` vs `spend`
(`runtime/throttle/throttle.go:137-164` vs `:208-236`); `evaluate`/`spend`
themselves are near-twins.

- **Fix:** unexported `fold(ctx, checks, per func(...) (Decision, error))`;
  `Allow`/`Take` become nil-guard + one call. ~~Optionally merge
  `evaluate`/`spend` behind an `(n, resetAt, allowed)` builder.~~
- **Size:** ~40 lines. **Risk:** low.

**The optional half is struck.** The builder's parameters are exactly the
decisions the current code comments on — `n < Max` against `n <= Max`, counting a
window against incrementing one, a reset derived against one reported — and they
would become positional arguments where a swapped bool is a silent off-by-one in
a rate limiter. The differing nil guards stay above the shared call, because they
are the only thing the two verbs disagree about before the loop.

### C2. `rigclient/auth.go`: twelve inlined copies of four helpers `[x]`

- "call, then install the session" ×4 (`:68-79`, `:124-144`, `:240-260`,
  `:288-309`) — ~~`adopt` (`:540-549`) already does the pair version.~~ **It does
  not.** `adopt` goes through `Session.replace`, which keeps the refresh token in
  hand when the incoming pair carries none. That is right for a tenant switch;
  for a sign-in it would hand B's session A's refresh token. They are `install`
  and `adopt`, two functions, each doc naming the other. There is a fifth site
  this entry missed: `AcceptInvitation` (`:329-344`), the same shape over a bare
  `TokenPair`.
- `append([]CallOption{Anonymous()}, opts...)` ×7 and
  `append([]CallOption{withBearer(...)}, opts...)` ×5.
- API-key mount guard ×3 (`:502-529`) with the same hint sentence.
- **Fix:** `install(res)`, `anon(opts)`, `asIdentity(token, opts)`,
  `needsAPIKeys(route)` (mirroring the existing `needsIdentity`).
- **Size:** ~60 lines. **Risk:** low.

**It also fixed a panic.** `Do` returns nil on a 204, which `adopt` and `list`
both honour and the five install sites did not — so a login or register answered
204 dereferenced nil inside the SDK.

### C3. `rigclient/transport.go` call loop: extract the retry tail `[x]`

Two verbatim five-statement retry tails (`:209-216`, `:297-304`);
`op.Multipart.rewind(marks)` ×4, `readError(res, rt.Now())` ×4, inside a
150-line loop that is the hardest function in the module to review.

- **Fix:** extract `backoff(ctx, op, marks, wait, cause) error` and ~~hoist the
  status branches (`fallback`, `reauthorize`, `refusal`) into methods~~ hoist
  `reauthorize` into `rt.refresh`.
- **Size:** loop body ~~~150 → ~40~~ **107 → 84** lines. **Risk:** medium — this
  is the retry engine; ~~the mirror change in TS is D1~~ D1 is not a mirror of
  this, see below.

**Two of the three branches stay inline.** Extracting the fallback needs `(Op,
error)` back plus a `fellBack` the caller sets anyway — more total code than the
eleven lines it replaces, and the read-before-drain ordering stops sitting beside
the identical ordering below it. Extracting the refusal arm needs seven
parameters, five of them loop state, and shrinking that means a parameter object
holding the attempt accounting, which is what the loop is written not to bury.
The type assertion on `Reauthorizer` also stays in the loop: a 401 for a
credential that cannot refresh has to fall through to the refusal arm with its
body untouched, and that is only obvious while nothing sits between the `ok` test
and the `continue`.

**D1 is not this change on the other side.** Both helpers here exist for a
response body that is closed, drained or handed on by hand and for a form that
has to be seeked back to where it started; TypeScript has neither problem, and
mirroring would produce two functions whose doc comments describe machinery that
is not there. The divergence is recorded on `Runtime.call` so the next person
diffing the two files knows it was decided. D1 stays open on its own terms — the
duplicated ladder there is real — but it is not this.

### C4. `electric.Proxy` cleanups `[x]`

- `answer` (`runtime/electric/proxy.go:451-504`) repeats the
  snapshot-refusal arm twice; extract `trySnapshot(w, r, s) (Snapshot, bool)`.
- ~~`Serve` (`:297-401`) does four jobs; split the "ask upstream under the
  initial deadline" half so the `answered()`/`Stop()` trick is legible.~~
- `isHopHeader` (`:595-602`) → ~~`slices.ContainsFunc` or~~ a canonical-key map.
- `splitTable` written twice in `fallback.go` (`:65-69`, `:127-131` — the
  anchors here were stale).
- **Size:** ~60 lines. **Risk:** low-medium.

**The `Serve` split is struck, and it would have been a bug.** `defer cancel()`
holds the context `p.client.Do` used, and `res.Body` is copied a hundred lines
later; an extracted "ask upstream" function fires that defer on return and
cancels a body somebody is still reading. Making `cancel` escape puts the timer's
creation in one function and its `Stop` in another, which is the opposite of the
stated goal — the trick is legible precisely because `answered()` and `io.Copy`
are visible in the same function.

**`slices.ContainsFunc` is struck too**: it allocates a closure per header per
response and still scans. The map is keyed the way `net/http` canonicalizes, so
note `"Te"` — the RFC's `"TE"` would match nothing, silently, on every response.

**What `trySnapshot` was actually worth** was not the seven duplicated lines but
the `if r.Context().Err() != nil { return }` that appeared twice with no comment
at either site. It has a reason and now says it.

### C5. Zero-default boilerplate `[x]`

~~`Or[T comparable](v, fallback T)` and `First[T comparable](vs ...T)` in a tiny
shared spot.~~ **They are `cmp.Or`**, standard library since 1.22, which this
repository's own `servergo` already emits into generated code. A hand-rolled pair
would have to be exported from `runtime`, doc-gated by `make godoc-check`, and
compiled by every generated application forever — and reaching it from
`rigclient` would have meant a new module edge for a two-line function.

Most of the sites this entry names are not that shape, which is the other half of
why it shrank to two files:

- `electric/proxy.go:241-256` — three of the four are `== 0` because a negative
  value is documented and read back (`no timeout`, `no bound`, `never opens the
  circuit`). Those three are `cmp.Or` now; the fourth keeps its `<= 0` and a
  comment saying why it is the odd one out. **Done.**
- `rigclient/client.go:242-283` — four of the eight guards fit, plus the 3-arm
  switch, which is `cmp.Or(a, b, Default)`. The other three cannot: two fall back
  to a value to build rather than a constant, and `now` is a func type, which is
  not comparable. **Done.**
- `throttle/local.go:102-110` (not `:120-129`) — all three are `<= 0`. `cmp.Or`
  would let a negative through and reconcile on every request. **Left.**
- `cache/cache.go` — `MaxEntries` is `<= 0`; `Now` is a func type. **Left.**
- `cache/bus.go` — only `Channel` fits: one conversion in a four-guard block,
  plus an import for it. **Left.**
- `serve.orDefault` — the backlog is right, and its `v < 0 → 0` arm is something
  `cmp.Or` cannot express. **Left.**

No `atLeast[T cmp.Ordered]` for the `<= 0` sites. It would differ from `cmp.Or`
only in whether a negative survives, which is the distinction that is load-bearing
three inches away in `electric.New` — two helpers whose difference is invisible at
the call site is how that confusion gets institutionalised rather than fixed.

### C6. Dead runtime surface `[x]`

- `ir.Resource.IsPublic` (`pkg/ir/api.go:660`) — no non-test caller (**no caller
  at all**); ~~delete or make the generators use it~~ **deleted**. The
  alternative is struck: `Resource.Public` is the pre-expansion declaration and
  `Endpoint.Public` is the union, because `applyconfig` sets the endpoint flag
  straight from a custom endpoint's own `public:` without touching the resource
  slice. A generator taught to use `IsPublic` would emit an auth check on a route
  the project declared open.
- ~~`electric.Where.NotEq` (`runtime/electric/where.go:26-29`) — exported,
  doc'd, never emitted by `electricgo`, never called.~~ **Kept.** `electricgo`
  emits only `Eq`, `EqText`, `IsNull` and `NotNull`, so "never emitted by the
  generator" condemns `Gt`, `Gte`, `Lt`, `Lte` and `In` equally. `Where` is the
  builder a hand-written scoping stub fills in; deleting the one negation while
  `Gt` keeps `Lte` is an asymmetry that comes back as a surface change. What is
  actually missing there is coverage — `NotEq`, `Gt`, `Lt` and `Lte` have no
  test — and that is a smaller, better item than a deletion.
- `tenancy.RequireScope` (`runtime/tenancy/scope.go:73-81`) — keep the doc,
  collapse the body. **Done**, signature untouched: `servergo` emits this call
  into every scoped handler, so a shape change would move three examples'
  generated output.
- `query.qualified()` written twice (`runtime/query/query.go:106-112`,
  `:292-298` — the second anchor was a call site, not the definition) — one
  package-level `qualify(table, column)`. **Done.** The hazard was not the six
  duplicated lines but the two identical doc comments, which can drift with
  nothing to catch it. The two methods stay as forwarders; rewriting the five
  call sites would make each longer and net zero.
- ~~`serve.bounded` (`serve.go:601-614`) vs `App.run` (`app.go:163-180`) — same
  select-on-ctx pattern; one `await(ctx, f)` helper.~~ **Struck.** (`App.run` is
  in `serve/app.go`, same package, so it was possible.) A correct `await` has to
  report which select arm fired or `bounded` mislabels a phase that legitimately
  returned `ctx.Err()`. That is eight lines to take two callers from 14+16 to
  9+11: **30 lines become 28**. Everything interesting stays outside it — the
  per-step `WithTimeout`, the two error shapes, the two timeout sentences (both
  asserted on) — and the two doc comments explain *why* the wait is bounded
  differently and correctly, so a third restatement would go on `await`.

---

## D. TypeScript workspace (`ts/`)

### D1. `client/src/transport.ts`: extract the duplicated retry ladder `[x]`

The transport-throw path and the bad-status path ran the identical
check-repeatable → check-attempts → delay → budget → wait → increment ladder.
Both are now `backoffOrThrow`, which waits and answers what attempt it is, or
throws the caller's own error.

~~Mirror of C3~~ — C3 landed and this is not the other half of it. Its two
helpers are about a response body closed by hand and a form seeked back to where
it started, and TypeScript has neither. The ladder duplicated here was real and
worth extracting; it was just its own item. See the note under C3.

- **Two parameters, because the two sites disagree about exactly two things.**
  What makes a failure worth repeating at all — `!isAbort(err, signal)` on the
  transport side, `retryable(res.status)` on the server's — and what the server
  asked to be waited. Everything under those is one ladder. Both are computed at
  the call site as a plain boolean, so the helper has no opinion about which
  kind of failure it is looking at.
- **The invariants travel as one `Ladder` object built before the loop**, rather
  than as four more parameters. This is C3's lesson applied on the other side:
  the reason C3 left its refusal arm inline was a seven-parameter signature with
  five of them loop state. Here only `attempt` is loop state, and it is the
  return value.
- **`attemptsOf(retry)` is hoisted**, which it had to be anyway — it was already
  being called once before the loop for the idempotency-key decision and twice
  more inside it, three calls to answer one question that cannot change.
- **The `MAX_RETRY_AFTER_MS` ceiling is now on both paths, and that is a no-op
  rather than a change.** The transport-throw site passes `retryAfterMs: 0`,
  and no server said anything, so there is nothing to refuse.
- **Size:** loop body 107 → 84 lines; the helper is 20 including the reason
  it exists. **Risk:** low-medium (retry engine) — all 18 of
  `transport.test.ts` pass unchanged, which is what the risk was about.

### D2. Trim `@rig-ts/client`'s public surface `[x]`

Sixteen exports gone: `asPost`, `isIdempotent`, `writes`, `isBindable`,
`readError`, `parseRetryAfter`, `formatParam`, `retryable`, `retryDelayMs`, the
four retry `DEFAULT_*`/`MAX_*` constants and the three `RATE_LIMIT_*` header
names. Each is still exported from its own module — this is the entry point's
surface, not the package's internals.

- **How the consumed set was established**, since a `grep` over `ts/` is the
  wrong instrument: the generator is a consumer that writes TypeScript from Go
  string literals. Three sources, all of them checked. `b.Import` and
  `b.ImportType` call sites in `internal/gen/tsclient/*.go` — ten values and six
  types, and `emitter.ref`/`refValue` only ever name the project's own generated
  modules, never `@rig-ts/client`. Every `from "@rig-ts/client"` block in the goldens
  and in `examples/linearlite/web` — the same set plus `Session`, `TokenPair`,
  `isConflict`. And `docs/`, where `fraction` and `fieldsAs` appear and none of
  the sixteen do. The generated `index.ts` names `RigError`, `Session` and
  `paginate` in its own doc comment as what a caller reaches for; all three
  stay.
- **Two the entry named that stayed, and the reason is the same one.**
  `DEFAULT_REQUEST_ID_HEADER` and `DEFAULT_REVISION_HEADER` are the documented
  defaults of two `Config` fields — a caller comparing what it is about to set
  against what it would get — and `tsclient.go:56` pins the second one by name
  in a comment. They are not plumbing; they are the answer to a question the
  configuration raises. `Op` also stays: it is what `send` takes, and the
  predicates over it are what went.
- **The entry point now says what it is for**, in a paragraph at the top,
  because "why is this not exported" is the question a reader will have and the
  answer is not derivable from the list.
- **Verification:** `make ts` plus `make linearlite-web`, which is the one real
  application compiled against this surface — 302 modules, `tsc --noEmit` and a
  production build.
- **Size:** 22 export lines. **Risk:** low, and the risk is not backwards: this
  is a removal from a published package's entry point, so it is a breaking
  change for anybody who had reached for one. Nothing in this repository had.

### D3. `electric/src/params.ts`: latent cache-key collision `[x]`

`paramsCacheKey` hand-rolled `join("&")`, so `{a: "b&c=d"}` and
`{a: "b", c: "d"}` produced the same key. It is `serializeParams` +
`URLSearchParams` now (`q.sort(); q.toString()`), which percent-encodes and
removes the duplication with `serializeParams`. **This one was a bug fix, not
just cleanup.**

- **What the collision actually did.** `createCollectionCache` keys on
  `${runtime.origin}|${paramsCacheKey(params)}`, and a collection is a live
  stream over a shape. Two collections asking for different rows shared one
  instance, so one of them was silently handed the other's rows — a wrong
  answer, not a slow one. Reachable from any param a person can type into.
- **The sort changed from `localeCompare` to `URLSearchParams.sort`**, which
  orders by code unit. Only stability matters — nobody reads this key — and a
  key that depends on the reader's locale was the weaker of the two.
- **Two tests where there were none for the property**, asserting that
  `{a: "b&c=d"}` and `{a: "b", c: "d"}` are now different keys. The existing
  "stable across literal order" test passes unchanged, which is the whole reason
  to keep it.
- **Size:** 12 → 12 lines of code, and 12 of comment saying why. **Risk:** low
  (cache keys change shape once).

### D4. Presence applies the credential twice per heartbeat `[x]`

`send()` awaited `credential.apply(headers)`; `presence.ts` then called
`authorizationOf(runtime)`, which built a second `Headers` and awaited `apply`
again just to read back the same value — with a `Session` credential, the whole
stale-check-and-exchange path, twice per beat, in every open tab.

`send` now returns `{ res, authorization }` — the header it already built, read
back off itself — and `beat` returns `{ answer, authorization }`.
`authorizationOf` is gone.

- **The rationale moved with the value.** The doc comment explaining why the
  leave cannot ask for a credential itself — `apply` may be async, and a page
  being unloaded may not outlive a promise — now sits on `Beat.authorization`,
  which is the thing that exists because of it.
- **Two tests, and the second one is the one that matters.** That the credential
  is applied once per beat and not twice is the fix; that the leave still sends
  the beat's `Authorization` is the property the fix could have broken, and
  dropping an apply that nothing checked would have dropped the header with it.
- **Nothing here was public.** `presence/src/transport.ts` is not re-exported
  from the package's `index.ts`, so `beat`, `send` and `authorizationOf` were
  internal and this is not a surface change.
- **Size:** ~20 lines. **Risk:** low.

### D5. TS small items `[x]`

- **`errors.ts` — a latent bug, not just an unreachable line.** The lowercase
  `x-request-id` fallback was indeed dead (`Headers.get` is case-insensitive by
  specification), but the half worth fixing is the other one: both lines
  hardcoded `X-Request-Id` past `Runtime.requestIdHeader`, so a project that
  renamed the header had the answer read back under the default name and lost
  the identifier from **every** refusal — in exactly the projects that cared
  enough to rename it. `readError` now takes the configured name, which
  `transport.ts` passes from `rt.requestIdHeader`. A required parameter rather
  than one defaulting to `DEFAULT_REQUEST_ID_HEADER`, because a default here is
  a second place the default lives; D2 had just taken `readError` off the entry
  point, so there was no published signature to keep. Two tests: a renamed
  header is read, and a server answering in lowercase still is.
- **`retry.ts` — the doubling loop is `Math.min(base * 2 ** …, cap)`.** With the
  exponent clamped at 32 rather than left to run: `attempts` is a caller-supplied
  number with no ceiling, and past about a thousand `2 ** n` is `Infinity`, which
  reaches the cap correctly for a positive base and answers **`NaN`** for
  `baseMs: 0` — which `?? DEFAULT_RETRY_BASE_MS` lets through, since `0 ?? x` is
  `0`. The loop it replaced had the mirror-image problem: for the same input it
  spun a thousand times to reach the answer it started with.
- **`credential.ts` — `apiKey` is `staticToken`.** An alias rather than a second
  body, with the doc naming the condition under which it stops being one.
- **`presence.ts` — `others()` is one pass.** Four chained `.filter`/`.map` calls
  and a sort became a loop and a sort. It is read back after every commit, which
  is what `useSyncExternalStore` does, so each link was another array as long as
  the room. The identity-cache below it is unchanged; that one is a correctness
  requirement rather than an optimization, and its own comment says so.

---

## E. CLI & project config (`internal/cli`, `internal/project`)

### E1. Foundation checks: three copies of one control flow `[x]`

`checkFilesFoundation`, `checkNotificationsFoundation` and
`checkPresenceFoundation` are one `checkFoundationBlock(p, set, block)`, driven
off a `foundationBlocks(p)` slice of `{name, enabled, expose, tables,
exposeReason}`.

- **The two messages turned out to be one message each.** Both differ only in
  the block name and the table, and the expose one in a single clause naming
  what the generated write path would be a way to do — full CRUD over a storage
  key, a write path over what somebody was told, a Create with a body in it.
  That clause is `exposeReason`, and it is the only per-block prose left.
- **`checkFoundationPresent` stays as it is, and the entry was wrong to file it
  as a variant of the same flow.** It answers a different question — not "is
  this block's table missing" but "is there any foundation at all" — so it tests
  `len(managed) == 0` rather than membership, its message names no table because
  none is missing in particular, its guard is `!Enabled || Own` rather than
  `!Enabled`, and it has no expose half because `auth.expose` is a list of
  tables rather than a switch. Folding it in would mean three flags to express
  one twelve-line function, and the flags would be read on every other block's
  behalf.
- **The test that makes the point is new**, because there was none:
  `TestEveryFoundationBlockReportsBothHalves` runs both halves against all three
  blocks and asserts each names its own block, its own table, its own remedy and
  its own reason. Before this, no test read any of the six messages — the only
  coverage was `files_test.go` following the advice one of them gives, which
  cannot notice a block that gives none.
- **Which is what the item was actually about.** The two halves are not equally
  important: a missing migration fails on the first request either way, while an
  exposed table with no configuration turns a table rig wrote into a resource
  with a generated write path over it. In three hand-written copies that second
  half is the one a fourth block would leave out. It is now impossible to add a
  block without it — there is nowhere to put the entry that does not carry it.
- **Size:** 118 lines become 112, so effectively nothing. The doc comments
  moved rather than merged, which is right: each block's danger is its own and
  none of that prose deduplicates. What was bought is the control flow and the
  test.

### E2. Small CLI/config dedups `[x]`

- **`configured[T any](v, bare T)` in `internal/project/config.go`**, and the
  six blocks call it. `reflect` went from six files to one, and the paragraph
  explaining what the question actually is — everything except the fields that
  say whether the thing exists and how its tables are generated — is written
  once instead of implied six times. Each site still names its own `bare`,
  because only the block knows which of its fields are not configuration.
- **The `db.go` count was four and the truth is two.** `db down` and `db reset`
  are identical: `mustProject`, `!UsesContainer`, and the same one-sentence
  refusal. They are `e.managedProject()` now. The other two are not the same
  check. **`db up` does not refuse at all** — pointed at a database rig does not
  manage, applying the migrations is the useful thing to do, and its refusal is
  about `database.electric`, a sync service rig would have to run itself, with
  its own three-clause message. **`db url` is `!dockerdb.Isolated() ||
  !p.UsesContainer()`**, a different condition, and it answers rather than
  refusing. Folding either in would have changed behaviour under cover of a
  dedup.
- **Size:** ~20 lines, and the point is not the lines. One refusal sentence in
  one place is a sentence that cannot come to be worded two ways, which is what
  two copies of it were on their way to.

---

## F. Dead code sweep (verify each before deleting) `[x]`

"Zero references found in code, tests, docs, or generated templates."
**Re-checked, row by row, and the claim held for three of the ten.** Two rows
were struck by B and two by C; of the six that were still open, three have
callers — `password.AtLeast` at `auth/password/password.go:169`,
`account.DefaultSlug` at `auth/account/tenant.go:191`, and
`apikey.Key.Allows` at `auth/apikey/apikey.go:427`, which also settles the ⚠:
it is not a missing call. Two were deleted under D and one is struck below.

That is a hit rate of three in ten for a table whose whole content is a claim
about references, and it is the reason for the parenthesis in this heading.
A `grep` that misses a call is not a rare accident here — the names are short,
some of them are ordinary English words, and one of the consumers writes Go and
TypeScript out of string literals.

| Symbol | Location | Note |
|---|---|---|
| ~~`migrate.Version`~~ | `migrate/migrate.go:358` | Done under B6 — and it had two test callers, not zero. |
| ~~`ir.Resource.IsPublic`~~ | `pkg/ir/api.go:660` | Done under C6. The "or make generators use it" half was wrong: it answers half the question `Endpoint.Public` answers. |
| ~~`electric.Where.NotEq`~~ | `runtime/electric/where.go:26` | Kept under C6 — `electricgo` emits no comparison but `Eq`, so this argument condemns `Gt`/`Gte`/`Lt`/`Lte`/`In` too. It wants a test, not a deletion. |
| `password.AtLeast` | `auth/password/password.go` | |
| `account.DefaultSlug` | `auth/account/tenant.go:264` | |
| ~~`(*account.Service).HasPassword`~~ | `auth/account/service.go:885` | **Kept.** Genuinely uncalled, and deleted anyway would have been the wrong call — see below. |
| ~~`(*files.File).Uploaded`~~ | `files/files.go:102` | Done under D — confirmed unreferenced anywhere, deleted. |
| ~~`(*blob.Memory).SetClock`~~ | `files/blob/memory.go:43` | Done under D, and it took more with it than the row says — see below. |
| `(*apikey.Key).Allows` | `auth/apikey/apikey.go:98` | ⚠ CIDR check — may be a **missing call**, not dead code. Decide, don't just delete. |
| ~~`var _ = dbx.IsNoRows`~~ | `notify/dispatch.go:513` | Done under B1 — the `dbx` import went with it, which is what the blank was papering over. |

**`HasPassword` has no caller and stays.** It is the only one of `account.Service`'s
twelve exported methods with no route in `authhttp`, no test and no caller, and
that pattern is what made it look dead. It is also the only one whose doc
comment names the flow it is for and argues against the alternative: an
application deciding between "set a password" and "change your password" would
otherwise fetch the credential to look, and be an application holding a hash it
has no use for. `auth/account` is a published module and its `Service` is what
an application embeds; that rig itself has no route for a question a UI asks is
not evidence the question is not asked. This is B4's argument at one-hundredth
the size, and it comes out the same way. What it wants is a test, which it does
not have.

**`SetClock` was a seam that did not do what its comment said.** The field it
wrote — `Memory.now` — was read in one place, `Put`'s `ModTime`. The doc comment
said it was "for a test that needs to age an object", but ageing an object is
`Mark(ctx, key, state, at)`, whose clock comes from the caller and always did.
So the deletion took the `now` field and `clock()`'s nil branch with it,
`Put` reads `time.Now().UTC()` directly, and what went was not just an unused
method but a second, non-functioning answer to a question `Mark` already
answers. Nothing was made harder to test.

---

## Suggested parallel batches

**All of section B landed on one branch**, in this order: B6, B8, B4, the auth
test net, B7, B2, B5, B3, B1. Two things made that possible and are worth keeping
if the same is attempted for another section. Nothing in B reached a generator, so
`make examples` reported the generated code unchanged throughout — the moment that
stops being true, goldens and examples enter the branch and it stops being one
branch. And where two items wanted the same file, one of them kept a one-line
wrapper as the boundary: `notify`'s `store.conn(ctx)` over `dbx.ConnFor` is why B5
never touched `dispatch.go`, which is B1's.

What follows describes the rest.

1. **A1+A2+A3** (SDK generator unification — one workspace, they overlap)
2. ~~**C1** (throttle) ·~~ **D1-D5** (all of ts/) · **E1+E2** — all of C went on
   one branch instead; see the note at the top of that section. D1 is no longer
   coupled to C3.
3. **A4, A5 and A6 went on one branch**, after D and E, and grouping them was
   the point: each one moves generated output, so each would otherwise drag
   `make update-golden` and `make update-examples` through a merge of its own.
   Done in that order — A6 first because it is the smallest and its diff is three
   lines per file, then A5, then A4 — with the goldens regenerated and read after
   each, so that every moved line had one item to belong to.
4. **F** (dead code) was done across D and E rather than on its own. Three of its
   six open rows turned out to have callers, two were deleted, one is struck with
   its reasoning. Doing it inside the branches that already touched those modules
   cost nothing; doing it alone would have been a branch touching six modules to
   delete three methods.

**Everything in this document is now closed.** The one item left open on purpose
is named in A6: the emitted `(*Store).connFor`, which B5 deferred and A6 declined,
with the reason in both places.

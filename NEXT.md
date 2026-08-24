# rig — where things stand, and what is left

Working notes, not documentation. The approved plan is at
`~/.claude/plans/dazzling-purring-flame.md`; this file records what actually got
built, where it departed from that plan, and what is still to come.

---

## Done: M0 through M5

| | |
|---|---|
| **M0** | `pkg/ir`, `internal/tableconf`, `internal/diag`, the pure compile stages, `rig ir` / `schema` / `validate`, golden corpus |
| **M1** | `internal/dockerdb`, `internal/introspect`, `rig init` / `migration new` / `db *` / `sync` |
| **M2** | `pkg/gen`, `runtime/{patch,query,readopt,rigerr,tenancy,dbx,dbhook}`, `persist-go`, `rig generate` / `check` / `generators` |
| **M3** | `service-go`, `server-go`, `examples/todo` serving end to end |
| **M4** | `runtime/throttle`, `auth/{password,session,apikey,account,authlog,authhttp,authpg,oauth}`, `rig setup-project` |
| **M5** | `runtime/electric`, the `electric` generator, live sync verified against a real ElectricSQL container |
| **M5.5** | `model-go`, `patch.Optional` / `patch.Nullable`, `…Input` types with normalize and validate, the conversions deleted |
| **M5.6** | Typed per-input errors answered as 422, `runtime/dbhook` lifecycle hooks declared per service, the audit log removed, `runtime/serve`, `rig/migrate` |
| **M5.8** | The whole authentication configuration moved into `rig.yaml`, resolved when it is read, and `server-go` writes the wiring |
| **M5.10** | `go-client` and the `rigclient` module: a typed Go SDK generated from the same document the router is, plus `runtime/authwire` and `examples/sdk` |
| **M5.7** | The API revision: `.rig/revision.json`, the `API-Revision` header both ways, `runtime/apirev`, `Request.BuiltBefore` for a compatibility shim, and an opt-in `Server.MinRevision` |

Next:

- ~~**M5.9** — files.~~ Shipped: a `rig_file` table, the blob seam, an upload and a
  download endpoint per file column nested under the row that owns them, and the two
  error codes M6 was waiting on. The S3 adapter and its MinIO suite did not ship —
  `blob` has `memory` only, and `files.backend: s3` is still the honest gap.
- ~~**M5.11** — referential actions.~~ Shipped: a table that points at a row gets a
  function call when that row is deleted, inside the same transaction, and can refuse
  it. The other half of the sentence RIG6040 already started.
- ~~**M5.12** — the auth log and who is signed in.~~ Shipped: `GET /auth/audit`
  answering the caller's own events without a permission and the tenant's with
  one, `scope=all` on both session routes, the `AuthLogEntry` shape declared in
  the IR ahead of the client that will read it, and `auth.log_retention` with a
  prune task that refuses a window the rate limits could not survive. (Was
  M5.11; referential actions had already taken it.)
- ~~**M12** — notifications, the engine and the inbox.~~ Shipped: a
  `rig_notification` table a project links its own tables to, two generated
  callbacks that say when a notification is due and — at the moment it is sent,
  not when it was written — who should get it, and an inbox somebody can empty.
- ~~**M13** — notification delivery.~~ Shipped: Desktop, Mobile and Email as
  channels an application implements, per-account settings with a window each,
  digests, and a dispatcher every replica can run because the claim is a lease
  rather than a lock.
- ~~**M13.1** — the send timeout.~~ Shipped, out of the question "do we need a
  circuit breaker somewhere": `notifications.send_timeout` bounding the one
  outbound call rig does not make itself, a pass that stops when its lease is
  spent rather than outliving it, and the missing-sender guard `ErrNoSender` was
  written for and never wired to. See *M13.1* below; the breaker itself is a
  stated non-goal in M13's list.
- ~~**M9.0** — the logger.~~ Shipped, split off the front of M9 because it needed
  no otel, no module and no configuration: an error line carrying the cause of
  every 500 — which nothing anywhere recorded, the request identifier in the body
  pointing at a log that had never been written — a debug request line with the
  status and the size, and `runtime/reqlog` for the response writer none of it
  could be measured without. The request travels as one grouped attribute and
  there is no logger on the context — both `publicapis`'s shape rather than the
  first draft's. It also settled M9's correlation question for free: point
  `Server.RequestID` at the trace id and the log, the error body and the
  collector all say the same string.
- ~~**M9** — tracing.~~ Shipped: an `observe/` module that is where
  OpenTelemetry lives so that nothing else has to depend on it, a one-key
  `tracing:` block, and spans in the generated code — one per request named by
  its route, one per repository call, one per hook, one per statement from a pgx
  tracer on the connection. Three things came out differently: the request span
  is rig's own eighty lines rather than `otelhttp`, because outside the mux the
  matched route does not exist yet; nothing configured installs a real provider
  that samples nothing rather than a no-op, because the ids are worth having on
  their own; and the file exporter arrived here instead of in M10, which is now
  a reader over it. `rigclient` took a callback seam rather than an import.
- ~~**M10** — the built-in monitoring page.~~ Shipped: a `monitoring:` block that
  the compiler refuses without `tracing:`, a reader over the span file M9 wrote,
  and a page at `/_rig/monitor` listing the last few hundred requests with their
  spans underneath. Three things came out differently: it mounts on the API's own
  mux rather than beside the probes, because spans and request lines are opened
  inside each generated handler and so a route that is not one is already
  invisible to both; the embed lives in `observe/` rather than in generated
  output, which is why `gentest` never had to learn about a file that is not Go;
  and who may look is a password from `$RIG_MONITOR_PASSWORD` — with nothing in
  it meaning no route at all rather than a 401 anybody can probe — optionally
  narrowed by a `monitoring.allow` list of addresses that answers 404 to
  everything else.
- ~~**M14** — the foundation gets a version.~~ Shipped: rig's own DDL moved out of
  a Go const and into append-only `.sql` sets the owning modules carry, each with
  its own bookkeeping table, and `migrations.foundation: vendored | embedded` for
  who keeps the file. Vendored stays the default and now has an upgrade path,
  which is the bug this actually fixed: `alreadyApplied` matched a filename, so
  no schema change to rig's tables had ever been able to reach an existing
  project.

### Modules

```
.               github.com/simonjanss/rig            CLI, compiler, generators
runtime/        github.com/simonjanss/rig/runtime    imported by generated code
auth/           github.com/simonjanss/rig/auth       sessions, oauth, api keys
migrate/        github.com/simonjanss/rig/migrate    a binary migrates itself
rigclient/      github.com/simonjanss/rig/rigclient  imported by a generated client
observe/        github.com/simonjanss/rig/observe    otel, for a project with `tracing:`
examples/todo/  a real project, built in CI
examples/sdk/   a program that calls two of them, through their generated clients
```

The root module now requires `auth` and `runtime` with local replaces, so the
Docker suites in `internal/authtest` and `internal/electrictest` can drive them.

### Generators registered

`model-go`, `persist-go`, `service-go`, `server-go`, `electric`, `go-client`.
All but `electric` and `go-client` are scaffolded by `rig init`; `electric`
emits nothing until a table opts in, and `go-client` is opt-in because not every
project wants a Go SDK of its own. `model-go` is listed first because the layers above import what it
writes. The authentication wiring is not a generator of its own: `server-go`
writes it into the API package for a project that has an `auth:` block.

### Test surface

- `make test` — no Docker, sub-second per package
- `make vet`
- `go test -tags docker ./...` — introspection, the CLI loop, `setup-project`,
  the auth foundation, live sync, and the repository's hooks
- `make examples` — regenerate, `rig check`, build, run the example's own suite

---

## Departures from the plan so far

Worth a look before going further; each was a deliberate call and each is
reversible.

1. **The `UNIQUE (parent_token_id, kind)` index is gone.** It cannot coexist
   with a rotation leeway: one child per parent means a retry inside the window
   cannot be served at all. Reuse detection rests on `rotated_at` instead, which
   `MarkRotated` pins to the first use with `WHERE rotated_at IS NULL`.

2. **`LoginAttempted` is in the enum and rig never writes it.** Every attempt
   produces exactly one row — `LoginSucceeded` or `LoginFailed`. Half the writes
   on the hottest auth path, and strictly more informative per row.

3. **`auth/authpg` was not in the plan.** The auth packages take interfaces, and
   without this every project writes four hundred lines of SQL against a schema
   rig itself designed. The interfaces are still there for anyone who diverged.

4. **`expose: false` was added to the table configuration.** A table that keeps
   its model and repository and gets no API. `rig_identity`,
   `rig_identity_credential`, `rig_account_token`, and `rig_api_key` use it.

5. **The `electric` generator emits into its own package**, not alongside the API
   layer, so a project can have live sync without the HTTP generator.

6. **A self-referencing foreign key is exempt from the FK naming rule.**
   `root_token_id` says more than `root_account_token_id` would.

7. **`Patch[T]` is gone, replaced by `Optional[T]` and `Nullable[T]`.** The plan
   had one wrapper for every update field and answered `Null()` on a NOT NULL
   column with a 400 at validation. Two wrappers give each column exactly the
   states it has, so that request cannot be written down at all. An explicit
   `null` arriving over the wire for an `Optional[T]` field is still a 400, but
   it now comes out of `UnmarshalJSON` naming the field.

8. **There is no `api.Lesson` and no `LessonCreateBody`.** The plan's API layer
   had its own copy of the entity and its own wire bodies, with `toLesson` in
   between. Both sides now speak `model.Lesson`, and a handler decodes straight
   into `model.LessonCreateInput`. The copy loop was a transcription whose only
   possible deviation was a missing field.

9. **The repository normalizes and validates.** `Create` runs `Normalize` then
   `Validate`; `Update` runs `Normalize` before the transaction and
   `Validate(prev)` inside it, against the row it already loads for the
   snapshot. A service that forgets to call either cannot write an invalid row.

10. **The audit log is gone, and snapshots are the history.** `runtime/audit`,
    the `audit` and `audit_value` tables, the `audit:` table-config key, and the
    per-column change entries the repositories wrote are all removed. The audit
    *columns* stay and are still stamped. Two mechanisms for "what happened to
    this row" is one too many, and the one that keeps whole rows answers more
    questions than the one that keeps rendered strings.

11. **A write takes an envelope, not an input.** `repo.Create(ctx, in)` became
    `repo.Create(ctx, dbhook.Create[…]{Input, Validator, Hooks})`. The plan had
    hooks living in `runtime/dbx` as a vague line item; this is what they turned
    into, and putting them in the same value as the input is what makes them run
    in the same transaction as the write.

12. **Create and update are validated by different types, and each travels
    with its operation.** `<Res>Validator` became `<Res>CreateValidator` and
    `<Res>UpdateValidator`, each with one entry per field *that operation* can
    set, and each lives on the hook set for that operation —
    `Hooks.Create.Validator`, `Hooks.Update.Validator` — rather than beside it
    on the contract. A write is handed one value and cannot be given the other
    operation's rules. One shared type meant an update
    running a rule for an immutable column, against a value the update could
    not have changed — and no way to say a thing about creating that does not
    apply to changing. The lifecycle fixture has both asymmetries: `starts_at`
    is settable once and has a hook only on create, `status` is not settable at
    creation and has one only on update.

13. **A service is handed a contract.** The plan had a default service you
    embedded and nothing else. It now takes a `<Res>Contract` — the validator,
    the hooks, and the custom endpoints — because a rule that was never
    attached is a rule that does not run, and a zero-valued field says nothing
    at the call site. Custom endpoints moved into it as an interface, so the
    plan's compile-time guarantee about them survives the move.

14. **Validation answers with a struct shaped like the request.** A failed
    create returns a `*LessonCreateInputError` whose members are the input's
    members, each holding the problem with that field or nothing. It reaches the
    client as the `fields` member of the 422 body. The plan had one message and
    a status; a client cannot highlight a field from a sentence.

    The `FieldError` inside carries no field name — the member it hangs off is
    the field, and a second copy is a second place to be wrong — and instead of
    prose alone it carries a `FieldCode` from a fixed set: CannotBeEmpty,
    CannotBeNull, TooLong, TooShort, OutOfRange, InvalidValue, AlreadyExists,
    NotFound, NotAllowed. Taken from what meitner's errors package settled on.
    A client switches on the code once; the message says the specifics. The
    whole `FieldErrors` accumulator went with it: validation now writes
    straight into the typed struct.

    `FieldError` and `FieldCode` live in `rigerr`, not in the generated model.
    Nothing about them is per project — the same nine codes and the same two
    members — so generating them into every model would be nine copies of one
    decision. The model still owns the struct that *arranges* them, which is
    the part that differs per input.

    A loose `FieldError` — one that reached the HTTP layer as the whole answer
    rather than attached to a field — implements `ErrorCode` as Internal, on
    purpose. It says what was wrong without saying what it was about, so it can
    only have come from somewhere with no field to attach it to, and a 422 with
    an empty body would blame the caller for a mistake in the service. It was
    already a 500 by default; now it is one deliberately, with the reason in
    the code.

    A hook returning something that is **not** a field error is a rule that
    could not be run, not a rule that failed — a lookup that could not reach
    another service. `rigerr.AsFieldError` is the boundary, and
    `rigerr.Wrap(err, "validate title")` is what happens on the other side of
    it: context added, the code kept if the error already had one, Internal if
    it did not. A rule that refuses with a Conflict is still a 409 once the
    layer above has said which rule it was; a bare error is a 500, because
    telling the caller their title is "connection refused" would be a lie about
    whose fault it is.

15. **The revision is stamped after Freeze, not during it.** M5.7 below said
    `ir.API` would gain a `Revision` set "at freeze from `Meta`". It cannot be:
    the revision is decided from the finished document's hash, so there is
    nothing to look up until the document exists. `rig generate` resolves it and
    calls `Document.SetRevision` once, before any generator runs, so a generator
    still sees one document that does not change under it.

    `Document.Hash` clears the revision itself rather than leaving that to each
    caller. That is the trap M5.7 wrote down — a hash that saw the revision
    would move every time it was set, which would move the revision — closed in
    the one place it cannot be forgotten. It also means `Manifest.IRHash`
    identifies the surface rather than the stamp.

16. **The revision is a type, not a date-shaped string.** `runtime/apirev` holds
    it, and the point of the type is that the zero value is *unknown* and
    `Before` is false whenever either side is unknown. That one rule is every
    safe default at once: a server with no `MinRevision` refuses nobody, a
    compatibility shim does not fire for a caller that sent nothing, and neither
    needs anybody to remember a check. `serveRevision` is one comparison because
    of it.

    `apirev.MustParse` is how a date written down in source becomes one, and it
    panics. A cutoff is a constant, so a typo in it is a bug rather than bad
    input — and a shim that silently never fires is the worst possible way to
    find that out. Declared at package scope, the panic lands at startup, which
    is where `Server.MinRevision` used to check itself from inside `Register`
    before the type made that impossible to get wrong.

17. **The `rig_` prefix and the names it strips to are reserved.** The prefix was
    documentation until now: `PartTables` said it existed "so a project can tell
    at a glance which tables arrived with the foundation and is free to have an
    `account` or a `tenant` of its own". That freedom was a bill nobody had paid
    yet. A project with its own `account` finds out on the day it sets
    `auth.expose: [rig_account]` — RIG2001, two tables projecting to `Account` —
    and the fix that day is a migration plus a rename in every client that reads
    the resource. Reserving the name now makes that day one line in `rig.yaml`.

    Two codes, both errors and neither configurable: **RIG2004** for a table
    projecting to a name one of rig's own tables takes, **RIG2005** for a table
    under the prefix. RIG2003's summary and hint were narrowed to what it
    actually reports, which is an endpoint's parameter — its one call site is the
    `scope` query parameter — and never a field or a table.

    The reserved set is **derived, not listed**. `config()` in
    `internal/scaffold` now returns the whole `tableConfig` rather than the
    file's text, so `ReservedResources()` reads the resource name back out of the
    configurations `setup-project` writes — a part added to the foundation
    reserves its names by existing, and a guard test in `reserved_test.go` makes
    that a build failure rather than something somebody has to remember.

    `plannedResources` is the escape from that, for a name decided before its
    table exists — reserving early is the cheaper mistake, because a project that
    took the name pays a migration and a rename in every client on the day the
    part lands. It is **empty now**, and empty is the healthy state. It held
    `Notification` and `NotificationRecipient` while M12 was being written; M12
    and M13 then shipped five notification tables whose configurations reserve
    all five names on their own, and the entries became a second copy of the
    answer. `TestPlannedNamesAreNotBuiltYet` is what makes leaving one behind a
    failure rather than dead weight shadowing the derived set.

    The notification part is also the proof the derivation was worth it: it
    arrived after this rule did, and it reserved five names without anybody
    editing a list. The only edits it needed were a `foundation.txt` for the
    `notify` fixture and the five `config()` call sites it had written in the old
    two-value form.

    Two things this needed that were not obvious. The rule runs **after
    `Freeze`**, not beside the RIG2001 collision in `Project`: the frozen
    document holds the name that won, and `resource: Account` on some other
    table is written by `ApplyConfig`, which runs later. And `IgnoreTables`
    could not answer "is this rig's table" — `auth.expose` takes a table *out* of
    that list while leaving it rig's, and `auth.own` empties it, which is also
    exactly what a project that never scaffolded looks like. Hence
    `Options.Foundation`, and hence `foundationTables` returning two lists from
    one reading of the migrations directory. The compiler's fixtures have no
    migrations directory, so an optional `foundation.txt` stands in for one.

    `auth.own: true` turns both rules off. A project that has forked the
    foundation owns those tables and owns `Account` with them, and it was already
    a one-way door. `rig migration new --table` asks the same question before it
    writes anything, and `rig sync` skips rather than refuses, because sync
    exists to repair a project that does not yet validate.

    The two halves are not equally final, which is why `Reserved` returns
    `escapable` and both commands read it. Nothing moves a table off the prefix,
    so that is a refusal. A reserved resource name is only what a table projects
    to by *default*, and RIG2004's own hint offers the `resource:` key that moves
    it — so `--table account` warns and names that key rather than refusing, or
    `table: file` with `resource: Document` would be a table rig's rules allow
    and rig cannot scaffold. Sync still skips the file, because the name it would
    fill in is the taken one; it now says the other way out too.

    RIG2005's advice for a foundation table nobody scaffolded is the migration
    filename `Managed` reads, not `rig setup-project`. That command decides what
    to write from those same filenames and never from the database, so telling
    somebody whose `rig_account` was hand-written or squashed to run it would
    have produced a second `CREATE TABLE` for a table that already exists, and a
    `rig db up` that fails.

---

## M5.5 — the model layer (shipped)

What was built, and the decisions behind it. Kept because the reasoning is not
visible from the code.

### The problem it solved

`persist-go` emitted `store.Lesson`, `service-go` emitted `api.Lesson` with the
same fields, and `toLesson(*store.Lesson) *api.Lesson` copied one into the
other. Two definitions of one entity and a copy between them: a field missing
from the copy is a field that silently stops being returned.

### The layout now

```
internal/model/     Lesson, LessonPriority, LessonQuery and its param structs,
                    LessonCreateInput / UpdateInput / DeleteInput / RestoreInput,
                    Normalize, Merged, Validate, ValidatorContext, Validator
internal/store/     LessonRepository and its pgx implementation
internal/api/       the Request envelope, the service interface, the default
                    implementation, routing
services/lesson/    yours
```

Enums live in `model` — they were referenced from both sides, and
`store.LessonStatus` in an API response is the wrong package on the wrong layer.
A field that is not readable carries `json:"-"`: the repository scans it, the
wire never sees it, one struct and one tag. `LessonQuery` is in `model` too,
because the service builds queries; rendering it to SQL stayed in `store` as a
function over it.

### Normalize, Merged, Validate

Taken from `meitner/platform/backend/intern/services/documentation`, with one
correction. meitner merges the previous row *into* the input, so every field is
defined before any validator runs. rig cannot: the repository builds its UPDATE
from which patches were touched, so merging in would turn every update into a
write of every column, and two requests changing different fields of one row
would clobber each other.

So `Merged(prev)` returns the intended end state as a separate value. Validation
runs against that; the input keeps its patches. A cross-field rule — "ends after
starts" when only `ends` was sent — is answerable, and the UPDATE still touches
one column.

Generated `Validate` covers what the schema knows: NOT NULL, length from
`varchar(n)`, enum membership. `Normalize` covers what the schema implies: trim
strings, parse an enum label case-insensitively, apply the column default on
create. Everything else is a per-field hook on the `Validator` struct, which a
service fills in at construction.

### Two wrapper types

Create inputs are plain — a `CreateInput` with no `Title` and one with `""` are
the same value, so generated validation says "cannot be empty" where meitner
would say "cannot be undefined". For a NOT NULL column both are errors anyway,
so the difference is in the message.

Update inputs distinguish nullability at the type level:

```go
type LessonUpdateInput struct {
    Title  patch.Optional[string]        // NOT NULL: absent | set
    Notes  patch.Nullable[string]        // nullable: absent | null | set
}
```

`Patch[Nullable[T]]` was considered and rejected: four states for a column with
three, two of them meaning the same thing, and two unwraps to read a value.

The compiler now enforces what a runtime check used to. It also simplified the
repository — a NOT NULL column's update needs no null guard, because the type
cannot hold one:

```go
if in.Notes.Touched() { … values = append(values, in.Notes.Ptr()) }  // nullable
if v, ok := in.Title.Get(); ok { … values = append(values, v) }      // not null
```

### `internal/gen/genutil`

Rendering a field's Go type had been written three times and the copies had
begun to disagree about enums. It is one function now, taking a `func() string`
for the model qualifier so a file whose types are all builtin does not end up
with an unused import.

---

## M5.6 — validation, hooks, and the audit log (shipped)

### The audit log is gone

`runtime/audit`, the `audit` and `audit_value` tables, the `audit:` key, the
`AuditLog` field in the IR, and the change entries every repository wrote.
Snapshots keep the history instead: a whole row per version, which answers
"what did this look like on Tuesday" as well as "what changed", where a table
of rendered before-and-after strings only ever answered the second.

The audit *columns* stay. They are one write on a row that was being written
anyway, and tenancy and soft delete both read them.

One thing fell out of it: an update that changes no column now takes no
snapshot either. It never wrote an audit row before, for the same reason, and a
history full of copies of an unchanged row is a history nobody can read.

### Validation answers with the shape of the request

```go
type LessonCreateInputError struct {
    Title  *FieldError `json:"title,omitempty"`
    Notes  *FieldError `json:"notes,omitempty"`
    …
    Rest   *FieldError `json:"rest,omitempty"`
}
```

One member per member of the input, nil when that field was fine. `Validate`
returns it, the HTTP layer puts it in the `fields` member of the 422 body, and
a client attaches each message to the control it belongs to instead of parsing
one sentence for field names.

Two small additions to `runtime/rigerr` carry it: `Coder`, so an error can name
its own code and be returned as itself rather than wrapped in prose, and
`FieldReporter`, which is how `DefaultErrorMapper` finds the structure.

`Rest` exists because a hook can report a problem that belongs to no field, and
dropping it would mean refusing a request for a reason the answer never
mentions.

### Hooks

`runtime/dbhook` holds four envelopes — Create, Update, Delete, Restore — each
carrying the input, the validator where there is one, and the callbacks.

```go
repo.Create(ctx, dbhook.Create[model.LessonCreateInput, model.Lesson]{
    Input:     in,
    Validator: s.Validator,
    Hooks:     s.Hooks.Create,
})
```

Before and After run inside the write's transaction, so returning an error
undoes the write — which is the whole reason they are here rather than in the
caller. AfterCommit runs once the **outermost** transaction has committed:
`dbx.AfterCommit` registers it, and the `InTx` that actually began the
transaction runs the queue after the commit, each recovered. A repository call
nested in a larger unit of work therefore does not announce anything until that
unit lands.

`dbx.InTxIf` is the other half: a create is a single statement, and opening a
transaction around it costs two round trips to protect nothing. It opens one
only when a hook needs to be able to undo the write.

### The contract

`NewDefaultLessonService(repo, contract)`. `LessonContract` is what the service
layer owes the resource — the validator, the hooks, and the custom endpoints —
`DefaultLessonService` keeps it unexported, and there is no way to build a
service without saying what it is. An empty `LessonContract` is a fine answer,
it is just an answer somebody wrote.

Custom endpoints are part of it rather than methods the service happens to
have, and they arrive as an **interface**, not function fields:

```go
type LessonEndpoints interface {
    Publish(ctx context.Context, r Request[LessonPublishPath, struct{}, LessonPublishBody]) (*model.Lesson, error)
}
```

That is the whole reason for the shape. Declaring an endpoint in the table
configuration adds a method to the set, and the service that no longer
implements it stops building — where a nil function field would have been a 500
on a route that worked the day before. The default now answers those routes by
handing them over, so a resource whose endpoints are all written needs no method
of its own, and the constructor panics on a nil set rather than letting every
one of them fail at runtime.

The stub writes a `contract()` method listing **every** validator field and
every hook slot, nil included, the way meitner's `documentationCreateValidator`
does. Go does not require an exhaustive literal; spelling it out is what makes
adding a column show up as a field nobody filled in, rather than as nothing at
all.

`contract()` hangs off a second, unexported `service` type that the exported
`Service` holds:

```go
type Service struct {
    api.DefaultLessonService
    svc *service
}

func New(repo store.LessonRepository) *Service {
    s := &service{repo: repo}

    return &Service{
        DefaultLessonService: api.NewDefaultLessonService(repo, s.contract()),
        svc:                  s,
    }
}
```

A named field, not an embed: the default answers every custom endpoint by
handing it over, and `*service` implements the same set, so two promoted
methods of one name would make the selector ambiguous — which is a compile
error at the interface check, found the moment it was tried.

Two types because the default implementation is handed the rules while it is
being built, so something has to hold them before there is a `Service` to hold
— and building it in two phases, or threading each dependency through a
function signature, were both worse. The unexported half turns out to be the
better half anyway: a rule written against `*service` cannot call back into the
API surface it is part of, and a validator that called `Create` would be an
infinite loop found in production rather than at compile time.

The alternative considered was dropping the default service entirely and having
the stub carry a real body per operation, which is what meitner does. It was
turned down for one reason: everything the stub writes is `CreateOnce`, so a
generator fix — a pagination bug, a new lifecycle guard — would never reach a
project that already exists.

### `runtime/serve`

A dependency with a shutdown of its own is registered where it is built:
`Mount` now takes a `*serve.App` — the pool, the logger, `Drain` and `Close` —
instead of a bare pool. `Drain` runs when readiness turns false and before the
server stops accepting, which is where a queue consumer belongs: it should stop
fetching while the server is still finishing what it has. `Close` runs after the
last request, in reverse registration order, before the pool closes, and runs
even when startup failed halfway. `ShutdownTimeout` became `MaxShutdown`: one
budget for the whole sequence rather than one per phase, stated rather than
derived, because it is the number that goes in
`terminationGracePeriodSeconds` and nobody will re-add the parts by hand twice.
The parts are checked against it before the server listens — a step given
thirty seconds inside a twenty-second maximum can never finish, and an actual
shutdown is the worst time to find that out. `CloseWithin` and `DrainWithin`
give a single step a smaller limit on top of that, and a step is abandoned when
its deadline passes rather than waited for — a hook that ignores its context
would otherwise hold the process open until something outside killed it, and
take every step after it down too. The abandoned goroutine leaks, which is the
better of the two outcomes in a process that is about to stop existing.

**Both ends are bounded and both are stated.** `MaxStartup` (default 60s) covers
opening the pool, the `Migrate` hook and `mount`; the budget is released the
moment the server listens, since a deadline that outlived the boot would shut
the server down when it passed. Each phase runs the same way a shutdown step
does — in a goroutine, abandoned when the budget goes — so a mount function
that dials something slow and ignores its context produces an error naming the
phase rather than a process that is neither serving nor failing. `ConnectTimeout`
stays as the inner, more specific bound so "the database" and "startup" are
different messages; its default yields to a shorter `MaxStartup` rather than
requiring a second field to be lowered with it, while a value somebody actually
wrote that does not fit is refused.

**Logged and returned, and the two are not the same job.** A step logs its own
failure where it happens, because a later step that hangs past the budget
leaves the process to be killed from outside and the returned error then
reaches nobody — the log line is the record that survives. The error is still
returned, because `Run` is a library and the caller owns the policy. What
`Main` does with it is the third decision: a teardown failure comes back
wrapped in a `*ShutdownError`, `serve.Unclean` reports whether that is *all*
that went wrong, and if so the process exits 0 with one line saying it did not
stop cleanly. A server that served for a week and then failed to close an
exporter has done its job; exiting non-zero would mark an ordinary rollout as a
crashed container in every dashboard that counts them. A startup failure joined
with a failed teardown is still a startup failure, and still exits 1.


`main.go` was forty lines of pool, server, signals and shutdown that every
project would copy and one of them would get subtly wrong — a pool nobody pings
so a bad password surfaces on the first request, a server with no header
timeout, a SIGTERM that drops what is in flight.

```go
func main() {
    serve.Main(serve.Config{
        DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
        Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8080"),
        HealthPath:  "/healthz",
    }, func(_ context.Context, pool *pgxpool.Pool) (http.Handler, error) {
        return newMux(store.New(pool, store.Config{})), nil
    })
}
```

`Run` is the same thing with the process left to the caller. Every timeout has
a default and none of them is zero; a negative one means "no limit", so zero
can keep meaning "I did not say". The shutdown derives from
`context.WithoutCancel`, because deriving it from the context that just got
cancelled would give requests in flight no time at all.

**Two probes, not one.** The first cut had a single `HealthPath` that pinged the
database, which is a readiness check wearing a liveness name — point a
Kubernetes liveness probe at it and one slow database restarts every replica at
once. So:

- `LivenessPath` never touches a dependency. Answering at all is the answer.
- `ReadinessPath` pings the pool, runs an optional `Ready` hook, and returns 503
  from the moment a shutdown begins.
- `DrainDelay` keeps the server answering after readiness turns false, because
  removing an instance from a load balancer is not instant and requests sent
  during that window would otherwise arrive at a server that has stopped
  accepting them.

Both are opt-in: a path that always answers 200 is worse than no path.

The example's `main.go` moved to its root, and `go run ./cmd/server` became
`go run .`.

### The example uses all of it

`examples/todo/notify` is the dependency that is not the database: a batching
notifier, started in the mount function, drained before the server stops and
closed after it. It is fed by `Create.AfterCommit`, which nothing exercised
until now — and which is the only hook a write may be announced from, since
`Before` and `After` both run where a rollback can still take the row away.

Its own tests are ordinary and fast: draining stops it recording without
flushing, closing writes what is left, a message recorded after the drain is
dropped. The Docker suite adds the end that needs a database — that the hook
fired for a committed create and said nothing for a refused one.

### Where middleware goes, and who makes the mux

`Mount` returns a handler, and that is the extension point for anything
cross-cutting. `serve.App` does not own a mux: that would pin the server to one
router type and make the return value meaningless.

`api.Register` **does** make the mux now — `Register(h Handlers) *http.ServeMux`
— which reverses what I first argued. The argument against it was that a caller
might want to register into a mux it already has, or use a router that is not
`http.ServeMux`. The second was simply wrong: the signature has always been
concretely `*http.ServeMux`, so that was never possible. The first survives only
as an extra nested mux, and returning the concrete type rather than an opaque
handler means adding routes afterwards is still one `Handle` call. There is
nothing for the caller to decide about the mux — the patterns are absolute and
already carry the base path — so making it is the generator's job.

The electric generator's `Register` still takes one, which is the composition:
the API makes the mux, the shape endpoints go on it.

Cross-cutting concerns then read in the order they run:

```go
return otelhttp.NewHandler(logRequests(mux), "todo"), nil
```

The probes are answered outside whatever comes back, which is the part worth
having decided: a liveness check every second should not be a traced request, a
line in the request log, or a row in a latency histogram.

Both mechanisms stay, with the line between them stated: `api.Server.PreHooks`
for what only needs the request — the example's log line, written inline in the
struct field — and a wrapper for what has to see the response, which is what
tracing is.

The mount function is written inline at the call to `serve.Main`, so main.go
reads as one thing. The cost is that a test cannot reach it: `newHandler` in the
example's suite is a second copy of the wiring, and says so in its comment.
`api.Register` is the part that matters and it is the same call in both.

### Migrations, and where they run

`rig db up` was the only way to apply them, which is a development tool — it
wants Docker and cobra — and a deployment that installs a development tool to
move its schema is running whatever version that machine happens to have.

So: a fourth module, `rig/migrate`, wrapping the same goose the CLI uses.
`internal/dockerdb.Migrate` now calls it, so there is one reader of the files
and `rig db up` cannot drift from what production applies. It is a module of
its own rather than `runtime/migrate` because Go requires at the module level:
goose would otherwise be in the go.mod of every generated application,
including the ones that migrate some other way.

```go
//go:embed migrations/*.sql
var migrations embed.FS

applied, err := migrate.Up(ctx, migrate.FromPool(pool), migrations, migrate.Options{})
```

`FromPool` bridges pgx to database/sql, so the migration runs with the
credentials and settings the application itself is using. An advisory lock is
on by default: several replicas starting together is the normal case, and one
of them should apply while the rest wait and find nothing to do. There is a
Docker test with four concurrent runners asserting exactly one application.

`serve` gained three things for it. `Config.Migrate` runs after the pool opens
and before the server listens. `serve.Once` opens the pool, runs one function
and closes it. And `Config.Tasks` makes a subcommand out of any of them:

```go
Tasks: map[string]serve.Task{
    "migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
},
```

`Main` dispatches on the first non-flag argument, so `todo migrate` runs the
task and exits and `todo -addr :9000` still serves. A leading argument that
names nothing is refused with the list of what the binary does know — a typo in
a deployment script should exit, not quietly serve without having migrated.

`migrate.Apply` is the other half: `Up` as a one-argument function, borrowing
the pool and handing the handle back, so the three lines every project would
write the same way — including the deferred close that is easy to leave out —
are written once.

`migrate.Require` is the third option and what the example's server uses: it
checks `Pending` and refuses to start when the database is behind, without
being the thing that changes it.

**Which of the three to use is a real decision, and the argument for it lives
in `migrate`'s package documentation** rather than here — the four things that
make boot-time migration worse at more than one replica (the app's role needing
DDL rights, replicas queueing on the advisory lock past their startup budget, a
bad migration crash-looping the fleet instead of failing a job, and every
restart re-running it), and the one thing nobody should do, which is migrating
beside a server that is already answering. `serve.Config.Migrate` and the
example both point at it instead of restating it.

**Not done:** `rig init` still does not write a `main.go`. It cannot yet — the
api package it would import does not exist until `generate` runs — so a
scaffolded one would have to be commented out, and a half-written main is
exactly the kind of file that rots.

### Also

`model-go` had no test file; it has one now. The enum parser's doc comment
claimed it accepted "InProgress" for `in_progress`, which `strings.ToLower`
does not do — the comment now says what the code does. `rig init` never gave
`server-go` its `model_import`, so every newly initialised project failed at
`generate`; the CLI's Docker suite caught it.

---

## M5.7 — the API revision (shipped)

**Goal.** Every generated client says what it was built against, the server puts
that on the request context, and a log line can answer "how old are the clients
still calling this?" — which is the question you have to answer before you can
remove anything.

**What shipped**, against the design below. `internal/revision` owns
`.rig/revision.json` and the one rule that decides it: the recorded date stands
while the recorded hash matches, and today's is written when it does not.
`rig generate` resolves it and stamps the document; `rig check` and `rig ir`
read the record and never write it, so a check running the morning after a
schema change reports the drift rather than quietly accepting it. The two
departures — stamped after Freeze, and `Document.Hash` clearing the revision
itself — are 15 above.

`ir.API` gained `Revision` and `RevisionHeader`; `api.revision_header` in
rig.yaml names the header, defaulting to `API-Revision`. `service-go` writes
`Revision`, `RevisionHeader`, `RequestContext.ClientRevision`, `Client`,
`BuiltBefore` and `Stale`; `server-go` reads the header, announces its own on
every response including the failed ones, and grew `Server.MinRevision`.
`go-client` bakes the revision into the generated client and hands it to
`rigclient` through `rigclient.API`, which is where `Config.Revision` — the dead
seam this milestone was written around — finally gets its value from.

The revision is a value rather than a string, in `runtime/apirev`, and 16 above
is why. Both the comparison a shim is written in terms of and the one the server
refuses on are `Before` against it.

Two calls worth knowing about. `MinRevision` refuses only a caller that sent a
revision older than it: one that sent nothing is served, because an unknown
client is not the same as an old one and a check that turned every curl into a
426 would be an outage rather than a deprecation. And the refusal is its own
code, `rigerr.CodeUpgradeRequired` → 426, rather than a bad request: "regenerate
your client" is not advice anybody can take from a 400 that also means a
malformed body. The closed set grew by one, which is cheap now and expensive
after M6 bakes it into an OpenAPI document and a TypeScript union.

### Reading it from a service

The whole point of knowing how old a caller is, is doing something about it. A
compatibility shim reads:

```go
var notesAdded = apirev.MustParse("2026-04-30")

func (s Service) Create(ctx context.Context, r api.Request[…]) (*model.Todo, error) {
	if r.BuiltBefore(notesAdded) {
		r.Body.Title = "Unknown"
	}
	return s.DefaultTodoService.Create(ctx, r)
}
```

It goes in the service method, and that is not an accident of what is reachable:
this is the one place with the request as it arrived. On a create the generated
`Validate()` runs in the repository, *after* the service method and before every
hook — so the same shim written as a `Create` `Before` hook would only ever see
requests that had already passed the check it exists to satisfy. The method's own
documentation says so, because the failure is silent otherwise.

Below the service layer a validator and a hook are handed a `context.Context` and
nothing else, so `resolve` puts the whole `RequestContext` on it — before the
`Server.Context` hook, so that hook can build on it — and `api.RequestContextFrom`
takes it off. One value reached two ways rather than two copies that can drift.

The unknown caller is a decision, stated once and inherited everywhere: rig's
revisions describe what rig's own generated clients were built against, so a
caller that sends none is served the current behavior. Anything else would make
adopting a shim an outage for every curl. An application that would rather count
an unknown caller as ancient still has the raw `ClientRevision` and can say so
itself.

### The date has to mean something

The obvious implementation is a build timestamp, and it is the wrong one: it
changes on every regeneration, so two clients built a month apart against an
unchanged API look a month apart when they are identical. The number would be
noise within a week.

What is worth knowing is **when the API surface last changed**. rig can compute
that, because it already hashes the document:

- `rig generate` compares `Document.Hash()` against the last recorded one.
- Different → record today's date with the new hash. Same → keep the old date.
- The recorded pair lives in `.rig/revision.json`, committed, so it survives a
  clean checkout and shows up in `git log` as a record of when the API moved.

```json
{ "revision": "2026-08-01", "hash": "sha256:1f3a…" }
```

Generators stay pure: `rig generate` reads the file and passes the revision in,
so the same document and the same options still produce the same bytes, and
`rig check` the next morning does not fail because the date moved.

> One trap worth writing down now: the revision cannot be part of what gets
> hashed, or setting it changes the hash, which changes the revision. The hash
> is taken over the document with the revision cleared.

### Naming

`api.version` already means `v1` — the path segment. A second thing called
version, holding a date, would be a permanent source of confusion.

So: **revision**. Header `API-Revision`, constant `api.Revision`, config
`api.revision_header`, file `.rig/revision.json`. One word, one meaning. Say if
you would rather have Version and I will use it everywhere instead.

### The plumbing

**Compiler.** `ir.API` gains `Revision string`, set at freeze from `Meta`.

**`server-go`.** Reads the header in `prepare` and puts it on the context that
already carries the request id and the route:

```go
type RequestContext struct {
    RequestID  string
    Method     string
    Path       string
    Route      string
    RemoteAddr string
    UserAgent  string

    // ClientRevision is what the caller says it was built against, empty when
    // it did not say. A hand-rolled client or a curl will not say, and that is
    // a normal thing for a caller to be.
    ClientRevision string
}

// Revision is what this server was generated from.
const Revision = "2026-08-01"

// Stale reports how far behind the caller is, and false when it did not say
// or the two match.
func (rc RequestContext) Stale() (time.Duration, bool)
```

rig does not log it — rig has no logger. It puts it where the application's
logging hook, its error mapper, and every service method can already reach it.

**Response.** The server sets `API-Revision` on the way out too, so a client can
notice it is behind without anyone building a discovery endpoint.

**`ts-client`.** Sends the header on every request, with the revision baked in
at generation, and exposes it as an export so an application can log its own.

**`openapi`.** Documents the header as an optional request parameter, and puts
the revision in `info.version`.

### It must never fail a request

A missing header is an unknown client, not an error. A malformed one is the
same. This is telemetry, and telemetry that can 400 is an outage waiting for a
bad deploy.

The one exception is opt-in, and it is the reason for the whole feature:

```go
api.Server{
    // Refuse anything built before this. The zero value, the default,
    // refuses nothing.
    MinRevision: apirev.MustParse("2026-01-01"),
}
```

That is the endgame — you removed a field, you waited, the logs said nobody old
was left, and now you close the door. Off until somebody decides to.

### The open question, answered

Should a client that sends no revision at all be distinguishable in the logs
from one that sends an unparseable one? They are different failures — an old SDK
that predates the header versus something sending nonsense.

**Answered: no second field, one method instead.** `ClientRevision` is the raw
string, and `RequestContext.Client` is the one place it becomes an
`apirev.Revision` — unknown for both failures, because both mean the same thing:
this caller cannot be placed on the timeline. `Stale` and `BuiltBefore` are
written in terms of it, so "is this a valid revision" has one answer in one place
rather than a bool that has to be kept in step with the string beside it. An
application that wants to count the two apart still can: the raw string is right
there, and a non-empty `ClientRevision` with an unknown `Client` is exactly the
nonsense case.

---

## M5.8 — the auth block (shipped)

**Goal.** Everything about authentication that has a fixed answer lives in
`rig.yaml`, because the reference documentation and the client libraries are
generated from that file. A token lifetime written in a Go literal is a lifetime
nothing else can read.

### What moved

`auth:` in rig.yaml went from two keys about table generation to the whole
configuration: `enabled`, `base_path`, the tenant sources, the session lifetimes
including the rotation leeway and the verification cache, the password policy and
the breach check, whether registration and tenant creation exist, the six rate
limits, the trusted proxy ranges, and provider sign-in down to which environment
variable holds each client secret.

Durations are Go's syntax extended with `d` for days — `30d`, not `720h` — which
is `ir.ParseDuration`, shared by the config reader and the document.

Every value is **resolved when the configuration is read**, from rig/auth's own
constants rather than copies of them. A zero in `ir.Auth` means somebody wrote a
zero. That is what lets the emitted specification quote a number and have it be
the number the server enforces.

### Three gaps this closed

`docs/auth.md` documented `RotationLeeway`, `CacheTTL` and `OAuth.StateTTL` as
tunable, and `auth.Config` had no field for any of them: they were reachable only
by assembling the parts by hand. They are fields now — `RotationLeeway`,
`SessionCacheTTL`, `OAuth.StateTTL`.

`auth.Config.SigningKey` also claimed a random key was generated when it was
empty. `oauth.New` refuses anything under 32 bytes, and always did. The comment
was wrong, not the code.

### The wiring, in the generated server

`server-go` writes one more file — `auth.gen.go`, in the same package as the
routes and the handlers — for a project with an `auth:` block. A project without
one gets no file, which is what keeps its API package, and its module, free of
rig/auth: `examples/todo` serves a list of chores without depending on argon2.

One package rather than two is what makes the error mapper free. The wiring
calls this package's `DefaultErrorMapper` by name, so an authentication failure
is shaped like every other failure without an import path in a configuration
file to say where to find it, and there is no second package for a project to
import and keep in step.

The file is `Hooks`, `New` and `Config`, and Hooks has one field per function the
configuration actually makes necessary. `Grants` appears as required when the
schema derives permissions, `Tenant` only when `from:` names the hook source,
`Tenants` only when tenant creation is on. Each is checked at construction: a nil
`Grants` under derived permissions is an API where every endpoint answers 403,
which looks like a policy decision rather than a mistake.

Two things it generates rather than asks for, both previously hand-written in the
examples: the tenant-from-host slug lookup, and provider construction from the
environment.

### What is still Go, and why

A function, and a secret. `Grants`, `Notifier`, the tenant policy hooks,
`OnSignIn`, and `OAuthHooks.ReturnTo` — that last one because an application with
a tenant per subdomain has an origin per tenant, and a list in a file cannot name
a tenant created this morning.

Both examples shrank: `examples/auth`'s wiring is three hooks, and
`examples/auth_oauth` lost its resolver, its slug function, its scheme helper and
its provider construction — 90 lines that are now six lines of YAML.

---

## M5.9 — files (shipped)

**Goal.** Uploads and downloads that are tenant-scoped, permissioned per field,
and reachable from a synced row — without a polymorphic attachment table, and
without every project rewriting the same four hundred lines of multipart handling
badly.

Nothing about files exists today, and it is not a rejected non-goal either: the
plan never contemplated it. What exists is a generated HTTP layer that cannot do
it. `decodeBody` is JSON only, refuses unknown fields, and caps the body at a
megabyte; `writeJSON` sets `application/json` and the status before anything
reaches the wire. So an application that needs an avatar hand-writes the table,
the tenancy, the storage and the streaming — which is the exact class of work rig
exists to remove.

### Why this comes before M6

Not because `openapi` would document the endpoints — it would, but a
hand-written module's routes never reach the IR, which is already true of every
route `auth` mounts.

The reason is `rigerr.Code`. The set is closed on purpose, and `compile`'s builtin
`ErrorCode` enum mirrors it into every model, every schema M6 renders, and the
union type M7 hands a front end. There is no 413 and no 415, so an upload that is
too large or of the wrong type can only answer 400. Adding `TooLarge` and
`UnsupportedMediaType` is golden churn today and a breaking change for every
generated client the day after M7 ships. **Land the two codes first and alone**,
so that diff is a diff somebody can read.

### The column is the declaration

A file attaches to one of your tables through a foreign key, and the column's name
carries the role:

```sql
profile_image_file_id uuid references rig_file (id)
```

`<role>_file_id`, and everything else follows from it: the path segment
`profile-image-file`, the endpoint names, the permission keys, the Go field. This
is the same kind of fact as `deleted_at` meaning soft-deletable. The migration says
what the thing is and the table configuration never gets a key that could disagree
with it.

Two things in the compiler have to move for that to work.

**The foreign key naming rule has to exempt it.** `checkColumnConventions` wants
`<target>_id`, and the target is `rig_file`, so today the only accepted names are
`rig_file_id` and `profile_image_rig_file_id`. The second is horrible and the first
allows one file per table. This is the third exemption, after audit actor columns
and self-references, and it earns its place for the same reason they do: there is
only one table the key could point at, so naming the role says more than naming the
target.

**Recognition has to read `Table.ForeignKeys`, not `Column.ForeignKey`.** The
tenant-safe schema is `UNIQUE (tenant_id, id)` on `rig_file` and a composite key
`(tenant_id, profile_image_file_id)` referencing it, which turns attaching another
tenant's file into a constraint violation rather than something a hook has to
remember — and generated `Validate` would never catch it, because it checks null,
length and enum membership, not provenance. But `Column.ForeignKey` is denormalized
for single-column keys only; composite constraints live on the table. Read the
convenient field and rig fails to recognise precisely the shape it should be
recommending.

There is a third, quieter problem: `rig_file` is a managed table, so it is in
`IgnoreTables` and `Normalize` drops it, which means there is no resource for a
generator to name. Inject a builtin `File` object the way `Error` and `Pagination`
are injected. That is a model type, an OpenAPI schema and a TypeScript type for
free, without projecting a table nothing generates code for.

### Two endpoints, under the row that owns them

`Expand` synthesizes a set per file column, beside the CRUD it already synthesizes:

```
POST   /api/v1/profiles/{profileId}/profile-image-file
GET    /api/v1/profiles/{profileId}/profile-image-file/{fileId}/{filename}
DELETE /api/v1/profiles/{profileId}/profile-image-file
```

The segment is the resource's own `PathSegment`, plural, so these sit beside
`/api/v1/profiles/{id}` instead of inventing a second shape for the same resource.

**The nesting is the whole design.** It forces the handler to resolve the owning
row through the generated repository before it touches a byte, and every generated
read is tenant-scoped in SQL with no opt-out and narrowed further by
`access: { scope: own }`. So you cannot upload to a profile you cannot see, you
cannot download an image belonging to another tenant's profile, and an owner-scoped
table's attachments are exactly as invisible as its rows. None of that is a new
mechanism — it is the mechanism rig already has, reached by putting the file behind
its owner. A flat `/api/v1/files/{id}` cannot do any of it without a second
authorization model sitting beside the first.

Permissions derive per field: `profile.profile_image_file.read` and `.write`, so
"may edit a profile" and "may replace its picture" are separable grants. The
catalogue grows by two keys per file column, and that is the feature rather than
the cost.

**`profile.write` does not imply `profile.profile_image_file.write`.** The tempting
alternative makes the common case a single grant, and it means a role quietly gains
the ability to replace an image the day somebody adds a file column to a table it
could already edit. A permission that is implied is a permission nobody audits. The
price is honest and should be said out loud: two more rows per file column in every
grant table, and a role that may edit a profile which cannot touch its picture until
someone says so.

The filename in the download path is for the browser's save dialog and for cache
busting. It is not the lookup — the file id is. Compare it against the stored name
and answer 404 on a mismatch, and never let it near the storage key.

### A gallery is a table

A column holds one file, and most attachments are one file: a picture, a logo, a
signed contract, an import. A gallery or an attachment list is many, and the wrong
instinct here is to invent a second mechanism for it — an array column, a
`files: { many: true }` key, a polymorphic attachment table with a `owner_type`
discriminator. rig already has the answer, and it is the same answer it gives for
every other one-to-many: **write the table.**

```sql
create table profile_attachment (
    id                  uuid primary key default gen_random_uuid(),
    tenant_id           uuid not null references rig_tenant (id),
    profile_id          uuid not null references profile (id),
    attachment_file_id  uuid not null,
    caption             text,
    position            integer not null default 0,
    -- audit and soft-delete columns as usual
    foreign key (tenant_id, attachment_file_id) references rig_file (tenant_id, id)
);
```

That is an ordinary rig table and it gets everything an ordinary rig table gets. It
is a resource at `/api/v1/profile-attachments`, filterable by `profileId` through
the relation filtering that already exists, ordered by `position`, soft-deletable,
snapshotted if you want it, and synced by Electric. And because the file column
convention applies to *any* table rather than only to top-level ones, the row gets
its own file endpoints for free:

```
POST /api/v1/profile-attachments/{id}/attachment-file
GET  /api/v1/profile-attachments/{id}/attachment-file/{fileId}/{filename}
```

So the gallery is a list query, adding to it is a create, removing from it is a
delete, reordering it is an update to `position`, and replacing one image is an
upload to that row. Every one of those is a thing rig already generates. Captions,
alt text, an uploader, a category — all just columns, which is exactly what a
`many: true` key could never have given you.

**One thing has to be added to make it work, though.** As described so far, adding a
gallery item is two requests: create the row, then upload the bytes. That is bad in
two specific ways. The file column can never be `not null`, because the row has to
exist before the upload has anywhere to go. And a client that makes the first
request and not the second leaves a caption with no picture — a junk row in a table
rig has no business sweeping, because it is your table and rig cannot know the row
is meaningless.

So **the create endpoint of a table with a file column also accepts
`multipart/form-data`**: a part named `json` carrying the row exactly as the JSON
form would, and one part per file column named after the field.

```
POST /api/v1/profile-attachments
Content-Type: multipart/form-data; boundary=…

--…
Content-Disposition: form-data; name="json"
Content-Type: application/json

{"profileId":"…","caption":"On the summit","position":3}
--…
Content-Disposition: form-data; name="attachmentFile"; filename="summit.jpg"
Content-Type: image/jpeg

…bytes…
```

One request, the row and its file committed together, and `not null` becomes
expressible. The JSON form stays exactly as it is, for the case where the file is
optional or its id is already known — so `ir.EndpointRequest.ContentType` has to
become a list rather than a single string, because an endpoint can now honestly
accept two. It is written by `Expand` and read by nobody today, so widening it costs
one field and no behaviour.

This is create only. Replacing a file on an existing row is what the nested upload
endpoint is for, and an update that could also carry bytes would mean two ways to do
the same thing with different transactional shapes.

The same mechanism gives you sign-up-with-a-picture in one request, which the
single-column form otherwise could not: `POST /api/v1/profiles` as multipart, with
`profileImageFile` as a part.

### You cannot reference a row you cannot read

Writing the gallery down exposes a hole, and it is not a file hole.

`POST /api/v1/profiles/{id}/profile-image-file` cannot be aimed at a profile you
cannot see, because the handler resolves that profile through the generated
repository first. `POST /api/v1/profile-attachments` is an ordinary create, gated by
`profile_attachment.write`, and the `profileId` in its body is just a column value.
The composite key stops it naming another tenant's profile. Nothing stops it naming
*any* profile inside the tenant — including one an `access: { scope: own }` rule
makes invisible to the caller. Attach a picture to a stranger's profile, then read it
back through your own attachment row.

**This is true of every child table rig generates today.** Files did not cause it;
files made it visible, by putting a form that does the check right next to a form
that does not. The answer is not to special-case tables with file columns, which
would make one kind of table behave unlike every other one for reasons nobody could
infer from the schema. The answer is to close it everywhere:

> A generated write may not reference a row the caller could not have read.

Concretely: every foreign key to an exposed resource, on create and on update, is
checked before the write lands. Not a permission check — a *visibility* check, using
the same predicates the target's own reads are built from.

**It belongs in the repository**, beside `ownerFilter`, whose doc comment already
makes the argument: it sits with the tenant filter rather than being layered on in
the service "because this is the floor: a hook reaching for the repository, a custom
endpoint, and the generated handler all pass through here, and a narrowing that only
the generated read path applied would be a narrowing a custom endpoint silently
drops." A reference check the service performs is a reference check a custom endpoint
forgets. This is the same argument and it lands in the same place — next to the
normalize and validate the repository already refuses to let a service skip.

The predicate is one that already exists. `filterScope` renders tenancy, the live
predicate and the relation joins; `Storage.Owner` is the column an owner-scoped read
narrows by. So the check is a `SELECT 1 FROM profile WHERE id = $1` with that scope
appended, inside the transaction that is open anyway, on a primary key. One indexed
lookup per foreign key per write.

**The failure is a field error, not a 403.** `FieldCode` already has `NotFound`, so
the response is a 422 naming `profileId` — the same shape as any other bad input, and
deliberately indistinguishable from a `profileId` that names nothing at all. A
distinct "you may not reference that" would confirm the row exists, which is exactly
what an owner-scoped table is trying not to say.

Two boundaries worth stating plainly, because they are where somebody will expect
more than this gives:

- **Only foreign keys to exposed resources.** `rig_file`, `rig_account`, the audit
  actor columns and anything else unexposed have no repository and no reader to
  borrow a predicate from. `rig_file` is covered by the composite tenant key instead,
  which is why that key is not optional.
- **Only what the schema says.** Tenancy, owner scope, soft delete. It does not run
  the target's `Read.Narrow` hooks, because running one table's application hooks
  inside another table's write transaction is a re-entrancy problem nobody wants to
  debug at two in the morning. Application policy stays in `Create.Before`, and the
  docs should draw that line rather than implying the invariant covers everything.

It also does not answer "may I add to a gallery I can only read". That is policy — a
profile you can see is not necessarily a profile you may attach to — and it stays a
hook. What the invariant guarantees is narrower and worth having on its own: no
generated write can point at a row the caller was never allowed to know about.

The happy consequence is that nested collection routes stop being a security question
and become a question about URLs. `GET /api/v1/profiles/{id}/attachments` would read
nicely and rig has no nested resources at all today, so it is a milestone of its own,
and now it is one that can wait.

### The URL is a column

`rig_file` carries `url`, written at upload time by the handler that knows the
resource, the row, the field, the file and the name. Electric syncs the row, the
client reads `url` off it and renders it, and nothing had to make a request to
discover where the bytes were.

Two costs, both worth paying and both worth writing down.

It is denormalized routing: rename a `path_segment` and every stored URL is stale,
so that rename is a backfill migration. And it is not a capability — the URL is
stable and unsigned, so possessing it grants nothing and the endpoint behind it
still authorizes. That is deliberate, and it is the only reason syncing the URL is
safe.

> The storage key must never be in the shape. Same class of mistake as syncing a
> password hash, which `runtime/electric` already warns about. Narrow
> `Shape.Columns` to id, url, name, content type and size.

It also means **downloads always flow through rig**. A presigned S3 URL would
bypass the endpoint, and the endpoint is where the permission check lives, so
`Signer` is an upload-only seam. The honest cost is bandwidth through the
application, and a CDN that cannot sit in front without giving up per-endpoint
permissions.

### Which is why the download route takes a cookie

A URL that is stable, unsigned and sitting in a synced row exists to be used
directly — `<img src={file.url}>`, an `<a download>`, a `background-image`. None of
those attach an `Authorization` header, and a bearer token is the only credential
rig's auth understands. So the feature would arrive complete and immediately
unusable for the case that motivated it.

The alternatives are worse. Fetching the bytes with a token and handing the element
an object URL works, but it means every image in an application is imperative code
with a lifecycle, and it throws away the browser's cache. A short-lived token
appended at render time cannot be the stored value, which is the same as not storing
a URL at all.

So: **`files.Config` gains an opt-in that accepts the session cookie on file GET
routes.** `SameSite=Lax`, `HttpOnly`, `Secure`, and scoped to the download route
only — not the upload, not the delete, not anything else rig serves. Claims resolve
the same way afterwards; this changes where the token is read from, not what it
means.

> The reason it is GET-only is CSRF, and the reason it is opt-in is that an
> application which never renders a file in a browser should not be carrying a
> cookie path at all. Widening it past GET reintroduces exactly the problem the
> bearer header exists to avoid, so the doc comment should say so in the place
> somebody would go to widen it.

### Where the bytes live

`blob.Store` — `Put`, `Get`, `Stat`, `Delete` over `io.Reader` and
`io.ReadCloser` — with `Signer` and `Marker` as *separate* optional interfaces, so a
backend that cannot mint URLs, or has nowhere to record that an object is deleted,
simply does not have the method rather than having one that returns an error.
`Marker` is what keeps the bucket's idea of a deletion in step with the database's,
and it is worth reading below before implementing a backend.

Two implementations: `blob.Memory`, which is for tests and `go run` and is not
durable, and S3. The S3 SDK cannot go in `runtime` — that module depends on
`google/uuid` and `pgx` and nothing else, and every generated application imports
it — so the interface and `Memory` live in a new `files` module and the S3 adapter
is a nested module beside it with its own `go.mod`. A disk implementation falls out
of the same interface in an afternoon and is deliberately not shipped; memory and
S3 are the two that answer "how do I test this" and "how do I run this".

**Which backend, and everything else with a fixed answer, is a `files:` block in
`rig.yaml`** — M5.8 already settled the shape of this argument for `auth:`, and the
reasoning transfers without a change: a byte cap or a sweep interval written in a Go
literal is a number the generated documentation cannot quote. So `files:` carries the
backend and its options, the per-file byte cap, the inline content-type allowlist,
the abandoned-upload interval, the restore window, and the cookie opt-in above,
resolved when the configuration is read so a zero means somebody wrote a zero.

`server-go` writes `files.gen.go` beside `auth.gen.go`, in the same package, for a
project that has the block — and nothing at all for a project that does not, which is
what keeps `examples/todo` free of an S3 SDK it never calls. The bucket credentials
are named environment variables rather than values, the way the OAuth client secrets
already are.

### `rig_file`

Its own scaffold part, one migration, `rig_`-prefixed like everything else rig
creates. Metadata only: id, tenant, storage key, url, file name, sniffed content
type, declared content type, size, checksum, `uploaded_at`, the audit columns,
`deleted_at`, and `UNIQUE (tenant_id, id)` so a referencing table can put the tenant
inside its foreign key. No generated CRUD, for the reason the foundation already
gives: a client that can POST a row with an arbitrary key and no bytes has found a
way around the rules. Under this design there is no flat endpoint to generate
anyway.

**It is exposed read-only, though, because otherwise it cannot sync.** Validation
refuses a live-sync endpoint on an unexposed resource — correctly, since an
unexposed table that declares an endpoint has said two contradictory things — and
`rig_file` is the row the URL lives on, so a client that cannot sync it cannot use
the column that exists for it. So `rig_file` gets a real table configuration with
`operations: [read]` and its columns narrowed to id, url, file name, content type
and size. The storage key, the checksum and the tenant never leave the server, and
there is no write path to find.

The alternative was denormalizing the url onto every table that has a file column,
which puts a second copy of the same string in every profile, every document and
every message, and makes a rename a backfill across N tables instead of one.

**The row and the bytes cannot be written together, so pick which leads and make
the other reapable.** Insert with `uploaded_at NULL` and commit that alone, stream
to the store, then — **in one transaction** — set size, checksum, url and
`uploaded_at`, and write the owner: the column on an existing row, or the whole row
when the create carried the bytes. A file row with no bytes is invisible, because
every read filters `uploaded_at IS NOT NULL`, and reapable with one query. Bytes
with no row need a bucket scan to find. Never the other way round. This is also the
answer to where the checksum comes from: you cannot know it before `Put` returns.

> The single transaction is what keeps the sweeper's two rules sufficient. Finalize
> the file first and write the owner second and a crash between them leaves a file
> that is uploaded and referenced by nothing — which neither rule catches, and which
> would force the unreferenced-file reaper rejected below. Commit them together and
> the only failure state is a pending row, which rule one already sweeps.

**A file is immutable, and is never deleted because the thing referencing it
changed.** A table with snapshots copies the whole row on every update, so after
three picture changes the first file is referenced by three version rows. Deleting
the old one on replace corrupts history. That leaves sweeping, with two rules and
no third: abandoned uploads, and trash past the restore window. **No unreferenced
file reaper** — finding those means enumerating every foreign key pointing at
`rig_file`, and the failure mode of getting it wrong is deleting somebody's data.
The bytes have to outlive the window too, or a `Restore` inside it succeeds and
returns a row pointing at nothing.

The sweeper is a `serve.Config.Tasks` entry, so it is a subcommand in a cron job
rather than a goroutine racing itself in every replica. And the restore window lives
in `files.Config` rather than a table configuration, because `rig_file` does not have
one — an asymmetry with `restore_window_days` that is worth knowing about before
somebody goes looking for the key.

### Deleting marks the object too

As described so far a delete only touches Postgres. `deleted_at` is set, and the
bytes sit in the bucket completely unaware for the length of the restore window,
which is a month by default. That is wrong in three ways that only show up late.

A retention policy that depends on a cron job staying healthy is not a retention
policy. The bucket cannot answer "what here is deleted?" on its own, so a bucket
audit, or a bucket restored from a snapshot beside a database restored from a
different one, has no way to reconcile. And when somebody asks whether a file was
deleted, the object is the thing they mean — the row is rig's bookkeeping.

So `blob.Marker` joins `Signer` as a second optional interface: `Mark(ctx, key,
state)`, where the state is live or deleted and carries the timestamp. S3 sets object
tags, which is a separate cheap call that does not rewrite the object and which a
bucket lifecycle rule can act on. `blob.Memory` keeps a flag so tests can assert it.
A backend that has no such concept simply does not have the method, and for that one
the sweeper is the only path — the same shape as `Signer`, for the same reason.

**The row leads and the mark follows.** Set `deleted_at`, commit, then mark, and
treat the mark as best-effort. A failed mark leaves a deleted row beside an unmarked
object, which is the safe direction: the sweeper still knows from the row, and it
re-marks anything it finds out of step on its next pass. The mark is a projection of
the row and is always re-derivable from it. Doing it the other way round — mark
first, commit second — produces an object tagged deleted that the database says is
live, and nothing reconciles that back.

Restore clears the mark, the same way and in the same direction.

> **The trap is the lifecycle rule, and it deletes data.** The natural reason to want
> the tag is so S3 can expire the object without rig. Set that expiry to seven days
> while `files.restore_window` is thirty and a restore inside the window succeeds and
> hands back a row pointing at nothing — the exact failure the sweeper's rules were
> written to avoid, arriving through the bucket instead. The lifecycle rule has to
> outlive the window, and rig cannot enforce a bucket policy it does not own.

What rig can do is refuse to start. The S3 adapter reads the bucket's lifecycle
configuration and fails a `serve.Config.Ready` check when an expiry is shorter than
the configured window, so the mistake surfaces on the first deploy rather than a
month later when somebody tries to undo a delete.

Marking is for the window between a soft delete and its sweep, and nothing else. The
hard delete is `Store.Delete`, when the sweeper gets there. And an owner replacing its
picture marks nothing at all, because that file is not deleted — it is still what
three snapshot rows point at.

### What the generated server has to grow

These endpoints are rig's own, like Create and Restore, which means
`ir.EndpointRequest.ContentType` and `ir.EndpointResponse.ContentType` finally get
read by something. They have been in the IR since M0, written as
`application/json` by `Expand` and consumed by nobody. Now `Expand` writes
`multipart/form-data` and `application/octet-stream`, and `server-go` grows three
handler shapes: a multipart upload, a streaming download, and a create that
dispatches on the request's content type between the JSON body it already decodes
and a multipart form carrying the same JSON in one part.

That last one is the only place a *pre-existing* endpoint changes, and it changes
additively: a create with no `Content-Type: multipart/form-data` behaves exactly as
it does today, byte for byte. Worth stating, because a generated create handler is
the single most-exercised piece of code rig emits and this milestone should not be
able to break it.

Five things there have an obvious wrong answer:

- `http.MaxBytesReader`, not `io.LimitReader`. The generated `decodeBody` truncates;
  for JSON that surfaces as a syntax error, for bytes it is silent data loss.
- `r.MultipartReader()`, not `r.ParseMultipartForm`, which spills a second copy of
  every upload into `os.TempDir` inside a container with a small ephemeral disk.
- Sniff the content type with `http.DetectContentType` and keep the client's claim
  beside it; `Content-Disposition: attachment` unless the sniffed type is on a short
  inline allowlist; `X-Content-Type-Options: nosniff`; RFC 5987 for a non-ASCII
  name; and derive the storage key from the file's uuid, never from the supplied
  one. `evil.html` uploaded as `text/html` and served from the API origin is stored
  XSS, and it matters more here than usual because the URL is in a synced row and
  will end up in an `<img>` or an `<a>` without anybody thinking about it.
- Per-request deadlines through `http.NewResponseController`, not a new
  `serve.Config` field. `ReadTimeout` and `WriteTimeout` are set once on the one
  `http.Server`, so raising them for a 200 MB upload weakens every other route.
  **`serve.Config` needs no new field** — worth saying, because `UploadTimeout` is
  the tempting wrong answer.
- `http.ServeContent` over a `blob.Store` reader that can seek, so the download route
  answers conditional requests and ranges rather than only ever streaming the whole
  thing. Without it a `<video>` cannot seek and a resumed download starts over, and
  both are the kind of thing that gets discovered in production rather than in a
  test. It is also why `Store` has `Stat` — size and modification time are what
  `ServeContent` needs before it will do any of this.

Uploads route through the service like everything else: `ProfileService` gains
`UploadProfileImageFile` and `DownloadProfileImageFile`, `DefaultProfileService`
implements both, and `ProfileHooks` gains a file lifecycle so an application can
refuse a content type, cap a size, or start a derivative in `AfterCommit`. Nothing
about files should be a reason to abandon the generated service and hand-write a
handler.

Rate limiting mostly falls out: `throttle.Postgres` can point at `rig_file` as its
own log, and `DefaultErrorMapper` already turns a `throttle.Refusal` into the right
`Retry-After`. What does not fall out is a byte quota — `throttle` counts events,
and returning megabytes from something whose documentation says events is a lie
inside an interface. Ship a per-account upload count and a hard per-file byte cap,
and say out loud that rig does not do storage quotas.

### And what the generated client has to grow

The section above was written as though `server-go` were the only generator these
endpoints reach. It is not. `go-client` shipped in M5.10, and the day `Expand`
synthesizes a multipart endpoint it will emit a JSON-only method for it — a client
that compiles, calls the right route, and sends the wrong thing. So the SDK is half
of this milestone rather than a milestone after it: uploading a file is one
capability with two ends, and shipping the server end alone means declaring files
done while nothing can use them.

**The transport seam landed first and alone**, before any file code, for the same
reason the two error codes did. `rigclient.Op` grew `Multipart` and `Accept`; `Body`
was left exactly as it was, because its doc comment states one rule — encoded as
JSON when it is not nil — and a type switch would have made that comment false in a
way a reader only discovers by reading `send`. The form is assembled by the
transport rather than by the generated method, which is the same line the file
already draws for path escaping: the generated method knows which argument goes
where, and it does not know what a boundary is.

Two decisions there are worth the words.

**A form is streamed through an `io.Pipe`, so it is sent chunked.** Computing the
envelope's length up front is possible and buys a header almost nothing needs.
`bytes.Buffer` is the thing the whole design exists to avoid.

**A retry on 401 seeks the body, and refuses when it cannot.** `rt.do` re-sends when
a `Reauthorizer` says the credential is worth another try, and a stream cannot be
re-read. `http.Request.GetBody` is no help — that is `net/http`'s own path for
redirects, and this loop re-encodes from the `Op`. So every file part is asserted to
`io.Seeker` and rewound, which covers `*os.File` and therefore the case; anything
else gets `ErrCannotRetry`, joined to the 401 so the failure still answers
`IsUnauthorized`. Buffering every upload so that a retry which almost never happens
is always possible is the wrong trade for a feature whose entire point is that a
large file never lands in memory. The refusal happens *before* the refresh, so a
token is not spent on a call that cannot be made.

A download is a third verb, `DoContent`, answering a `Content` whose `Body` is the
caller's to close — the one fact a caller will get wrong, and the reason the
generated method's doc comment has to say it even though the document does not. Range
and conditional requests are `CallOption`s (`WithRange`, `WithIfNoneMatch`) rather
than generated parameters: the document says nothing about ranges, so nothing
generated should mention them, and `option.go` already exists for exactly the facts
that belong to one call rather than to an endpoint. `WithTimeout` is there for a
duller reason — `DefaultTimeout` caps a whole exchange at thirty seconds and a
context cannot raise it, so a 200 MB transfer on a default client fails with an
error naming nothing about files.

**The multipart create is a second generated method, not a wider first one.**
`CreateWithFiles` beside `Create`, taking a generated `<Resource>CreateFiles` struct
with one member per file column — a plain `Upload` where the column is not null and a
pointer where it is, so the one thing the multipart create exists for is a compile
error rather than a 422. `Create` is the most-called method rig emits and the
server's change to it is additive byte for byte; adding a parameter to it is not
additive, and would break every existing caller the day somebody adds a file column
to a table they already had. A variadic `files ...Upload` is source-compatible and
worse: it puts two wire shapes behind one call site, with different failure modes,
so which one went out becomes a runtime property of a slice's length.

**What the IR owes every generator, and does not have yet.** `ContentTypes` says an
endpoint takes a form and says nothing about what the parts are called. Deriving
`profileImageFile` from a `uuid.UUID` body param means re-deriving the
`<role>_file_id` convention by string-sniffing, in a package that cannot import
`compile.FileRole` — and three generators deriving it separately is exactly the
failure widening `ContentTypes` was meant to avoid. So `EndpointRequest` carries
`FileParts []FilePart` — part name, owning field, role, required — and `Expand`
populates it here. The type and the media constants moved to `pkg/ir` with the
transport commit; the compiler keeps one-line aliases, because a constant written
from one package and read from another is a fact with two spellings.

Two things this milestone has to settle for the client, both open in the prose above:

- **`File` or `RigFile`.** The section on `rig_file` says to inject a builtin object
  the way `Error` and `Pagination` are injected, while `files.expose` projects the
  table — and the projection produces `RigFile`. If both happen, an exposed project
  has two structs for one row and the upload method has to pick; if only the
  projection happens, a project with `expose: false` has uploads and no type to
  return. The builtin goes in regardless of `expose`, under the name the projection
  produces.
- **The upload's success response has to name a `BodyObject`.** `goclient`'s `reach`
  walks out from responses, so the file type is reached for free if it is named
  there and not at all if it is not.

### What rig does not do here

`bytea` stays supported and gains nothing. It works today — the type mapping is
already there — and it is genuinely the right answer under a few tens of kilobytes
for something always read with its row: a signature, an icon. It is wrong at size
for concrete reasons: the repository selects every readable column, so listing fifty
rows drags fifty payloads through the pool; every update copies the bytes into a
snapshot; the JSON encoder base64s them, so there is no streaming, no range and no
`ETag`; and the megabyte cap applies regardless. That is a scope, not a rejection.

Also not here, each ruled out by something above rather than by taste: a flat files
resource, presigned downloads, a CDN in front, image processing, virus scanning, and
storage quotas. File handling is where products diverge hardest, and a framework
that picks derivatives and retention policy for you is wrong for almost everybody.
The metadata table and its tenancy is the part that is the same everywhere, and the
part every project gets subtly wrong. That is the part rig takes.

Binary bodies on *custom* endpoints stay out too. M5.9 reads `ContentType` for the
endpoints rig synthesizes; it does not let a table's configuration declare one,
because that means the service method stops receiving a decoded body in the general
case — `Request[P, Q, B]` carrying an `io.Reader`, `writeJSON` becoming a branch, and
a typed 422 with no fields to attach to. That is a coherent milestone of its own, and
files is not its first consumer.

> One note for M6: it must not claim `application/json` for an endpoint whose IR says
> otherwise. Today it would, because every endpoint says JSON.

### Verification

The fast suite covers `blob.Memory` round-tripping, a key derived from the uuid so
`../../etc/passwd` as a filename cannot escape, idempotent `Delete`, and range reads
at the boundaries; `httptest` handlers proving the sniffed type beats the declared
one, that `attachment` and `nosniff` are always set, that `MaxBytesReader` refuses
rather than truncates, that multipart never spills to `os.TempDir`, and that a
mismatched filename segment does not resolve; and `internal/compile` goldens for
recognition through `Table.ForeignKeys`, the naming exemption, the builtin `File`
object, the derived path segment, the two permission keys and the synthesized
endpoints.

The create handler needs its own row of tests, because it is the one existing thing
this milestone touches: a JSON create on a table with a file column still behaves
exactly as it did, a multipart create binds the `json` part through the same
normalize-and-validate path so a 422 comes back with the same field errors, a
multipart create missing a part for a `not null` file column fails as a field error
rather than a 500, and an unknown part name is refused the way `DisallowUnknownFields`
refuses an unknown key.

The reference check needs a Docker test rather than a fast one, because the whole
point of it is a predicate running in Postgres: an account creating a child row that
names an owner-scoped parent belonging to somebody else gets a 422 on that field and
not a 403, the same request naming a parent it does own succeeds, a soft-deleted
parent is refused the same way a missing one is, and a foreign key to an unexposed
table is not checked at all. Add one that asserts the failure is worded identically
to naming a nonexistent id, since the entire security value is that a caller cannot
tell those two apart.

The Docker suite is a new `internal/filestest` alongside `internal/authtest`: upload,
`uploaded_at IS NULL` mid-flight, completion, url written, checksum matching on the
way back down, tenant B getting a 404 for tenant A's profile id, an owner-scoped
profile's picture invisible to another account, the composite key refusing another
tenant's file, soft delete and restore inside the window still downloading, and the
sweeper reaping an abandoned upload and expired trash while reaping neither a live row
nor one referenced only by a snapshot. That last case is the one that would otherwise
delete data. Pick its port deliberately: there are already a dozen pinned by hand
across the suites and no registry saying which belongs to which, so this is as good a
moment as any to fix that.

The mark needs its own cases in both suites, because it is the one part where two
systems hold the same fact. Against `blob.Memory`: a delete marks, a restore unmarks,
a mark that fails leaves the row deleted rather than rolling anything back, and the
sweeper re-marks an object it finds live in the bucket and deleted in the table. And
one that asserts a replaced picture is *not* marked, since that file is still what a
snapshot points at and marking it would be the same data loss as deleting it.

`examples/todo` gets both forms, or none of this has a regression test outside
goldens: a single `cover_file_id` on the todo itself, which is the table that already
has soft delete, snapshots and restore so the interaction comes for free, and a
`todo_attachment` child table with a `not null` file column, which is the only way to
prove the multipart create actually commits a row and its bytes together.

Honest gap: S3 is unproven until the adapter module has its own container suite,
MinIO or LocalStack. A fake `Signer` covers the handler and nothing else, and the
same is true of `Marker` — that object tagging works, and that the lifecycle
readiness check reads a real bucket configuration and refuses a window it cannot
honour, are both claims only that suite can make.

### Open question for you

**Is the reference check part of M5.9, or does it land first and alone?**

It is not a file feature. It changes the write path of every generated repository
that has a foreign key to an exposed resource, which is most of them, and it will
move goldens in `persist-go` and every example. Folding it into M5.9 makes one diff
that adds files and quietly rewrites how every create in the system validates —
which is the kind of commit that gets approved because it is too big to read.

There is also a real chance it breaks something on the way in. Any existing test that
creates a child row while authenticated as somebody who cannot read the parent will
start failing, and each of those is either a bug this found or a fixture that was
sloppy. Finding out which is a job, and it is a job that has nothing to do with
uploads.

I lean landing it first and alone, the same way the two error codes do — three
commits before any file code exists: the codes, the reference check, then M5.9 proper
on top of a floor that already holds. The cost of that order is that the invariant
ships with no feature to justify it in the changelog, and the reason it exists lives
only here.

The alternative is one milestone that tells a complete story, at the price of a diff
nobody can review in one sitting. Say which you would rather have.

---

## Done: M5.10 — `go-client`

**What shipped.** A Go SDK generated from the document, in two halves.

`rigclient/` is the hand-written half — one more module, depending on `runtime`
and `uuid`. The transport (`Do`, `DoNoContent`, the typed `*Error`, the call
options), the credentials (`StaticToken`, `APIKey`, and a `Session` that renews
itself), the query encoders, the pagination iterator, and `Auth`: rig's own
authentication endpoints, which are the same routes with the same bodies in every
project that turns authentication on and so are not generated at all.

`internal/gen/goclient` is the generator, registered as `go-client`. Options are
`package`, `client_import` and `default_base_url`; everything else it needs is in
the document. It emits the entity and its enums, the filters, the create and
update inputs, each endpoint's own body and query, the shape a 422 arrives in,
and one method per endpoint keyed on `Impl.ServiceMethod` — plus, where there is
a `List`, an `All` that walks every page.

`runtime/authwire` came out of the same work: the authentication request and
response shapes used to live unexported inside `auth/authhttp`, so a client had
to restate them. Now both ends import one definition, and `authhttp` fills in the
same structs a client reads.

**Decisions worth knowing about.**

- **Three things are not restated.** Update inputs use `runtime/patch`, error
  codes use `runtime/rigerr`, and the scope parameter uses `runtime/tenancy`.
  Everything else — the entity, the filters, the page — is the client's own, so
  an SDK does not drag the server's `internal/` packages behind it.
- **`omitzero` on every update member, and `IsZero` on both patch wrappers.**
  This was a real bug on the way in. `Optional.MarshalJSON` encodes absence as
  `null`, because a marshaler cannot remove its own field — harmless while patch
  types were only ever decoded, and on a client it would have meant a 400 on
  every not-null column and a *silent clearing* of every nullable one. The tag is
  what removes the field; the test that guards it asserts that an untouched
  update marshals to `{}`.
- **Query parameters are pointers.** `server-go` applies a parameter's default
  only when it is absent, so a client that helpfully sent `limit=0` would be
  asking for an empty page. Nil means not sent, and `rigclient.P` takes an
  address in one expression.
- **The QUERY fallback is the client's, once.** A search issues `QUERY`; on 405
  or 501 it retries the `POST …/_search` alias, remembers that on the runtime,
  and never tries `QUERY` again. Under `search_method: query` there is no alias
  and the refusal is simply reported.
- **Methods are grouped per resource** — `c.Todos.Complete(...)` — which is
  symmetric with the server's own service methods. The `OperationID` is in each
  doc comment so the router, this client, and the OpenAPI document to come can
  still be cross-referenced.
- **`API-Revision` is a seam, not a feature.** M5.7 has not shipped, so there is
  nothing in the document to bake in; `Config.Revision` is sent when it is set,
  and that is where the generated constant will go.

**Where it is exercised.** `examples/todo` and `examples/auth` both generate one,
into `./client` rather than `./internal/client` — a client exists to be imported
by somebody else, and Go's internal rule would keep it out of exactly the code
that wants it. Each has a `client_docker_test.go` driving the real server: the
lifecycle and the one-field patch verified by re-reading the row for todo, and
sign-in, refresh-ahead under a moved clock, tenant switching and API keys for
auth. `examples/sdk` is the third example — not a rig project, just a program
that calls the other two — with database-free tests pinning the three properties
a caller depends on, and a third subcommand that is the case a client library
actually gets used for: a CSV import, with a bounded worker pool, a retry that
distinguishes the server asking for a moment from the row being wrong, and a
report naming the line of everything that did not go in.

**Loose ends.**

- `examples/fantasyfootball` and `examples/auth_oauth` do not generate a client.
  Nothing stops them; there was no question they would have answered.
- The `.gitignore` pattern `rig` matched `cmd/rig` as well as the built binary,
  which is why the command's own `main` was never committed. Anchored to `/rig`,
  and `cmd/rig/main.go` is in the tree.
- A `Decimal` column is `pgtype.Numeric` in the client as well as in the model,
  so a project with one drags pgx into its SDK's dependency graph. `runtime`
  already does, through `patch`, so it costs nothing today — but if `rigclient`
  is ever cut loose from `runtime`, this is the field to reconsider.

---

## M5.11 — referential actions (shipped)

**Goal.** When a row is deleted, every table pointing at it gets a function call —
inside the same transaction, before the row goes and again after it is committed — so
a child can refuse the delete, clean itself up, or do nothing, in code it wrote.

Shipped as one milestone, which answers the last open question below: the closure
argument held up, so there was never a separate registry to land first. Two things
came out differently from the sketch and both are noted where they belong — the
registry is assembled in `api.Register` rather than at `Bind`, and a parent must
offer `Delete` for its children to get hooks at all.

### rig already wrote the argument and never finished the sentence

`checkCascades` refuses `ON DELETE CASCADE` outright, RIG6040, error by default:

> A cascade deletes rows behind the service layer's back: no hooks run, no audit
> entries are written, and soft-deletable children are hard-deleted regardless.
> Deleting through the service is slower and correct.

Every word of that is true, and there is nothing on the other side of "instead".
Deleting through the service means deleting one table, because that is all a
generated `Delete` touches. So every scaffold and example foreign key is plain, a
parent with children raises `23503`, and generated `writeError` turns it into
`rigerr.Conflict("a related row is missing or still in use")` — a 409 that names no
table, no relation and no field, for a condition the schema knew about at compile
time.

The result is that the correct answer today is to hand-write it: a `Delete.Before`
hook that reaches for a repository it was never given, or a raw `DELETE` that skips
the very hooks RIG6040 exists to protect. Both are the class of work rig removes
everywhere else.

### Most of the machinery is already here

**The transaction is.** `dbx.InTx` reuses a `pgx.Tx` already on the context and does
not commit it — that is the documented reason it exists. A child repository's
`Delete` called from inside a parent's `Delete` already joins the parent's
transaction and already commits with it. Nothing has to be built for "in the same
transaction"; it is the property nesting was designed for.

**The after-commit queue is.** `dbx.AfterCommit` appends to a `*pending` on the
context and only the outermost `InTx` drains it. So a child's `AfterCommit` hook
already fires exactly once, after the parent commits, in the right order, with the
claims captured. The half of this request that is "inform the other services
afterwards" is finished code.

**The relations are.** `projectRelations` already walks every table looking for a
foreign key pointing back, and emits `ir.RelationHasMany` with the foreign table and
column, disambiguated when a child points twice. `ResourceStorage.Relations` carries
them. Today the only consumer is `sc.hasMany(...)` inside filter subqueries — the
compiler knows the shape of every parent-child edge and no write has ever read it.

**The veto is.** `DeleteHooks.Before` runs inside the transaction and an error from
it aborts. A child that may not be deleted for a reason only the application knows
is already expressible; it just has no way to be consulted, because nothing calls it.

**What is missing is the call.** Every piece above is a mechanism waiting for
somebody to use it. `projectRelations` knows that `player` points at `team`, `InTx`
would carry a `player` write inside a `team` delete, `AfterCommit` would fire the
follow-up in the right place, and `Before` would let the application refuse — and no
line of generated code ever puts those four together, because nothing tells a child
that its parent is going away.

### The declaration is a function, not a key

The tempting shape is an enum on the relation — `on_delete: restrict | cascade |
set_null | ignore` — and it should be rejected before anybody builds it. Those four
words are a vocabulary, and a vocabulary can only ever cover the cases whoever wrote it
thought of. `set_null` cannot say "null it, unless it was the last one, in which case
archive the row". `cascade` cannot say "delete the drafts and reassign the published
ones". Every application has a clause like that, and the moment one appears the key is
worth nothing and the work is hand-written anyway — next to a configuration line that
now lies about what happens.

Which is also to say the enum is not one feature, it is four, and each of them is three
lines of code somebody could have written themselves if rig had simply told them the
delete was happening. **So rig tells them, and that is the whole milestone.** A child
declares a function; the parent's delete calls it.

```go
// services/player/player.go
func (s *service) Hooks() api.PlayerHooks {
	return api.PlayerHooks{
		Delete: dbhook.DeleteHooks[…]{Before: s.beforeDelete},

		// One field per foreign key this table has to another resource.
		Parents: api.PlayerParentHooks{
			TeamDeleting: s.teamDeleting,
			TeamDeleted:  s.teamDeleted,
		},
	}
}

// Runs inside the transaction that is deleting the team, before the team row
// is touched. Returning an error refuses the delete and rolls back everything
// the other children already did.
func (s *service) teamDeleting(ctx context.Context, claims tenancy.Claims,
	team *model.Team, del model.TeamDeleteInput) error {

	return s.repo.ClearTeam(ctx, team.ID)   // …or delete them. Or refuse. Your call.
}
```

The three cases the enum was for are now the three obvious bodies of that function: an
update that nulls the column, a loop calling the child's own `Delete`, and a `return
rigerr.Conflict(...)`. None of them needed a keyword, and the fourth case — the one with
the clause in it — is the same function with an `if` in it.

`Deleting` rather than `Deleted` because it runs before the row is gone and can still
say no. `TeamDeleted` is the other half the request asked for: it runs after the commit,
through the `AfterCommit` queue that already exists, best-effort, for the cache eviction
and the search index and the email — the things that must not be able to fail a delete
that already happened.

`del` is passed rather than dropped because `TeamDeleteInput.Hard` is the difference
between a soft delete the parent can undo and a permanent one, and a child that nulls a
link on the first has destroyed the only record of what to re-link on a restore. That is
the trap the `set_null` key would have walked straight into, and handing the child the
input is how it becomes visible instead of surprising.

### The registry is the outbox, and the parent never sees the child

The parent's repository does not need a `PlayerRepository`. **It needs the closure, and
the closure already carries the repository it closed over.** That is the part that makes
this small: the reach problem above evaporates, because nothing has to be injected
backwards. Each service registers its parent hooks where it is already wired —
`XRules.Bind(XWriter)` is that moment — and `Delete` runs whatever is registered against
its own table.

The list is compiler-generated and typed, one entry per `HasMany` the IR already
derives — not a map assembled at startup, so it cannot depend on the order somebody
happened to construct services in. A parent delete is then five steps:

1. the parent's own `Delete.Before`, exactly as today;
2. every registered `<Parent>Deleting`, in the order below;
3. the parent row, exactly as it is deleted today;
4. the parent's own `Delete.After`, still inside the transaction;
5. every `<Parent>Deleted` queued onto `dbx.AfterCommit`, in the same order as step 2,
   firing once after the outermost transaction commits.

The parent's own veto comes first on purpose. "This team may not be deleted while the
season is open" is the cheapest and most specific rule in the building, and it should not
require running every child's cleanup before it gets to say so.

A child that deletes its own rows by calling its own `Delete` triggers *their* children
the same way, so depth is just the call stack and recursion falls out rather than being
designed. What does have to be designed is the guard: a visited set keyed by table and id,
and a depth cap in the shape of the `MaxFilterDepth` the filter builder already has, so a
cycle in the schema terminates instead of exhausting the stack.

### The order of siblings is a fact the compiler can already derive

Depth takes care of itself. Siblings do not: `player` and `fixture` both point at `team`,
and something has to decide which one hears about the delete first.

**It does not matter for correctness.** Everything is in one transaction, so any hook
returning an error unwinds every hook before it, and no ordering can leave the database
half-done. It matters for exactly two things, and both are worth naming because they are
the reason somebody will file a bug:

- **What one sibling can see of another.** If `fixture`'s hook counts the team's players,
  it gets a different answer depending on whether `player`'s hook has already run.
- **Which error the caller gets** when two siblings would both refuse. Whatever the order
  is, it has to be the same on every request and on every process, or the same delete
  answers differently on Tuesday.

Alphabetical would satisfy the second and nothing else, and "your hooks run in alphabetical
order" is the kind of rule that is technically documented and never once anticipated.

**So order the siblings by their own foreign keys.** If `fixture` references `player`, then
`fixture` hears about the team's deletion before `player` does — referencing tables before
referenced ones, which is the same order the rows themselves would have to go in. The
compiler has that graph already; it is the same edges `projectRelations` walks to build the
`HasMany` list in the first place. Topologically sorted, ties broken by the IR's order so
the result is stable.

That default is right for the case that actually recurs, and it costs no configuration. A
cycle among the siblings has no topological order, so fall back to IR order and say so in a
diagnostic rather than silently picking one.

**When the derived order is wrong, the parent overrides it**, because the parent is the only
place that can see all its children at once:

```yaml
# services/team/team.yaml
on_delete:
  order: [fixture, player]   # anything unlisted runs after, in the derived order
```

Note what is and is not in that file. The parent states the *sequence*, which is a fact
about the relationships between its children and belongs to no single one of them. It does
not state the *action* — that is still a function on the child, and the parent has no
opinion about it. The two things the enum was conflating come apart cleanly here, and only
one of them turns out to be configuration.

There is deliberately no hook point between step 3 and step 4 — after the row is gone but
still inside the transaction. A child with a non-null foreign key cannot still be pointing
at the parent by then or the write would have failed, so anything it had to do it did in
`Deleting`, and anything it can do afterwards it can do in `Deleted`. Adding the third point
would mean a hook whose only legal actions are the ones the other two already cover.

**Implementing nothing is a supported answer and stays the default.** A table that
declares no parent hooks behaves exactly as it does today: the foreign key refuses, `23503`
becomes a 409, and nothing about existing projects moves. The improvement available for
free is the message — the parent knows which relations it has, so a refusal can name the
one that blocked it instead of saying "a related row is missing or still in use".

### Where it cannot reach

Same boundary M5.9's reference check draws, for the same reason: a table with no resource
has no hooks to declare and no service to declare them in. rig cannot notify it and should
not pretend to. `rig_file`, `rig_account` and the audit actor columns are all on that side
of the line, and their foreign keys keep behaving exactly as the schema says.

The rule is one sentence: **rig calls the tables it generates a service for.**

### What it costs, honestly

One function call per relation per delete — not one per child row. That is the reason to
hand the child the parent rather than each of its own rows: nulling ten thousand links is
one `UPDATE` the child writes itself, and rig has no business turning it into ten thousand
statements on the child's behalf.

The cost that is real is the one the child chooses. A `teamDeleting` that loops calling its
own `Delete` to get each row's hooks and snapshots is as slow as the number of rows, and
that is the correct price for what it bought. rig should say so where the function is
scaffolded, since the fast version and the correct version are both one line and they do
not look different.

### RIG6040 stays, and gains nothing

Nothing here makes `ON DELETE CASCADE` acceptable; it makes the alternative exist, which is
what the rule's own comment has been promising. And because the alternative is a function
rather than a configuration key, there is no second place for the same decision to live and
no new contradiction to detect — which is a small argument for functions over keys all by
itself.

### Verification

The fast suite can prove most of this. `dbhook` and `dbx` are testable against a stub
`Conn`: that a registered `Deleting` runs before the parent row is written, that returning
an error from one aborts the transaction and unwinds what an earlier one already did, that a
nested `Delete` does not commit early, and that every `Deleted` lands on the `AfterCommit`
queue and drains exactly once when the outermost transaction commits.

`internal/compile` goldens for the generated `XParentHooks` struct — one field pair per
foreign key to an exposed resource, and none for a foreign key to `rig_file` or an audit
column — and, separately, for the derived sibling order, which is the part with an
algorithm in it: the topological sort, the IR-order tie-break, the `on_delete.order`
override, and a deliberate sibling cycle asserting the diagnostic rather than a silently
chosen order.

`examples/fantasyfootball` is the fixture worth using for both. `fixture` points at `team`
twice, so its golden proves the `foreignKeyQualifier` disambiguation already in
`projectRelations` produces two distinct hook names rather than a collision — and `fixture`
also points at `player`, so deleting a team is a real three-table sort rather than a
one-element list that would pass under any implementation.

The Docker suite is where it has to be real: a child that refuses and leaves the parent
present, a child that nulls a link and a parent delete that then succeeds, two levels of
child, a cycle that terminates, and a soft parent delete whose child correctly read
`del.Hard` and did the reversible thing. Also the case with no hooks at all, asserting the
`23503` path still answers a 409 — that one is the regression test for every project that
does not want this feature.

Honest gap: nothing here tests that the *scaffold* teaches the difference between the fast
body and the correct one, and that is the part most likely to be got wrong in the field.

### The questions, answered

**The child gets the parent row, not its own rows.** One call per relation. The
alternative — rig running a `SELECT` on the child's behalf — is a query the child may not
want, and the generated doc comment says instead what the two bodies cost, because the fast
one and the correct one are both one line and do not look different.

**The sibling order is derived, and `rig ir` prints it.** `Resource.Children` carries the
resolved sequence, so the answer exists somewhere you can ask for it rather than in nobody's
head. `on_delete.order` overrides it; a cycle among siblings falls back to schema order and
says so as RIG5060 rather than silently picking.

**`Deleting` does not fire for a restore.** `Restore` stays the one path that deliberately
walks nothing. It is still the obvious next request.

**One milestone.** The closure argument held: nothing had to be injected backwards, so there
was no dependency-injection change to land separately.

### Two departures worth reading before changing either

**The registry is assembled in `api.Register`, not at `Bind`.** `Bind` was the sketch's
answer and it cannot work as written: `Default<Res>Service` is a value, `Handlers` holds it
in an interface, and a value in an interface cannot be addressed — so a child constructed
after its parent has nothing to register with. `Register` sees every service at once and
runs before anything is served, and `Handlers` already has one field per resource, so the
compile-time check survives. The writer holds `children *<Res>ChildDeletes` and reads it at
delete time; `Link` is exported for the program that builds services and serves them some
other way.

**A parent has to offer `Delete`.** Being exposed is not enough, and the case that forces
the distinction is `rig_file`: with `files.expose` it is a resource, and it is
`operations: [Get, List]` — the write path is the upload endpoint on the row that owns the
file. A `RigFileDeleting` field on every table with a file column would be a hook nothing
can ever reach, and a field that can never fire is worse than no field, because somebody
implements it and waits.

---

## M5.12 — the auth log and who is signed in (shipped)

**Goal.** A tenant can read its own authentication trail and see every session
open inside it, and a person can read their own, through endpoints rig serves
rather than a query the application writes.

**What shipped**, against the design below. `authlog` gained `Record`, `Query`,
`Reader` and `Pruner` beside `Log`, and a `Memory` double that is all four;
`authpg`'s writer moved out of `apikey.go` into an `authlog.go` of its own and
grew the read, the count and a batched prune. `session.Store` gained
`TenantFamilies`, `Manager` gained `ListTenant` and `FindSession`.
`authhttp` mounts `GET /auth/audit` when a reader is configured and answers
`scope` on it and on both session routes, through one `scope` helper that every
endpoint here shares. `authwire` gained `Page[T]`, `Pagination` and
`AuthLogEntryView`; `rigclient` gained `AuditLog`, `AuditLogAll`, `AuditQuery`,
and `Wide()` — which needed a per-call query parameter the transport did not
have. `internal/compile` injects `AuthLogEntry` for any document with an `auth:`
block. `auth.log_retention` in rig.yaml writes an `AuthLogPruner` task, and both
the compiler and `auth.New` refuse a window shorter than the longest rate-limit
window.

Four departures from the design below, all deliberate:

1. **`Manager.Revoke` never stamped a tenant, so every `Logout` entry was
   invisible to the trail.** `rig_auth_log.tenant_id` is nullable for the
   attempts that resolved to nobody, and every reader filters on it — so the one
   event this milestone most obviously needs to show was the one event it could
   not have shown. `RevokeBy` reads the root token before revoking and stamps the
   tenant, the account, and the address; `Revoke` is it with no actor. The bug
   was invisible for as long as nothing read the table, which is the argument for
   this milestone in one sentence.

2. **`SessionView` gained `AccountID`.** "See every session open in the tenant"
   is not answerable without knowing whose each one is, and the narrow list fills
   it in too — a member present for one reading of an endpoint and absent for
   another is a member no client can rely on.

3. **The IR object's wire names are literal, not run through the namer.**
   `api.json_case` shapes the keys rig *generates*, and `authwire` is
   hand-written and shared by every project, so `/auth/*` answers camelCase
   whatever a project sets. An object rendered through the namer would describe a
   response nobody receives. A reflection test compares the object's field order
   and wire names against the struct's json tags, which is what makes declaring a
   shape nothing reads yet safe rather than aspirational.

4. **The example derives the auth permission keys instead of listing them.**
   `authz.AuthKeys()` reads `authhttp.Permissions()`, so the three new keys
   reached the seeded Owner role without anybody editing three call sites — which
   is the same argument `api.PermissionKeys()` already made for the application's
   own. The demo page now reads the trail through the endpoint and shows the
   refusal when the permission is missing, the way it already did for notes.

`rig_auth_log` has been recording since M4 and it records nearly everything: 22
events across sign-in, lockout, logout, refresh, token replay, key
authentication, impersonation, invitation and tenant switch, written from about
twenty call sites in `account`, `session`, `apikey` and `oauth`. **Nothing reads
it.** There is no route for it in `authhttp`, and the only reader in the
repository is a hand-written `SELECT … FROM rig_auth_log WHERE tenant_id = $1
ORDER BY created_at DESC LIMIT 40` in the demo's web page, whose own comment
says what the situation is: `auth.expose: [account, auth_log]` "would give them
a REST resource with filters and paging, and this is the other answer — a query,
in the application, for a page that needs one." Two answers, and neither of them
is rig serving the thing rig collected.

Sessions are half-served rather than unserved. `GET /auth/sessions` and
`DELETE /auth/sessions/{id}` exist and both are yours only — `Sessions.List(ctx,
tok.TenantID, tok.AccountID)`, with a deliberate 404 for anything that is not.
So a session can be ended by the person holding it and by nobody else, which is
the wrong answer the first time somebody leaves the company with a phone in
their pocket.

### `scope` is the whole design

`runtime/tenancy/scope.go` already settled this argument for every generated
resource, and its doc comment makes the case better than a restatement would:
one endpoint that quietly returns different sets to different callers, or two
endpoints where a client written against the narrow one silently keeps working
when its credential is widened — "both hide the width of the answer."

So this milestone adds **one** route and **widens two**, rather than growing an
administrative surface beside the end-user one:

```
GET    /auth/audit                      your own events        no permission
GET    /auth/audit?scope=all            the tenant's events    authlog.read.all
GET    /auth/sessions?scope=all         every session open     session.read.all
DELETE /auth/sessions/{id}?scope=all    end somebody else's    session.revoke.all
```

The self-service half falls out for nothing. `ScopeOwn` on the log is
`account_id = $me`, which is "where have I signed in from, and did anything fail"
— a screen every product eventually wants and which would otherwise be a second
endpoint with a second shape and its own bugs.

The alternative is `/auth/admin/*`, and it is worth naming because it is what
most systems do. It would be the first split surface in a package that has
deliberately kept one mux, one `Authorization` header and one `Claims` type, and
it would need its own answer to the existence-oracle question — which `scope`
answered once, loudly, for everything.

Three keys join `authhttp.Permissions()`, beside `apikey.manage` and
`account.impersonate`, so a reconcile picks them up without an application
listing them.

> **The widening is the permission, not the identifier.** `DELETE
> /auth/sessions/{id}` answers 404 today for a session that is not yours, and it
> must keep answering 404 for a caller who lacks `session.revoke.all` — the same
> 404, for a real session and a made-up one alike. A 403 there would confirm the
> session exists, which is the one thing the current handler's comment says it is
> avoiding.

### The reader is not the writer

`authlog.Log` is `Write(ctx, Entry)` and returns nothing, on purpose: an entry
describing a failed login must never fail the login, and `authpg` writes outside
the caller's transaction so the row survives the rollback that noticed it. A
reader cannot live under that contract. A query that could not reach the database
has to say so, and a reader bolted onto `Log` would be one interface where half
the methods report failure and half swallow it.

So `authlog` gains a separate `Reader`, for the reason `authhttp`'s identity
reader already gives about being separate from `Claims`: the two answer different
questions, and "a function that could return either would eventually be used
where only one of them is safe."

`session.Store` gains a tenant-wide `Families` alongside the account one rather
than a nullable account argument, for the same reason and with the same shape.
`authpg` implements both.

### Strictly `tenant_id = $1`, and the hole stays

`rig_auth_log.tenant_id` is the one nullable tenant column in the foundation.
The reason is in the migration: an attempt that resolved to no tenant is a real
event and the one a rate limit needs most — somebody guessing an address nobody
has, or signing in without naming a tenant.

Those rows stay invisible to everybody. The query is `tenant_id = $1` and nothing
else. The tempting widening is to match on the email address instead, so a tenant
sees failed attempts against its people's addresses even when the tenant was
never named — and it hands tenant A a record of tenant B's people typing their
own addresses into a login form, keyed on an address that resolved to nobody.

**The hole is narrower than it sounds and should be documented rather than
closed.** `failLogin` stamps the tenant whenever there was one, so what a tenant
cannot see is attempts that named no tenant at all and attempts against addresses
with no account anywhere. That is exactly the population a single tenant has no
standing to see, and the place to fix a global brute-force view is not a
per-tenant endpoint.

### The first paginated route in `authhttp`

Every `/auth/*` list answers `authwire.List[T]`, which is `{"data": […]}` and
nothing else — no limit, no offset, no total. `listAPIKeys` loads the tenant's
keys and filters them in Go, and the comment justifying that says "a tenant's
keys are a handful of rows," which is true of keys, invitations and tenants and
is the reason the shape has survived.

An auth log is millions of rows and cannot borrow the argument. So `/auth/audit`
answers `{data, pagination}` with the numbers the generated endpoints already
use — `DefaultLimit` 50 and `MaxLimit` 500 from `internal/compile/builtin.go`,
clamped by `query.Page.Clamp`, whose own comment is the reason ("an unbounded
list is a production incident waiting"). The other four stay exactly as they are.
Two shapes in one package costs a sentence in the docs; paginating four endpoints
that will never need it costs four handlers and four sets of tests.

M5.10 makes this cheaper than it would have been. `authwire` is where the shape
goes, beside `List[T]`, and `rigclient.Paginate` already walks a `{data,
pagination}` read to its end — so the Go client's method for this is an iterator
on the first day rather than a loop every caller writes.

Filters are `accountId`, `event`, `outcome`, `since` and `until`.

> **The indexes on this table are shaped for counting, not browsing**, and the
> migration says so. `(tenant_id, created_at DESC)` covers the default page and
> the `since`/`until` window. Filtering by event within a tenant scans that
> tenant's slice, and the fix — `(tenant_id, event, created_at DESC)` — is a
> write cost paid by every login on the hottest write path in the system, to make
> a screen somebody opens twice a month faster. Ship the filter, say what it
> costs, and add the index when a real log is big enough to argue for it.

### Exposing the table is the other answer, and it stays

`--expose rig_auth_log` already works: the scaffold ships a table configuration
with `operations: [Get, List, Search]` and `order_by: [-created_at, id]`, and
`TestEveryCreatedTableCanBeExposed` holds it there, because `--expose` that works
for some of the foundation and silently writes nothing for the rest is worse than
no `--expose` at all.

That is not a contradiction with the migration's warning; it is the warning's
subject. A generated reader filters by `tenant_id`, so it cannot see the
tenant-less rows and has nowhere to explain that it cannot. The endpoint's whole
job is that distinction — it excludes those rows deliberately and the
documentation says so.

**So both stay, and the difference between them is the point.** `--expose` is for
a project that wants the log as an ordinary synced resource with the generated
filters, and accepts a trail with a silent hole. The endpoint is for a project
that wants the trail. Saying that plainly in `docs/auth.md` is cheaper than
removing an option somebody is already using.

### Retention

Nothing prunes `rig_auth_log`, and exposing it is what makes that visible rather
than what causes it. A `serve.Config.Tasks` entry, the shape M5.9's sweeper
settled on, so it is a subcommand in a cron job rather than a goroutine racing
itself in every replica.

> **The trap is that this table is what the rate limiter counts.** A retention
> window shorter than the longest limit window would clear a lockout by deleting
> the rows it is counted from — a limiter that silently stops limiting, which is
> the failure nobody notices until it is being exploited. The longest today is an
> hour, so any sane window satisfies it, and that is precisely why somebody will
> set it to fifteen minutes during an incident without knowing the constraint
> exists. Refuse a window shorter than the longest configured limit at startup,
> the way M5.9's S3 adapter refuses a bucket lifecycle shorter than the restore
> window.

### What rig does not do here

- **No cross-tenant view.** That is `readopt.WithoutTenantScope()`, which is
  already documented as being for administrative tooling and cross-tenant
  reporting "and for nothing a request handler should ever do." A global view of
  attempts that named no tenant is a real need and it is an operator's need, not
  an endpoint's.
- **No export, no webhook, no alerting, no anomaly detection.** The events are in
  a Postgres table with seven indexes; a product that wants them in a SIEM reads
  them out. Choosing a destination format would be wrong for everybody using a
  different one, and a framework that ships half an alerting system ships the
  half nobody can extend.
- **No row-level audit for application tables.** That was removed in M5.6 and
  snapshots replaced it, and this must not read as its return. This is
  authentication only: what happened to a credential, never what happened to a
  row.
- **No IR entry, so M6 will not document these.** True of every route `auth`
  mounts, and M5.9 already says so. Whether that stays true is the open question
  below.

### Verification

The fast suite covers scope parsing and refusal, the page clamp at both bounds,
filter rendering, and the one thing that is easy to get wrong when two interfaces
sit in one package: that `Reader` returns its errors while `Log.Write` still
swallows its own.

`internal/authtest` covers what only a database can say. Tenant B sees none of
tenant A's entries. A tenant-less row is invisible to both. `scope=own` returns
the caller's and nothing else, and does not need a permission to do it.
`scope=all` without the key is a 403 while an unknown session identifier is still
a 404 — asserted side by side, since the whole point is that those two answers
stay different for different reasons. An administrator revoking somebody else's
session kills the family and writes a `Logout` naming who did it. And the
retention task leaves anything inside the longest limit window alone, which is
the case that would otherwise disable the lockout.

`examples/auth/web/page.go` drops `authLog` and calls the endpoint, which is the
regression test worth having: the endpoint either answers the question that query
was written to answer or the page stops working.

`docs/auth.md` gains rows in the endpoint table, the three permission keys, a
paragraph on the tenant-less rows under what the log does not show, and a note in
the status-code section that the 404-not-403 rule survives the widening.

### Open question, answered

**Should the auth log reach the IR, or is `authhttp` its home for good?**

**Answered: the shape is declared now.** `AuthLogEntry` is injected as a builtin
for any document with an `auth:` block, so the day `openapi` and `ts-client` can
describe an `/auth/*` route, the trail is already in the document they read.
Nothing consumes it yet — no generator emits it, because none of them iterate
builtin objects and no endpoint references it — so the cost is one object in two
golden files and the risk is drift, which the reflection test closes. The lean
below was the other way; what changed it is that the guard test makes the
declaration checkable, which was the whole objection to declaring a shape early.

The reasoning below stands otherwise, and the conversion it describes is still
the milestone it always was.

Every route `auth` mounts is invisible to `openapi`, to `ts-client` and to
`go-client`, and M5.10 put a number on what that costs: `rigclient/auth.go` is
460 hand-written lines mirroring `authhttp` method for method, because those
routes "are the same routes with the same bodies in every project that turns
authentication on and so are not generated at all." Adding an endpoint here means
adding a thirty-first method there by hand, and this is the first one where that
is a real loss rather than a bookkeeping one: an audit screen is exactly the kind
of thing somebody builds against a generated client.

The fix is not exposing the table — that is the other answer, above, and it has
the hole. It is teaching the IR about routes rig serves from a hand-written
module, which would eventually pull all thirty `/auth/*` endpoints in with it and
delete most of that file.

That is a milestone of its own and a large one. The question is only whether
M5.12 should be built knowing it is coming — the response shape declared as an IR
object even though nothing reads it yet, the way `EndpointRequest.ContentType`
sat unread from M0 until files needed it — or built as an ordinary `authhttp`
handler and converted later with everything else.

I lean ordinary. A shape declared for a consumer that does not exist is a shape
nobody can check, and the conversion is the same size either way. Say which you
would rather have.

---

## M7 — `ts-client`

**Goal.** A TypeScript client generated from the same document, so the types a
front end holds are the types the server sends.

**Shape.**

- `internal/gen/tsclient`, generator name `ts-client`.
- Options: `package_name`, `out_dir: web/src/api`.
- Types from `ir.Object`; enums as `const` objects plus a union type, carrying
  both the identifier and the wire value — a TypeScript `enum` cannot express
  `InProgress = "in_progress"` without becoming a runtime import.
- A `fetch` client, no dependencies. One method per endpoint, named from
  `OperationID`.
- Refresh behaviour comes from `ir.API.Auth`: a client that knows the access
  lifetime and the rotation leeway can refresh ahead of expiry instead of waiting
  for a 401, and it knows those because they are in the document.
- `Patch` semantics on update calls: a field left out of the object is left
  alone, `null` clears it. The type is `T | null | undefined` and the doc
  comment says which is which.

**The QUERY fallback.** Issue `QUERY`; on `405` or `501`, fall back to the
`POST /_search` alias, remember that for the rest of the process, and never try
`QUERY` again. One flag on the client instance.

**Echo the Go client.** `go-client` shipped first (M5.10) and answered several
of these already: methods grouped per resource, a fallback tried once and
remembered per client instance, query parameters that are absent rather than
zero, and a per-input shape for the 422 body. Differing from it needs a reason
better than a different author.

**Open questions for you.**

- Runtime validation, or types only? Types only is smaller and honest about
  what a compiler can prove; a `parse` step catches a server that lied. I lean
  types only, with the response typed as what the document promises.
- React Query / SWR helpers, or plain functions? Plain functions compose with
  either; helpers save a file per resource in the projects that use them. I lean
  plain, with the shape chosen so a `useQuery` wrapper is two lines.
- Is `web/src/api` the right default, or should it be configurable per project
  without a default at all?

**Verification.** `tsc --noEmit` over the generated client in CI, plus a fetch
test against the M3 server exercising the QUERY→POST fallback.

---

## M8 — docs, the second example, and the remaining polish

**The full demo shipped as `examples/linearlite`, not fantasyfootball.** A
Linear-style board: the auth foundation end to end (registration with an
OnRegistered auto-invite, tenant creation, personal API keys), soft delete,
snapshots, files, notifications with the actor excluded, live sync wired all
the way to a browser — the first caller of `electric.Register` and the first
consumer of the generated TypeScript client (a React app in `web/`, served by
the binary from `web/dist`) — plus a CSV import job over the generated Go
client. Built and tested by `make examples` like the others; the front end has
its own `linearlite-web` target because the example suite deliberately needs
no pnpm. What it surfaced along the way: `OnRegistered` on the auth config,
`database.settings`/`database.electric` so `rig db up` runs the sync service,
`Where.EqText` because Electric cannot type a value against a Postgres enum,
and an `Omit<>` on the multipart create so the documented TS call compiles.

**`examples/fantasyfootball`** stays the relations-and-observability example;
the "many tables, custom endpoints, OAuth" expansion below is still open:

> The full demo: many tables, relations, enums, soft delete, snapshots, custom
> endpoints, RBAC, OAuth sign-in, API keys, live sync, and a small TypeScript
> consumer. Built end to end in CI the way `examples/todo` is.

**`docs/`.**

```
tutorial.md        build the todo app from an empty directory
concepts.md        the three layers, what is generated, what you own
schema.md          column conventions, soft delete, snapshots, audit, tenancy
configuration.md   rig.yaml and per-table reference, generated from the JSON Schema
generators.md      built-ins, options, writing your own
auth.md            setup-project, sessions, rate limits, API keys, OAuth, RBAC
electric.md        live sync
```

The configuration reference is generated from the same JSON Schema the validator
uses, so it cannot describe keys that do not exist. The tutorial is verified by
CI: a script builds `examples/todo` from an empty directory and diffs the result
against what is committed.

**Remaining smaller work.**

- `rig sync --prune` — remove configuration for columns that no longer exist,
  after showing them.
- The `exec` generator — IR JSON on stdin, `[{path, content_base64, mode}]` on
  stdout, so a third party does not have to compile their own binary.
- `rig schema table --bind` — inject `propertyNames.enum` from the live database
  so an editor autocompletes real column names.
- Shell completion.
- The remaining convention rules from the plan's validation list that are not
  yet implemented.
- A `README.md` worth reading. The current one is a stub.

**Open question for you.** `examples/fantasyfootball` is most of M8 by weight.
Is it worth building in full, or is a smaller second example — enough to
exercise relations, snapshots, and RBAC together, without the TypeScript
consumer — a better use of the time?

---

## M9.0 — the logger (shipped)

The half of M9 that costs nothing, split off and landed first. `log/slog` is the
standard library and `serve` already depended on it, so there is no new module
here, no `rig.yaml` key, and nothing to switch on. M9 below is what is left, and
it is the half that takes an otel dependency.

### The bug this actually fixed

`DefaultErrorMapper` read the code, rewrote an internal message to "something
went wrong", and dropped the wrapped cause on the floor. Nothing anywhere
recorded why a 500 happened. A generated server answered with a request
identifier and then kept no line that identifier could be found in — so the one
piece of machinery rig had for connecting a complaint to a cause connected it to
nothing.

That is not a missing feature. It is a shipped one that did not work, which is
why this went first and why it is an error line rather than something a project
opts into.

### Three lines, and they are three different prices

- **The error line**, at `ERROR`, carrying the whole wrapped error. Written in
  `fail`, which is the single funnel every error response in the generated server
  passes through.
- **The refusal line**, the same call at `DEBUG`. A 404, a 422, a refused
  permission — the server worked. Logging those at error is how a log becomes a
  thing nobody reads.
- **The request line**, at `DEBUG`, deferred from the handler with the status and
  the byte count.

### Before the mapper, not inside it

`fail` consults `Server.OnError` and falls back to `DefaultErrorMapper`. The log
call goes above that branch, so a project that replaced the mapper still gets the
line: setting `OnError` says how a failure is *answered*, and reads nothing like
asking to stop being told what the failure was.

The auth wiring was the other half of the same point, and it was worse. It called
`DefaultErrorMapper` directly, so `/auth/*` — the routes a project cannot wrap,
and the ones where an interesting 500 lives — bypassed the funnel entirely. It
goes through `fail` now, which is why `Hooks` grew a `Logger`: the auth config is
built before the `Server` it ends up attached to, so there is no `Server` in
scope to take one from. Two fields that should hold the same value, and nothing
enforces it. Named in `docs/auth.md` rather than solved.

The inbox routes were the third of the same kind. They went through `fail`
already, but with a blank `RequestContext{}` — harmless while nothing read it,
and once `fail` logs, an inbox 500 whose line names no method, no path and no
request identifier, because `slog` drops an empty group outright. Collecting the
context is a function now, `requestContext(s, r)`, which `resolve` and the inbox
mount both call: a route that does not go through `resolve` still has one way to
get the same fields rather than a literal of its own to leave empty.

### `runtime/reqlog`, and the two interfaces it must not swallow

The request line needs the status and the size, which means wrapping the
`ResponseWriter` — something that existed nowhere in the repository. The wrapper
is four methods and two careful ones:

- It does **not** implement `http.Flusher` or `http.Hijacker`. It implements
  `Unwrap`, and `http.ResponseController` follows that to the writer that can.
  Forwarding them means claiming them, and a wrapper that answers to `Hijacker`
  over a connection that cannot be hijacked is worse than one that does not
  answer at all.
- It **does** implement `ReadFrom`, because `io.Copy` prefers an `io.ReaderFrom`
  over a plain `Write` and the writer net/http hands a handler has one. Without
  it every file download would lose its sendfile path to a userspace buffer.

`runtime/electric/proxy.go` was doing exactly the thing the first point forbids —
`if f, ok := w.(http.Flusher)` — which a wrapper in front of it turns into an ok
that is false and a long poll that sits in a buffer. It uses a
`ResponseController` now. The electric routes do not go through a generated
handler today, so this was a latent break rather than a live one; it is fixed
because the next wrapper will not be so lucky.

### The rule that makes M9's correlation free

Every log call takes the context: `ErrorContext`, `DebugContext`,
`LogAttrs(ctx, …)`, never a bare `Info`.

`slog.Handler.Handle` is *given* the context, which is how a handler attaches the
trace and span a line belongs to — the thing you asked for when you said logging
should get the trace id from the spans. A bare `logger.Info` passes
`context.Background()`, the handler finds no span, and the line is orphaned:
silently, and only where there are spans to be orphaned from, which is
production.

So it is a linter and not a paragraph in this file. `sloglint` with
`context: all` is in `.golangci.yml`; it found ten in `runtime/serve` and they
are fixed. Honest limit: `exclusions.generated: lax` means it does not read the
generated code, and the generators hold their log calls as strings anyway, where
no linter would see them. The goldens pin the emitted text and the behaviour
tests below run it, which is the substitute.

### There is no logger on the context

The first cut put one there, with a `Logger(ctx)` for a service to reach. That
came out again, and the argument against it is `publicapis`, which has been
running this shape in production for a while:

- The logger is **passed explicitly**. `RegisterAPI(..., logger, ...)` hands it
  to closures — `errorHook(logger)`, `preHookLogger(logger)` — and the `Server`
  holds those. Nothing is smuggled through a context.
- **Resource implementations never log at all.** They return errors; the error
  hook logs them. A resource that wanted to would be handed a logger by its
  constructor, which is where rig already was: `examples/todo` calls
  `todo.New(repos.Todos, fileSvc, notifier, inbox, app.Pool, app.Logger)`. The
  context version was a second way to reach a logger that every service already
  had.

So `resolve` puts the `RequestContext` on the context and nothing else, and a
service that wants the request on its own lines does
`api.RequestContextFrom(ctx)` — which existed before this milestone.

### The request is one attribute, and that is `publicapis` too

Every line there is `slog.Any("requestContext", requestContext)` rather than
five loose keys. Same here: `slog.Any("request", rc)`, with everything about the
answer — status, code, error — beside it rather than in it.

`RequestContext` implements `slog.LogValuer` to do it. `publicapis` gets the
same result from JSON struct tags, which rig's cannot use: that type is never
serialized and a `TextHandler` would print Go syntax for it. `LogValue` also
keeps the property the loose version had, which is that an empty field is absent
rather than empty — a project that set no `RequestID` gets no `request_id` key
rather than one with nothing in it.

### Where it is tested

`examples/todo/log_test.go`, and the interesting part is that it needs no
database and no build tag. A generated handler takes its service as an interface,
so a service that only fails drives the whole path a real 500 takes. Eight tests:
that the cause is logged and withheld from the body, that a custom `OnError` does
not lose the line, that a 404 is debug and silent at the default level, that the
`requestId` in the body and the `request_id` in the log are the same string, that
an absent one is absent rather than empty, that the request is one grouped
attribute, and that a service can put it on its own lines with
`api.RequestContextFrom(ctx)`.

`examples/todo/main.go` lost something to this: a `PreHook` that logged the
method and the path. The generated line replaces it and is strictly better —
after the handler rather than before it, so it has the status and the size, and
labelled by the route that matched rather than by a path with an identifier in
it.

### What came out differently

- **`Hooks.Logger` was not planned.** The auth mapper bypassing `fail` was found
  while wiring the rest, and fixing it needed a logger where no `Server` exists.
- **`runtime/electric`'s flusher.** Also not planned, also found by asking what a
  wrapper breaks.
- **The plan said the request line would need no new package.** It needed
  `runtime/reqlog`, because the wrapper has to live somewhere a generator can
  import.
- **The logger went on the context and then came off again.** `publicapis` was
  the argument: an explicit logger and a service that is handed one by its own
  constructor. See above — the reversal deleted `WithLogger`, `Logger(ctx)` and
  the `loggerKey`, and nothing was lost, because `RequestContextFrom` already
  did the half that mattered.
- **The trace correlation stopped being M9's problem.** `RequestID` returning a
  trace id is `publicapis`'s answer and it needs nothing rig does not have.

### Honest gaps

- Two `Logger` fields on a project with authentication, which should hold the
  same value and are not checked. A `rig check` rule could compare them; nothing
  does.
- The wrapper allocates per request even where debug is off. One small struct,
  and the status has to come from somewhere, but it is a cost paid by every
  server for a line most of them never print.
- Nothing logs at the repository layer. "The validator was slow" and "the SQL was
  slow" are M9's spans, not this.

---

## M9 — tracing (shipped)

**Goal.** A request traceable from the handler through the hooks to the SQL,
without every generated application taking a dependency it did not ask for.

**What shipped.** `observe/`, a module of its own with otel v1 in it and nothing
else importing it; a `tracing:` block with one key; spans in `server-go`,
`persist-go` and `rigclient`; a JSONL span file; and
`examples/fantasyfootball` turned on, with a Docker suite asserting the shape
against a real database.

The rule everything follows, and it came from `platform`'s documentation
repository rather than from this plan: **a span is opened at the top of a
function and ended by a defer, and a function opens at most one.** Where a
method has several stages, each stage is a callback handed to a helper —
`r.trace(ctx, name, func(ctx) error {…})` — so the stage *is* a function and its
span is that function's. No call site holds a span it could fail to end, no
early return can skip one, and the generator never had to rewrite an `if err :=
…; err != nil` one-liner into three lines: it wraps the existing call in a
closure and leaves the shape alone.

### What came out differently

- **The request span is rig's own, not `otelhttp`.** Four reasons, in the order
  they mattered: `net/http` sets the matched pattern on the request the mux
  dispatches, so a middleware in front of the mux has one that has matched
  nothing and can only name a span by path — one span name per identifier
  anybody ever fetched, which is the thing that makes a trace useless.
  `runtime/reqlog` already wraps the `ResponseWriter` for the same status and
  byte count, and M9.0 wrote a section on what a second wrapper costs.
  `contrib/…/otelhttp` is v0, and would have been a v0 pin in a module every
  traced application imports. And not importing it costs a user nothing —
  `Register` returns a `*http.ServeMux` and `Config.HTTPClient` takes an
  `*http.Client`, so anybody who wants it still has both seams. What rig does
  take from otel core is `propagation` and `semconv`, so the spans carry
  `http.route` and `http.response.status_code` and read like everybody else's.
- **Nothing configured is not a no-op provider.** The plan said otel supplies
  the no-op itself and that was the default to choose. It is not: a no-op
  invents no identifiers, and the identifiers are the correlation story. So an
  SDK provider is installed with `NeverSample`, which records nothing, exports
  nothing, and still hands out a real trace id — which is what the request id in
  the error body and on every log line becomes.
- **The file exporter is here rather than in M10.** The question M10 asks — ring
  buffer or `rig_trace` table — was answered "neither, a file". So `observe`
  writes one JSON object per finished span, bounded by `FileMaxBytes` with one
  rotation, and M10 is a reader over that rather than a store of its own.
- **The correlation is automatic now, not a paragraph.** M9.0 settled that
  `RequestID` *is* the trace id; with the block on, the generated
  `requestContext` falls back to `observe.TraceID(r)` when a project set no
  `RequestID` of its own. Four lines in a main became none.
- **`store.Config` grew a `Tracer`.** It had been an empty struct taken by value
  precisely so that giving a store something to hold would be a new field rather
  than a signature change. That is what it was for.
- **`Op` grew a `Name`.** The operation id existed only in a doc comment. A
  client span cannot be named from `Op.Path` — by then the identifiers are
  substituted in — so the generated methods now carry it, and the hand-written
  auth calls in `rigclient` name themselves too.
- **The SQL span needed no stage of its own.** A pgx tracer on the connection
  sees every statement, including the ones a hook or a task runs and the ones no
  generator wrote, and it lands under whichever stage's context issued it. Its
  span name is the verb and the table — `INSERT team` — because a generated
  INSERT names every column it writes and a trace listing that as a name is
  unreadable.

### Honest gaps

- **The `auth:` routes and the inbox routes are not traced.** They are mounted
  rather than generated, so no span is opened for them; they still log, and a
  failure there still carries a request id. Fixing it means a wrapper that can
  ask the mux which pattern matched — `mux.Handler(r)` can, since 1.22 — and
  `Register` returns a `*http.ServeMux` by contract, so the wrapper would have
  to go inside. Named, not built.
- **A method's span is not marked failed.** The stage that refused is, and so is
  the statement that failed, but the enclosing `repository.Team.Create` stays
  green with a red child. Recording it would mean a named return and a closure
  in every method, which is the shape this milestone deliberately avoided; most
  trace UIs propagate a child's error upward anyway.
- **`observe.LogHandler` was not built**, and the reasoning below still stands:
  the correlation is `RequestID`, and a handler that also puts the *span* id on
  a line is a smaller, separate thing.
- **That an exporter reaches a collector is untested**, as predicted. The file
  sink is what the Docker suite asserts against instead.

---

### The plan, as it stood

The logging half shipped as M9.0 above, including the constraint this half
depends on: every log call already carries its context, so a `slog.Handler` that
reads the span off one correlates every existing line with no call site
changing. That is the shape the answer to "logging should also get trace id from
the spans" takes — `observe.LogHandler`, below, rather than a field anywhere.

**Shape.**

- The seams exist already, and two of them say so in their own doc comments.
  `serve.Config.Pool` is a `func(*pgxpool.Config) error` whose comment already
  offers "a tracer"; that is where `ConnConfig.Tracer` goes. `Mount` returns an
  `http.Handler` and its comment already shows `otelhttp.NewHandler`.
  `rigclient.Config.HTTPClient` takes an `*http.Client`, so a `RoundTripper` is
  the client seam. `serve.App.CloseWithin` is where a `TracerProvider.Shutdown`
  registers, and a flush that takes too long is then arithmetic the shutdown
  budget already does. What is missing is not the seams. It is anything to put
  in them, and the argument about who pays for it.
- The tracing is optional, and optional means absent. A project that does not
  ask to be traced gets no spans in its generated source, no `observe/` import,
  and no otel in its go.mod — not spans that call a no-op. Wanting otel is a
  decision, and plenty of projects will not want it.
- Which is why otel does not go in `runtime`. That module has two direct
  dependencies and every generated application imports it, so putting otel there
  puts otel in every go.mod, including the applications that trace some other
  way or not at all. It is the goose argument that made `migrate` a module of
  its own, and the same one that makes `server-go` write no `auth.gen.go` for a
  project with no `auth:` block. So: an `observe/` module holding the wiring, a
  block in `rig.yaml` that turns it on, and emitters that write nothing when it
  is absent.
- Enabled is not the same as exporting, and the default is a tracer that goes
  nowhere. That is a second switch, at run time rather than at generation: a
  project that turned tracing on still spends most of its life on a laptop and
  in CI, where the spans are in the code and there is no collector to send them
  to. Configuring no endpoint is the normal case, not the broken one, so
  `observe/` starts as a no-op and a collector is something you add. otel
  supplies the no-op itself — a global provider nobody set already is one — so
  this is a default to choose rather than code to write.
- Which matters more than it sounds. An OTLP exporter pointed at nothing retries,
  and the retry comes due in `App.CloseWithin`, spending the shutdown budget
  flushing to a host that was never there — so the failure mode of getting this
  wrong is slow shutdowns in exactly the environments nobody is watching them.
  `make examples` builds and runs generated servers on every check, and the
  Docker suites run more, so the default has to be the one that costs nothing
  and needs nothing running.
- Three settings at three prices, and M9.0 shipped the first: logging always,
  spans when the project asks, an exporter when the environment has somewhere to
  put them. What is left here is the second and the third.
- **The correlation is already done, and it is `RequestID`.** This was going to be
  an `observe.LogHandler` wrapping a `slog.Handler` to add `trace_id` and
  `span_id` from the span on the context. `publicapis` does something much
  simpler and better: `GetRequestIDFunc: otel.GetTraceID`, so the request
  identifier *is* the trace id. rig's `Server.RequestID` is the same seam and
  already takes a function, so a project that runs otel writes four lines in its
  own main and gets the trace id in the error body, in every log line, and in the
  collector, with no otel anywhere in rig. Documented in
  `docs/observability.md` as of M9.0; nothing here has to build it.
- A `LogHandler` is still the only way to get the *span* id onto a line, and to
  correlate lines written inside a nested span rather than at the request's root.
  That is a real difference and a much smaller one. Build it if the span-level
  question comes up; do not build it as the correlation story, because that is
  settled.
- Spans on the handler, and on each stage of a generated write. The stages are
  already discrete named calls in the emitted code: the validator, `Before`, the
  SQL, `After`, `AfterCommit`, around the `dbx.InTx` and `dbx.InTxIf` blocks
  `persist-go` writes. Every one of them takes `ctx` first, so none of this
  changes a signature, and `AfterCommit` runs inside `InTx` after the commit, so
  it is still a child of the request rather than an orphan. A span per stage is
  the point: "the create was slow" is not worth collecting, "the validator was
  slow" is.
- Names come from the IR, where they are already unique and already low
  cardinality. `Endpoint.Pattern` is proven unique across the whole API at
  freeze — that is what the duplicate-route diagnostic is — and
  `RequestContext.Route` is that same pattern, already on the context `resolve`
  builds. So a span is labelled by endpoint rather than by every distinct
  identifier that ever appeared in a path, which is the reason `Route` exists at
  all.
- The client's span wraps the call, not the attempt. `rigclient`'s `do` can call
  `send` up to three times: the QUERY→POST fallback, and the retry after a 401
  refreshes. One span at `do` and a child per `send`, or a fallback reads as
  three unrelated requests. `Op` carries the method and the path template but
  not the operation id — the generated method knows it and today only says so in
  a doc comment — so that has to go onto `Op`.
- ~~rig gets a logger.~~ Shipped in M9.0: `Server.Logger`, the base logger put on
  the context in `resolve`, and `Logger(ctx)` deriving the request fields when a
  service asks. The reversal of "rig has no logger" stands and is now in the
  code.
- ~~What it says, at `slog.LevelDebug`.~~ Shipped, with `runtime/reqlog` for the
  status and the byte count.
- ~~One line is not debug.~~ Shipped, and it turned out to be the reason to split
  the milestone rather than the strongest argument inside it.
- The probes stay outside all of it. `withProbes` answers liveness and readiness
  before the application's handler sees them, and the reason is already written
  down: a check every second is not a traced request, a line in the log, or a
  bar in a latency histogram.

**Verification.** Goldens for the emitted spans and log calls, and the generator
suites compile what they emit, which is what catches a span opened on a path
that returns early. The end that needs a real database is the lifecycle: a
Docker test asserting one span per stage under the right parent, and none at all
for a write refused before it began. Honest gap: that an exporter reaches a
collector is not something this repository can test without becoming a
deployment.

**The open question, answered. Two blocks.** The question was whether tracing and
M10's page share one `observability:` block. They do not: exporting to a
collector somebody else runs and serving a page this server runs are different
decisions, and most projects that want the first do not want the second. So
`tracing:` here, and M10 names its own when it is planned.

I had leaned one block, on the argument that the page cannot be shown without the
spans it displays and that the dependency was worth encoding. That argument
survives as a validation rule rather than as a schema — the page's block can
refuse to be set without `tracing:` — which gets the same guarantee without
making two decisions look like one setting.

Note what this costs: logging is in neither block. It is always on and has no
configuration at all, which came out of M9.0 and is the right shape, but it does
mean "observability" is spread across two keys and a thing that is not a key.
`docs/observability.md` is the one place that says so.

---

## M10 — the built-in monitoring page (shipped)

**Goal.** See the last requests and their spans on a page the server already
serves, so a deployment too small to be worth a Grafana is not a deployment that
is blind.

**What shipped.** A `monitoring:` block that cannot be set without `tracing:`;
`observe.ReadSpans` / `GroupTraces` / `ReadTraces` over the span file M9 wrote,
exported because reading it from a script is a thing to do; `observe.Page`, an
embedded HTML file and a JSON endpoint beside it, behind a password; a
`monitoring.gen.go` and a `Server.Monitor` field that mount it; and
`examples/fantasyfootball` turned on, which is already the example that traces.

### What came out differently

- **The page is on the API's mux, not next to the probes.** The plan argued it
  had to be outside the application's handler so that looking at the page does
  not appear on the page. That property was already free, and the reason is
  M9's: rig opens its spans and writes its request lines *inside each generated
  handler* — `reqlog.Wrap` and `observe.Server` are emitted per handler, not as
  a middleware around the mux — so anything on the mux that is not a generated
  endpoint is invisible to both. Mounting next to the probes would have meant
  the page's asset living in `runtime/`, which every generated application
  imports, and that is the one thing the block exists to avoid. There is a test
  asserting the page writes no span, because a future change that moved
  span-opening into a wrapper would silently make the page watch itself.
- **The `go:embed` problem dissolved.** The plan expected `gentest` to have to
  learn about a file that is not Go. The HTML is hand-written in `observe/`,
  beside the Go that serves it, and nothing is generated but wiring — so there
  was nothing for `gentest` to learn. (It would have coped: it writes artifacts
  by base name with `os.WriteFile`.)
- **Who may look is a password and, optionally, an address list.** Not localhost
  and not the project's `auth:` block, which were the two the plan named. HTTP
  Basic against `$RIG_MONITOR_PASSWORD`, constant-time, twelve characters
  minimum. Empty is not a misconfiguration but the ordinary case on a laptop and
  in CI, and it mounts *nothing* — not a route that answers 401, which would
  tell anybody scanning that there is a page here. `Page.Unarmed()` returns the
  reason so a main logs one line instead of guessing.
- **`monitoring.allow` narrows it, and cannot replace it.** CIDR ranges or
  single addresses; anything else is answered 404 before the password is
  compared, so a scan learns nothing and a leaked password is not enough on its
  own. It reads `RemoteAddr` and never a forwarded header, which is
  `auth.trusted_proxies`'s argument — and which is exactly why it is not allowed
  to stand alone: behind a load balancer every request arrives from the
  balancer, so the list matches everything or nothing, and that failure is
  silent and total. Refusing the combination is cheaper than documenting it.
  rig.yaml's entries are parsed twice, here and in `observe`, because sharing
  the eight lines would mean the rig binary importing OpenTelemetry.
- **A password in rig.yaml is accepted and warns.** `internal/project/config.go`
  already says the two things that stay out of the file are "a function, and a
  secret", and this is a secret. But a throwaway staging box and a production
  deployment are not the same decision, so RIG3006 says it once and leaves the
  call where it belongs.
- **The block does not spend an API revision.** `Document.Hash` clears
  `API.Monitoring`, on exactly the argument already written there for
  `EmbeddedFoundation`: no client can tell whether the page is mounted, and a
  project that turned it on should not be telling every caller it was built
  against something older than the server.
- **`Provider.Page`, not a free function.** The page reads the span file that
  provider resolved, environment variables included. Two places naming a path is
  one too many, and the failure would have been a page that is permanently empty
  for no visible reason.
- **The reader is the reusable half.** `TraceRecord` and the three functions are
  exported, so a script gets the same grouping the page has rather than
  re-deriving it from `SpanRecord`. `ReadSpans` keeps the last N *raw lines* and
  decodes only those — the whole file is scanned either way, and decoding a
  hundred thousand records to throw all but three hundred away is the one part
  of this that would have cost something.

### Honest gaps

- **No lockout and no rate limit on the password.** One secret, compared in
  constant time. A limiter would mean `observe` depending on
  `runtime/throttle`, which is a dependency this module is not going to take —
  so what stands in for it is the length minimum, `monitoring.allow`, and
  whatever TLS the deployment has.
- ~~**The page is trace-only.**~~ Closed: `observe.OpenLogs` is a `slog.Handler`
  over the same bounded file the spans use, `observe.Tee` puts it beside the
  handler an application already has, and `PageConfig.Logs` is the sink itself
  rather than a path — so the page's two halves cannot disagree about which file
  they mean. The line carries the trace id off the context, which is what makes
  a request and the lines it wrote one view. Two things about it are worth
  knowing: the sink keeps its own level, defaulting to debug, because rig's
  request line *is* a debug line and a page over a server running at info would
  otherwise list nothing; and the span file and the log file are refused when
  they are the same path, since two rotating writers on one file rotate each
  other's data away.
  **Still open:** a page for a project with `tracing:` off. The log half no
  longer needs the spans, but RIG3005 still refuses `monitoring:` without
  `tracing:`, so a logs-only page is a config-chain change nobody has asked for
  yet.
- **One process, one file.** A second replica shows its own spans. That is what
  a file is, and it is the point at which a collector is the answer instead.
- **A rotation mid-trace loses the root**, so `TraceRecord.Root` can be nil. The
  trace is still listed, by id, with the spans it still has.
- **`tracing:` still spends a revision when it is turned on**, and it changes
  nothing a client can see either. Only `monitoring:` was cleared, because
  clearing tracing would move the hash of every project that already traces — a
  one-time spurious bump to fix a spurious bump. Named, not fixed.
- **Nothing renders the page in a browser.** The handler is tested, and the
  render path was run over real data against a stub DOM, but no test opens it.
  A generated page that compiles and answers 200 can still lay out wrong. The
  redesign was driven in a real browser by hand — both tabs, both schemes, both
  breakpoints — which is a check somebody performed and not one the suite
  repeats.

---

### The plan, as it stood

**Shape.**

- Off unless the project asks, and absent rather than disabled. A project that
  does not want it gets no page, no store, no exporter and no dependency for any
  of them — not a page behind a flag that is false. Same rule as the `auth:`
  block: `server-go` writes no `auth.gen.go` at all for a project without one,
  which is what keeps its API package, and its module, free of `rig/auth`. A
  monitoring page carries an HTML asset, a store and an exporter, and a server
  that declined it should not be a byte larger for having been offered.
- So a top-level block in `rig.yaml`, resolved when the config is read, the way
  `auth:` and `files:` are: the struct in `internal/project`, the defaults
  applied there rather than left as zero values meaning something later, an
  `IR()` translation, and the compile stages that rebuild the API carrying it
  through. The generator reads a nil and emits nothing.
- Mounted outside the application's handler, next to the probes, for the reason
  the probes are: looking at the monitoring page should not appear on the
  monitoring page.
- It reads what M9 produces — a `slog.Handler` and a span exporter writing into
  whatever the page reads. So it inherits M9's split: the log half works in any
  project, because the logger is always there, and the trace half is empty in a
  project that did not turn tracing on. A page that shows requests and no spans
  is a reasonable thing for such a project to see, and it has to say why rather
  than look broken.
- **The store question is answered, and the answer is the file M9 shipped.**
  Neither of the two this section weighed: a bounded ring in memory costs
  nothing and loses everything exactly when a restart is the thing you were
  trying to explain, and a `rig_trace` table means rig owns a retention policy,
  an index that is still useful at a billion rows and the vacuum behaviour of an
  append-only table — a tracing backend, which rig does not want to be. What
  `observe` writes instead is one JSON object per finished span, appended to a
  file, bounded by `FileMaxBytes` with one rotation kept beside it. It survives
  a restart, it costs no schema, and `SpanRecord` is exported so reading it back
  is a `json.Unmarshal` rather than a format anybody has to agree on.
- So M10 is a reader and a page, not a store. It opens the current file and the
  rotated one, newest first, groups by `trace_id`, and shows the last few
  hundred requests with their spans underneath. The interesting work left is the
  page itself and the two questions below — the embed, and who is allowed to
  look.
- It would be the first `go:embed` in a module that is not an example.
  Everything generated today is Go source assembled through `internal/gen/gobuf`,
  and the only embedded assets in the repository are an example's templates and
  its migrations. `gentest` copies artifacts by base name into a throwaway module
  to compile them, and would have to learn about a file that is not Go.
- It is not public. The page shows paths, user agents, request ids and the error
  causes M9 starts recording, which together are a list of what every caller
  did. It goes behind the project's own authentication — and a project with no
  `auth:` block has nothing to put it behind, so either the block requires one or
  the page binds to localhost and says so.

**Verification.** The generator suites own most of it: a project without the
block emits nothing, a project with it emits a page that compiles. The page
itself wants an ordinary handler test against a store with a few requests in it.

**The open question, answered. Build it, over a file.** It was worth asking
whether M9 should export OTLP and leave the page to somebody else. The answer
was that a deployment too small for a collector is exactly the one that needs
this — and that the store it needs is a file rather than memory or a table,
which is what M9 built. What is left for M10 is the reader, the page, the embed
`gentest` has to learn about, and the decision about who may look at it.

---

## M11 — the interactive setup

**Goal.** `rig init` asks what the project needs — authentication, files, live
sync, a Go SDK of its own, whether permissions are derived, which port the
throwaway database gets — and leaves behind a project that already says so and
already builds: a `rig.yaml`, a `go.mod`, and a `main.go`, rather than a fixed
template and a documentation page telling you what to change in it. And the
questions keep pace with the configuration: a switch nobody is ever asked about
is a switch nobody finds.

**Shape.**

- Where it stands: `init` takes three flags — `--module`, `--name`,
  `--db-image` (`internal/cli/init.go`) — and everything else is one template
  assembled by string concatenation in `internal/scaffold/scaffold.go`. The
  tutorial's first instruction after running it is to open `rig.yaml` and change
  `database.port`, because init hands out 55432 and two rig projects on one
  machine will fight over it (`docs/tutorial.md`). That is a setup question,
  printed as a documentation step.
- Interactive by default, and not behind `--interactive`. A flag somebody has to
  know about is a flag for the people who least need it: the person the
  questions are for is the one who has just installed rig and does not yet know
  which flags exist. So plain `rig init` on a terminal asks, and the cost is
  admitted rather than argued away — everybody who already types `rig init` gets
  a different command than they had, once.
- Which is bearable only because the non-interactive path stays the whole
  surface. `make examples`, the Docker suites and the tutorial script all run
  init with nobody attached, and a command that blocks on stdin there does not
  prompt, it hangs. `isTerminal` already exists in `internal/cli/cli.go` and
  today decides exactly one thing, which is colour; this is the second. Every
  question also has a flag, and `--no-input` takes the defaults for all of them,
  so any answer a person can give is an answer a script can give.
- The questions come from configuration that already exists rather than from new
  schema, which is what makes this a CLI milestone and not a compiler one. All
  of it is modelled in `internal/project/config.go` and reachable only by hand
  today: `auth:` (enabled, `tenant.from`, whether registration is open, which
  OAuth providers), `files:` (enabled — and `backend: s3` has not shipped, so
  the question offers `memory` and says why), the two registered generators the
  scaffold never writes (`electric`, `go-client`), `api.permissions`
  (`derived|none`), `database.port`, and how strict `validate` starts out.
- Two of the questions cannot be asked yet: tracing is M9 and the monitoring
  page is M10. The wizard ships with the ones it can ask and gains those when
  they land, which is the second half of this milestone rather than an
  afterthought to it.
- The answers land in two places and only one of them is a file rig writes
  today. `rig.yaml` is scaffolded whole, so those answers are just fields on
  `scaffold.ProjectOptions`. But authentication and files also need migrations,
  and that is `rig setup-project`: a second command with its own parts and
  dependency graph (`scaffold.Parts()`, `scaffold.Requires()` in
  `internal/scaffold/foundation.go`) which ends by *printing* `auth:` /
  `enabled: true` and asking you to go and type it yourself
  (`internal/cli/setup.go`). Answering "yes, authentication" should produce a
  project where it is already true. So either init runs the foundation for the
  parts that were asked for, or it ends by printing the single `setup-project`
  invocation matching the answers. I lean the first, with `setup-project`
  staying as the way to add a part to a project that already exists.
- `go.mod` is init's to write, and it should be nearly empty: a module path and
  a `go` directive, which is what `go mod init` would have written. rig should
  not carry a list of what a generated project depends on. That list is a
  function of what the generators emit — `runtime` always, `auth` for a project
  with an `auth:` block, `files` for one with file columns, `rigclient` if
  `go-client` is on, plus `pgx` and `uuid` — and a hand-maintained copy of it is
  a copy that goes stale the first time `runtime` gains or drops a dependency
  and nobody thinks to look here.
- So the requires are `go mod tidy`'s, and it runs after `generate`, not before.
  That order is the whole trick: tidy resolves the imports that are actually in
  the source, and before `generate` there is no source — running it on a fresh
  directory would write a `go.mod` requiring nothing and then have to be run
  again. Afterwards it reads the emitted api package, the store, the service
  stubs and the main, and writes exactly the set that project needs. It also
  means the answers reach `go.mod` without the wizard telling it anything:
  declining authentication is not a rule about `require github.com/simonjanss/rig/auth`,
  it is an api package with no import of it.
- Which needs a Go toolchain, and that is a new assumption worth stating rather
  than discovering. rig emits Go source that somebody has to compile, so `go` is
  already required to use the output — but nothing rig runs today shells out to
  it, and the container-engine question above is the model for this one:
  `exec.LookPath("go")` first, and where there is none, the file stays as init
  wrote it and the command to run is printed.
- What stops all of it today is that none of rig's modules are published — every
  example carries `v0.0.0` and a `replace` at a relative path, and the backlog
  below already notes that `auth`'s replace needs a real version on first
  publish. Tidy is what makes that a loud failure instead of a quiet one, which
  is the argument for it rather than against: a project that cannot resolve
  `github.com/simonjanss/rig/runtime` should say so on the first run, not on the
  first build. So this half of the milestone is gated on tagging the modules,
  not on the setup.
- `main.go` is not init's to write, and that is the reversal M5.6 was asking
  for. It was right that the file cannot be written at init — a main names
  `internal/api`, `internal/store` and one package per service, and none of
  those names exist until the table set does — and wrong that this makes it
  unwritable. It makes it *`generate`'s*, at the moment those names are
  finally known. The mechanism is already there and already carries this exact
  arrangement: `gen.CreateOnce` writes a file once at its final name and never
  touches it again, which is how `service-go` emits the service stub that
  "belongs to the developer" from then on. A main is the same bargain one level
  up — rig writes the first one, you own it after, and a second `generate` does
  not walk over the flags you added to it.
- What goes in it is `examples/todo/main.go` minus the parts that are that
  example's own: `serve.Main` with a `serve.Config`, the two probes, the
  `DATABASE_URL` fallback to the DSN `rig db url` prints for this project, the
  migrations `embed.FS` so the schema travels with the binary, and `api.New`
  with the generated services. Which means the setup's answers reach it —
  authentication changes what it mounts — and it is the one file in the project
  where the questions become something you can read.
- Ending in a project that builds costs a database, and that is the honest
  price. `rig generate` with no `--schema` migrates and introspects a real one
  (`resolveSchema`, `internal/cli/schema_source.go`), so the shortest path from
  an empty directory to a binary is init, `db up`, `generate`, `go mod tidy` —
  Docker, a Postgres image pulled, a module graph fetched, and a first run
  measured in minutes rather than the second `init` takes today. An empty
  project is a legitimate input to all of it: introspection returns cleanly with
  no tables, so what comes out is an api package with no endpoints, which is
  still an api package and still compiles.
- So the setup runs them, and asks first. It is the last question and it is a
  real one rather than a courtesy: answering no leaves a directory of files and
  the commands printed, which is what init does today and a perfectly good
  outcome for somebody who already knows the tool; answering yes is the reason
  to have built any of this. What it must never do is ask a question whose
  answer it cannot honour, so the question is only put when there is something
  to put it about — `dockerdb.FindRuntime` already looks for docker and then
  podman, and `CLIRuntime.Available` already asks the daemon for its version
  rather than trusting `LookPath`, which is the difference between installed and
  running. No runtime, or a project that answered with a `database.url` of its
  own and wants no throwaway container: no question, and the commands are
  printed instead.
- One question, not four. "Set it up now?" covers the database, the generate and
  the tidy, because they are not separate decisions — a person who wants two of
  the three has a shell, and asking three times is how a setup becomes the thing
  people learn to skip.
- And it says what it is about to do before it does it, because the honest
  version of "yes" is a first run that pulls a Postgres image and a module graph
  over somebody's hotel wifi. `rig db up` already exists and already streams what
  it is doing; the setup's job is to name the price in the question rather than
  to discover it afterwards, and to stop at the step that failed and say which
  one it was rather than unwinding. Every file is `CreateOnce` or idempotent and
  the steps are the commands, so the recovery is running the one that failed
  again, and there is nothing to roll back.
- No prompt library. The only dependency the CLI has is cobra, and bubbletea or
  huh in the root module puts a TUI stack in the module every user installs to
  get the compiler. A question is a line, a small set of choices and a default;
  `bufio` over stdin is the whole implementation, and arrow keys are not worth
  the go.mod. It is the same argument that keeps otel out of `runtime` and made
  `migrate` a module of its own.
- **Keeping the questions in step is a test, not a rule.** A line in AGENTS.md
  is a reminder, and a reminder is a thing somebody forgets on the commit where
  it mattered; this repository already prefers the mechanical version, which is
  why `documented` in `internal/compile/validate.go` fails a comment beginning
  `TODO` instead of asking for a better one. So: a test that walks the project
  configuration surface — the same JSON Schema `rig schema project` emits and
  `writeSchemas` writes into `.rig/` — and fails when a key or feature switch is
  neither asked about by the setup nor named in an explicit table of things that
  are deliberately not questions, each with its reason. Adding a configuration
  block then has two legal outcomes and no third: a question, or a written-down
  reason there is no question.
- And a matching rule in AGENTS.md's documentation section, in the shape of the
  same-commit rules already there — written when the setup ships and not before,
  because a rule pointing at a command that does not exist is worse than no rule
  at all.

**Verification.** Goldens over the produced `rig.yaml` and `go.mod` for a few
answer sets: everything declined, everything accepted, authentication without
OAuth. A test driving the questions from a scripted stdin, and — the regression
that would otherwise hang CI rather than fail it — a test that init with no
terminal and no flags never reads stdin and produces exactly what it produces
today. For the main: a generator test that it is emitted `CreateOnce`, that an
edited one survives a second `generate`, and that a project with no tables at
all still compiles. For the last question: that it is not asked at all with no
container runtime, none with a `database.url` already set, and no tidy where
there is no `go` on the path — which is the one way this feature turns into a
wizard nobody can get out of. The strongest check is the one M8's tutorial
script already wants to be — a Docker test that runs the whole sequence into an
empty directory and ends in `go build ./...`, which is the only statement of "it
builds" that cannot rot, and which also proves the tidy ran after the generate
rather than before: a `go.mod` requiring nothing is what the wrong order leaves
behind. The drift test above is its own check and needs no fixture. Honest gap:
whether the questions *read* well, in order, to somebody who has never seen rig,
is not something a test can tell you.

**Open question for you.** Before the modules are tagged, does the setup skip
the tidy and say why, or write replaces into a checkout? Replaces work only for
somebody who has one, which is nobody who has just installed rig, and a
scaffolded replace is a line every new project then carries forever. I lean
skipping it with a sentence naming the reason, so the day the tags exist the
step starts working and nothing has to be unwritten — and, given that this
milestone's whole promise is a project that builds, treating publication as the
thing that unblocks M11 rather than something M11 works around.

---

## M12 — notifications: the engine and the inbox (shipped)

**Goal.** A project declares that one of its tables is worth notifying people about,
answers two questions rig asks it — when, and who — and gets an inbox: a row per
person per thing that happened, live over the sync stream, markable read, deletable,
collapsing ten comments on one post into one line saying ten. The audience is
resolved when the notification is sent rather than when it was written, which is the
decision the rest of this section follows from.

rig has no notifications, and the word appears in the repository only as an argument
for something else. `checkCascades` refuses `ON DELETE CASCADE` because a cascade is
a delete the application never sees: "no hook runs, **nothing is notified**, nothing
is snapshotted, and the rows are gone" (`docs/schema.md`). `examples/todo/notify`
exists to demonstrate the shape of a background dependency — something built while
the routes are wired, fed by a hook after a write commits, drained before the server
stops — and what it does with what it is given is print it. Grep for
`rig_notification` and there is nothing.

So every project rig is for writes this itself, and writes the same five things
wrong. A recipients column that was true the day it was written. A cron job that
sends the same mail twice because two pods ran it. An inbox with no way to empty it.
Ten rows where the person wanted one. Mail at 3am. The tables and the arithmetic are
the same in every application; the sentence and the audience are not. **That split is
what rig can take and where it has to stop.**

### The audience is a question, and it has to be asked late

A blog post scheduled for Friday notifies whoever can read it on Friday. Somebody
added to the group on Thursday evening is one of those people, and a recipient list
computed on Monday does not know that. This is not an edge case somebody contrived —
it is the ordinary behaviour of every scheduled write in a system where membership
changes, and a notification system that captures a list at write time spends the rest
of its life patching around it: a job that reconciles lists, a second table of
pending additions, a support ticket that says "I never got the announcement".

**So a pending notification does not carry recipients at all.** It carries what
happened, what it happened to, and when it is due. The audience is computed at the
moment it is sent, by asking the table it is about.

The cost is honest and worth stating first, because everything below pays it: the
audience is computed in a background job, so it is computed without a request, without
a caller, and after the transaction that caused it has long since committed. Which
means the code that computes it has to be reachable from a job — see the wiring
section, which is the most invasive part of this milestone and the only part that
touches a file rig does not own.

### The subject says what happened and when; the engine asks who

Three statements, and the split between them is the whole design.

**The service says a thing happened.** One line, in a hook it already has:

```go
// services/blog_post/blog_post.go
Create: dbhook.CreateHooks[model.BlogPostCreateInput, model.BlogPost]{
    After: func(ctx context.Context, claims tenancy.Claims, p *model.BlogPost) error {
        return s.notify.Announce(ctx, notify.Announcement{
            Kind:    model.NotificationKindBlogPostPublished,
            Subject: api.NotifyAboutBlogPost(p.ID),
        })
    },
},
```

No recipients, no time. `After` rather than `AfterCommit`, and that is deliberate:
the notification row is part of the change, and a change that committed without it is
a notification nobody will ever send. `dbhook`'s own package doc makes the general
form of the argument — "a hook that fires after a commit that later rolls back has
told the world about something that did not happen" — and this is the same sentence
read backwards.

**Two generated methods answer the rest**, on the rules interface the table already
has:

```go
// NotifyAt says when notifications about this row are due, and whether they are due
// at all. Returning false cancels anything still pending about it.
//
// rig calls it after every create and every update, so a publish_at that moves takes
// its notifications with it and there is no hook to remember.
func (s *rules) NotifyAt(p *model.BlogPost, kind model.NotificationKind) (time.Time, bool) {
	if p.PublishAt == nil {
		return time.Time{}, false
	}
	return *p.PublishAt, true
}

// NotifyWho answers, at the moment of sending, which accounts should hear about this
// row.
//
// It runs in the dispatcher rather than in a request, under System claims for the
// row's own tenant, which is what makes the answer current: an account added to the
// group after the post was written is in this list, because this list is built now.
//
// It must be a pure read, and it may be called more than once for the same
// notification. rig makes a repeat harmless — the recipient index below — but a
// method with side effects would make it visible.
func (s *rules) NotifyWho(ctx context.Context, p *model.BlogPost, kind model.NotificationKind) ([]uuid.UUID, error) {
	return s.groups.MemberAccountIDs(ctx, p.GroupID)
}
```

Both are **required**, because `blog_post_notification` exists. A project that
declares a notifiable table and does not answer these does not compile, which is the
mechanism a declared endpoint already uses: "declare an endpoint in your table
configuration and your service stops compiling until you implement it. Not a 501 at
runtime — a build failure" (`docs/design.md`). They go on `BlogPostRules` beside the
declared endpoints, for the reason that interface gives for being an interface — "the
rules and the endpoints are one value: a resource whose configuration declares an
endpoint cannot be wired up without an implementation of it". The stub rig writes
returns `(time.Time{}, true)` and `nil, nil`, so a freshly generated project compiles
and the developer fills in two bodies.

**The engine does everything else**: holds the announcement until it is due, calls
`NotifyWho`, writes an inbox line per account, and in M13 reads each account's
settings, writes a delivery row per channel and hands them out. None of that is in
anybody's service.

The `kind` argument is a `string`, and the project's own Go enum type when the project
narrows `rig_notification.kind` to a Postgres enum of its own. Nothing new is needed
for that — the enum machinery already produces the type and the labels stay the values
on the wire — and it is worth doing for exactly one reason: a `switch` over kinds
inside `NotifyWho` becomes one the compiler can see. Say so in the documentation
rather than making the enum mandatory, because rig cannot know a project's kinds and a
foundation migration that shipped an empty enum type would be a worse start than a
`text` column somebody narrows.

**The tempting alternative is a key, and it should be rejected before anybody builds
it:**

```yaml
notify:
  - on: create
    kind: BlogPostPublished
    to: group.member_account_ids   # ← this
```

`to:` is a path expression, and a path expression is a vocabulary. It cannot say
"everybody in the group, minus whoever muted the thread, plus the moderators if it
was flagged, and not the author of the change". Every real audience is that sentence
after the first month. This is the same refusal M5.11 already made about
`on_delete: restrict | cascade | set_null | ignore` — those four words are a
vocabulary, and a vocabulary covers only the cases whoever wrote it thought of — and
it lands in the same place. **The declaration is a function.** What rig contributes is
that the function is required, that its subject is typed, and that nothing else about
the mechanism is the application's problem.

### Immediacy is `deliver_at = now()`, and there is only one path

A direct notification is not a special case. `NotifyAt` returns the zero time,
`deliver_at` is `now()`, M13's nudge fires on commit, and `NotifyWho` runs
microseconds later. A scheduled one returns a timestamp and the same code runs when
that timestamp arrives. **The difference between "now" and "Friday" is one column.**

That is what makes late resolution cheap rather than expensive: there is no fast path
to keep in step with a slow one, and the interesting case — a scheduled notification
whose audience changed — is the same code as the boring one.

There is one exception, and naming it is better than pretending there is not.
`Announcement.AccountIDs` skips `NotifyWho` for that announcement. Some audiences
genuinely cannot be re-derived: the five people who were @-mentioned in a body that has
since been edited. Without the exception, `NotifyWho` would have to read a version row
to reconstruct a list somebody already had in their hand. It is documented as the
exception rather than as the parameter, because a list captured at write time is a list
that stops being true, and the whole of this milestone is an argument about that.

### A join table per subject, and rig already knows what one is

`rig_notification` gains no columns for any project. A table declares itself notifiable
by being joined to it:

```sql
-- migrations/00012_blog_post_notification.sql
create table blog_post_notification (
    tenant_id       uuid not null references rig_tenant (id),
    blog_post_id    uuid not null,
    notification_id uuid not null,
    primary key (blog_post_id, notification_id),
    foreign key (tenant_id, blog_post_id)    references blog_post (tenant_id, id),
    foreign key (tenant_id, notification_id) references rig_notification (tenant_id, id)
);
```

**Almost none of this is new code, because `classifyLinkTable` already recognizes the
shape.** It accepts a base table whose primary key is exactly two foreign-key columns
and whose only other columns are rig's own managed ones; `tenant_id` is one of those,
so the table above classifies. What follows costs nothing:

- `projectRelations` derives `ManyToMany` in both directions —
  `BlogPost.Notifications` and `Notification.BlogPosts` — with the filter and the
  `embed` option every relation gets.
- A link table is not projected as a resource at all, so nobody gets a CRUD surface
  over a join row.
- The composite `(tenant_id, blog_post_id)` form is denormalized onto the non-tenant
  column, so **the tenant-safe shape rig recommends is the shape that classifies**.
  That was not true until M5.9 fixed it, and it is why the recommendation can be made
  without an asterisk: pointing at another tenant's row is a constraint violation
  rather than something a hook has to remember. It needs `UNIQUE (tenant_id, id)` on
  the subject table, and on `rig_notification`, which the foundation ships for the
  reason `rig_file_tenant_id_key` exists.

**rig finds notifiable tables by scanning link tables, not by parsing names.** Any
link table one side of which is `rig_notification` makes the other side notifiable. So
`blog_post_notification` is a recommendation in the documentation and nothing depends
on it — the same position the file convention takes, minus the part where the name has
to carry a role, because here there is nothing for a name to say that the foreign key
does not.

The link points at **the notification, not at an inbox line**, and that is what keeps
it small: one row per subject row per notification, not one per recipient. An
announcement to a group of two thousand adds one link row. The other reason it is a
join table rather than a nullable `blog_post_id` on `rig_notification` is worth stating
too: **it keeps `rig_notification` the same table in every project.** The foundation
ships it complete, no project migration ever alters it, and that is what makes it safe
for rig to hand-write its store and its routes rather than generating them — the same
bargain `rig_file` and the auth tables already made.

`notification_id` has to be exempt from the foreign-key naming rule, which wants
`rig_notification_id`. The same exemption `<role>_file_id` needed, for the same reason,
and it goes in the same place.

> **One constraint that will bite, and it is easier to write down than to rediscover.**
> `classifyLinkTable` looks its targets up in a map built *after* the ignored tables
> have been dropped. So `rig_notification` has to stay in the schema even when it has
> no endpoints, or a project with `expose: false` watches its link tables silently stop
> being link tables. `notifications.enabled` keeps it out of `IgnoreTables`;
> `notifications.expose` sets `Resource.Unexposed` instead. That is a departure from how
> `files.expose` works and the two are worth reading side by side before changing
> either.

### Two tables in this milestone, and what each one is for

```sql
-- What happened. One row per subject row per kind. The link tables point here.
create table rig_notification (
    id                      uuid primary key,
    tenant_id               uuid not null references rig_tenant (id),

    created_at              timestamptz not null default now(),
    created_by_account_id   uuid,
    created_by_api_key_id   uuid,
    updated_at              timestamptz,

    kind                    text not null,
    state                   rig_notification_state not null default 'Pending',
    deliver_at              timestamptz not null default now(),
    resolved_at             timestamptz,
    payload                 jsonb not null default '{}'
);

create unique index rig_notification_tenant_id_key on rig_notification (tenant_id, id);
create index rig_notification_due_idx on rig_notification (deliver_at)
    where state = 'Pending';

-- The inbox line. One row per (notification, account), written when the audience is
-- resolved rather than when the notification was written.
create table rig_notification_recipient (
    id                      uuid primary key,
    tenant_id               uuid not null references rig_tenant (id),
    notification_id         uuid not null references rig_notification (id),
    account_id              uuid not null references rig_account (id),

    created_at              timestamptz not null default now(),
    updated_at              timestamptz,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid,

    kind                    text not null,
    group_key               text,
    event_count             integer not null default 1,
    read_at                 timestamptz
);
```

`rig_notification_state` is `Pending | Resolved | Cancelled`. `Resolved` means the
audience was determined and the inbox lines exist. It does not mean anything was sent,
which is M13's table's business and not this one's.

`kind` is copied onto the recipient row rather than read through the join, and that is
not denormalization for speed — it is what lets the collapse index and the live-sync
shape work without the inbox touching `rig_notification` at all. Both of those are
below and both depend on it.

**There is no `title` and no `body` anywhere.** Those are rendering: they are
locale-dependent, they belong to a template, and a copy of them in the row is a copy
that goes stale the day somebody rewords it. `kind` plus `payload` plus the linked
subject is everything a template needs, and `payload` gets a named Go type through the
`go_type` key jsonb columns already have. What rig knows is that something happened and
what it happened to. The sentence is the application's, the same division
`account.Notifier` already draws about mail — "it knows the templates, the sender, the
locale, and whether it uses a queue".

**A notification is addressed to an account, not an identity.** An identity has no
tenant, so an identity-addressed row falls outside every generated query's filter, and
`tenancy.Claims` carries no identity id at all — a handler could not narrow to one
without a join. This is the line `account.Notifier` already draws for itself: a
password reset is about the person, an invitation is about one tenant, and "'you have
been invited' with no answer to 'invited where' is a mail nobody can act on". A product
notification is the invitation case. The consequence is that a service account — `kind
= 'Service'`, `identity_id IS NULL` — has an inbox and no mailbox, which M13 has to say
out loud rather than discover.

### Fan-out is idempotent, because it is going to run twice

`NotifyWho` may be called more than once for the same notification: a dispatcher that
resolved and died before committing, two replicas racing M13's nudge. So the recipient
write is idempotent by construction rather than by care:

```sql
create unique index rig_notification_recipient_key
    on rig_notification_recipient (notification_id, account_id);
```

A repeat fan-out is `on conflict do nothing`. **That index is the whole reason the
method's contract can be "a pure read that may be called again"** rather than "a read
that had better only run once", and it is the difference between a system that recovers
from a crash and one that duplicates somebody's inbox after it.

**Collapsing is a second index, and it is what turns ten comments into one line:**

```sql
create unique index rig_notification_recipient_group_key
    on rig_notification_recipient (account_id, kind, group_key)
    where group_key is not null and read_at is null and deleted_at is null;
```

Ten announcements about one post upsert into one recipient row with `event_count = 10`,
pointing at the most recent of them, and the link table can still name all ten comments
underneath it. Read the row and the next comment starts a fresh one — which is what
anybody would expect, and it falls out of the index predicate rather than being coded,
so there is no rule to get wrong about when a group ends. `notify.GroupBySubject`
derives the key from the subject; `notify.GroupBy("thread:" + id)` sets a coarser one;
leaving it nil opts out and every event is its own line.

What it costs: an upsert per recipient. An announcement to a group of ten thousand is
ten thousand statements in bounded batches, and the bound belongs here for the reason
`sweepBatch` belongs in the sweeper — "so a bucket with a bad week does not become a
single query holding a connection for an hour". A fan-out is not one query and the
section should not imply it is.

### Deleting the blog post takes its notifications with it, and rig writes that code

"Somebody commented on ⟨deleted⟩" is the failure mode of every notification system, and
the link table does not fix it on its own. The link row's foreign key restricts, so a
hard delete of the subject fails on 23503 until something clears it — the problem moved
rather than went away.

**So rig generates the propagation, on both sides of the lifecycle**, into the subject's
writer:

- **Soft-deleted** — cancel the notifications about it that are still `Pending`,
  soft-delete the recipient rows of the ones already `Resolved`, keep the link rows.
- **Restored** — restore those recipient rows, because the link rows are still there to
  say which they were. A notification cancelled while its `deliver_at` went past stays
  cancelled: reviving it would announce something that was gone when it was due.
- **Hard-deleted** — delete the link rows as well, so the delete succeeds.

All of it inside the transaction that deletes the row, so a rollback takes the
propagation with it. **Nothing for the developer to implement and nothing to forget**,
which is the point: this is the one part of a notification system that is pure
bookkeeping, and pure bookkeeping is exactly what a generator should own.

It is also, precisely, M5.11's registry with one child hardcoded. rig knows this child
by name, which is what lets the propagation ship at all — but building it as a special
case and then building the general mechanism beside it would leave two things that do
the same job. **So this comes after M5.11 and registers as its first
`<Parent>Deleting` entry**, and the sibling ordering, the visited set and the depth cap
are that milestone's and not this one's.

`NotifyAt` returning `false` reaches the same machinery from the other direction: a
`publish_at` cleared, a post put back to draft. Cancellation touches `Pending` only.
Mail that is out cannot be recalled, and a state transition that pretended otherwise
would be a lie the schema tells.

### `access: { scope: own }` narrows to the wrong column

An inbox is the canonical owner-scoped resource and the existing key cannot express it.
`applyAccessConfig` filters `created_by_account_id` and says the column is not
configurable, with a good reason: "what a read narrows to and what a write records are
the same fact, and there is no way to point the filter at a column nothing maintains."

A recipient column breaks that premise honestly rather than quietly. Nothing *audits*
`account_id` — it is not who acted — but it is `NOT NULL`, it is written by the engine
and by nothing else, and it is therefore not a column nothing maintains. The premise
was about columns a caller could leave empty, and this is not one.

```yaml
access:
  scope: own
  owner: account_id   # defaults to created_by_account_id
```

One field on `tableconf.Access`, one branch in `applyAccessConfig`, and **every layer
above it works unchanged** — the repository predicate that is the floor, the `?scope=all`
parameter, the `.read.all` permission the catalogue derives from it, `RequireScope`'s
refusal. Refuse a column that is not a `uuid` referencing `rig_account`, and refuse a
nullable one when the key names it: the nullability caveat the existing code documents —
"a row created by a migration or by a service has no account behind it… invisible to a
narrow read, which is the correct answer and a surprising one" — is exactly what a
recipient column must not have, and here it can be checked rather than tolerated.

It lands in this milestone rather than before it, because the inbox is what makes it
testable end to end. It is useful well beyond notifications: an `assignee_account_id` on
a task table has wanted this since owner scoping shipped.

### One live-sync shape, and it is not on `rig_notification`

A recipient row is written in a transaction that commits. Electric notices. **That is
the entire realtime story** — no `LISTEN/NOTIFY`, no socket, no fan-out to connections,
nothing new to run that a project doing live sync is not already running. The inbox is
live because the inbox is a table.

```yaml
# services/rig_notification_recipient/rig_notification_recipient.yaml
electric:
  enabled: true
  auth: tenant
```

**The shape is on `rig_notification_recipient`, and `rig_notification` is not
Electric-exposed at all.** That is a security statement rather than a convenience:
the notification table holds rows that are `Pending` for people who are not recipients
yet and may never be, so a tenant-scoped shape over it would stream Friday's
unpublished post to the whole tenant on Monday. The recipient row carries `kind` and
`group_key` for this reason as much as for the index — a subscriber gets its inbox
without a join to a table it must not read.

Which needs one generator fix, and it is a hole that exists today. The shape builder
emits the tenant, soft-delete and snapshot predicates and **ignores
`ResourceStorage.Owner`**, though the IR has carried it since owner scoping shipped. So
an owner-scoped table with `electric: enabled` streams the whole tenant right now
unless the developer remembers to narrow it in the scope stub — which is the one
narrowing a stub should never have been responsible for, because the repository does not
make anybody remember it:

```go
where.Eq(storage.Owner.Name, claims.AccountID.String())
```

beside the tenant predicate and before the stub runs, so the stub can still only narrow.
It has to refuse a caller whose `AccountID` is `uuid.Nil` rather than emit the predicate
— an API key and a system credential both have one, and `Eq` against nil matches nothing
*silently*, which is the wrong kind of correct: a subscriber that got an empty stream
cannot tell it from having no notifications. `deliver_at` does not appear here at all,
because a recipient row does not exist until its notification was due, which is the
second thing the two-table split buys.

The subject rows are the project's own shapes, which a project doing this already has.
So a client syncs its inbox and its content separately and joins them locally, which is
what a live-sync client does anyway; the inbox route below is what serves the same join
to a client that is not syncing.

### The engine needs the services, and that is the real cost of this milestone

`NotifyWho` is a method on a service. The dispatcher is a background job. **Today those
two cannot see each other**: services are built inside the mount function, and a
`serve.Task` is a subcommand handed a pool and nothing else.

This is the reach problem M5.11 states better than a restatement would — "the parent's
repository does not need a `PlayerRepository`. **It needs the closure, and the closure
already carries the repository it closed over**" — and the answer is the same one. A
compiler-generated, typed registry, one entry per notifiable table, populated where each
service is already wired, which is `Bind`. Adding a link table and forgetting to
register the service does not compile, and the dispatcher walks a typed list rather than
a map assembled in whatever order somebody happened to construct services in.

**The honest cost is a change to `main.go`, which rig does not own.** The task needs the
same object graph the server does, so the file grows a constructor both call instead of
building services inside the mount closure. `examples/auth` already has the shape —
`newAPI(ctx, pool)` extracted so it can be tested — so this is a diff somebody has
already accepted once, but it is a diff, and `rig init`'s template has to grow into it
or every project's first notification is a refactor. That is an argument for landing
this after M11, where the template stops being a fixed string.

The dispatcher runs `NotifyWho` under `tenancy.System(tenantID)`, so the reads inside it
are tenant-scoped without anybody threading a tenant through. **One thing about those
claims is surprising and the generated doc comment has to say it**: `AccountID` is
`uuid.Nil`, so an owner-scoped read inside `NotifyWho` returns nothing until it is given
`readopt.WithoutOwnerScope()`. It is the one trap in writing one of these methods, it
fails as an empty audience rather than as an error, and an empty audience is the hardest
bug in this system to notice.

### The routes are hand-written, because the tables are rig's

`notifyhttp`, mounted by `server-go` beside `authhttp` and `filehttp`, for the reason
those two are hand-written: the tables are fixed in every project, so there is nothing
for a generator to vary.

```
GET    /notifications                 the caller's inbox, paginated, newest first
GET    /notifications/_unread-count   the badge, one number
POST   /notifications/{id}/_read      mark one read
POST   /notifications/_read-all       mark the page's worth read
DELETE /notifications/{id}            remove one from the inbox
```

Every one of them narrows to `account_id = claims.AccountID` and none of them takes a
`scope` parameter: there is no widening for an inbox, because "read everybody's
notifications" is not a thing an application means. The delete is a soft delete against
the recipient row and the notification is untouched — one person clearing their inbox
must not change what anybody else sees, which is the one structural argument for the
recipient row existing separately at all beyond the collapse index.

`_read-all` marks what the caller can currently see rather than everything, and takes
the filter the list took. "Mark all read" on a filtered inbox that silently cleared the
unfiltered one is the interaction people complain about.

Exposing `rig_notification_recipient` as a generated resource stays available and is the
other answer, the way exposing `rig_auth_log` is: a project that wants the filter
grammar, the sort keys and the generated client for its inbox turns
`notifications.expose` on and gets all of it, narrowed by `access.owner` above. **So
both stay, and the difference between them is the point** — the routes are what a
project gets without thinking about it, and the resource is what it reaches for when the
routes are not enough.

### What rig does not do here

Each of these is ruled out by something above rather than by taste:

- **No templates, no rendering, no localisation, no `title` column.** rig stores `kind`,
  `payload` and the link. The sentence is the application's, for the reason
  `account.Notifier` already gives about mail.
- **No path expressions for recipients.** `NotifyWho` is a function, because every
  vocabulary runs out.
- **No polymorphic subject.** A link table or nothing: `(subject_table, subject_id)`
  buys a narrow table and gives up referential integrity, the relation, the filter, the
  embed and every join a client could follow. It is the same instinct M5.9 refused for
  galleries.
- **No CRUD over a link row.** `classifyLinkTable` already refuses it.
- **No Electric shape on `rig_notification`.** Pending announcements are not the
  tenant's business.
- **No cross-tenant inbox.** A person in two tenants has two accounts and therefore two
  inboxes, which is the same answer `rig_account` has been giving since M4.
- **No delivery, no channels, no settings.** That is M13, and this milestone is useful
  without it: an inbox that fills itself, updates itself live and can be emptied is the
  whole of what most applications show in a bell icon.

The tables, the tenancy, the idempotent fan-out and the arithmetic of collapsing are the
parts that are the same in every project and the parts every project gets subtly wrong.
**That is the part rig takes.**

### Verification

Compile goldens first, because the link-table recognition is the load-bearing claim and
it is asserted against existing code: a fixture whose `blog_post_notification` classifies
and yields `ManyToMany` in both directions, and — asserted beside it, since the pair is
the point — one with a data column on the join that *stops* classifying and becomes an
ordinary resource. A golden for `access.owner`, and one for `notifications.expose: false`
proving the link table still classifies, which is the trap in the blockquote above and
the only place it can be caught cheaply.

Generator assertions next, each against the text: `persistgo` emits `account_id = $n`
and not `created_by_account_id` for the inbox; `electricgo` emits the owner predicate
before the stub call and refuses a nil `AccountID`; `servicego` writes both methods into
a notifiable table's rules interface and neither into an ordinary one's. The registry's
check is a compile failure rather than a string, so it belongs in the Docker suite where
something is actually built.

Then `internal/notifytest`, and its first test is the one the milestone exists for: **an
announcement written before an account joined the group, resolved after, reaches that
account.** Asserted beside its inverse — an account that left before the resolve does not
— since the whole claim is that the answer is computed late and both halves have to hold
for that to mean anything. Then: a fan-out run twice inserting one recipient row; ten
announcements collapsing to `event_count = 10` and the eleventh, after a read, starting a
fresh row; `NotifyAt` moving `deliver_at` on an update and cancelling on a cleared
`publish_at`; delete, restore and hard-delete propagating inside one transaction, with a
rollback leaving no trace; and the inbox routes answering only the caller's own rows for
two accounts in one tenant, asserted side by side.

`examples/todo` grows a second table with a group and a `publish_at` and both methods
implemented, since `make examples` is the strongest regression test in the repository —
and it is the only check that the `main.go` rewiring is a diff somebody would accept
rather than one that reads like a framework leaking.

Honest gap: nothing here tests that a project's *first* `NotifyWho` is easy to write, and
the `readopt.WithoutOwnerScope()` trap says it may not be. The empty-audience failure
mode is silent by construction — a notification with no recipients looks exactly like a
notification nobody was owed — and the only thing standing between that and a support
ticket is a doc comment. A `DispatchReport` that logs resolutions with zero recipients,
the way `SweepReport` logs its zeros, is the cheapest thing that would catch it in the
field, and it belongs in M13 with the rest of the reporting.

### What came out differently, and why

Four departures, each forced by something the milestone could not have known
without building it.

**`NotifyAt` is not on the registry, and the dispatcher never asks it.** The
sketch had the engine ask a subject when its notification was due, which would
have meant reading the row back — and a generated `Get` goes to the pool, not to
the transaction on the context, so a hook that announced inside its own write's
transaction would have asked about a row that had not committed. The time comes
from the announcement instead, asked where the row is already in hand. The
registry is down to one question: who. `api.Announce<Res>` asks `NotifyAt` for
you, because that is the line to forget and forgetting it makes every draft go
out the moment somebody saves it.

**The two methods are an interface on the contract, not fields on the hooks
struct.** Both are required, and a field that could be left nil moves the failure
from the constructor to a background job — where it arrives as an audience of
nobody, hours later. `<Res>Notify` is refused at construction the way
`<Res>Endpoints` already is.

**`Deleted` leaves the notification row.** A notification can be about rows in
two tables, and deleting it from inside one table's delete would fail on the
other table's link — aborting a delete that had already succeeded. The link rows
and the inbox lines go; the notification is left for the retention sweep, which
is the second thing that sweep is for.

**`notifications.enabled` does not require `auth.enabled`.** What an inbox needs
is `rig_account`, which is a question about migrations. `examples/todo` has an
inbox and reads its claims from two headers.

Two things this closed on the way through, both holes rather than decisions. The
Electric shape builder ignored `ResourceStorage.Owner`, so any owner-scoped table
with a shape streamed the whole tenant unless the application remembered to
narrow it in the stub. And RIG3250 claimed an unexposed resource's live-sync
endpoint would never be served, which was never true — the electric generator
mounts its own routes and has never read `expose`.

### The questions, answered

**`NotifyWho` gets the whole `notify.Notification`**, payload included, and the
documentation says that depending on the payload is the case `AccountIDs` covers
better — a recipient list smuggled through a jsonb column is the thing late
resolution exists to prevent, and it would arrive without the honesty of a name
that says what it is doing.

**The inbox routes hand back identifiers.** A line carries the notification's id
and its kind, not the subject row. `notifications.expose` is the answer for
anybody who wants otherwise, and it is named as the limitation it is.

**The boilerplate question is still open**, and now has one data point: two real
services, and their `NotifyWho`s are both "everybody in the tenant except whoever
caused the change". A third that looks the same would be the argument for
`notify.Column("assignee_account_id")` — a helper the method can return, which
closes it without touching the contract because it is still a function returning
accounts.

### The original open questions

**Does `NotifyWho` get the notification's `payload` as well as the row?** It gets the row
and the kind as written above. An announcement might reasonably want to carry "and here
is who this reply was aimed at", and then the audience depends on the payload. The
counter is that a payload the audience depends on is a recipient list smuggled through a
jsonb column, which is the thing late resolution exists to prevent, and it would arrive
without the honesty of `AccountIDs` — which says what it is doing in its name. I lean
passing the whole `notify.Notification` so the method can read it, and documenting that
depending on it is the case `AccountIDs` covers better.

**Should the inbox route hand back the subject row, or its identifier?** A live-sync
client has the blog post already and wants the id. A client that is not syncing wants the
row and would otherwise make a request per line. `embed: true` on a `ManyToMany` is a
second query per page, and the subject is one hop further from the recipient row than a
relation normally is — through the notification — so embedding is two joins deep on the
hottest read in the system. I lean identifiers from `notifyhttp`, named as a limitation
with `notifications.expose` as the answer for anybody who wants otherwise, because
guessing wrong here bakes a query shape into a route no configuration can change.

**And how does a table whose audience is one column avoid the boilerplate?** Every
notifiable table implements two methods, and for `assignee_account_id` both are two lines
that will look identical in every project that has one. That is the price of refusing
path expressions and it is probably the right price — but if the first real project has
five near-identical `NotifyWho`s, a `notify.Column("assignee_account_id")` the method can
return closes it without touching the contract, because it is still a function returning
accounts. Worth naming as the follow-on rather than building now, since building it now
means guessing which shape recurs.

---

## M13 — notification delivery (shipped)

### What came out differently

**A `digest` column on the delivery row.** The plan grouped a claimed batch by
account and channel, which folds an Immediate account's three simultaneous copies
into one message as readily as an Hourly account's three — and "tell me as things
happen" and "give me a summary" are different requests. The setting that decided
it is copied onto the row, so a claim knows without a join, and an Immediate row
is sent on its own.

**The propagation orders its deletes.** A delivery points at an inbox line, so a
subject's hard delete removes the copies first; a soft delete marks the pending
ones Skipped rather than deleting them, because what was owed is a fact and
anything already Sent cannot be recalled.

**The retention sweep runs in the dispatch task**, the way the file sweeper's two
rules share one, and it has the two rules the plan named and no third.

**Goal.** M12's inbox reaches somebody who has the application open. This is
everybody else: three channels an application implements, per-account settings with a
delivery window each, one mail instead of nine, and a dispatcher every replica can run
at once without anybody getting the same notification twice.

Nothing here exists, and the absences are worth listing because each one is a decision
this milestone has to make rather than a gap it can lean on. There is no mail
transport — `go.mod` has no mail library and `account.Notifier` is an interface with a
`NoNotifier` that drops every link. There is no device or push concept anywhere: a
grep for `push_token`, `apns`, `fcm` and `webpush` returns nothing, and the nearest
thing in the schema is `rig_account_token.client`, an enum saying `Web | Mobile |
Machine` about a session, which knows a request came from a phone and holds no address
you could reach it at. There is no scheduler: the only `time.Ticker` outside a test is
`examples/todo/notify`. There is no queue, no outbox, and no `SKIP LOCKED` anywhere in
the repository. The one thing named "outbox" is a twenty-item ring buffer in
`examples/auth` whose own doc says it is what a real notifier must never do.

So this milestone writes rig's first piece of concurrent background machinery, and
most of the section is about that rather than about mail.

### Desktop, Mobile, Email — and deliberately no fourth

```sql
create type rig_notification_channel as enum ('Desktop', 'Mobile', 'Email');
```

**The inbox is not a channel.** It is the table M12 ships, it is always on, and a
switch that turned it off would produce a notification nobody can ever find — the
badge would be wrong, the count would be wrong, and the row would sit there unread
forever. Every channel here is a *copy* of an inbox line sent somewhere else, which is
why they can all be refused and it cannot.

Desktop and Mobile are separate channels rather than one push channel with a platform
column on the device, and that is the whole reason to name them this way: they are
separately *answerable*. "Not on my phone during dinner, yes on my laptop while I am
working" is the setting people actually reach for, and a platform on a device row
cannot express it — the platform says where a token points, and the question is what a
person wants. One switch per thing somebody has an opinion about.

```sql
create table rig_notification_device (
    id, tenant_id,
    account_id    uuid not null references rig_account (id),
    channel       rig_notification_channel not null,
    token         text not null,
    label         text,
    created_at    timestamptz not null default now(),
    last_seen_at  timestamptz,
    revoked_at    timestamptz,
    constraint rig_notification_device_channel
        check (channel in ('Desktop', 'Mobile'))
);
```

The `CHECK` refuses `Email`, because there is nothing to register: the address is on
the account and the identity already, and a third copy of it is a third thing that can
disagree. `label` is what a person sees in a list of their devices, and it is the
column that makes revoking one possible for somebody who has four.

Channels themselves are an interface, `NoChannel` is the default, and **rig ships no
transport**. That is the `account.Notifier` bargain repeated without alteration — "what
rig knows is when a link exists and what it says" becomes "what rig knows is who is
owed what, and when" — including the part where the default says so out loud, because
a production deployment whose notifications all silently succeeded into a discard is
worse than one that refused to start.

### A window, stated positively

```sql
create table rig_notification_setting (
    id, tenant_id,
    account_id   uuid not null references rig_account (id),
    kind         text,                              -- null is the default for the channel
    channel      rig_notification_channel not null,
    is_enabled   boolean not null default true,
    digest       rig_notification_digest not null default 'Immediate',
    active_from  time,                              -- null means all day
    active_until time,
    active_days  smallint[] not null default '{}'   -- ISO weekdays; empty means every day
);
```

Resolution is three steps and the section states them once, because a settings system
whose precedence is folklore is one nobody trusts: **the row for this kind and this
channel, else the row for this channel with a null kind, else the default in
`rig.yaml`.** A partial unique index per `(account_id, channel)` where `kind is null`
keeps the middle step single, and a full one on `(account_id, kind, channel)` keeps the
first.

**The window is stated as when to deliver, not when to stay quiet**, and that is not a
naming preference. The setting people describe is "mobile, weekdays, nine to five" —
one row. As quiet hours it is two, because the quiet period wraps a weekend and a
night, and a person who wants to change the end of their working day has to reason
about the complement. Positive costs nothing here and reads as what somebody meant.

`active_days` is an ISO weekday array rather than a bitmask, because it appears in a
`jsonb` settings payload a client renders and `[1,2,3,4,5]` is legible in a way `62` is
not. Empty means every day, which matches every other "unset is not a restriction"
default in the schema.

Times are read in the account's own zone, from `rig_account.time_zone`, which already
exists and already says "IANA name, for example Europe/Stockholm. Null means UTC." So
`09:00` means nine where the person is, which is the only reading of a work-hours
setting that is not a bug. **A window that wraps midnight has to work** — `22:00` to
`06:00` is the ordinary way to say "not overnight" — and it is the arithmetic somebody
will get wrong, so it is named here and tested below.

**Outside its window a delivery is held, not dropped.** `deliver_at` moves to the next
opening. Dropping is less code and the wrong answer for a reason the two tables make
structural: the inbox line exists either way, so a channel that silently discarded its
copy has made the badge and the mailbox disagree, and the person will eventually see
the notification and wonder why they were never told. Late is a delay; dropped is a
lie.

### The row is the truth; the nudge is only latency

Storing is transactional and sending is not. A reply notification must not wait for a
cron tick, and a scheduled one must survive a process that dies. Three pieces, and the
order of the argument is the order of trust:

1. **The rows are written in transactions.** M12's announcement in the one that caused
   it; the recipient rows and one delivery row per channel the settings allow, in the
   one that resolves it. Those rows are the only durable statement that something is
   owed, and everything below is a way of working through them.
2. **`AfterCommit` nudges an in-process dispatcher** for whatever is already due —
   built in the mount function, `app.Drain` to stop it taking more while the server
   still answers, `app.CloseWithin` to let it finish. The shape `examples/todo/notify`
   demonstrates and `App.Drain` documents: "for anything that fetches its own work
   rather than being handed it — a queue consumer, a scheduler, a poller." This is what
   makes a direct notification immediate.
3. **`Config.Tasks["dispatch-notifications"]` is the guarantee.** It takes everything
   the nudge did not: a process that died mid-send, a channel owed a retry, a
   `deliver_at` in the future, a digest whose window closed, a delivery held outside
   somebody's hours.

**The nudge is an optimization and nothing may be built on it.** Say it in those words,
because the shape invites the opposite reading. Nothing is lost when it is skipped: the
row is `Pending`, the task is coming, and the inbox was live the moment the recipient
row committed regardless. What the nudge buys is that the mail arrives in seconds
rather than by the next tick, and that is all it buys.

This is the one place rig runs periodic work in-process, against a position stated in
five other places — `files/sweep.go` is the clearest: "a task rather than a goroutine,
so it is a subcommand in a cron job rather than something racing itself in every
replica." The departure is defensible because the nudge holds no state, so racing
itself costs a wasted claim rather than a wrong answer. **But it does mean there are
now three claimants on the same rows in the ordinary case**, which the sweeper never
had to consider, and that is the next section.

### Scaling out is a lease, not a lock

Every replica runs a dispatcher and the operator's cron runs another. Ten claimants on
one row is normal operation here, not an edge, so the guarantee has to be stated rather
than inferred.

**The obvious answer is wrong.** `select … for update skip locked` inside the
transaction that sends is correct and unusable: a row lock lives as long as its
transaction, so it would be held across a call to SMTP or APNs. One slow provider then
holds a pool connection per message in flight, and a provider that hangs holds them
until the statement timeout — a notification backlog that takes the API down with it.

**So the claim is a lease, and a send is three short transactions.** The first:

```sql
update rig_notification_delivery set
    claimed_at = now(), claimed_by = $1, attempts = attempts + 1
where id in (
    select id from rig_notification_delivery
    where state = 'Pending'
      and deliver_at <= now()
      and (claimed_at is null or claimed_at < now() - $2::interval)   -- $2 = claim_ttl
    order by deliver_at
    limit $3
    for update skip locked
)
returning *;
```

Then the send, with no transaction open at all. Then the third: `state = 'Sent'` and
`sent_at`, or on failure a backoff into `next_attempt_at` and `state = 'Failed'` once
`attempts` passes `max_attempts`. `skip locked` is what makes the claim itself
contention-free — a second claimant walks past the rows the first is taking instead of
queueing behind them, so throughput rises with replicas rather than flattening.

`claimed_by` is a uuid generated once per process, with the hostname beside it in the
log line, so a lease that is stuck can be traced to a pod rather than to a mystery.
`claim_ttl` is what makes a crashed process recoverable at all: the row is still
`Pending` with a stale `claimed_at`, and the next dispatcher past it takes it.

**Which buys at-least-once, and the section has to use those words.** A process that
handed a message to a provider and died before its third transaction will hand it over
again when the lease expires. No arrangement of one database prevents that — the send
and the bookkeeping are two systems and no transaction spans both — so what rig does
instead is make the duplicate survivable, in two halves that are worth separating
because one is much stronger than the other:

- **The inbox cannot duplicate.** M12's unique index on `(notification_id,
  account_id)` makes a repeated fan-out `on conflict do nothing`, so the thing a person
  actually reads is exactly-once by construction. That is the half that matters most
  and it holds unconditionally.
- **A channel gets a stable idempotency key and has to use it.** `Delivery.ID` is a
  uuid that does not change across retries, it is passed to the channel, and the
  interface documentation says to hand it to the provider as its own key —
  `Message-ID`, `apns-id`, whatever the SDK calls it. rig cannot enforce that, and
  saying so is better than implying exactly-once and letting somebody find out.

`rig_notification` carries the same four columns for its resolve step, so two replicas
do not both call `NotifyWho` for one announcement. Two that did would be harmless —
the recipient index absorbs it — but resolving an audience is a read of a membership
table, and paying for it twice for nothing when `Pending → Resolved` under the same
lease costs one clause is not a trade worth making.

**The rejected alternative is a leader**, and it is worth naming because it is simpler
and rig already contains one: goose takes an advisory lock so exactly one process
migrates. Rejected here for two reasons rather than one. A leader is a throughput
ceiling, which is the reason people expect. The reason that actually decides it is that
a leader is a single point of *stall*: wedge it on one slow provider and every channel
stops, including the ones that were fine. The lease is four lines of SQL and it
degrades per-row instead of per-fleet.

`claim_ttl`, `max_attempts` and the backoff base are `rig.yaml` keys, and `claim_ttl`
gets a startup check. Set it shorter than a channel's own timeout and every message is
claimed twice under ordinary load — at-least-once stops being a crash-recovery property
and becomes an everyday one. Refuse it at boot rather than at dispatch time, the way
M5.9's S3 adapter refuses a bucket lifecycle shorter than the restore window and the
way `checkShutdown` refuses a budget that does not fit.

### Stopping is three steps, and the last one gives the work back

The dispatcher holds leases and may be mid-send when the process is asked to stop. One
that simply exited would strand every claimed row for a full `claim_ttl`, which turns
every ordinary rollout into a delivery delay — and a rollout that replaces every pod
turns it into that delay repeatedly.

The lifecycle already covers this, and the wiring is four lines beside the line that
builds it:

```go
engine := notify.NewEngine(app.Pool, notify.EngineConfig{Channels: channels, Registry: reg})
engine.Start()
app.Drain("notifications", engine.StopClaiming)
app.CloseWithin("notifications", 15*time.Second, engine.Close)
```

**`Drain` stops claiming.** It runs after readiness goes false and *before* the server
stops answering, which is the right order and the reason `Drain` exists as a separate
step: the requests still in flight are the last ones whose commits will nudge the
engine, and the engine should spend what is left of the window finishing rather than
picking up work it will not finish.

**`Close` finishes what is already claimed**, and it runs before the pool closes — "so
a final write still has a database to write to", which is not incidental here, because
the third transaction is exactly that final write. A shutdown that closed the pool
first would send every in-flight message and record none of them, which is the
worst-case shape for at-least-once: every one of them sent twice.

**Then whatever is still unfinished is released** — `claimed_at = null`, one statement,
for every lease the engine still holds. Another replica takes it immediately instead of
waiting out the TTL. **A clean shutdown must not cost a lease:** the TTL exists for
crashes, and a process that knows it is going has no excuse for being slow about
saying so.

Two consequences to state rather than discover:

- **The 15 seconds is a number that has to fit.** `checkShutdown` sums every registered
  step and refuses to start when the total exceeds `MaxShutdown`, naming both — so a
  channel with a 30-second timeout under a 20-second `MaxShutdown` is a startup failure
  on the machine that made the change, not a truncated shutdown in production six weeks
  later.
- **A send that outlives the close budget is abandoned, not cancelled.** The provider
  may still deliver it. So the release leaves `attempts` incremented and the retry after
  it is the at-least-once case again — the same case, not a new one, which is why there
  is no second mechanism for it.

The cron-invoked dispatcher stops through the same `Close`. `serve.Once` hands a task an
unbounded context on purpose — "a migration that takes ten minutes is a migration, not
a hang" — so the task has no deadline to stop at and stops on signal instead.

### One mail instead of nine

Two things are called grouping and only one of them is M12's. Collapsing is per
notification and already done: ten comments on one post are one inbox line with
`event_count = 10`. **Digesting is per channel**, and it is this milestone's.

`rig_notification_digest` is `Immediate | Hourly | Daily | Weekly | Off`. Anything but
`Immediate` means a delivery is not sent on its own: it waits for the window to close,
and then every pending delivery for that account on that channel goes out as one
message. The channel is handed the slice and decides what to say with it — "you have 2
unread notifications" and a link to the inbox is the obvious rendering and rig does not
write it, for the reason it writes no other template.

`Off` is not the same as `is_enabled: false` and the difference is worth a sentence,
because somebody will set the wrong one. `Off` means never send on this channel and
still write the inbox line — the person will see it when they look. `is_enabled: false`
means the same thing today and would stop meaning it if a future channel ever needed to
refuse the recipient row too, so both exist and `Off` is the one to reach for.

A digest is claimed as a unit under the same lease so two replicas do not both send
one, which is the reason `deliver_at` on a deferred delivery is set to the window's
close rather than left at the notification's own time: the due-set query does not need a
second concept, and a digest is just a claim whose batch happens to share an account.

### Retention

Nothing in rig prunes anything. `rig_auth_log` grows forever, expired session tokens
are never deleted — expiry is evaluated at read time and the rows accumulate — and
M5.12 names both as known and unfixed. This milestone adds three more tables to a
schema that already does not clean up after itself, and the busiest of them gets a row
per person per channel per event.

So the dispatch task takes a second rule, the way the file sweeper has two: recipient
rows that are read and deleted, past `notifications.retention`, and the resolved
notifications and link rows left with nothing pointing at them. Batched, for the reason
every sweep here is batched. Refuse a retention shorter than the longest digest window
at startup — a weekly digest under a daily retention is a digest assembled from rows
that were deleted before it ran, and it would present as "the weekly mail is
sometimes empty" rather than as a configuration error.

`DispatchReport` follows `SweepReport` exactly, including the part that looks like
noise and is not: **every count, including the zeros**, because "a pass that reaped
nothing is the ordinary case and still worth seeing, because the absence of a line
cannot be told from the job not running." Claimed, sent, failed, held outside a window,
digested, pruned — and resolutions that produced no recipients at all, which is M12's
silent failure mode and the only place it becomes visible.

### What rig does not do here

- **No transport.** No SMTP, no APNs, no FCM, no web-push, and no dependency for any of
  them. Channels are an interface for the reason `account.Notifier` is one.
- **No templates and no localisation**, same as M12.
- **No exactly-once sending.** The inbox is exactly-once by index; a channel is
  at-least-once with a stable key. No database promises more about a network call it
  does not make, and the ones that claim to are describing the key, not the send.
- **No leader election, no broker, no external queue.** A lease in Postgres, because
  the database is already running and a second piece of infrastructure is a second
  thing to deploy, monitor, and be down.
- **No per-notification delivery state on the inbox line.** The delivery table knows.
  A row a client live-syncs should not carry retry counts and SMTP errors, for the
  reason `rig_identity_credential` lives apart from `rig_identity`.
- **No read receipts and no delivery receipts to the sender.** Whether a mail was
  opened is the provider's answer and a different product.
- **No rate limiting per recipient.** `throttle` counts requests against a credential,
  which is not the same question as "this person has had forty notifications this hour",
  and answering the second one properly is a milestone rather than a column.
- **No circuit breaker per channel.** Asked directly, and the answer turned out to be
  that the question named the wrong mechanism. A breaker trips on counted failures, and
  the thing that actually hurt here produces no failure to count: a `Sender` that never
  returns. `send_timeout` is what that needed, and it is below.

  What a breaker would add on top is avoiding N doomed calls to a provider already known
  to be down — real, but second-order next to `max_attempts` and the backoff, and a
  *fleet*-level mechanism in a milestone that rejected a leader specifically to degrade
  per-row. The shape if it is ever wanted: skip a channel for the rest of a pass after
  *k* consecutive failures on it, releasing rather than marking those rows so a dead
  provider does not burn their `attempts`. In-process, no table, no config, a counter
  reset each pass. `DispatchReport` would owe it a `Tripped` count.

### Verification

The concurrency claims come first, because they are the ones a reader will not
otherwise believe and the ones a bug in would be invisible in production until it was
expensive:

- **N dispatchers, one message.** Ten goroutines on one pool claiming from the same due
  set against a counting channel: every delivery sent exactly once, and no claimant
  blocked behind another. This is the `skip locked` assertion, and without it the lease
  is a comment.
- **A crashed claimant is recovered, and not before it should be.** Claim, never mark,
  advance the injected clock past `claim_ttl`, claim again: the row comes back with
  `attempts = 2`. Asserted with its inverse in the same test — before the TTL it does
  not come back — because a lease that expires too eagerly is the failure that sends
  everything twice.
- **A clean shutdown gives the work back.** Claim, `Close`, assert `claimed_at is null`
  on whatever was unfinished, then assert a second dispatcher takes it immediately
  rather than after the TTL. Plus `checkShutdown` as a unit test: a close budget over
  `MaxShutdown` refuses to boot and names both numbers.
- **`max_attempts` terminates.** A channel that always fails reaches `Failed` and stops
  being claimed, rather than consuming a lease forever and a log line every minute.

All of these take the injected clock, the way `files.Config.now` does. A lease test that
sleeps for real is slow, flaky, and deleted within a year.

Then the settings arithmetic, which is where the ordinary bugs are: the three-step
resolution asserted at each step and at the fallthrough; `is_enabled: false` writing no
delivery row at all rather than a `Skipped` one; a window that wraps midnight in a
non-UTC account zone, asserted at four times — inside, outside, and both sides of the
wrap; `active_days` excluding a weekend and the held delivery landing on Monday morning
rather than Saturday; and a delivery held and then released arriving once, not twice.

Digests: an `Hourly` account with nine pending deliveries receives one message
containing nine, and an `Immediate` account beside it in the same pass receives nine.
Asserted together, because the interesting failure is one setting leaking into the
other's batch.

The example carries the end-to-end proof. `examples/todo` gains a recording channel —
the `examples/auth` outbox shape, whose own doc explains why putting a live credential
on a screen is acceptable only in a demo — and `make examples` asserts that a create
produces a notification, a mail, and one of each.

Honest gaps, two, and neither is closable by a test. Whether a digest *reads* well is
not something a test can tell you. And at-least-once is only tolerable if a real
channel actually passes `Delivery.ID` to its provider — that is the application's code,
outside anything rig runs, and a doc comment on the interface is the entire enforcement
mechanism. The section should say that rather than let the word "idempotency" imply
otherwise.

### Open questions for you

**Is the in-process nudge worth it, now that the lease is the thing that makes it
safe?** It is the reason a direct notification is immediate, and it is also the reason
there are three claimants instead of one — the `Drain`/`Close` handshake, the release
statement, and half the tests above exist for it. Cutting it makes this milestone a
`serve.Task` and nothing else, at the cost of up to a tick before a reply notification
reaches a mailbox. Note what it does *not* cost: the inbox is live either way, because
the recipient row is committed and Electric carries it, so this is about email and push
latency and nothing else. I lean keeping it, because "they got the mail four seconds
later" is the difference somebody notices — but if M13 wants to be half its size this is
the thing to cut, and because the lease is in the schema either way, cutting it now and
adding it later changes no table.

**What is `claim_ttl`'s default?** The number that matters is the slowest channel's own
timeout, which rig cannot know. Too short and every slow provider's messages are claimed
twice under normal load; too long and a crashed pod's queue sits idle for that long
while nine healthy replicas walk past it. I lean 5m, refusing anything under 1m at
boot, and documenting the relationship to the channel timeout as the one
misconfiguration worth understanding before deploying this — but a default that assumes
mail may be wrong for a project whose only channel is a websocket push that either
works in 200ms or does not.

---

## M13.1 — the send timeout (shipped)

**Where it came from.** The question was "do we need a circuit breaker in rig
somewhere". Reading the delivery path to answer it turned up something worse than
the thing a breaker would have addressed, and the answer is that the question
named the wrong mechanism — a breaker trips on counted failures, and a `Sender`
that never returns produces no failure to count.

### What was wrong

`Sender.Send` was called with `context.Background()`. Every other outbound call in
rig bounds itself — three seconds for the breach check, ten for a token exchange,
thirty in `rigclient`, four budgets in `serve` — and this was the only one that did
not, because it is the only one rig does not make. The far side is application
code calling SMTP or APNs, and Go's default `http.Client` has no timeout.

A pass is one goroutine, and `run` resolves before it dispatches. So one hung
sender:

1. **stopped every channel on that replica**, not just the wedged one — the exact
   single-point-of-stall the lease was chosen over a leader to avoid, reintroduced
   at the replica level by a serial loop;
2. **stopped the inbox with it.** `Resolve` writes the recipient rows and never ran
   again. The M13 note above says "the inbox is live either way … this is about
   email and push latency and nothing else" — true of a *skipped* nudge, not of a
   wedged one. The cron dispatcher is what kept it from being data loss, which is
   exactly the guarantee it was introduced as;
3. **left the leases stranded.** `Close` waits on `e.done`, which never closed, so
   it returned `ctx.Err()` at its budget and never reached `ReleaseClaims` — the
   one outcome that section says must not happen.

And it needed no hang at all. A hundred rows against a thirty-second provider,
serially, is fifty minutes against a five-minute lease. `MinClaimTTL` guards that
ratio for *one* send and cannot see the batch multiplier.

### Three things, and the third was a crash

**`notifications.send_timeout`**, thirty seconds, resolved where the other three
delivery numbers are and carried through `ir.Notifications` into the generated
wiring. It makes `claim_ttl`'s own advice checkable rather than advisory:
`claim_ttl`'s comment said "the number that matters is the slowest channel's own
timeout, which rig cannot know", and now rig knows it, so `checkNotifications`
refuses the pair instead of describing it. `NewEngine` panics on the same
relationship for a caller that builds an `EngineConfig` by hand.

**A pass budget, not a smaller batch.** Bounding each send is not enough — a
hundred timeouts still outlive the lease. The pass gets a context worth
`claim_ttl`, started *before* the claim because that is when the lease starts, and
stops while a whole `send_timeout` still fits inside what is left — the question
being whether the next send fits, not whether the budget has already run out,
because a send begun with a millisecond left is a call in flight as the lease
expires. This is why `claimBatch` is still a hundred: shrinking it to
`claim_ttl / send_timeout` would have been the same arithmetic paid by every
healthy channel, and a provider answering in a hundred milliseconds should still
get all hundred in one pass. `DispatchReport.Abandoned` is what says the batch
stopped fitting.

The rows it did not reach are handed back by `abandon` rather than by the
deferred release, for one reason: `claim` charges every row in the batch an
attempt before anything is sent, so a row released without a send would have paid
for one it never got, and `max_attempts` of those would Fail a delivery no channel
had ever been asked about. `abandon` gives the attempt back and `ReleaseClaims`
deliberately does not, because the send *it* gives up on was made. `claimed_by` is
in its WHERE clause: past a lease that expired anyway the row may be somebody
else's, and the attempt to give back would be theirs.

**The missing-sender guard.** `e.senders[m.Channel].Send(...)` indexed a map with
no `ok`, and `claim` does not filter by channel. A deploy that dropped a channel
from the map while `Pending` rows existed for it produced a nil `Sender`, a method
call on it, and a panic in a goroutine with no `recover` above it — the process,
not the delivery. `ErrNoSender` had been declared for this and never used, which
is the tell. Those rows now fail like any other undeliverable copy.

### Verification

Three tests in `examples/auth/dispatch_docker_test.go`, beside the concurrency
claims they belong with. A sender that waits on its context returns, the pass
returns, and the rows come back unclaimed — bounded from the outside too, because
a test that just called `Dispatch` would hang the suite rather than fail it, and
asserting the sender's deadline actually fired is what makes it a test of the
deadline rather than of the sender. Email rows against an engine that can only
send Mobile: not an empty map, which `Dispatch` already short-circuits, but a map
with the wrong channel in it. That one completing at all is the assertion. And the
budget, in real seconds because a lease under a minute is refused: three Immediate
rows, a four-second send and a fifty-six-second timeout inside a one-minute lease,
so exactly one fits — one sent, two abandoned, nothing still claimed, and
`attempts` back at zero on the two nothing was tried on.

Plus the unit half: the boot check three ways — longer, equal, and an ordinary
pair — the defaults building an engine, because a default pair this package
refuses would be a panic on a configuration nobody wrote, and the report line
naming every count. On the configuration side the same relationship, plus the
floor a seconds-resolution document needs: `500ms` resolves to zero, zero reads as
unset, and unset is thirty seconds, so it is refused rather than silently
multiplied by sixty.

### What this did not do

No breaker, and M13's non-goal list now says why rather than leaving it unasked.
Two other unbounded calls turned up while reading and are **not** fixed here,
because they are different modules and one of them is a different argument:

- **`auth/oauth/flow.go`'s token exchange has no timeout at all.** `cfg.Exchange`
  is handed the request context and no `oauth2.HTTPClient`, so `x/oauth2` falls
  back to `http.DefaultClient`. The only place the repo sets that key is a test.
  `providers.go` sets ten seconds on the very next call, which is what makes this
  look like an oversight rather than a decision — a hung IdP parks a goroutine per
  callback, and `WriteTimeout` does not cancel a handler.
- **`auth/account`'s `Notifier` is called inline in the request path**
  (`service.go`, password reset and verification resend) with no timeout, no
  retry and no outbox, and the rate-limit budget is spent *before* the send — so a
  provider outage burns somebody's five-an-hour on mail that never left. The
  engine three directories over solves this exact problem properly; the asymmetry
  is worth a milestone rather than a patch.

Two smaller ones, same class: the delivery backoff has **no jitter**, so a hundred
failures in one pass retry in lockstep for all five attempts; and `files/sweep.go`
returns on its first error, so one un-deletable object stalls every pass behind it
— where `notify`'s `address()` deliberately swallows to avoid precisely that.

---

## M14 — the foundation gets a version (shipped)

**The bug it fixed, which was not the one asked for.** The request was to stop
vendoring rig's migrations into the project's repository. Looking at why that was
awkward turned up something worse: rig could not ship a schema change to its own
tables at all. `setup-project` decided what to write by matching a filename —

```go
func alreadyApplied(existing []string, part string) bool {
	suffix := "_rig_" + part + ".sql"
```

— so editing `tenancySQL` reached exactly nobody who had already run it. The
foundation had no version. M5.12 had already added columns to `rig_auth_log` and
there was no way to get them to a project set up the month before. Harmless while
nothing is published; the first support issue after tagging.

Both answers to that need the same missing piece, which is why this milestone is
one thing and not two: an append-only, versioned set. Vendoring then means copying
whatever is not already there, and embedding means not copying at all.

### Where the DDL lives now

Six Go consts in `internal/scaffold/foundation_sql.go` became `.sql` files behind
a `go:embed`, in a leaf package per owning module: `auth/foundation` (tenancy,
apikeys, sessions, oauth), `files/foundation`, `notify/foundation`. The extraction
was byte-exact, and a test holds it that way — see Verification.

`runtime/dbschema` describes a set: the filesystem, the order, the tables each
migration creates, and the table it records itself in. It is in `runtime` because
`runtime` is the one module every generated application already imports, so a set
declared in those terms costs nobody a dependency. It holds no migration engine:
applying is `rig/migrate`'s, and a module carrying schema should not thereby carry
goose.

**The cross-module edge, which is real.** `rig_notification_recipient.account_id`
references `rig_account`, and `rig_account` is auth's *tenancy* migration. So
`examples/todo` — notifications, no `auth:` block — needs `auth/foundation`. It
pays one go.mod line and ~30KB of SQL text, not `rig/auth`'s code, because
`auth/foundation` imports nothing but `embed` and `dbschema`. That constraint is
why the packages are leaves, and it is load-bearing rather than tidiness.
`files/foundation` has no such edge: `rig_file.tenant_id` carries no reference to
`rig_tenant`, deliberately, so uploads work with no authentication at all.

The part boundaries did not move. They describe schema already applied to real
databases; resplitting `tenancy` because `rig_account` is shared would be a
migration, not a refactor.

### A table per set, and it is not tidiness

| set | records itself in |
|---|---|
| the project's own | `rig_migrations` |
| `auth/foundation` | `rig_auth_migrations` |
| `files/foundation` | `rig_files_migrations` |
| `notify/foundation` | `rig_notify_migrations` |

The three modules are tagged separately. One shared table would mean one numbering
sequence across three release cadences, and two modules adding a migration in the
same release would collide on a version — which goose refuses outright rather than
resolves (`provider_collect.go:71,155`). So each numbers from one in its own
namespace. `migrate.Source`, `UpAll`, `PendingAll`, `ApplyAll` and `RequireAll`
take an ordered list, and `checkSources` refuses two sets that would record
themselves as one, because that failure is silent: one set's version 2 marks the
other's as applied and the migration that never ran never runs.

Named for the module rather than the tables (`rig_notify_migrations`, not
`rig_notification_migrations`), and with rig's own word rather than goose's —
`DefaultTable` is already `rig_migrations` and not `goose_db_version`, so the
engine's name does not leak into rig's schema and should not start.

Sequential rather than concurrent is not a weakness: goose's session lock is a
single fixed identifier, so replicas starting together still serialise across all
of it. The per-set tables are bookkeeping, never a concurrency boundary.

### Order is one rule, and it holds

rig's DDL never references a project's table; a project's routinely references
rig's — `examples/todo/migrations/00008_notify_about_todos.sql` points at both
`rig_notification` and `rig_tenant`. So: all of rig's, then the project's. That
holds under upgrades too, and within rig's own the existing `Requires()` graph
already states it.

### The evidence moved, and that is what actually broke

Everything downstream hangs off `scaffold.Managed()`, which read the migrations
directory. Under `embedded` it returns nothing, and then `checkReservedTable`
fires **RIG2005 on all fifteen rig tables**, the ignore list empties, and three
"foundation present" checks invert and tell a working project to run
`setup-project`. So `scaffold.Wanted{...}.Parts()` is the second reading — the
parts a configuration brings, expanded through `Requires()` — and
`foundationParts` in `internal/cli/load.go` picks by mode.

**Brings, not asks for, which the first draft got wrong.** A set is applied whole
because goose reads a directory, so `Wanted.Parts()` answering with the narrow ask
while `SetsFor()` applied whole sets meant `rig db up` created tables the very next
`rig validate` did not recognise: **RIG2005 on `rig_identity_oauth`** for any
`auth:` block with no provider configured, and on all five of auth's session and
key tables for an inbox in a project with no `auth:` block. With advice nothing in
that mode could follow — rename the migration that creates it — and, had the code
not been refused, the table missing from the ignore list too, so a resource
projected over it. `examples/auth_oauth` happens to configure providers, which is
why `make examples` was green. The widening now lives inside `Wanted.Parts()`
rather than in each caller, so there is one answer to "which of rig's tables does
this project have" and no narrower one to reach for by mistake;
`TestEveryTableASetCreatesIsAccountedFor` holds the two ends together.

It is weaker than the vendored reading in exactly one way worth writing down: with
no migration in the project to point at, it cannot tell rig's `rig_account` from a
hand-written one. What stands in for the missing evidence is that the mode is
declared, and `auth.own` is still how to say the opposite — loudly enough that the
two together are refused rather than reconciled.

**And the four bookkeeping tables are `rig_`-prefixed tables no migration
creates.** Introspection returns everything in the schema, so without care they
draw RIG2005 and then get projected — a generated PATCH on `is_applied`. The
exemption already existed for one name and became `compile.Bookkeeping`, read by
both `Compile` and `rig sync`.

### The two silent failures the CLI had to keep away from

`rig generate` introspects a live database, so under `embedded` the CLI must apply
the module sets too. Not doing so does not fail — it succeeds and generates the
wrong code: `e.doc.Table("rig_api_key") == nil` drops the "cannot be changed with
an API key" guard from *every* user repository
(`internal/gen/persistgo/repository.go:156`), and `rig_notification` leaving the
schema un-notifiables every table (`internal/compile/project.go:189`). One place
changed — `(*env).migrate` — and both are asserted.

### The mode is a one-way door

The two modes record their history in different tables, so flipping a live project
finds the new mode's bookkeeping empty, re-applies a schema that is there, and
dies partway through `rig db up`. **RIG3004** refuses `embedded` with rig's
migrations still on disk. The reverse direction is deliberately *not* a second
diagnostic: `checkFoundationPresent`, `checkFilesFoundation` and
`checkNotificationsFoundation` already report it, naming the block that wants the
part, which is the better message. Two diagnostics for one mistake is noise.

RIG3004 also refuses `embedded` with **`auth.own`**, which is the same
contradiction stated the other way: `own` says the project forked rig's migrations
and maintains those tables, `embedded` says the modules do. Silencing the mode
check for `auth.own` — the first draft — left `foundationSources` applying the
modules' sets over the forked ones, so `rig db up` stopped on `relation
"rig_tenant" already exists`. Refused rather than resolved in favour of one of
them, because whichever won would be silent about the other; and `foundationSources`
returns nothing under `auth.own` regardless, since `rig db up` runs no diagnostics
and the rule has to hold where the schema is applied as well as where it is
checked.

Adopting an existing schema into a fresh bookkeeping table is a real feature and
is not here. goose has no baseline command; what there is, is a refusal naming the
one supported move.

### `server-go` writes the wiring, and only when there is any

`api.MigrationSources(project migrate.Source) []migrate.Source` — the module sets
in order, with the project's own last. Emitted only under `embedded`, which is the
rule `auth.gen.go` already follows and what keeps goose out of a vendored
project's API package. The project's own set is an argument rather than generated
because it is the project's to describe: the filesystem is its embed directive,
the directory and table are its rig.yaml. Which the doc comment on the generated
function has to say out loud, because `UpAll` reads `Dir` and `Table` off each
`Source` and ignores the ones on `Options` — so a project that changed
`migrations.table` and set it in `Options`, which is what `docs/rig-yaml.md` tells
a vendored project to do, would record its own set in `rig_migrations` while `rig db
up` used the configured name. `RequireAll` then refuses to start, saying the
database is behind. Loud, but for the wrong reason, and one line of example prevents
it.

That needed one IR field, `API.EmbeddedFoundation`, and **it is cleared before
hashing**. The revision is what a client reads to decide whether it is talking to
an API it was built against, and moving DDL from a directory into a module changes
nothing observable over HTTP. Caught by flipping `examples/auth_oauth` and
watching its revision move; worth remembering that any field added to `ir.API` is
in the hash by default.

### Verification

- **The two modes cannot drift.** `TestVendoredMigrationsStillMatchTheirSets`
  holds every `_rig_*.sql` committed in an example byte-identical to the set's
  copy. `make examples` would not catch this — a migration is copied once and then
  owned, never regenerated — and it is also the append-only rule from the other
  end: editing a shipped migration fails here, which is correct, because those
  files have been applied.
- **Append-only, mechanically.** `dbschema.Set.Validate` refuses a gap, a repeat, a
  manifest entry with no file, and — the dangerous direction — a `.sql` file the
  manifest does not name, which goose would apply and every other reader would
  miss. Each owning module calls it. A golden per module holds the shipped list.
- **Ordering, against a real database.** `TestUpAllAppliesSetsInOrder`: two sets
  both numbered from one, the second referencing a table the first creates.
- **What is applied and what is recognised are one list.**
  `TestEveryTableASetCreatesIsAccountedFor` walks every configuration shape and
  asserts `TablesFor(Wanted.Parts())` is exactly the tables `SetsFor(Wanted.Parts())`
  creates — both directions, because a gap either way is a table rig refuses or a
  table rig claims and nothing made. `TestEmbeddedAcceptsEveryTableItsSetsCreate`
  is the same property from the command line, on the two shapes that actually broke.
- **`examples/auth_oauth` is `embedded`**, `examples/auth` stays vendored, so both
  readings are generated, built and run on every `make examples`. Neither would have
  caught the widening bug on its own: `auth_oauth` configures providers, so its
  narrow list and its wide one happen to agree.

### What came out differently

- The module bookkeeping constants live in each `foundation` package rather than
  beside `migrate.DefaultTable`. The set's owner is the obvious home and a second
  spelling would be a second thing to keep in step.
- `runtime/dbschema` was not in the plan. Without a shared type there is no
  `[]Set` to iterate, and `internal/scaffold`, the CLI and the generator would
  each need three code paths instead of one.
- A migration may create no tables. An upgrade that only alters what an earlier
  one created is the ordinary shape, and the first draft of `Validate` refused it.

### Honest gaps

- Nothing proves an upgrade against a database set up by an *older* rig binary,
  because there is no older binary to run. The append-only test is the substitute
  and it is weaker.
- Version skew is sharper than before: a project pinning `rig/notify v1.2` with a
  `rig` binary shipping v1.3's set has the CLI migrate a schema the application
  cannot reproduce. `RequireAll` refuses to serve, which turns it into a startup
  failure rather than a silent one, but `rig validate` comparing each set's applied
  version against the binary's is the check that would catch it earlier. Cheap now
  that the manifests exist.
- The examples share Docker container names and ports across Conductor
  workspaces, and a sibling still on `vendored` populates `rig-oauth-db` with
  rig's migrations under `rig_migrations`. The embedded example then fails on
  `relation "rig_tenant" already exists` — which is the mode-drift failure working
  exactly as designed, on a database two workspaces disagree about. `rig db reset`
  is the fix; the collision is the pre-existing hazard, now with a louder symptom.

---

## Done: retries in the SDK, and the idempotency that lets writes have them

`docs/clients.md` said the `rigclient` module carried "the transport,
credentials, retries, pagination, error decoding". It carried everything on that
list but retries. The first real consumer had noticed —
`examples/sdk/import_demo.go` hand-rolled `worthRetrying` and `backoff`, with
linear waits and no jitter — and the reason nobody had moved it into the library
was that half the calls worth retrying are writes, and rig had no way to tell one
write sent twice from two writes.

So both halves. `rigclient` retries 429, 500, 502, 503, 504 and a request that
never got an answer, over four attempts: immediate, then a second, then two, each
wait half fixed and half random. The whole thing happens inside the timeout the
call already had, so turning it on cannot make an existing call slower — a call
that spends its budget on the first attempt simply gets no second one.
`Retry-After` wins, in both the seconds form rig's own server sends and the date
form something in front of it might; an interval longer than the call has left,
or longer than thirty seconds, is handed back on the error rather than slept
through, because whether a program has a minute is a question about the program.

Writes are retried because they go out named. Every write the SDK might repeat
carries an `Idempotency-Key`, generated per call and identical across attempts,
and `rig_idempotency` — a new set under `runtime/foundation`, the first thing
runtime has ever carried SQL for — records what the write answered **in the write's
own transaction**. That single property is what the rest follows from: a claim
stored apart from the effect it claims can outlive a rolled-back write, and then
a key that wrote nothing replays a success forever. A second request holding the
same key blocks on the unique index and then either replays what committed or,
if it rolled back, does the work itself. No lease, no in-flight status, no TTL:
Postgres was already keeping that bookkeeping.

Making that true cost one line three directories away. A generated repository
read its connection through `Store.conn()`, which resolved
`context.Background()` — so it could only ever answer with the pool, and a
create with no hooks committed its row outside whatever transaction was around
it. The record and the effect were two facts, in exactly the case that matters
most. `connFor(ctx)` everywhere, and the argless version is gone rather than
left as a trap. It also means `Store.InTx` finally does what its own comment
has always said it does.

Two things the tests found rather than the design. The response column was
`jsonb` first, and jsonb normalises — the replayed body came back with its keys
reordered and its whitespace redone, so "replayed verbatim" was false and a
client that signed what it received would have seen two answers to one request;
it is `text` now. And `MaxRetryAfter` clamped an hour to thirty seconds instead
of declining it, which meant going back before the server said to and sleeping
ninety seconds to be refused three times.

The generated server has no middleware and no `ResponseWriter` wrapper, and
wanted neither: the handler already holds the typed response value with its
status as a compile-time constant, so the emit captures the value rather than
buffering bytes — which also keeps it away from `filehttp.Serve`, which streams
and can answer 206. `Server.DB` is new and `Register` panics without it; a nil
pool would make every key a header nobody read, and a client retrying a create in
the belief that it is safe would write a row every time.

**The upload route is the one write with no record.** Streaming turned out to cut
both ways: a download is not the only thing on the file path that is still moving
bytes when the handler is deep inside it. `OnePart` hands the service the form
part itself, so guarding an upload would hold a pooled connection open for the
length of a transfer — thirty minutes, at `filehttp.DefaultDeadline` — and a few
slow clients would be the whole pool. `files.Service` had already drawn this line
for itself and said so out loud: "a create is one transaction and an upload cannot
be in it." The multipart create takes the same order, storing its parts before the
guarded write begins, and is recorded. So the SDK does not name or repeat a form
body at all — it cannot tell the two routes apart, and guessing wrong in the other
direction stores the file twice.

### What rig does not do here

- **No configuration.** The lock timeout is five seconds and the retention is a
  day, both constants. Neither is a number a project has an opinion about until
  it has an incident, and a block with two knobs nobody turns is scope for its
  own sake. If one of them ever needs to move, it moves for a reason somebody
  can write down. The lock timeout is also `SET LOCAL` for the claim and then
  put back: leaving it would have quietly put five seconds on every lock the
  write went on to take, which is a 500 where there used to be a slow 201.
- **Nothing schedules the retention.** `api.IdempotencyPruner` is generated for
  every project and, like `FileSweeper`, it is a `serve.Task` waiting for a cron
  entry. A goroutine per replica racing to delete the same rows is not the shape
  this repository uses, and picking a schedule on a deployment's behalf is not
  something a generator knows how to do.
- **No retry budget across calls.** A hundred goroutines each retrying three
  times is three hundred extra requests at a server that just said it was
  overloaded. A shared token bucket — retries may be at most a tenth of traffic —
  is the known answer; it needs state on the `Runtime` and a fraction nobody can
  pick on a caller's behalf.
- **No circuit breaker**, for the reason M13's list already gives.
- **Deletes are not keyed.** A delete is idempotent in what it leaves behind, so
  the only thing a record would buy is a 204 where there is now a truthful 404,
  and it would cost a transaction on every delete to buy it.
- **The multipart fingerprint does not cover the bytes.** For the create that
  carries a file — the one multipart write that is recorded — two runs under one
  key are the same write as far as the server can tell. Hashing the upload means
  buffering a file that may be larger than memory, which is the one thing the
  whole file path exists to avoid.
- **A create carrying a file loses its retry.** The server would record it; the
  SDK will not send it, because from `Op` a form body on a POST is a form body
  on a POST. An `Op` field the client generator fills in would fix it, and it is
  a field to add when somebody has an upload worth retrying rather than in
  advance of one.
- **rig's own drain 503 still carries no `Retry-After`.** `runtime/serve` sends
  none, so a client mid-rollout guesses. One line, and it belongs in `serve`.
- **`notify`'s delivery backoff still has no jitter.** Now that the client has
  the rule written down, the engine three directories over is the one that still
  retries a hundred rows in lockstep. Same fix, different module; it is on the
  list below.

## Things I would fix if nobody asked for anything else

- `internal/gen/servicego/servicego.go` still has an `elemType` helper that is
  now used in exactly one place; it could move.
- The `auth` module's `go.mod` carries a `replace` to `../runtime`. Harmless —
  consumers ignore a dependency's replaces — but it will need a real version on
  first publish.
- ~~`examples/todo` pins the database to port 55440 and `internal/*test`
  packages each pin their own. There is no registry of which port belongs to
  which suite, and the next one added will collide with something.~~ Done in
  M5.9: `internal/dockerdb/ports.go` names all thirteen and a test refuses a
  collision. The examples are listed but not read from there — their ports live
  in their own `rig.yaml`, in modules that cannot import it.
- `throttle.Postgres.qualify` prefixes known column names by string replacement.
  It is fed from a closed map rig owns, and it is still the least pleasant code
  in the runtime.
- **Impersonation may not belong in rig.** `POST /auth/impersonate` and its
  `DELETE` are mounted unconditionally by `authhttp.Handler.Mount`. The handler
  is careful — it requires `account.impersonate` and refuses to nest, so an
  impersonating session cannot impersonate again — but no configuration key
  gates the routes the way `allow_registration` and `allow_tenant_creation` gate
  theirs, which makes this the one authentication feature a project gets whether
  or not it asked.

  And the key is in `authhttp.Permissions()`, so any role model that grants an
  owner everything grants this too. `examples/linearlite/services/authz` is
  exactly that model, which means the seeded Owner can already act as anybody in
  the tenant and nobody decided that — the same shape as the
  `rig_notification_device.read.all` widening that file now subtracts on
  purpose.

  Nothing in any example calls it, and no front end has a control for it.
  `account.Service.Impersonate` does write `ImpersonationStarted` and
  `ImpersonationEnded`, so the trail is honest and `GET /auth/audit` shows them.

  What to settle: whether rig should have an opinion here at all. Who may act as
  whom, what the application looks like while it is happening, and how somebody
  gets back out are product decisions, and the ones a product gets wrong here
  are expensive. Three answers, in the order I would consider them: **remove it**
  and let a project that needs it mint a session itself; **gate it** on an
  `auth.allow_impersonation` that defaults to off; or **keep it** and have
  `Levels()` in the examples refuse the key out loud, so the shipped role models
  stop granting it by accident. Leaving it mounted, permitted by default, and
  demonstrated nowhere is the one answer that is hard to defend.

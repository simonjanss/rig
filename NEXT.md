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

Next:

- **M5.7** — the API revision: clients say what they were built against, so the
  logs can say how old the oldest one still calling is. (Was M5.6; the number
  moved when the model layer's second round took it.)
- **M5.9** — files: a `rig_file` table, a storage adapter, and an upload and a
  download endpoint per file column, nested under the row that owns them. Before
  M6 because it adds two error codes, and the code set is baked into an OpenAPI
  document and a TypeScript union the moment those ship.
- **M5.11** — the auth log and who is signed in: a read endpoint over
  `rig_auth_log`, and `GET /auth/sessions` widened past the caller's own. The
  events have been recorded since M4 and nothing has ever served them. (M5.10 is
  `go-client`, on a branch; if that lands second this number moves, the way files
  moved from M5.8.)

### Modules

```
.               github.com/simonjanss/rig          CLI, compiler, generators
runtime/        github.com/simonjanss/rig/runtime  imported by generated code
auth/           github.com/simonjanss/rig/auth     sessions, oauth, api keys
migrate/        github.com/simonjanss/rig/migrate  a binary migrates itself
examples/todo/  a real project, built in CI
```

The root module now requires `auth` and `runtime` with local replaces, so the
Docker suites in `internal/authtest` and `internal/electrictest` can drive them.

### Generators registered

`model-go`, `persist-go`, `service-go`, `server-go`, `electric`. All but
`electric` are scaffolded by `rig init`, and `electric` emits nothing until a
table opts in. `model-go` is listed first because the layers above import what it
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

## M5.7 — the API revision

**Goal.** Every generated client says what it was built against, the server puts
that on the request context, and a log line can answer "how old are the clients
still calling this?" — which is the question you have to answer before you can
remove anything.

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
    // Refuse anything built before this. Empty, the default, refuses nothing.
    MinRevision: "2026-01-01",
}
```

That is the endgame — you removed a field, you waited, the logs said nobody old
was left, and now you close the door. Off until somebody decides to.

### Open question

Should a client that sends no revision at all be distinguishable in the logs
from one that sends an unparseable one? They are different failures — an old
SDK that predates the header versus something sending nonsense — and telling
them apart costs one more field on `RequestContext`. I lean yes, one field,
`ClientRevisionInvalid bool`.

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

## M5.9 — files

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

## M5.11 — the auth log and who is signed in

**Goal.** A tenant can read its own authentication trail and see every session
open inside it, and a person can read their own, through endpoints rig serves
rather than a query the application writes.

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

Every `/auth/*` list today returns a bare `{"data": […]}` — no limit, no offset,
no total. `listAPIKeys` loads the tenant's keys and filters them in Go, and the
comment justifying that says "a tenant's keys are a handful of rows," which is
true of keys, invitations and tenants and is the reason the shape has survived.

An auth log is millions of rows and cannot borrow the argument. So `/auth/audit`
answers `{data, pagination}` with the numbers the generated endpoints already
use — `DefaultLimit` 50 and `MaxLimit` 500 from `internal/compile/builtin.go`,
clamped by `query.Page.Clamp`, whose own comment is the reason ("an unbounded
list is a production incident waiting"). The other four stay exactly as they are.
Two shapes in one package costs a sentence in the docs; paginating four endpoints
that will never need it costs four handlers and four sets of tests.

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

### Open question for you

**Should the auth log reach the IR, or is `authhttp` its home for good?**

Every route `auth` mounts is invisible to `openapi`, to `ts-client` and to
`go-client`, and this one is the first where that is a real loss rather than a
theoretical one: an audit screen is exactly the kind of thing somebody builds in
TypeScript against a generated client. The fix is not exposing the table — that
is the other answer, above, and it has the hole. It is teaching the IR about
routes rig serves from a hand-written module, which would eventually pull all
thirty `/auth/*` endpoints in with it.

That is a milestone of its own and a large one. The question is only whether
M5.11 should be built knowing it is coming — the response shape declared as an IR
object even though nothing reads it yet, the way `EndpointRequest.ContentType`
sat unread from M0 until files needed it — or built as an ordinary `authhttp`
handler and converted later with everything else.

I lean ordinary. A shape declared for a consumer that does not exist is a shape
nobody can check, and the conversion is the same size either way. Say which you
would rather have.

---

## M6 — `openapi`

**Goal.** An OpenAPI document from the same IR, so it cannot describe an API
that does not exist.

**Shape.**

- `internal/gen/openapigen`, generator name `openapi`.
- `pb33f/libopenapi` to build and render. Not text templates: a document
  assembled from strings is a document that eventually stops parsing.
- Options: `formats: [json, yaml]`, `version: "3.1" | "3.2"`, `out_dir: docs`.
- One `components/schemas` entry per `ir.Object` and per `ir.Enum`. The filter
  shapes are objects too, so `Search` documents itself.
- `ir.API.Auth` is where the security scheme comes from: which endpoints exist
  under the auth base path, and the lifetimes and rate limits to document. It is
  resolved rather than optional, so the numbers in the specification are the ones
  the server enforces — that is the reason the configuration lives in rig.yaml.
- Every endpoint's `Errors []int` becomes a response referencing the shared
  `Error` schema. That is why the IR stores bare codes.
- `Endpoint.Pattern` is the path, unchanged. The router, this document, and the
  TypeScript client read the same field.

**The QUERY problem.** OpenAPI 3.1 has no `query` field on a path item.

- Under `"3.1"`: document the `POST /_search` alias, and note in the operation's
  description that `QUERY` on the collection path is the primary form.
- Under `"3.2"`: emit the `query` operation directly.
- Default `"3.1"` until 3.2 tooling is common. `openapi.version` in `rig.yaml`.

**Open question for you.** Is the 3.1 fallback worth it, or should rig emit 3.2
and let people who need 3.1 downgrade with a tool? The fallback is roughly
forty lines and a permanent branch in the generator.

**Verification.** Validate the emitted document in-process with libopenapi, and
in CI with an external linter (`vacuum` or `spectral`). Golden files for the
rendered JSON and YAML.

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

**`examples/fantasyfootball`.** The full demo: many tables, relations, enums,
soft delete, snapshots, custom endpoints, RBAC, OAuth sign-in, API keys, live
sync, and a small TypeScript consumer. Built end to end in CI the way
`examples/todo` is. This is the strongest regression test in the repository and
also the biggest single piece of work left.

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

## Things I would fix if nobody asked for anything else

- `internal/gen/servicego/servicego.go` still has an `elemType` helper that is
  now used in exactly one place; it could move.
- The `auth` module's `go.mod` carries a `replace` to `../runtime`. Harmless —
  consumers ignore a dependency's replaces — but it will need a real version on
  first publish.
- `examples/todo` pins the database to port 55440 and `internal/*test` packages
  each pin their own. There is no registry of which port belongs to which
  suite, and the next one added will collide with something.
- `throttle.Postgres.qualify` prefixes known column names by string replacement.
  It is fed from a closed map rig owns, and it is still the least pleasant code
  in the runtime.

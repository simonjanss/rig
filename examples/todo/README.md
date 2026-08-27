# todo

One table, no authentication, a complete API. This is the tutorial's subject and
the smallest thing rig can build.

Everything here except three files was generated from
[`migrations/`](migrations/):

| File | Who wrote it |
|---|---|
| `migrations/*.sql` | you |
| `services/todo/todo.yaml` | you, starting from `rig sync` |
| `services/todo/todo.go` | you, starting from `rig generate` |
| `main.go` | you |
| `notify/notify.go` | you — a dependency that is not the database |
| `internal/model/*.gen.go` | rig — the entity, its enums, its filter, its inputs and their validation |
| `internal/store/*.gen.go` | rig — typed queries and the pgx repository |
| `internal/api/*.gen.go` | rig — the service interface, routing, handlers |

## Run it

```bash
rig db up && go run .
```

`rig db up` starts a throwaway Postgres and applies the migrations, which is
the development shortcut. Forget it and the server says so within a second,
rather than spending its connect timeout and then printing a driver error:

```
WARN cannot reach the database yet addr=localhost:55440 waiting=10s
     hint="run `rig db up` to start a local Postgres for this project, …"
```

That line comes from `Hint` in the `serve.Config` below. Starting the database
is deliberately not this binary's job — a server that boots its own Postgres is
the wrong thing to copy into a real project, and rig keeps container handling in
the CLI where it cannot end up in your deployment.
 The binary can also do it itself:

```bash
go run . migrate
```

The migrations are embedded, so the schema a build expects ships with that
build. It is a separate command rather than something the server does on the
way up: run it as its own step before a rollout, so one process migrates and
the replicas only serve.

The server does check. `Migrate: migrate.Require(...)` refuses to start when
the database is behind, so deploying before the migration ran is a process that
stops with a message rather than one that fails a query at a time. Swapping
`Require` for `Apply` migrates on the way up instead — one line, and fine for a
single instance.

Then, with a tenant — every generated query is scoped by one, so a request
without it is unauthenticated:

```bash
export T=11111111-1111-1111-1111-111111111111

curl -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"title":"Write the tutorial","priority":"high"}' \
  http://127.0.0.1:8080/api/v1/todos

curl -H "X-Tenant-Id: $T" http://127.0.0.1:8080/api/v1/todos
```

The pool, the HTTP server and the shutdown are `serve.Main`, so `main.go` is
the wiring and nothing else — all of it in that one call: what the server is
configured with, then what it is made of. The parts are not independent, so the
order is the code: the notifier before the service that reports to it, the
service before the handler that routes to it. It also answers two probes:

```bash
curl -i http://127.0.0.1:8080/livez    # is the process running
curl -i http://127.0.0.1:8080/readyz   # should it be sent traffic
```

Anything with a shutdown of its own — a queue consumer, an exporter, a client
with its own pool — is registered where it is built. `notify` is this example's
one of those: it collects a line per created todo and writes them in batches.

```go
notifier := notify.New(os.Stdout, 30*time.Second)
notifier.Start()
app.Drain("notifier", notifier.StopRecording)               // before the server stops
app.CloseWithin("notifier", 5*time.Second, notifier.Close)  // after it has stopped
```

It is fed by the create hook that runs *after* the transaction commits. That
set is everything the service says about a create, in the order it runs:

```go
Create: dbhook.CreateHooks[model.TodoCreateInput, model.Todo]{
    Validator:   model.TodoCreateValidator{Title: s.validateTitle, …},
    Before:      nil,
    After:       nil,
    AfterCommit: s.announceCreated,
},
```

That is the only place a write may be announced from. From `Before` or `After`
the row can still disappear under a rollback, and the message would be about
something that did not happen.

Reads have a set of their own, and it is the one that does not run in the
repository:

```go
Read: dbhook.ReadHooks[model.TodoFilter, model.Todo]{
    Narrow: nil,   // conditions every filtered read is limited to
    Rows:   nil,   // what a read is about to answer with, once per read
},
```

`Narrow` **returns** a filter rather than editing the caller's, and that is the
whole design. The two are combined with AND, so a search whose own filter is an
OR cannot widen its way past it — appending a condition to somebody else's OR
would turn a restriction into another way to match. It lands in the WHERE
clause, so the total in the pagination block is the narrowed count rather than
the whole table with rows dropped afterwards.

`Get` is not narrowed: it fetches by primary key, and there is no filter to add
a condition to. `Rows` sees what it found, which is where a rule about whether
this caller may have this row goes.

The tenant needs none of this — it is already forced, by the same mechanism.
Every read the repository runs is

```sql
WHERE tenant_id = <claims> AND ( …whatever the client sent… )
```

so a filter cannot reach past it with an OR or by nesting one. The tenant is not
in the filter shape at all, because a condition on it could only ever be a
no-op or a contradiction; naming it is a 400 for an unknown field rather than a
control that quietly does nothing. Reading across tenants needs
`readopt.WithoutTenantScope()`, which is a Go argument no handler is in a
position to pass.

Neither runs on the read a write makes to find its row, and that is deliberate:
an update that judged a narrowed or redacted row would validate something that
is not what is stored, and snapshot it. These shape what an answer looks like on
the way out. Deciding which rows exist for a caller at all is what the tenant
column is for.

Drain runs while the server is still finishing what it has, so a consumer stops
pulling work nobody will complete. Close runs in reverse order after the last
request, before the pool closes.

`MaxStartup` bounds the other end — opening the pool, the migration check and
the mount function — so a boot that hangs on something slow fails with the
phase named instead of sitting there neither serving nor failing.

`MaxShutdown` is the longest the whole stop sequence may take — the number to
copy into `terminationGracePeriodSeconds`. Each step is bounded by what is left
of it, and `…Within` gives one its own smaller limit so it cannot spend the
budget the steps after it need. rig adds the declared limits up and refuses to
start when they do not fit, so the stated maximum and the parts cannot drift
apart. A step that ignores its context is abandoned rather than waited for: the
process exits on time either way.

This example states `api.ShutdownBudget() + 10*time.Second`, and the two halves
are the point. `ShutdownBudget()` is generated: it adds up the closers rig itself
registers for the blocks in `rig.yaml` — here just the notification engine's
fifteen seconds, plus ten seconds of headroom for the requests in flight. The ten
added to it is this example's own two closers, the recorder and the store's cache
subscription, which no generator can know about. Adding rather than restating is
what keeps a new block from silently eating the headroom.

`api.Register` makes the mux and hands it back, so adding a table is one field
in `api.Handlers` and anything else this server answers is a `Handle` call on
the same mux.

Something that only needs the request — the log line this example writes — is a
`PreHooks` entry on `api.Server`. Something that has to see the response is a
wrapper around the handler instead:

```go
return otelhttp.NewHandler(mux, "todo"), nil
```

That is where tracing goes: a hook running before the handler has no status, no
duration and no panic to report. rig answers the probes outside whatever is
returned, so a check every second is not a traced request.

The probes are separate on purpose. Liveness never touches the database — a liveness
probe that fails when a dependency does turns one slow database into every
replica being restarted at once. Readiness pings it, and turns false as soon as
a shutdown starts, so a load balancer stops sending work before the server
stops accepting it.

## The page

`go run .` serves a small UI at [localhost:8080](http://localhost:8080) beside
the JSON API.

It is there because the lifecycle features are the hard part to appreciate from
a curl transcript. Soft delete, a version history, restore and revert are about
what happened to a row over time, and time is easier to see in a list you can
click:

- **Live** and **Trash** are the same table. Deleting moves a row from one to
  the other and nothing is removed — the trash can say *when* each row was
  deleted because the row is still there with a stamp on it. The trash is folded
  away with a count beside it, and a delete opens it, so a soft delete looks like
  a move rather than a disappearance.
- **History** is a timeline: newest first, the current state at the top, and
  beside each entry the change that reverting to it would undo — `title`,
  `Buy milk → Buy oat milk`. The oldest entry is the row as it was created.
- **Editing a title saves on change**, and each save leaves the previous version
  behind.
- **Revert to this** puts an old version back through the ordinary update path,
  so the state it replaced is itself snapshotted on the way past. A revert can
  be reverted.
- **Done** calls the custom `_complete` endpoint rather than an update, so
  clicking it twice shows the service's own rule: finishing a finished task is a
  conflict, and the page shows the message the service wrote.

`web/` is hand-written and about 300 lines, most of it HTML. Two things about it
are worth more than the page itself:

**It calls the service, not the database.** `web.New(svc, claims)` takes the
same `api.TodoService` the JSON handlers take. The rules, the hooks and the
tenant scoping belong to the service layer, so a second transport needs no
second copy of them — which is the claim the three-layer split makes, checked by
something other than the layer that makes it.

**It has no build step.** HTMX over server-rendered fragments: every action
posts a form and swaps in the HTML that comes back. There is no client state to
get out of step with the server, which is also why every action returns the
whole board — deleting a row has to empty it from one list and add it to
another, and that way it is one response rather than two that could disagree.

`web/web_docker_test.go` walks the whole thing: add, rename, complete twice,
delete, restore, revert. It drives the handlers with `httptest` and asserts on
the rendered HTML, including that a second tenant's page does not show the
first's rows.

## Things worth trying

**A patch distinguishes three states.** Leaving a field out and setting it to
null are different requests, and the difference survives to the UPDATE:

```bash
# notes is left exactly as it was
curl -X PATCH -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"title":"Write it today"}' http://127.0.0.1:8080/api/v1/todos/$ID

# notes is cleared
curl -X PATCH -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"notes":null}' http://127.0.0.1:8080/api/v1/todos/$ID
```

**Search is a read that carries a body**, so it uses QUERY rather than
misrepresenting itself as a POST. The POST alias exists for intermediaries that
reject unfamiliar methods, and reaches the same handler:

```bash
curl -X QUERY -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"filter":{"like":{"title":"%tutorial%"}}}' \
  http://127.0.0.1:8080/api/v1/todos

curl -X POST -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"filter":{"equals":{"priority":"high"}}}' \
  http://127.0.0.1:8080/api/v1/todos/_search
```

The filter in that body is `model.TodoFilter` — the same type the repository
takes, not a wire copy of it with a conversion in between. A rule you write
builds one the same way a client sends one:

```go
filter := model.NewTodoFilter()
filter.Equals = model.NewTodoFilterEquals()
filter.Equals.Title = &title

rows, total, err := s.repo.List(ctx, filter, model.TodoPage{Limit: 2})
```

How many rows and in what order is `TodoPage`, separate from the filter because
it arrives separately: the filter is the body, `?limit=` and `?offset=` are
query parameters. Keeping them in one struct meant a client could ask for a page
inside a filter, and an ordering it was never offered.

Each operator is its own struct, which is what keeps the whole thing typed:
`FilterRange` only carries columns that can be ordered and `FilterLike` only
text, so `{"like":{"createdAt":"..."}}` is not expressible rather than merely
rejected.

**Delete retires rather than removes.** The table has a `deleted_at` column, so
rig made it soft-deletable: DELETE stamps the row, reads stop returning it, and
it stays restorable for the 30 days `todo.yaml` asks for. Which means there is
somewhere it went, and a generated route to read it:

```bash
curl -H "X-Tenant-Id: $T" http://127.0.0.1:8080/api/v1/todos/_deleted
```

The two listings are mirror images: what is in one is not in the other. Only
rows inside the restore window appear — past it a row is gone as far as anyone
is concerned, so it is not in the trash either.

The way back is generated too, and it runs the restore hooks, so a rule about
whether a row may come back has somewhere to live:

```bash
curl -X POST -H "X-Tenant-Id: $T" \
  http://127.0.0.1:8080/api/v1/todos/$ID/_restore
```

Restoring a task that was never deleted answers 200 rather than an error: it is
already in the state the caller asked for, and a retry of a request whose
response went missing should not look like a failure. Past the window it is a
409.

**A restore is where soft delete gets sharp.** Deleting a task frees its title —
the duplicate check does not look in the trash, deliberately, because refusing
to reuse the name of something you deleted for thirty days would be a strange
thing to explain. So this sequence is three requests none of which is wrong on
its own:

```bash
# 1. delete a task called "Same title"
# 2. create a new task called "Same title"   <- fine, the title was going spare
# 3. restore the first one                   <- two live tasks under one name
```

Nothing about the restore is wrong; what is wrong is the state of the world it
returns to. And a restore carries no fields, so there is nothing for a rule to
judge and nothing for a caller to fix — which is why the decision belongs to a
hook. `Before` is handed the row as it was retired and an empty input, and
setting a field on that input writes it as the row comes back:

```go
func (s *service) beforeRestore(ctx context.Context, in *model.TodoUpdateInput, prev *model.Todo) error {
    taken, err := s.titleHeldBy(ctx, prev.Title, prev.ID)
    if err != nil || !taken {
        return err
    }
    in.Title = patch.NewOptional(restoredTitle(prev.Title, time.Now()))
    return nil
}
```

So the task comes back as `Same title (restored @ 2026-08-02 19:36)`. Returning
an error instead would refuse the restore, and which of the two to do is the
application's call: a todo is somebody's note to themselves, and getting it back
under a slightly longer name beats being told to go and rename something else
first. A resource whose name means something to another system would want the
error.

The hook runs **before** the validator, which is what makes this work — the
rules then judge what the hook settled on, not what the row was retired with.
That is the order for updates too, and the reason is the same: a hook that ran
afterwards could write a value nothing had checked. (A create is the exception,
and says why in its own comment: its rules run before any transaction is open,
so that one calling out to another service is not holding a row lock.)

A restore also runs *every* rule rather than only the ones for fields the hook
touched. The row was not live, so nothing about it has been checked against the
world it is returning to — that is the single line of difference between the
generated `RunUpdate` and `RunRestore`.

What actually prevents the duplicate, in the race no check-then-write can close,
is a partial unique index in
[`00003_one_live_title.sql`](migrations/00003_one_live_title.sql):

```sql
CREATE UNIQUE INDEX todo_live_title_key ON todo (tenant_id, title)
    WHERE deleted_at IS NULL AND version_type = 'Original';
```

The predicate has to exclude both — the trash, so a freed title stays free, and
the snapshots, or every update would collide with the copy it had just taken.

**Every update keeps the version it replaced.** The table also has the snapshot
triple — `version_type`, `snapshot_from_todo_id`, `snapshot_from_todo_at` — so
rig made it versioned. An update copies the row as it was before writing the
change, in the same transaction. There is no trigger, no history table, and
nothing in the service layer maintaining it:

```bash
curl -X PATCH -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"title":"Write it today"}' http://127.0.0.1:8080/api/v1/todos/$ID

curl -H "X-Tenant-Id: $T" http://127.0.0.1:8080/api/v1/todos/$ID/_versions
```

The versions stay out of `GET /todos` and out of search — a history that turned
up in the task list would double its length after a day's editing.

Both endpoints are generated. Nothing in `todo.yaml` asks for them: the snapshot
columns are in the table, so the resource has a history, and a resource with a
history has a route to read it. `_versions` follows from `Get` and `_revert`
from `Update`, so dropping either operation drops the endpoint that depends on
it.

Putting a version back is `_revert`, and it goes through the ordinary update
rather than writing over the row:

```bash
curl -X POST -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d "{\"versionId\":\"$VERSION\"}" \
  http://127.0.0.1:8080/api/v1/todos/$ID/_revert
```

That is the whole reason it works the way it does. The state being replaced is
snapshotted on the way past, so a revert is itself revertible, and the title
rule still runs — reverting to a title another task has taken since is refused
like any other update. The values come from the version and the hooks come from
the contract, so there is one write path rather than two with different rules.

A column marked `snapshot_ignore: true` is left out of the replay: it is still
copied into the version, but the live value wins when one is put back. That is
for the fields that describe the row now rather than the state being restored.

**An endpoint can answer without a credential.** `public:` in the table
configuration names the operations that do — generated ones and custom
endpoints alike, since they share one namespace:

```yaml
public: [Get, List]
```

Anything not named still needs one, which is the direction the default has to
run in: a list somebody forgot to add to is a protected endpoint, not an open
one. A name matching no operation is an error rather than a setting that quietly
does nothing. A custom endpoint can also say `public: true` in its own block,
which is where the rest of it is declared.

What it means is narrow, and worth reading twice: the claims lookup still runs.
An application that resolves a tenant from the host rather than from a token
gets one either way, and a caller who does present a credential is still
identified by it. What changes is that a caller who presents nothing is served
instead of refused — so on a table with a `tenant_id`, a genuinely anonymous
caller still gets no further than the repository, which has no tenant to scope
by.

**A custom endpoint writes through the same path a generated one does.**
`Writer()` is the repository with this service's rules already attached, so
`Complete` is:

```go
in := model.TodoUpdateInput{IsDone: patch.NewOptional(true)}
...
return s.Writer().Update(ctx, r.Path.ID, in)
```

rather than rebuilding the contract and assembling a `dbhook.Update` envelope by
hand. That is not only shorter: reaching for the repository directly means
passing the hooks yourself, and forgetting once is a second way into the table
where the rules do not run. It comes off the embedded default, so there is
nothing to wire and nothing to wire wrongly — and the generated operations go
through the very same writer:

```go
func (s DefaultTodoService) Update(ctx context.Context, r Request[…]) (*model.Todo, error) {
	return s.write.Update(ctx, r.Path.ID, r.Body)
}
```

`Create`, `Update`, `Delete`, `Restore` and `Revert` are all on it, each taking
the arguments that operation needs and nothing else.

Which is why the whole service is one line to build:

```go
func New(repo store.TodoRepository, notifier Notifier, logger *slog.Logger) api.DefaultTodoService {
	return api.NewTodoService(repo, &rules{repo: repo, notifier: notifier, logger: logger})
}
```

`rules` never mentions the service. It answers three questions —

```go
func (s *rules) Hooks() api.TodoHooks       { … }   // what happens around a write
func (s *rules) Bind(w api.TodoWriter)      { s.write = w }
func (s *rules) Complete(ctx, r) (…)        { … }   // the endpoints rig cannot write
```

— and `NewTodoService` asks, builds the writer from the answer, and hands it
back. That is the cycle gone: previously the value had to exist before it could
describe itself, so construction was two statements and, before that, two types.

`Bind` is in the interface rather than looked for by name, so a service that
never writes says so with an empty body instead of finding out at runtime that a
misspelling left it without a writer. rig calls it once, during construction,
before anything can reach a hook.

Overriding a generated operation is wrapping what `New` returns:

```go
type Service struct{ api.DefaultTodoService }

func (s *Service) Get(ctx context.Context, r api.Request[api.TodoGetPath, struct{}, struct{}]) (*model.Todo, error) {
	// …then hand off
	return s.DefaultTodoService.Get(ctx, r)
}
```

The custom endpoints keep working through the value inside it, so only what you
shadow changes.

**Claims are a parameter, and their type says whether there is a caller.**
Nothing reaches into the context for them.

A custom endpoint takes `api.Request[…]`, which carries `.Claims`. A write hook
takes them as a value:

```go
func (s *service) beforeDelete(_ context.Context, _ tenancy.Claims, _ *model.TodoDeleteInput, prev *model.Todo) error
func (s *service) announceCreated(ctx context.Context, claims tenancy.Claims, created *model.Todo)
```

A rule reads them off the context struct it already takes, so every field rule
does not grow a parameter:

```go
func (s *service) validateTitle(ctx context.Context, c *model.TodoValidatorContext, title string) error {
    _ = c.Claims.AccountID
```

A **read** hook takes a pointer, and that is the difference rather than an
oversight. A write cannot happen without a caller — the repository refuses
before any hook of any kind runs — so those claims are always real. A read
marked `public:` is reached by somebody who presented nothing, and nil is that
somebody:

```go
Narrow: func(ctx context.Context, caller *tenancy.Claims) (*model.TodoFilter, error) {
    if caller == nil {
        return publicRowsOnly(), nil
    }
    ...
```

Handing a zero-valued `Claims` there instead would be a tenant of all zeroes
that reads like a real one, which is the mistake worth making impossible.

**Another tenant sees nothing.** A row belonging to someone else answers 404
rather than 403, so identifiers cannot be probed:

```bash
curl -i -H "X-Tenant-Id: 33333333-3333-3333-3333-333333333333" \
  http://127.0.0.1:8080/api/v1/todos/$ID
```

**A failed validation answers with the shape of the request.** The body carries
a structure whose members are the input's members, so a client attaches each
message to the control it belongs to instead of parsing a sentence:

```bash
curl -X POST -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"title":"   "}' http://127.0.0.1:8080/api/v1/todos
# {
#   "code": "UnprocessableEntity",
#   "message": "todo is not valid: title CannotBeEmpty: cannot be empty",
#   "fields": {"title": {"code": "CannotBeEmpty", "message": "cannot be empty"}}
# }
```

The field error carries no field name — the member it hangs off is the field —
and the code is from a fixed set, so a client switches on it once rather than
matching on prose. A rule of your own picks its own code:

```bash
curl -X POST -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"title":"Untitled"}' http://127.0.0.1:8080/api/v1/todos
# "fields": {"title": {"code": "NotAllowed", "message": "give the todo a real title"}}

curl -X POST -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"title":"Write the tutorial"}' http://127.0.0.1:8080/api/v1/todos
# "fields": {"title": {"code": "AlreadyExists", "message": "another todo already has that title"}}
```

The duplicate check is the one that has to ask the database, and it is why the
validator context knows which fields changed: on an update that leaves the
title alone the only row it would find is the one being changed, so the rule
returns early rather than making the round trip.

## Change the schema

```bash
rig migration new add_todo_tags   # write the SQL
rig sync                          # read the database into todo.yaml
rig validate                      # check it
rig generate                      # rewrite the generated layers
```

`rig check` exits non-zero when the committed output no longer matches the
schema, which is what CI runs.

# Your service layer

> **Not written yet.** This page will document the service interface, lifecycle
> hooks, transactions, overriding a generated operation, and running the server.
>
> Until it exists, the best material is real code:
> [examples/todo/services/todo/todo.go](../examples/todo/services/todo/todo.go)
> is a heavily commented service layer with validation rules, hooks, and a
> custom endpoint, and [examples/todo/main.go](../examples/todo/main.go) is the
> wiring.

This is the layer rig will not write. It sits between the generated repository
and the generated HTTP handlers:

```
   generated            YOU WRITE THIS           generated
┌──────────────┐   ┌──────────────────────┐   ┌──────────────┐
│  repository  │ ← │    service layer     │ → │  API layer   │
└──────────────┘   └──────────────────────┘   └──────────────┘
```

rig writes the file **once** — `services/{table}/{table}.go` by default — and
never touches it again. A working implementation of every operation is already
there, so a project runs before you have written anything.

## Where things go

| | |
|---|---|
| A rule about a field | the validator |
| Something that must happen with a write, in the same transaction | a lifecycle hook (`rig/runtime/dbhook`) |
| What happens to your rows when a row you point at is deleted | a parent hook, below |
| An endpoint rig cannot write | a method, declared in the table's `endpoints:` |
| Replacing a generated operation | wrap what the constructor returns and shadow the promoted method |

Declaring an endpoint in the table configuration makes the build fail until you
implement it. Not a 501 at runtime — a compile error.

## When a row you point at is deleted

Every foreign key to another table gets a pair of fields on
`Hooks().Parents`, named after the relation:

```go
Parents: api.PlayerParentHooks{
    // Inside the transaction that is deleting the team, before the row goes.
    // Returning an error refuses the delete.
    TeamDeleting: func(ctx context.Context, claims tenancy.Claims,
        team *model.Team, in model.TeamDeleteInput) error {

        return s.repo.ClearTeam(ctx, team.ID)   // …or delete them. Or refuse.
    },

    // After that transaction has committed. It cannot fail the delete, which
    // is what makes it the place for the cache, the search index and the mail.
    TeamDeleted: nil,
},
```

Both are optional and nil is the default: the foreign key refuses the delete, and
the resulting `23503` is answered as a 409, exactly as before you wrote anything.

**What you get is the parent row, not your own rows.** One call per relation
rather than one per row, because clearing ten thousand links is one `UPDATE` you
write yourself. The two obvious bodies do not look different and do not cost the
same: a single statement skips your table's own hooks and snapshots, and a loop
calling your own `Delete` on each row gets both and costs a statement each. Which
one is right is a question about your table, and it is the reason this is a
function rather than a `cascade:` key.

**You also get the delete input**, and `Hard` is the part that matters: a hook
that nulls a link on a soft delete has destroyed the only record of what to
re-link if the parent is restored.

The order children are told in is derived — tables that reference each other are
told outermost-first, so a sibling never runs after something pointing at it.
It only matters for what one sibling can see of another and for which error wins
when two would both refuse; when it is wrong, the parent overrides it with
[`on_delete.order`](tables.md#on_delete).

`api.Register` wires all of this. If you build services and serve them some other
way, call `api.Link(handlers)` yourself — until you do, a delete runs your own
hooks and none of your children's.

## Running it

`rig/runtime/serve` is the pool, the HTTP server, the probes, and the shutdown.
`api.Main` is that with everything `rig.yaml` already decided filled in: a config
and a function that builds your handler.

`serve.Config` has no defaults. A value it invented for a config that said
nothing would be a value nobody chose, discovered only by whatever it costs when
it is wrong — so every field that would otherwise be one is refused unset, all of
them named at once, before anything opens or listens. What a deployment supplies
rather than states is still read from the environment: `DatabaseURL` and `Addr`
fall back to `$DATABASE_URL` and `$ADDR`. Fields where saying nothing means there
is nothing to do — `Ready`, `Pool`, `Monitor`, the `On…` hooks, `Hint` — stay
optional, because nil is the whole of what those have to say.

```go
api.Main(serve.Config{
    DatabaseURL: ...,
    Addr:        ...,

    // Two questions rather than one, and both read by whatever is checking
    // them. serve.NoProbe is how a project says it wants neither.
    LivenessPath:  "/livez",
    ReadinessPath: "/readyz",

    MaxStartup:        30 * time.Second,
    ConnectTimeout:    10 * time.Second,
    ProbeTimeout:      2 * time.Second,
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       30 * time.Second,
    WriteTimeout:      30 * time.Second,
    IdleTimeout:       2 * time.Minute,
    MaxShutdown:       api.ShutdownBudget(), // read it, then write the number

    Migrate: migrate.Require(migrations, migrate.Options{}),
    // Only the ones you can write. The housekeeping this project's blocks
    // already decided is merged in, and yours wins on a shared name.
    Tasks: map[string]serve.Task{
        "migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
    },
}, func(ctx context.Context, app *serve.App) (api.Parts, error) {
    repos := store.New(app.Pool, store.Config{})
    return api.Parts{Handler: api.Register(api.Handlers{
        Server: api.Server{GetClaims: yourClaimsFunc, DB: app.Pool, Logger: app.Logger},
        Todo:   todo.New(repos.Todos),
    })}, nil
})
```

`api.Parts` is one field per thing whose lifetime is longer than a request's.
Above it is the handler and nothing else; turning a block on in `rig.yaml` adds a
field, and `api.Main` starts, drains or closes whatever is in it. `serve.Main`
is still there for a project that wants the sequence itself, and `api.Mount` is
the same sequence as a `serve.Mount` for one running `serve.Run`.

Migrations live in `rig/migrate` and are embedded in your binary, so the schema a
build expects ships with that build. `migrate.Require` refuses to start when the
database is behind; `migrate.Apply` migrates on the way up instead.

`Logger` is where the server records why a request failed. It is optional and
nil means `slog.Default()` rather than silence — see
[observability.md](observability.md).

**`api.Server` is an alias.** The struct itself is
`rig/runtime/apibase.Server`, along with the request envelope, the request
context and the plumbing every handler shares — none of it derived from your
schema, so it is imported rather than written into your repository. Its godoc is
where every field is documented. What `Register` sets for you is everything the
project already decided: the revision this build serves, the headers it names,
its rate limiter, and where its spans go. What is left for you is the handful
above.

A project with `tracing:` on gets no more lines here: `api.Main` builds the
process, and the `page` it hands your wiring is the other half of it —
[observability.md](observability.md#wiring-it-up) has that in full. It also gets
`api.ShutdownBudget()`, which adds up the closers rig registers so you do not
have to. **Read it and write the number out.** `MaxShutdown` is the one field in
that struct with no default, because it is the one that leaves the program: it is
what goes into `terminationGracePeriodSeconds`, and whoever writes that manifest
should be able to read it off the struct rather than run the binary. A
`serve.Config` that leaves it out is refused before the server listens.

Add your own closers to the budget and write the sum; if you have none, write the
budget. A project with a `DrainDelay` adds that too — `serve` counts it against
the same number, and it is spent inside the grace period like everything else.

A literal is safe: `serve.App` sums every step actually registered before the
server listens and refuses a budget that cannot hold them, naming the parts. A
number left stale by a closer of your own is a process that will not start and
says why, rather than a shutdown that quietly truncates under load.

**What stops, and in what order.** `MaxShutdown` covers the whole sequence:
readiness turns false, `DrainDelay` gives a load balancer time to look away,
`app.Drain` steps stop whatever fetches its own work, the server finishes the
requests it has, and only then do the `app.Close` steps run and the pool shut.
The steps that declared a timeout are reserved out of the budget, so a request
that will not finish spends what is left over and not their share of it —
`serve.App` adds the parts up before the server listens and refuses a
`MaxShutdown` that cannot hold them.

A project with live sync has one more field, because a subscription is a request
the server is deliberately not answering yet and nothing else can end it:

```go
return api.Parts{Handler: mux, Shapes: proxy}, nil
```

See [electric.md](electric.md) for what a drained proxy tells a subscriber.

## See also

- [tutorial.md](tutorial.md#7-wire-it-up) — the smallest working `main.go`
- [tables.md](tables.md#endpoints) — declaring an endpoint
- [observability.md](observability.md) — the log, the spans, and reading a 500
- [concepts.md](concepts.md) — why this layer is yours

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
`serve.Main` takes a config and a function that builds your handler:

```go
serve.Main(serve.Config{
    DatabaseURL: ...,
    Addr:        ...,
    Migrate:     migrate.Require(migrations, migrate.Options{}),
}, func(ctx context.Context, app *serve.App) (http.Handler, error) {
    repos := store.New(app.Pool, store.Config{})
    return api.Register(api.Handlers{
        Server: api.Server{GetClaims: yourClaimsFunc, DB: app.Pool, Logger: app.Logger},
        Todo:   todo.New(repos.Todos),
    }), nil
})
```

Migrations live in `rig/migrate` and are embedded in your binary, so the schema a
build expects ships with that build. `migrate.Require` refuses to start when the
database is behind; `migrate.Apply` migrates on the way up instead.

`Logger` is where the server records why a request failed. It is optional and
nil means `slog.Default()` rather than silence — see
[observability.md](observability.md).

## See also

- [tutorial.md](tutorial.md#7-wire-it-up) — the smallest working `main.go`
- [tables.md](tables.md#endpoints) — declaring an endpoint
- [observability.md](observability.md) — what gets logged, and reading a 500
- [concepts.md](concepts.md) — why this layer is yours

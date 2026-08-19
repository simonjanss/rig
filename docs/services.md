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
| An endpoint rig cannot write | a method, declared in the table's `endpoints:` |
| Replacing a generated operation | wrap what the constructor returns and shadow the promoted method |

Declaring an endpoint in the table configuration makes the build fail until you
implement it. Not a 501 at runtime — a compile error.

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
        Server: api.Server{GetClaims: yourClaimsFunc},
        Todo:   todo.New(repos.Todos),
    }), nil
})
```

Migrations live in `rig/migrate` and are embedded in your binary, so the schema a
build expects ships with that build. `migrate.Require` refuses to start when the
database is behind; `migrate.Apply` migrates on the way up instead.

## See also

- [tutorial.md](tutorial.md#7-wire-it-up) — the smallest working `main.go`
- [tables.md](tables.md#endpoints) — declaring an endpoint
- [concepts.md](concepts.md) — why this layer is yours

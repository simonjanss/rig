# rig

An opinionated Postgres-first web system generator.

You write a good Postgres schema and your business logic. `rig` writes everything else: models,
repositories, HTTP handlers, routing, filter plumbing, live-sync endpoints, a Go client, OpenAPI,
a TypeScript client, and an authentication foundation.

```
   generated            YOU WRITE THIS           generated
┌──────────────┐   ┌──────────────────────┐   ┌──────────────┐
│  repository  │ ← │    service layer     │ → │  API layer   │
│  models      │   │  business logic      │   │  handlers    │
│  queries     │   │  validation, rules   │   │  routing     │
│  pgx impl    │   │  orchestration       │   │  filters     │
└──────────────┘   └──────────────────────┘   └──────────────┘
      pgx                                          net/http
```

## Four steps

```bash
rig sync       # 1. migrations → throwaway Postgres → introspect → one config file per table
               # 2. edit those config files
rig validate   # 3. schema conventions and configuration consistency
rig generate   # 4. compile to one IR, fan out to generators
```

## Status

Early development. See [docs/](docs/) for the tutorial and reference, and [examples/](examples/)
for complete applications built with rig.

Working on rig itself: run `make hooks` once after cloning, to install the
pre-push hook that runs the checks. [AGENTS.md](AGENTS.md) has the rest.

## Layout

| Path | What |
|---|---|
| `cmd/rig` | the CLI |
| `pkg/ir` | the intermediate representation every generator reads |
| `pkg/gen` | generator interface, registry, artifact writing |
| `internal/compile` | the pure compile pipeline |
| `runtime/` | a separate module, imported by generated code |
| `auth/` | a separate module: sessions, OAuth, API keys, RBAC |
| `migrate/` | a separate module: apply the project's migrations from its own binary |
| `rigclient/` | a separate module: the half of a generated Go client that is not generated |

A generated application depends on `rig/runtime` (and optionally `rig/auth` and
`rig/migrate`) — never on the CLI. A program that *calls* one depends on
`rig/rigclient`; see [examples/sdk](examples/sdk) for what that looks like.

# rig

An opinionated Postgres-first web system generator.

You write a good Postgres schema and your business logic. `rig` writes everything else: models,
repositories, HTTP handlers, routing, filter plumbing, live-sync endpoints, typed Go and TypeScript
clients, an authentication foundation, file uploads, and an inbox.

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

## Install

```bash
go install github.com/simonjanss/rig/cmd/rig@latest
```

Or download a binary from [the releases](https://github.com/simonjanss/rig/releases).
In a GitHub Actions workflow:

```yaml
- uses: simonjanss/rig/.github/actions/setup-rig@v0.1.0
- run: rig validate --strict
```

rig is eleven Go modules released together at one version, because the CLI
generates code that imports the runtime and the two have to agree. `rig version`
is the version to pin the libraries to:

```bash
go get github.com/simonjanss/rig/runtime@$(rig version)
```

## Four steps

```bash
rig sync       # 1. migrations → throwaway Postgres → introspect → one config file per table
               # 2. edit those config files
rig validate   # 3. schema conventions and configuration consistency
rig generate   # 4. compile to one IR, fan out to generators
```

## Generators

`rig generators` lists them. All but the two clients are scaffolded by `rig init`.

| Name | What it writes |
|---|---|
| `model-go` | the shared entity, its enums, its query types, and its inputs |
| `persist-go` | the repository interface and its pgx implementation |
| `service-go` | API types, service interfaces, and a working default implementation |
| `server-go` | net/http routing, request decoding, the handler registration struct, the live-sync shape endpoints with their tenant and lifecycle filters built in, and the delete propagation |
| `openapi` | an OpenAPI 3.1 document: every endpoint, schema and status the API answers with |
| `go-client` | a typed Go client: the wire types and one method per endpoint |
| `ts-client` | a typed TypeScript client: the wire types, one method per endpoint, and the live-sync collections |

## Documentation

[docs/](docs/) is the user documentation — how to build an application with rig.

| | |
|---|---|
| [Tutorial](docs/tutorial.md) | An API from an empty directory, in twenty minutes |
| [Concepts](docs/concepts.md) | The three layers, what is generated, what stays yours |
| [Design](docs/design.md) | Why rig works this way, and what each choice costs |
| [Schema](docs/schema.md) | The columns rig recognizes by name |
| [rig.yaml](docs/rig-yaml.md) · [Tables](docs/tables.md) | The two files you write |
| [Authentication](docs/auth.md) | Sessions, API keys, OAuth, RBAC |
| [Notifications](docs/notifications.md) | An inbox, with the audience worked out when it is sent |
| [Presence](docs/presence.md) | Who is here, and which field they are editing |
| [Observability](docs/observability.md) | The log, the spans, and how to read a 500 |
| [Clients](docs/clients.md) | The generated Go and TypeScript SDKs |

[examples/](examples/) holds complete applications, built and tested in CI.

## Status

Early development.

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
| `files/` | a separate module: uploads, the blob seam, the sweeper |
| `notify/` | a separate module: the notification engine and the inbox routes |
| `observe/` | a separate module: OpenTelemetry, for the projects that ask for it |
| `migrate/` | a separate module: apply the project's migrations from its own binary |
| `rigclient/` | a separate module: the half of a generated Go client that is not generated |
| `rigs3/` | a separate module: the S3 adapter for uploads, so a project on the memory backend carries no AWS SDK |
| `ts/` | a pnpm workspace: the half of a generated TypeScript client that is not generated |

A generated application depends on `rig/runtime` (and optionally `rig/auth` and
`rig/migrate`) — never on the CLI. A program that *calls* one depends on
`rig/rigclient`; see [examples/sdk](examples/sdk) for what that looks like. A
front end that calls one depends on `@rig-ts/client`, and on `@rig-ts/electric` as
well if it subscribes to a live-sync stream.

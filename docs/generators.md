# Generators

> **Not written yet.** This page will document each generator's options, exactly
> what it puts on disk, and how to write one of your own.
>
> Until it exists: `rig generators` lists them, and the `generators:` block in
> [examples/todo](../examples/todo/rig.yaml) is a working, commented
> configuration for every built-in.

Seven generators ship with rig. `rig init` scaffolds all but the two clients.

| Name | What it writes |
|---|---|
| `model-go` | the shared entity, its enums, its query types, and its inputs |
| `persist-go` | the repository interface and its pgx implementation |
| `service-go` | API types, service interfaces, and a working default implementation |
| `server-go` | net/http routing, request decoding, the handler registration struct, the live-sync shape endpoints with their tenant and lifecycle filters built in, the `Link` that wires deletes to the tables they reach, and the process around the server — `Main`, `Parts`, `Tasks`, `ShutdownBudget`, and the log sink, provider and page as one `Process` |
| `openapi` | an OpenAPI 3.1 document: every endpoint, schema and status the API answers with |
| `go-client` | a typed Go client: the wire types and one method per endpoint |
| `ts-client` | a typed TypeScript client: the wire types, one method per endpoint, and the live-sync collections |

`model-go` is listed first in a generated `rig.yaml` because the layers above
import what it writes. `server-go` writes nothing about live sync until a table
opts in with `electric: {enabled: true}`, so leaving `electric_url` and
`stub_dir` configured costs nothing — and `ts-client` makes the same promise
about the streaming half of its output, so a project that streams nothing never
installs the streaming package. The two clients are opt-in, because not every
project wants an SDK of its own.

Live sync had a generator of its own until it did not. What that bought was a
second package with a second server in it, whose claims lookup and error writer
an application had to fill in twice — and a second `Register` call it could
forget, on routes whose absence looks exactly like a front end nobody wrote yet.
A shape route is an API route: same mux, same caller, same refusal, same drain.
A `rig.yaml` that still names `electric` is told where the options moved.

Several blocks in `rig.yaml` change what these write without being generators of
their own. `auth:` makes `server-go` write the authentication wiring, and
`tracing:` makes `server-go` and `persist-go` open spans — a `tracing.gen.go`
beside the routes, a span per handler, and a span per repository call and per
hook. Without each block, not one line about it is emitted, which is what keeps
the corresponding module out of the application's `go.mod`.

`files:` writes a `files.gen.go` beside them with two constructors in it.
`NewFilesWithStore` takes the store, so a project can be run and tested against
`blob.NewMemory()` whatever its configuration says; `NewFiles` builds the one
`files.backend` named. On `memory` that cannot fail and returns the service. On
`s3` it takes a context and returns an error, because it reaches the bucket
before the first upload does — see [rig-yaml.md](rig-yaml.md#files) for what it
checks there and why the two signatures differ. Only the `s3` form imports
`rig/rigs3`, which is what keeps an AWS SDK out of the `go.mod` of a project
that does not use one.

`process.gen.go` is where those blocks meet the process your `main` runs.
`tracing:` and `monitoring:` give it a `Process` — the log sink, the provider and
the page, built in the order they depend on each other; `files:`, `presence:` and
`throttle:` give it housekeeping subcommands in `Tasks`; and whichever of
`tracing:`, `notifications:`, `presence:`, `auth:` and a table's `electric:`
register a shutdown step are added up into a `ShutdownBudget` — the number your
`main.go` writes as `MaxShutdown`, which is the one `serve.Config` field with no
default because it is the one an operator has to copy into a manifest. The same
steps give it a `Shutdown`, with a field per step this project registers and no
others, for the deployment that wants one of them sized differently —
[services.md](services.md) has it.

`run.gen.go` is the order those parts come to exist in, which used to be a
sequence every `main.go` wrote out. `Parts` has one field per lifetime longer
than a request's — the handler always, and an `Engine` or `Auth` when the block
that gives it one is on — and `Main` is a `serve.Config` and the one function
only your application can write. The live-sync proxy is not one of them: it is
named in `Handlers.Shapes`, which mounts its routes and registers their drain in
the same call. Everything between the two is
generated: the process built before the config it fills in and attached to the
server it flushes for, the sweeper started before your wiring because it needs
nothing from it, and each of the rest started, drained or closed after, with the
numbers `ShutdownBudget` counted. A field left nil is said at startup rather than
discovered under load.

Both are written for every project, because every project has a task to merge
into and routes to serve — but everything in either that names `rig/observe` is
behind the same `tracing:` predicate, so the rule above holds.

## Options, briefly

Every generator takes `out_dir` and an `options` block. The common ones:

```yaml
- name: persist-go
  out_dir: internal/store
  options:
    package: store
    model_import: github.com/you/app/internal/model
```

| Option | Which generators | |
|---|---|---|
| `package` | all | The Go package the generated files declare |
| `model_import` | `persist-go`, `service-go`, `server-go` | Import path of the generated model package. Required |
| `store_import` | `service-go` | Import path of the generated persistence layer |
| `api_import` | `service-go`, `server-go` | Import path of the generated API package, so a stub elsewhere can refer back |
| `stub_dir` | `service-go`, `server-go` | Where your hand-owned files go — the service stub and the shape scopes, in the same directory. `{table}` and `{Table}` are substituted. Empty writes no stubs |
| `stub_package` | `service-go`, `server-go` | Package a stub declares. Empty uses the table name |
| `electric_url` | `server-go` | The sync service to proxy to |
| `electric_required` | `server-go` | Whether a sync service that is not answering stops the server starting, instead of a warning at boot. Off by default |
| `formats` | `openapi` | Which renderings to write: `json`, `yaml`, or both. Both by default. Also decides which of the document's own routes are described when [`api.openapi.serve`](rig-yaml.md#openapiserve) is on |
| `electric` | `openapi` | Whether the live-sync routes are described. On by default |
| `client_import` | `go-client`, `ts-client` | Import path, or npm specifier, of the SDK runtime. For a fork or a vendored copy |
| `electric_import` | `ts-client` | npm specifier of the streaming runtime. Same reasons |
| `request_id_header` | `server-go` | Header a caller may name its own request in. Read on every route, `/auth/*` included |

**Two options are not in that table, because they left it.** `servers` on
`openapi` and `default_base_url` on the two clients both moved to rig.yaml's
[`servers:`](rig-yaml.md#servers) block, which every SDK generator and the
document read — so the three cannot disagree about where the API is, and a
generator added later gets the deployments without an option of its own. Both
keys still work when there is no `servers:` block, and rig warns (RIG3010) when
it sees one.

**Whether the document is served is not an option here either**, and neither is
where the router finds it. It is a route, so it is
[`api.openapi.serve`](rig-yaml.md#openapiserve) in rig.yaml — read by this
generator, which describes the two routes and writes the embed that carries
them, and by `server-go`, which imports that package and mounts them. The import
path is `project.module` joined to this generator's `out_dir` taken from your
module root — one right answer, everything it needs already written down, so rig
joins them rather than adding an option whose only wrong value is a typo. Where
the module root is comes from the nearest `go.mod`, so `out_dir: api/docs` in a
project whose module begins at `api/` is the package `<module>/docs`, and an
`out_dir` outside that module is refused (RIG3011). What stays here is `formats`,
because that is about which files get written; see [api.md](api.md#serving-it).

## The two clients

`go-client` and `ts-client` read the same document and answer the same shape:
methods grouped per resource, a QUERY that falls back to the `_search` alias once
and remembers, query parameters that are absent rather than zero, a per-input
shape for the 422 body, and a multipart create beside the JSON one wherever a
file column would otherwise be unreachable. Where they differ, the difference is the
language's — a Go `patch.Optional[T]` is a TypeScript `?: T`, and a Go
`client.TodoCreateError(err)` is a TypeScript `isTodoCreateError(err)`.

The one place they differ about the API itself is live sync, and it is worth
knowing before you write against it: **a streamed row is not the same shape as
the row the API sends.** See [clients.md](clients.md#two-shapes-for-one-row).

## `.gen.` versus stubs

A file with `.gen.` in its name is rewritten on every run. Anything else a
generator produces — the service stub, the electric scoping stubs, one per
shape — is written once and then belongs to you.

The naming is not only a hint for your `.gitignore`. It is how rig recognizes
its own work: a `.gen.` in the name, or `// Code generated by rig. DO NOT EDIT.`
on the first line, and rig will report the file as a leftover once no generator
produces it. So do not hand-write a file with `.gen.` in its name inside a rig
project — `rig check` will report it and `rig generate --prune` will delete it.
A stub carries neither mark, which is what makes it safe from both.

`rig generate --force` overwrites generated files that were edited by hand.
`rig generate --prune` deletes files no generator produces any more.

## See also

- [rig-yaml.md](rig-yaml.md#generators) — the `generators:` block
- [concepts.md](concepts.md) — what the three Go layers are for

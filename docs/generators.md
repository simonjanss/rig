# Generators

> **Not written yet.** This page will document each generator's options, exactly
> what it puts on disk, and how to write one of your own.
>
> Until it exists: `rig generators` lists them, and the `generators:` block in
> [examples/todo](../examples/todo/rig.yaml) is a working, commented
> configuration for every built-in.

Seven generators ship with rig. `rig init` scaffolds all but `go-client`.

| Name | What it writes |
|---|---|
| `model-go` | the shared entity, its enums, its query types, and its inputs |
| `persist-go` | the repository interface and its pgx implementation |
| `service-go` | API types, service interfaces, and a working default implementation |
| `server-go` | net/http routing, request decoding, the handler registration struct, and the `Link` that wires deletes to the tables they reach |
| `electric` | live-sync shape endpoints, with the tenant and lifecycle filters built in |
| `openapi` | an OpenAPI 3.1 document: every endpoint, schema and status the API answers with |
| `go-client` | a typed Go client: the wire types and one method per endpoint |

`model-go` is listed first in a generated `rig.yaml` because the layers above
import what it writes. `electric` emits nothing until a table opts in with
`electric: {enabled: true}`, so leaving it configured costs nothing. `go-client`
is opt-in, because not every project wants a Go SDK of its own.

Two blocks in `rig.yaml` change what these write without being generators of
their own. `auth:` makes `server-go` write the authentication wiring, and
`tracing:` makes `server-go` and `persist-go` open spans — a `tracing.gen.go`
beside the routes, a span per handler, and a span per repository call and per
hook. Without each block, not one line about it is emitted, which is what keeps
the corresponding module out of the application's `go.mod`.

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
| `api_import` | `service-go` | Import path of the generated API package, so a stub elsewhere can refer back |
| `stub_dir` | `service-go`, `electric` | Where your hand-owned files go. `{table}` and `{Table}` are substituted. Empty writes no stubs |
| `stub_package` | `service-go`, `electric` | Package a stub declares. Empty uses the table name |
| `electric_url` | `electric` | The sync service to proxy to |
| `shape_import` | `electric` | Import path of the generated shape package |
| `formats` | `openapi` | Which renderings to write: `json`, `yaml`, or both. Both by default |
| `servers` | `openapi` | Origins the API answers on. Defaults to a single relative server |
| `electric` | `openapi` | Whether the live-sync routes are described. On by default |
| `client_import` | `go-client` | Import path of the SDK runtime. For a fork or a vendored copy |
| `default_base_url` | `go-client` | Emitted as a constant. Leave it out for anything that runs in more than one place |
| `request_id_header` | `server-go` | Header the generated auth error mapper reads a request identifier from |

## `.gen.` versus stubs

A file with `.gen.` in its name is rewritten on every run. Anything else a
generator produces — the service stub, the electric scoping stubs, one per
shape — is written once and then belongs to you.

`rig generate --force` overwrites generated files that were edited by hand.
`rig generate --prune` deletes files no generator produces any more.

## See also

- [rig-yaml.md](rig-yaml.md#generators) — the `generators:` block
- [concepts.md](concepts.md) — what the three Go layers are for

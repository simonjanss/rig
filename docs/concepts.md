# Concepts

What rig generates, what stays yours, and how the pieces fit. Read this once and
the reference pages will make sense.

## The shape of a rig project

You write a Postgres schema and your business logic. rig writes the layers on
either side of it.

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

The middle column is the point. rig does not try to guess your business rules
from your schema, and it does not give you an escape hatch to hand-edit the
layers it wrote. It writes the parts that are mechanical, gives you a typed seam
in the middle, and stops.

## Four steps

```bash
rig sync       # 1. migrations → throwaway Postgres → introspect → one config file per table
               # 2. edit those config files
rig validate   # 3. schema conventions and configuration consistency
rig generate   # 4. write the code
```

Steps 1 and 2 are the loop you spend time in. Steps 3 and 4 are fast and get run
constantly.

## The database is the declaration

rig does not have a schema language. Your migrations are the schema, and rig
reads them by **applying them to a real Postgres and introspecting the result**.

This has one consequence worth internalising, because it explains most of the
surprises: **there is no configuration key for a behaviour the schema can state
itself.** You do not write `soft_delete: true` — you add a `deleted_at` column,
and rig sees it. You do not declare a foreign key — you declare it in SQL, and
rig derives the relation. Two places to say the same thing would eventually
disagree, and the database is the one that wins arguments at runtime.

[schema.md](schema.md) lists every column rig recognizes by name and what having
it turns on.

## What table configuration is for

Your schema says what *exists*. It cannot say what things *mean*, or which of
them belong on a public API. That is what the per-table YAML file is for:

- documentation (`comment:`)
- names on the wire (`resource:`, `field:`, `path_segment:`)
- what the API exposes (`operations:`, `exclude:`, `read_only:`, `immutable:`)
- what is beyond CRUD (`endpoints:`)

It can never contradict the database. There is no key for a column's type, its
nullability, or its keys, because those are facts rather than choices.

Files are keyed by **physical names** — the table, column, and enum label as
spelled in Postgres — not by the names that end up in your API. API names are
derived, so keying on them would move your configuration under you the first
time you renamed a resource.

See [tables.md](tables.md).

## `.gen.go` is not yours

Every generated file has `.gen.` in its name, and is rewritten from scratch on
every `rig generate`. A fix to something in one of them belongs in your schema
or your configuration, never in the file.

Some generators also write **stubs** — a file written once, and then never
touched again. Your service layer is one. rig writes a working starting point
and gets out of the way; that file is yours from the moment it exists.

```
services/todo/todo.go        written once by rig, then yours forever
internal/api/todo.gen.go     rewritten on every run
```

By default, `rig init` puts `*.gen.go` in your `.gitignore` and expects your
build to run `rig generate`. Committing it instead is a legitimate choice — the
examples in this repository do, so that a generator change shows up as a diff in
review — and `rig check` is what you run in CI either way: it regenerates in
memory and fails if the result differs from what is on disk.

That covers all three ways committed generated code goes wrong. A file the
schema has moved past differs. A file somebody edited by hand differs. And a
file left behind by a renamed table is reported too, because rig recognizes its
own output by name and by banner rather than by consulting a record CI does not
have. See [cli.md](cli.md#what-the-gate-covers).

## Every query is scoped to a tenant

rig is multi-tenant at the foundation, not as an add-on. Every table it
generates for carries a `tenant_id`, every generated query filters on it, and
the value comes from the caller's credentials rather than from anything in the
request body.

A single-tenant application is a multi-tenant one with a single tenant. That is
a real cost — one extra column and one extra index on every table — paid so that
the day a second customer appears is not a rewrite.

## The three layers, concretely

For a table called `todo`, a default project gets:

**Model** — `internal/model/`. The entity, its enums, the type you filter with,
and the create and update inputs with their validation.

```
model.gen.go            shared types
todo.gen.go             the Todo entity
todo_input.gen.go       TodoCreateInput, TodoUpdateInput, and their validation
todo_query.gen.go       the filter type
todo_priority.gen.go    one file per enum
```

**Store** — `internal/store/`. A repository interface per table and its pgx
implementation. Typed queries, no SQL strings in your code.

```
store.gen.go            the repository set
todo_repository.gen.go  TodoRepository and its implementation
```

**API** — `internal/api/`. The wire types, the interface your service implements,
routing, and handlers.

```
api.gen.go              shared wire types, Register
todo_service.gen.go     the service interface and a working default
todo_routes.gen.go      routing
server.gen.go           request decoding, the handler registration struct
```

Both outer layers import the model, so a row has one definition rather than a
copy on each side and a conversion between them.

**Your service** — `services/todo/todo.go`. Written once. It declares the
business rules, the lifecycle hooks it wants, and any endpoint rig cannot write
for you.

The model, store, and API generators are configured together because they only
work together: the API layer calls the repository and the HTTP layer calls the
API layer, so a project with one of the three does not build.

## The IR

Everything above is produced from one intermediate document. `rig generate`
compiles your schema and configuration into it, then hands the same document to
every generator.

You can look at it:

```bash
rig ir              # the whole document
rig ir --schema-only   # just the normalized schema
```

The encoding is canonical, so the output is stable and diffable — committing it
and watching it change across a refactor is a good way to see exactly what a
migration did to your API. You never have to look at it, but when a generator
produces something surprising, this is where the answer is.

## Nothing generated depends on the CLI

Your application imports `rig/runtime` (and optionally `rig/auth`,
`rig/migrate`). It never imports rig itself. The CLI is a build-time tool: it
does not ship with your binary, and there is no rig running in production.

That is also why container handling lives in the CLI and not in `runtime/serve`.
A server that starts its own Postgres is the wrong thing to copy into a real
deployment.

## Where to go next

- [Tutorial](tutorial.md) — build this from an empty directory
- [Design](design.md) — why each of the above is the way it is
- [schema.md](schema.md) — the columns rig recognizes

# rig documentation

How to build an application with rig. If you are working on rig itself, you want
[AGENTS.md](../AGENTS.md) instead.

## Start here

| | |
|---|---|
| [Tutorial](tutorial.md) | Build a working API from an empty directory. Start here if you have never used rig. |
| [Concepts](concepts.md) | The three layers, what is generated, what stays yours. Read it once and the rest of these pages make sense. |
| [Design](design.md) | Why rig works the way it does, and what each choice costs you. |

## What you write

rig reads two kinds of file you author: your migrations, and one configuration
file per table.

| | |
|---|---|
| [Schema](schema.md) | The columns rig recognizes by name: `id`, `tenant_id`, the audit columns, `deleted_at`, the snapshot triple. Your schema is the declaration. |
| [rig.yaml](rig-yaml.md) | The project file: layout, API shape, database, naming, validation rules, generators. |
| [Table configuration](tables.md) | The per-table file: which operations exist, what each column is called, custom endpoints. |
| [Authentication](auth.md) | Sessions, API keys, OAuth, RBAC, and the `auth:` block that configures them. |

## What rig writes

| | |
|---|---|
| [The CLI](cli.md) | Every command and flag. |
| [Generators](generators.md) | The six built-ins, their options, and what each one puts on disk. |
| [The HTTP API](api.md) | The endpoints you get: CRUD, filtering, pagination, PATCH semantics, error codes. |
| [Your service layer](services.md) | The part rig will not write: business rules, lifecycle hooks, and running the server. |
| [Clients](clients.md) | The generated Go client. |
| [Live sync](electric.md) | Shape endpoints, for a client that subscribes instead of polling. |
| [Diagnostics](diagnostics.md) | Every `RIG####` code, what it means, and how to change its severity. |

## Complete applications

The [examples](../examples/) are real projects, built and tested in CI. Each has
a README that walks through it.

| | |
|---|---|
| [todo](../examples/todo) | One table, no authentication, a complete API. The tutorial's subject. |
| [fantasyfootball](../examples/fantasyfootball) | Many tables: relations, enums, filtering. |
| [auth](../examples/auth) | Sign-in, roles, API keys, and what stays in Go. |
| [auth_oauth](../examples/auth_oauth) | A tenant per host, and provider sign-in that survives the redirect. |
| [sdk](../examples/sdk) | A program that calls two rig applications through their generated clients. |

## A note on what is not here

Several pages below are stubs, and say so at the top. rig documents what it
does, not what it is going to do — planned work lives in
[NEXT.md](../NEXT.md), which is working notes rather than documentation.

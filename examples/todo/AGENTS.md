# todo

Generated with [rig](https://github.com/simonjanss/rig).

## Four layers, you write one

    generated            YOU WRITE THIS           generated
  ┌──────────────┐   ┌──────────────────────┐   ┌──────────────┐
  │  repository  │ ← │    service layer     │ → │  API layer   │
  │  pgx, SQL    │   │  business logic      │   │  handlers    │
  │              │   │  rules, hooks        │   │  routing     │
  └──────┬───────┘   └──────────────────────┘   └──────┬───────┘
         │             ┌──────────────────┐            │
         └────────────►│      model       │◄───────────┘
                       │   (generated)    │  entities, enums, queries,
                       └──────────────────┘  inputs, validation

Both outer layers speak the model's types, so nothing is converted between
them: a field is defined once and returned as it is stored.

The service layer under `services/<table>/` is the only place to write code.
It implements a generated interface and calls a generated repository. A table
with no business logic needs nothing but its constructor and an empty set of
rules: the generated default implementation already satisfies the interface.

Every service says what it owes, in a `contract()` function the stub writes. It
is passed to the constructor, so there is no service whose rules were never
attached, and every field is listed there even when it is nil — adding a column
shows up as a field nobody filled in.

What goes in the contract:

- **A rule about a field** — a function in the create validator, the update
  validator, or both. Each has one entry per field that operation can set, so
  an update has no hook for a column it cannot touch. The hook sees the row the
  request would produce. Return a `model.FieldError` and the client is answered
  422 with that field named.
- **Something that must happen with the write** — a hook: `Hooks.Create.Before`,
  `.After`, `.AfterCommit`, and the same for Update, Delete and Restore. Before
  and After run inside the write's transaction, so returning an error undoes it.
  AfterCommit runs once it has landed, which is the only safe place to tell
  anything outside the database.
- **An endpoint the table configuration declares** — a method on the same type,
  handed over as `Endpoints`. rig has no default for one, so the set is an
  interface: declare an endpoint and forget to write it and the build fails.

Anything else — taking over a generated operation entirely — is a method on the
service that overrides the embedded default.

What the schema already declares — NOT NULL, lengths, enum membership — is
checked by the generated code. Do not write it again.

## Which files you may edit

| Pattern | Who owns it |
|---|---|
| `rig.yaml` | you — the project's whole configuration |
| `migrations/*.sql` | you |
| `services/<table>/<table>.yaml` | you, via `rig sync` |
| `main.go` | you |
| `services/<table>/<table>.go` | you |
| `*.gen.go`, `*.gen.ts` | rig — rewritten on every run, never edit |

## Migrations

    rig db up          development: a throwaway Postgres, migrated
    go run . migrate   anywhere else: the binary applies what it embeds

They are the same files read by the same library, so the two cannot disagree
about what the schema is. Applying them from the binary rather than from the
rig CLI is deliberate: the build that carries the code carries the schema it
expects, where a CLI on a deployment machine is whatever version was installed
there.

Run it as its own step before a rollout. Migrating at boot is one line
(`cfg.Migrate`) and fine for a single instance; with replicas it means every
one of them migrating at once, a slow migration holding the rollout open, and a
bad one taking down the fleet instead of one job.

## The loop

    rig migration new <name>   write a migration
    rig sync                   read the database into the table configuration
    rig validate               check the schema and the configuration
    rig generate               write the code

`rig validate` reports every problem in one pass, each anchored to the exact
line. `rig codes RIG3101` explains any code it prints.

## Schema conventions

rig infers behavior from column names, so the schema is the source of truth:

| Column | Effect |
|---|---|
| `id uuid primary key` | required on every table |
| `tenant_id uuid not null` | every generated query is scoped by it |
| `created_at`, `created_by_account_id` | stamped automatically |
| `updated_at`, `updated_by_account_id` | stamped automatically |
| `deleted_at` | makes the table soft-deletable, and adds `_deleted` and `_restore` |
| `version_type` + `snapshot_from_<table>_id` + `_at` | keeps prior versions, and adds `_versions` and `_revert` |

Add them in a migration; do not try to configure them.

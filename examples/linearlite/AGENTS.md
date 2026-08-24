# linearlite

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

`client/` is generated too, and is the one generated directory not under
`internal/`: it is the Go SDK for this API, and it exists to be imported by
somebody else's program. `import/` is that somebody: the CSV job that fills the board through it.

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

## Running it

`make demo` in this directory is the whole setup — containers, migrations,
seed, front end, server — and every step of it is also its own target. One
caveat for working inside the rig repository: under `RIG_DB_ISOLATE` the sync
service publishes on a kernel-chosen port rather than 55445, so export
`ELECTRIC_URL` from the address `rig db up` printed before `go run .`, or the
generated proxy points at the default and the streams answer 502.

## This example's extras

Beyond the todo example's layout, five directories and one file:

- `web/` — the React front end. Its generated client is `web/src/api`
  (`*.gen.ts`, rig's, never edit) and everything else in `web/src` is yours.
  `pnpm build` writes `web/dist`, which `main.go` serves from disk; `make
  examples` deliberately never builds it, so the Go suite needs no pnpm. The
  `linearlite-web` make target at the repository root typechecks and builds it,
  and is part of `make check` — this is the only front end in the repository that
  anything compiles.
- `web/src/presence/` — the browser half of presence, and the one place in
  `web/` where reading the comments before editing actually matters. Three
  decisions are load-bearing and each has a plausible wrong version: the handle
  is built in an effect rather than during render (StrictMode's double mount
  orphans one built in the body, and `close()` is final), the idle stand-in
  returns a *stable* empty array (`useSyncExternalStore` compares snapshots by
  identity, so a fresh one is an unbounded re-render), and `useSpot` is two
  effects because reporting a target and ending a lifetime need different
  dependency lists. `docs/presence.md` says all three in prose.
- `importer/` + `import/` — the batch job over the generated Go client, split
  so the docker test drives the same loop the command runs.
- `services/rig_presence/` — one file, the scope stub, and the only filled-in
  scope stub in the repository. Every other shape here leaves the generated
  tenant filter as the whole scope; this one narrows on `scope` and `target_id`,
  because a heartbeat is a row change delivered to every subscriber and the
  fan-out is what decides whether presence is affordable. There is no
  `rig_presence.yaml` beside it: `presence.expose` is off, so the table has a
  model, a repository and a shape and no REST surface.
- `services/authz/` — the roles-and-permissions model, an adapted copy of
  examples/auth's; that copy's comments explain every decision. Note the one
  name spelled out in it: `todo.claim` is the key rig derived from the custom
  endpoint, and a derived key nobody grants is a 403 on a working button.
- `services/outbox/` — one ring buffer implementing both interfaces rig ships
  no transport for: `account.Notifier` (the links auth mints) and
  `notify.Sender` (the email copy of an inbox line). It records instead of
  sending, which is the thing a real one must never do, and the front end says
  so where it shows them.
- `demo.go` — the only hand-written HTTP here: `GET /_demo/outbox` and
  `GET /_demo/tour`. Hand-written because neither is about a table, so there is
  nothing for rig to generate from. Add a route here rather than inventing a
  resource for something that lives in memory. Both need a session: whether
  rig's monitoring page is mounted is not a fact to hand an anonymous caller,
  since rig mounts no route at all rather than one that refuses.
- `services/rig_notification_device/` + `services/rig_notification_setting/` —
  the two notification tables a person owns rather than reads, exposed as
  ordinary resources by `notifications: expose: true` plus an `operations:` line
  each. Both are `access: {scope: own, owner: account_id}`, so reads and updates
  are narrowed before any code here runs; the one thing each service layer adds
  is the rule a create needs, because a create has no row to be narrowed by.
  The other three notification tables say `expose: false` in their own files.

The `/auth/*` screens are worth knowing about before adding another: `web/src/`
already covers sign-in, registration, the picker, reset, invitations (send,
list, withdraw), API keys, a password change, session listing and revocation,
the authentication trail, and switching tenants. Those calls are hand-written in
`web/src/auth/authApi.ts` because they are rig's endpoints rather than this
schema's — the generated client covers the API and stops at `/auth`.

`presence:` is on with nothing but `enabled`, which is why `main.go` has three
lines for it — `Presence` on `api.Handlers`, `RigPresence` on
`genelectric.Handlers`, and the sweeper with its own `CloseWithin` — and why
`MaxShutdown` is thirty-five rather than thirty: a third closer changed the
arithmetic the comment above that number states.

`tracing:` and `monitoring:` are on, which is why `main.go` opens a log sink
before `serve.Main`, tees it into the logger, passes `observe.Pool`, and takes
a `*observe.Page` in `newAPI` — nil from a task, since a cron entry serving a
page nobody can reach is not worth the wiring. The three environment variables
the page needs are set by `make demo` into a gitignored `.run/`, never by
rig.yaml, which is checked in.

The auth foundation's tables came from `rig setup-project` (migrations 1–7),
and `rig_account` is exposed read-only as the `Account` resource — the board's
member list. The seed task (`go run . seed`) is `seed.go`, and the fixed
identifiers at the top of it are what the docker tests and the README sign in
with.

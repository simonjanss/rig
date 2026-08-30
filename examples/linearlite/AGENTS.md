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

The service layer under `api/internal/services/<table>/` is the only place to
write code. It implements a generated interface and calls a generated
repository. A table with no business logic needs nothing but its constructor and
an empty set of rules: the generated default implementation already satisfies
the interface.

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

## Where things live

Two halves and the document that describes both:

    rig.yaml        the whole configuration — above both halves, because it
                    describes both
    api/            the Go module: main.go, client/, importer/, import/,
                    integration/, and internal/, which holds generated/,
                    services/, migrations/ and app/
    web/            the React front end, including its generated client
    docs/           the generated OpenAPI document

Every path in `rig.yaml` is relative to `rig.yaml`, which is what puts the Go
module root a directory below it. Three consequences worth knowing before moving
anything:

- **`api/internal/migrations/` is under the module because `api/main.go`
  embeds it.** `go:embed` cannot reach outside its own package's directory, so
  SQL that ships in the binary has to live inside the module that ships it. It
  can reach downwards, which is why the SQL sits under `internal/`: the embed
  pattern and the `Dir:` on the `migrate.Source` both name
  `internal/migrations`.
- **Everything `rig generate` writes under `api/` lands in
  `api/internal/generated/`**, bar `api/client/`. The split is the point —
  `generated/` is rewritten on every run, and every other directory under
  `api/internal/` is yours.
- **`go` commands run in `api/`; `rig` commands run anywhere.** rig walks up to
  the first `rig.yaml`, so `rig check` and `rig db url` work from either
  directory. `go build ./...` does not: run from here it matches no packages,
  and — this is the trap — *succeeds*. The repository Makefile computes the
  package pattern for exactly this reason.

The module path is `github.com/simonjanss/rig/examples/linearlite`, without the
`api`. `api/go.mod` is where the module begins, so
`api/internal/generated/model` is the package that path names.

## Which files you may edit

| Pattern | Who owns it |
|---|---|
| `rig.yaml` | you — the project's whole configuration |
| `api/internal/migrations/*.sql` | you — and only yours; rig's are carried by its modules |
| `api/internal/services/<table>/<table>.yaml` | you, via `rig sync` |
| `api/main.go` | you |
| `api/internal/app/*.go` | you — hand-written wiring, beside `generated/` rather than inside it |
| `api/integration/*_test.go` | you |
| `api/internal/services/<table>/<table>.go` | you |
| `*.gen.go`, `*.gen.ts` | rig — rewritten on every run, never edit |

`api/client/` is generated too, and is the one generated directory outside
`api/internal/generated/`: it is the Go SDK for this API, and it exists to be
imported by somebody else's program, which `internal/` would forbid. `api/import/` is that somebody: the CSV job that fills the board through it.

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
seed, front end, server — and every step of it is also its own target. It runs
each step where that step belongs, which is `api/` for everything Go; by hand,
`go run .` means `cd api` first. One caveat for working inside the rig
repository: under `RIG_DB_ISOLATE` the sync service publishes on a
kernel-chosen port rather than 55445, so export `ELECTRIC_URL` from the address
`rig db up` printed before `go run .`, or the generated proxy points at the
default and the streams answer 502.

## This example's extras

Beyond the todo example's layout, seven directories and one file:

- `web/` — the React front end. Its generated client is `web/src/api`
  (`*.gen.ts`, rig's, never edit) and everything else in `web/src` is yours.
  One thing to know before adding a `/_demo/` call: the routes outside
  `api.base_path` each need a line in `web/vite.config.ts`'s dev proxy, and a
  missing one fails quietly — the request lands on `index.html` and the caller's
  `.catch` swallows the parse error. `/_demo` was missing for as long as it had
  only one caller, which is why the tour's nav items never appeared under
  `pnpm dev`.
  `pnpm build` writes `web/dist`, which `api/internal/app` serves from disk; `make
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
- `api/importer/` + `api/import/` — the batch job over the generated Go client, split
  so the docker test drives the same loop the command runs.
- `api/internal/app/` — the server itself: every service, hook and route the
  binary mounts, built by `app.New` over a pool. Hand-written, which is why it
  sits beside `api/internal/generated/` rather than in it, and a package rather
  than a block in `api/main.go` for one reason — a test cannot import a `main`,
  so this is what `api/integration/` builds. What is left in `api/main.go` is
  the process around it: the log sink, the tracing provider, the embedded
  migrations, and the `serve.Config` naming the tasks.
- `api/integration/` — every test in this example, the docker suite and the one
  file that needs no database. `go test -tags docker ./integration/` in `api/`
  runs it;
  `make examples` reaches it like any other package. Nothing else in the
  repository puts its suite in a folder: everywhere else the tests sit in the
  package under test, which here would be `main`.
- **There are no fallback files here, and that is the point.** What answers a
  shape when the sync service cannot be reached is `DB: pool` on
  `electric.Config` in `api/internal/app` — one field, and every shape survives an outage
  on a snapshot of its own rows. There used to be a file per shape, and the
  hazard they carried was that **a fallback had to narrow exactly as far as its
  shape did** with nothing checking it; the read the proxy builds *is* the
  shape's `WHERE` clause, so there is no second description to keep in step.
  Presence is the exception and rig decides it rather than this project —
  `applyPresenceTable` sets `Fallback: false`, and
  `api/integration/electric_docker_test.go`'s 502 assertion is the guard on that.
- `api/internal/services/rig_presence/` — one file, the scope stub, and the
  only filled-in scope stub in the repository. Every other shape here leaves the
  generated tenant filter as the whole scope; this one narrows on `scope` and
  `target_id`, because a heartbeat is a row change delivered to every subscriber
  and the fan-out is what decides whether presence is affordable. There is no
  `rig_presence.yaml` beside it, and there is none anywhere under
  `api/internal/services/` for a table of rig's: rig ships their configuration
  and the compiler reads it from there. It is also the one
  `api/internal/services/rig_*` file left, and it is hand-written rather than
  scaffolded — rig no longer writes stubs for its own tables, because the three
  others here were `return nil` and two were imported by nothing.
- `api/internal/services/authz/` — the roles-and-permissions model, an adapted
  copy of examples/auth's; that copy's comments explain every decision. Note the
  one name spelled out in it: `todo.claim` is the key rig derived from the
  custom endpoint, and a derived key nobody grants is a 403 on a working button.
- `api/internal/services/outbox/` — one ring buffer implementing both
  interfaces rig ships no transport for: `account.Notifier` (the links auth
  mints) and `notify.Sender` (the email copy of an inbox line). It records
  instead of sending, which is the thing a real one must never do, and the front
  end says so where it shows them.
- `api/internal/app/demo.go` — the only hand-written HTTP here:
  `GET /_demo/outbox`, `GET /_demo/tour`, and the sync switch
  (`GET /_demo/sync`, `POST /_demo/sync/stop`, `POST /_demo/sync/start`).
  Hand-written because none is about a table, so there is nothing for rig to
  generate from. Add a route here rather than inventing a resource for
  something that lives in memory. All of them need a session: where rig's
  monitoring page listens is not a fact to hand an anonymous caller, since rig
  opens no port at all rather than one that refuses. `/_demo/tour` hands back
  the page's absolute URL, because it is on a port of its own and a relative
  href reaches the API instead.

  The switch stops and starts the ElectricSQL container so the board can
  demonstrate surviving without it — the README's
  "Take the sync service down" is the script. Two things about it are
  load-bearing. **It is gated on `$RIG_DEMO_SYNC_CONTAINER` and the gate is not
  a 403**: a handler that shells out to `docker stop` must not exist in a build
  nobody told which container to touch, and a route answering 403 still tells a
  scanner this process can reach a container engine. `make demo` sets the
  variable; `rig.yaml` is checked in and never will. And **it reports the
  container's state and `Proxy.SyncReachable()` as two separate fields**,
  because they come apart in both directions and that gap *is* the circuit
  breaker — the only way it is visible from a browser. Collapsing them into one
  "is sync up" boolean is the plausible wrong version.

  It shells out rather than using `internal/dockerdb`, which an example cannot
  import: `examples/linearlite/api` is its own module and does not require the
  root one, and adding that for three subcommands would pull the CLI's
  dependencies into a module that serves a board.

  **Starting the container again does not work under `RIG_DB_ISOLATE`, and the
  reason is worth knowing before trying to fix it.** Isolation publishes the
  sync service on a port the kernel chooses, and Docker chooses a *new* one
  every time such a container starts — verified: a `--publish 127.0.0.1:55999:3000`
  container keeps 55999 across a stop, a `--publish 127.0.0.1::3000` one does
  not. The proxy's URL is fixed by `electric.New` at boot, so the container comes
  back running, healthy and unreachable from this process. That is why
  `SyncState` carries `upstream`, `published` and `moved`: nothing can be done
  about it here short of restarting the server, and the failure is
  indistinguishable from the outage the switch exists to demonstrate. The pill
  reads "Sync moved" and the strip names both ports.
- **The notification devices and settings have no service layer**, and nothing
  under `api/internal/services/` at all. They are the two notification tables a
  person owns rather than reads, projected as `NotificationDevice` and
  `NotificationSetting` by `notifications: expose: true`; `api/internal/app`
  builds each with `api.NewDefault<Name>Service(repo, api.<Name>Contract{})`,
  which is the documented way to say there are no rules to add.
  `internal/compile` makes both owner-scoped on `account_id`, so reads and
  updates are narrowed before any code here runs, and the create — the one write
  with no row to narrow by — is checked in the generated writer. That check used
  to be a hand-written validator in each of these two directories, the same
  eleven lines twice.

The `/auth/*` screens are worth knowing about before adding another: `web/src/`
already covers sign-in, registration, the picker, reset, invitations (send,
list, withdraw), API keys, a password change, session listing and revocation,
the authentication trail, and switching tenants. Those calls are hand-written in
`web/src/auth/authApi.ts` because they are rig's endpoints rather than this
schema's — the generated client covers the API and stops at `/auth`.

`presence:` is on with nothing but `enabled`, which is why there are two
lines for it — `Presence` on `api.Handlers` and `Presence` on the
`api.Shapes` beside it, both in the one `api.Register` call in `api/internal/app`. The sweeper is `api.Main`'s: it
runs before this application's wiring, because the service it sweeps through is
its own over `app.Pool`.

**None of the shutdown arithmetic is in this directory.**
`api.ShutdownBudget()` is forty-five seconds, and it is forty-five because
`api/internal/generated/api/process.gen.go` adds up the five steps rig registers
for this project's blocks — fifteen for the engine, five each for the live
subscriptions, the trace flush, the sweeper and the auth cache's channel — and
leaves ten for the requests in flight. A sixth block would change the number
without anything here being edited.

`api/main.go` states both halves of the total: `DrainDelay` of two seconds and
`MaxShutdown` of forty-seven, which is the budget plus that delay. Nothing
settles it — `MaxShutdown` is the one field here with no default, because it is
the one that leaves the program, and `terminationGracePeriodSeconds` has to be
read off this struct rather than out of a function call. The delay is inside the
total because `serve` counts it there and because the grace period is wall clock
from the signal, which the delay is spent within.

`serve.Config.Shutdown` is where a deployment disagrees with one of those five
numbers, and `api.Shutdown` — generated beside the budget — is what fills it: a
field per step this project registers, which here is all five, so a step this
server does not have is one there is no way to write a number for. A number in
it replaces the limit its step was registered with. It does not impose one on a
step that has none, which is what leaves the engine's drain — the half that
stops it claiming, registered with no limit on purpose — bounded by what is
left of the total rather than by `Notifications`. And it does not raise
`MaxShutdown`: the literal above is still the literal, so a step sized past it
is a process that refuses to start and names what no longer fits.
`api.Shutdown{...}.Budget()` is the total with the new numbers in it, for a main
that would rather compute the sum than keep two of them in step by hand.

Nothing in `api/main.go` fills it, and that is the ordinary case: the generated
numbers are rig's answer to what each step costs, and this example has no reason
to disagree with them. It is a `serve.Config` field rather than a `rig.yaml` key
for the reason the addresses are — the same build runs where
`terminationGracePeriodSeconds` is thirty seconds and where it is five, so it is
a number an environment can supply.

`serve.App` adds up every step actually registered, before the server listens,
and refuses a budget that cannot hold them with the parts named — so a literal
left stale by a new block is a process that will not start and says why. A wrong
number that fails loudly at boot is worth more than a right one nobody can read.
`api.Main` says it earlier still, before a database is opened: it compares the
literal against what this project's blocks add up to and warns when the literal
is behind. Warns rather than refuses, because a step whose `api.Parts` field a
build returns nil for is counted on that side and registered on neither — so a
smaller number is sometimes exactly right, and `serve` is the one that knows.

`api.Parts` is what `app.New` returns, and its three fields are the whole of what
outlives a request here: the handler, the engine and the auth foundation. The
live subscriptions are the one that is not a field, and that is the point of
where they went instead — `api.Shapes` on the `Handlers` literal mounts the shape
routes and registers their drain in the same call, so the proxy is named once
rather than once to serve it and once so the shutdown would know. A live
subscription is a request the server is deliberately not answering yet, so
`http.Server.Shutdown` waits for it and nothing else can end it: one open board
would otherwise spend the entire budget, and the three steps after it would each
find a deadline that had already passed.

`app.Config` is the other side of that call, and it is why the constructor takes
a struct: the server fills all of it, `dispatch-notifications` fills a pool and a
logger, and `api/integration/` fills those plus an `App` whose ending it runs itself.
`ElectricURL` is a field there rather than an `os.Getenv` inside `New`, so the
sync service is decided in `api/main.go` beside `DatabaseURL` and `Addr` — and an
empty one builds no proxy, which is what the cron entry wants.

`tracing:` and `monitoring:` are on, which is why `api.Main` builds a process at
all: the log sink, the provider and the page have to exist before the
`serve.Config` that two of its fields come out of, and the page listens on
`127.0.0.1:9084`, its own port beside the API's 8084. That ordering is what used
to make `api.NewProcess()` a line in `api/main.go` *before* `serve.Main`, holding a
value across both ends of it. `Configure` fills `Monitor`, `MonitorAddr` and
`OnExit`; `Process.Mount` registers the flush and says which half of the page is
unarmed; `OnExit` is `Process.Close`, the same flush for the path a `Tasks:`
entry takes, which never reaches the closure at all — and for the three paths
that end in `os.Exit`, where a `defer` would not have run. `app.Config` still
carries a `*observe.Page` — the one `api.Main` hands the build function — but
only so `/_demo/tour` can say where the page is; nil from a task, since a cron
entry serving a page nobody can reach is not worth the wiring. The password and the two
file paths are set by `make demo` into a gitignored `.run/`, never by rig.yaml,
which is checked in — the address is the one part of it that *is* in rig.yaml,
because who can reach the page is a decision worth reading off the file.

The auth foundation's tables came from `rig setup-project` (migrations 1–7),
and `rig_account` is exposed read-only as the `Account` resource — the board's
member list. The seed task (`go run . seed`, from `api/`) is `app.Seed` in
`api/internal/app/seed.go`, and the fixed identifiers at the top of it are what the
docker tests and the README sign in with.

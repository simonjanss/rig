# rig.yaml

The project file. It marks the root of a rig project and configures everything
that is not per-table.

rig commands work from anywhere inside your project — discovery walks up from the
working directory until it finds this file. Every path in it resolves relative to
**the file's own directory**, not to where you ran the command, so the same
configuration means the same thing wherever it is run from.

Your editor can complete and validate it. `rig init` writes the directive, and
`rig schema` writes the file it points at:

```yaml
# yaml-language-server: $schema=.rig/rig.schema.json
version: 1
```

That is the same schema `rig validate` uses, so what your editor accepts and
what rig accepts cannot drift apart.

A minimal file is short — everything below has a default:

```yaml
version: 1

project:
  name: todo
  module: github.com/you/todo
```

---

## `project`

Required. Identity of the application.

```yaml
project:
  name: todo
  module: github.com/you/todo
```

| Key | |
|---|---|
| `name` | Short name of the application. Also the default for `api.name` and the container name. |
| `module` | Go module path. Every generated import path is built from it, so getting it wrong means generated code that does not compile. |

---

## `layout`

Where a table's configuration file and your service code live.

```yaml
layout:
  table_dir: services/{table}
  config_file: "{table_dir}/{table}.yaml"
```

| Key | Default |
|---|---|
| `table_dir` | `services/{table}` |
| `config_file` | `{table_dir}/{table}.yaml` |

Templates take `{table}` for the snake_case name, `{Table}` for PascalCase, and
`{tables}` for the plural. One of the two keys has to name a table somewhere,
or every table would share a single configuration file.

The default puts a table's configuration next to the service code that
implements it, so everything about one table is in one directory. A flat layout
is a one-line change:

```yaml
layout:
  config_file: rig/{table}.yaml
```

---

## `api`

The shape of the generated HTTP surface.

```yaml
api:
  version: v1
  base_path: /api/v1
  permissions: derived
```

| Key | Default | |
|---|---|---|
| `name` | `project.name` | Used in documentation and generated type prefixes. |
| `version` | `v1` | Version segment. |
| `base_path` | `/api/{version}` | Prefix every route sits under. |
| `description` | — | What this API is for. |
| `permissions` | `derived` | `derived` or `none`. See below. |
| `search_method` | `both` | `query`, `post`, or `both`. |

### `permissions`

**`derived`** — the default — gives every endpoint a permission taken from its
resource and operation (`todo.read`, `todo.write`, `todo.delete`) and generates
the check. An authenticated caller holding no grants then reaches nothing.

That is the right posture, and it is a real behaviour change for a project that
had no authorization: turn it on and every request starts failing until somebody
is granted something.

**`none`** generates no checks at all. It is for a project with no authorization
— a demonstration, or an API sitting behind something else that does the
deciding. It is deliberately the thing you have to write down, because being
unprotected should be a decision somebody made rather than a default nobody
noticed.

`public:` on a resource or an endpoint is the per-endpoint escape hatch, and
works either way.

### `search_method`

`Search` is a read with a body, so `QUERY` is the correct method for it. Some
proxies and CDNs still reject methods they do not recognize, so rig also exposes
it as `POST` to a `/_search` sub-path.

`both` — the default — offers `QUERY` with the `POST` alias. `query` or `post`
picks one.

---

## `database`

Where rig runs your migrations and reads your schema from.

With no `url`, rig starts a throwaway container, applies the migrations, reads
the schema back, and leaves the container running for the next command.

```yaml
database:
  image: postgres:17-alpine
  port: 55440
```

| Key | Default | |
|---|---|---|
| `image` | `postgres:17-alpine` | Container image. |
| `container_name` | `{project.name}-db` | |
| `port` | `55432` | Host port. Set it explicitly if you run more than one rig project. |
| `name` | `rig` | Database name. |
| `user` | `rig` | |
| `password` | `rig` | This is a local throwaway database. Do not put a real secret here. |
| `schema` | `public` | Postgres schema to read. |
| `settings` | — | Extra server parameters, passed as `-c name=value` flags. |
| `electric` | — | A sync-service container managed beside the database. See below. |
| `url` | — | Connection URL of a database you manage. Set it and no container is started. |

Set `url` to point at a database you manage instead — which is what CI does,
where a service container is already running and starting another one would be
wasteful.

A `url` you wrote is used exactly as written. rig does not append parameters to
it, because quietly editing a connection string is a good way to break one that
already carries its own.

### `database.electric`

A project doing [live sync](electric.md) needs an ElectricSQL service following
its database. This block makes that `rig db up`'s job:

```yaml
database:
  port: 55440
  electric:
    enabled: true
```

| Key | Default | |
|---|---|---|
| `enabled` | `false` | Start the sync service beside the database. |
| `image` | `electricsql/electric:1.6.9` | Pinned, because live sync is exactly where a version skew looks like an application bug. |
| `container_name` | `{project.name}-electric` | |
| `port` | `55433` | Host port the sync service answers shapes on. The `electric` generator's `electric_url` option should agree with it. |

Enabling it also adds `wal_level=logical` to `settings` when nothing lists one —
logical replication is how the sync service follows changes, and it cannot be
turned on after the server has started. A container started without it is
replaced on the next `rig db up`, not adapted.

It is nested under `database` because its lifecycle is the database container's:
`up` starts it, `down` stops it, `reset` removes and rebuilds it. It needs the
managed container — with `url` set, rig runs no sync service, because it cannot
promise anything about a database it does not manage. This block is also a
different thing from the per-table `electric:` key ([tables.md](tables.md)),
which says a table has shapes, and from the generator's `electric_url` option
([generators.md](generators.md)), which says where the proxy forwards.
[electric.md](electric.md#running-electric-alongside-your-application) puts the
three together — and covers `server-go`'s `electric_required`, which decides
whether a sync service that is not answering is a line in the log at boot or a
refusal to start.

---

## `migrations`

```yaml
migrations:
  dir: migrations
  foundation: vendored
```

| Key | Default | |
|---|---|---|
| `dir` | `migrations` | Directory holding your goose migration files. |
| `table` | `rig_migrations` | The bookkeeping table. |
| `foundation` | `vendored` | Who keeps rig's own migrations. `vendored` or `embedded`. |

`rig db up` and a binary migrating itself have to agree about which migrations
have run, so a project that changes `table` passes the same name to
`migrate.Options` — two bookkeeping tables mean the second reader thinks nothing
has been applied.

### Who keeps rig's migrations

rig owns about a dozen tables of its own — identities, tokens, API keys, the
authentication log, file rows, notifications — and their DDL has to reach your
database somehow. `foundation` is who keeps the file.

**`vendored`, the default.** `rig setup-project` copies rig's migrations into your
`migrations/` directory, numbered into your own sequence and recorded in your own
bookkeeping table. You can read them, review them in a pull request, and see the
whole of your schema in one place.

That last part is the reason it is the default. Your migrations directory is the
complete truth about your database, and somebody debugging at three in the morning
does not have to know which version of rig is installed to read it.

Upgrades arrive as new files. rig's migrations are append-only — a shipped one is
never renumbered or rewritten, because somebody's database has already run it — so
when a newer rig has more of them, `rig setup-project` writes the ones you do not
have at the next free number and leaves the rest alone.

**`embedded`.** rig's migrations stay in the modules that own them — `rig/auth`,
`rig/files`, `rig/notify` — and each applies and records its own. Your migrations
directory holds only your own schema.

```yaml
migrations:
  foundation: embedded
```

What you get is a repository without a thousand lines of somebody else's SQL in it,
and upgrades that are a `go get` rather than a file to copy. What you give up is
the property above: your migrations directory no longer says what is in your
database on its own.

`rig setup-project` then writes no SQL. Turn on the blocks you want — `auth:`,
`files:`, `notifications:` — and `rig db up` applies the sets those blocks need,
from the modules, before your own migrations.

Your application has to apply them too, and rig generates the wiring for that:

```go
srcs := api.MigrationSources(migrate.Source{
    Name:  "myapp",
    FS:    migrations,
    Dir:   "migrations",     // migrations.dir
    Table: "rig_migrations", // migrations.table
})

serve.Config{
    Migrate: migrate.RequireAll(srcs, migrate.Options{}),
}
```

`api.MigrationSources` is generated into your API package, and only for a project
in this mode — so a vendored project's module never depends on goose. It returns
the module sets in the order they must be applied, with yours last. That order is
not cosmetic: rig's DDL never references a table you created, while yours
routinely references rig's — a join table pointing at `rig_notification`, a file
column pointing at `rig_file`.

`dir` and `table` go on the `Source`, not on `migrate.Options`. They are per-set
facts, so `RequireAll` and `ApplyAll` read them from each `Source` and ignore the
ones on `Options` — which is the note above about `table` restated for this mode:
set it in the wrong place and your own set records itself in `rig_migrations` while
`rig db up` used the name you configured, and the server refuses to start saying
the database is behind.

Each set records itself in a table of its own — `rig_auth_migrations`,
`rig_files_migrations`, `rig_notify_migrations`. They are separate because those
modules are released separately: each numbers its migrations from one, and a shared
table would make two of them collide on a version the first time both shipped a
migration in the same release.

A set is applied whole, because goose reads a directory. So `auth:` with no
provider configured still creates `rig_identity_oauth`, and `notifications:` in a
project with no `auth:` block still creates all of auth's tables — `rig_account` is
what an inbox line names. They are rig's tables either way: rig generates nothing
for them and does not ask you to.

### Pick it once

The two modes record what they applied in different places. A project that
switches after it has a database finds the new mode's bookkeeping empty, re-applies
a schema that is already there, and fails partway through `rig db up` on a table
that already exists.

So `rig validate` refuses a mode that contradicts your migrations directory
(**RIG3004**) rather than letting you find out in psql. There is no adopt step: to
change the mode, start from an empty database.

`auth.own` and `embedded` are refused together under the same code. `own` says you
forked rig's migrations and maintain those tables yourself; `embedded` says the
modules do. Whichever rig believed, the other would be silently ignored — so it
believes neither and says so.

---

## `naming`

How database names become Go and JSON names.

```yaml
naming:
  json_case: camel
  initialisms: [SCB, ACME]
  plurals:
    person: people
```

**`json_case` reaches your tables and not rig's.** The Go for `rig_notification`
and the four tables around it is compiled once, in `rig/notify`, and the Go for
`rig_account` in `rig/authmodel`, so their struct tags are fixed — and a Go
struct tag cannot be parameterised. A project that asks for `snake` gets it on
its own resources and camelCase on rig's, which is the same trade `/auth/*` and
`/presence` have always made. `rig check` reports it — RIG3260, once per exposed
table — rather than leaving it to be found in a response body, and setting
`camel`, the default, is what makes the question go away.

| Key | Default | |
|---|---|---|
| `json_case` | `camel` | `camel`, `pascal`, or `snake`. The shape of generated JSON keys. It shapes the keys rig *generates*: the `/auth/*` endpoints come from a hand-written module shared by every project and answer camelCase whatever this says. |
| `initialisms` | — | Extra acronyms that stay uppercase in Go identifiers. Added to the built-in list. |
| `plurals` | — | Plural overrides keyed by table name, for the words English inflection gets wrong. |

---

## `validate`

The severity of each convention rule. Every value is `off`, `warn`, or `error`.

```yaml
validate:
  unmentioned_column: warn
  missing_comment: error
  fk_needs_index: error
  tenant_id_leading_index: error
  boolean_prefix: warn
```

| Key | What it catches |
|---|---|
| `unmentioned_column` | A column exists in the database but is not mentioned in its table configuration |
| `missing_comment` | A table or column has no comment |
| `fk_needs_index` | A foreign-key column is not covered by an index |
| `tenant_id_leading_index` | No index leads with the tenant column |
| `boolean_prefix` | A boolean column does not read as a predicate |
| `timestamp_suffix` | A timestamp column is not named `_at`, or an `_at` column is not a `timestamptz` |
| `date_suffix` | A date column is not named `_date`, or a `_date` column is not a `date` |
| `fk_naming` | A foreign-key column is not named after the table it references, with or without the `rig_` prefix |
| `cascade_delete` | A foreign key declares `ON DELETE CASCADE` |
| `migration_filename` | A migration file is not named `NNNNN_snake_case.sql` |

Structural rules are not listed here. A schema that breaks one of those — no
primary key, a partial snapshot triple — cannot be generated from at all, so
there is no severity to set.

`rig validate --strict` treats every warning as a failure, which is what you
want in CI. A warning nobody ever fails on is a warning nobody ever fixes.

---

## `generators`

Which generators to run, in order, and how to configure each.

```yaml
generators:
  - name: model-go
    out_dir: internal/model
    options:
      package: model

  - name: persist-go
    out_dir: internal/store
    options:
      package: store
      model_import: github.com/you/todo/internal/model
```

| Key | |
|---|---|
| `name` | A registered generator name. `rig generators` lists them. |
| `out_dir` | Output directory, relative to the project root. |
| `options` | Generator-specific. Each generator publishes its own schema for this block. |

Order matters only in that the model must be generated before the layers that
import it, which is why `rig init` lists it first.

See [generators.md](generators.md) for what each one accepts.

---

## `auth`

The authentication foundation: sessions, API keys, OAuth, rate limits, password
policy. It is off by default, and large enough to have its own page.

```yaml
auth:
  enabled: true
  allow_registration: true
  tenant:
    from: [host]
```

Everything with a fixed answer lives here rather than in a Go literal, because
the generated documentation and the client libraries read this file: a token
lifetime written in Go is a lifetime nothing else can quote.

See **[auth.md](auth.md)** for the whole block.

Two keys from it are worth knowing even if you never turn authentication on,
because they affect code generation:

| Key | |
|---|---|
| `expose` | Foundation tables (`rig_account`, …) to generate a model, repository, and API for anyway — for an administration screen. |
| `own` | Generate for every foundation table, for a project that has forked the schema and no longer imports `rig/auth`. Also stops rig reserving the `rig_` prefix and the names its tables project to ([schema.md](schema.md#names-rig-reserves)). A one-way door. |

And one that writes a subcommand:

| Key | |
|---|---|
| `log_retention` | How long an authentication log entry is kept, for example `90d`. Absent keeps everything. Setting it writes an `AuthLogPruner` task into your API package, and refuses a window shorter than the longest rate-limit window — those limits are counted from that table ([auth.md](auth.md#retention)). |

---

## The whole file

From [examples/todo](../examples/todo/rig.yaml), lightly trimmed:

```yaml
# yaml-language-server: $schema=.rig/rig.schema.json
version: 1

project:
  name: todo
  module: github.com/simonjanss/rig/examples/todo

api:
  version: v1
  base_path: /api/v1
  permissions: none

database:
  image: postgres:17-alpine
  port: 55440

layout:
  table_dir: services/{table}
  config_file: "{table_dir}/{table}.yaml"

validate:
  unmentioned_column: warn
  missing_comment: error
  fk_needs_index: error
  tenant_id_leading_index: error
  boolean_prefix: warn

generators:
  - name: model-go
    out_dir: internal/model
    options:
      package: model

  - name: persist-go
    out_dir: internal/store
    options:
      package: store
      model_import: github.com/simonjanss/rig/examples/todo/internal/model

  - name: service-go
    out_dir: internal/api
    options:
      package: api
      model_import: github.com/simonjanss/rig/examples/todo/internal/model
      store_import: github.com/simonjanss/rig/examples/todo/internal/store
      api_import: github.com/simonjanss/rig/examples/todo/internal/api
      stub_dir: services/{table}

  - name: server-go
    out_dir: internal/api
    options:
      package: api
      model_import: github.com/simonjanss/rig/examples/todo/internal/model
```

## Next

- [tables.md](tables.md) — the per-table file
- [generators.md](generators.md) — what each generator's `options` accepts
- [auth.md](auth.md) — the `auth:` block

## `files`

Uploads. Off by default, and what makes `server-go` write the wiring at all — a
project without this block carries no blob store and no multipart reader.

```yaml
files:
  enabled: true
  backend: memory       # memory or s3
  max_bytes: 5242880    # one upload, 25 MiB by default
  abandoned_after: 24h  # how long a row with no bytes is left alone
  restore_window: 720h  # how long a deleted file stays restorable
  inline_types: [image/png, image/jpeg]
  expose: false         # project rig_file as a read-only resource
  cookie_downloads: false
```

The block does nothing on its own: a file appears when a table has a
`<role>_file_id` column pointing at `rig_file`. See
[schema.md](schema.md#files).

`max_bytes` is a hard per-file cap and not a quota — rig does not do storage
quotas, and saying so is better than implying one. `inline_types` is the short
list served without an attachment disposition; everything else downloads,
because a file served inline from the API origin runs there.

`restore_window` is how long a deleted file stays restorable **and** how long its
bytes are kept, which is why it lives here rather than in a table configuration:
`rig_file` does not have one, and a second copy of this number could only
disagree with the first. If the bucket has a lifecycle rule, that rule has to
outlive this window, or a restore inside it hands back a row pointing at nothing.

The two sweeper intervals are read by `<binary> sweep-files`, which is a
subcommand rather than a goroutine so it is a cron job rather than something
racing itself in every replica. It has two rules — abandoned uploads, and trash
past the window — and no third: finding unreferenced files means enumerating
every foreign key pointing at `rig_file`, and the failure mode of getting that
wrong is deleting somebody's data.

> **`backend: s3` has not shipped.** The adapter is a module of its own, and
> `rig generate` refuses the setting rather than writing wiring that would keep
> every upload in a map a restart empties.

## `notifications`

The inbox. Off by default, and what makes `server-go` write the engine, the
dispatch task and the routes at all — a project without this block carries none
of it.

```yaml
notifications:
  enabled: true
  expose: false             # also project the inbox as a generated resource
  default_digest: Immediate # what an account with no setting gets
  claim_ttl: 5m             # how long a dispatcher's claim is honoured
  send_timeout: 30s         # how long one call into a channel may take
  max_attempts: 14
  backoff_base: 1m          # doubling: 1m, 2m, 4m, 8m, 16m, 32m
  backoff_cap: 1h           # and hourly after that
  retention: 2160h          # 90 days, and it has to outlive the longest digest
```

`enabled` needs the migrations behind it (`rig setup-project` writes them) and
the tenancy tables they depend on, because a notification is addressed to an
account. It does **not** need the `auth:` block: where the claims naming that
account come from is not this block's business.

The block does nothing on its own. A table becomes notifiable by being joined to
`rig_notification`, and then its service layer owes two methods — see
[notifications.md](notifications.md).

`expose` is the second answer rather than the only one. Without it the inbox is
served by the hand-written routes under `/notifications`, which is what most
applications show in a bell icon; with it, `rig_notification_recipient` is also
projected as a resource and gets the filter grammar, the sort keys and a typed
client — and a generated model and repository with them. Both stay, and the
difference between them is the point. Without `expose` there is no generated Go
over these tables at all: `rig/notify` reads and writes them itself.

`claim_ttl` and `send_timeout` are the two numbers here worth understanding
before deploying, and they are one decision. A dispatcher claims a delivery,
sends it outside any transaction, and marks it — the claim exists so that a
process which died between the second step and the third does not strand the row.
So `send_timeout` has to be **shorter than `claim_ttl`**: a send still running
when its own lease expires is a send whose row another dispatcher has already
taken, and at-least-once stops being about crashes and becomes ordinary load.
rig refuses the pair rather than explaining it, and refuses a `claim_ttl` under a
minute outright.

`send_timeout` is the deadline on the context your sender is handed, and it is
cooperative — a sender that ignores it hangs its dispatcher anyway. See
[notifications.md](notifications.md) for what that costs. Raise it for a provider
that is legitimately slow, and raise `claim_ttl` with it. Under a second is
refused rather than rounded: these numbers are carried to your application in
whole seconds, and `500ms` would arrive as no value at all.

A dispatch pass takes what it can send inside one lease and hands the rest back,
so a `send_timeout` that no longer fits the batch shows up as a count in the
dispatch log — `abandoned` — rather than as messages sent twice.

`max_attempts`, `backoff_base` and `backoff_cap` are one decision, and the thing
to read off them is the total: the defaults span **about eight hours**, doubling
from a minute to an hour and then knocking hourly. An outage is measured in hours
by everyone who has had one, which is why the number to tune is the window rather
than the count — `max_attempts` on its own cannot express "outlast a bad morning",
because the elapsed time is exponential in it.

`backoff_cap` is what makes a long count safe: doubling from a minute fourteen
times is five days, so without a ceiling the late attempts are not retries but a
row nobody will look at again. rig refuses a `backoff_cap` below `backoff_base`,
because the cap would then bind before the first doubling and nothing
`backoff_base` claims would be true — a schedule that reads as exponential and
behaves as fixed. Equal is allowed: a flat schedule stated outright is a choice,
not an accident.

Each wait is spread upward by up to half itself, so `backoff_base` is a floor
rather than an exact delay. Without that, one provider refusing one pass of a
hundred rows gets a hundred simultaneous retries a minute later, on every replica
at once. A sender can also say "do not retry this at all" or "come back in ten
minutes" — see [notifications.md](notifications.md#when-a-send-fails), and note
that the first of those is what makes eight hours of attempts affordable.

> **These defaults changed.** `max_attempts` was `5` with no cap, which spanned
> thirty-one minutes. A project that set neither value gets the longer schedule
> on its next `rig generate`, and a genuinely undeliverable message now occupies a
> row for about eight hours rather than half an hour.

`retention` prunes read-and-dismissed inbox lines, their copies, and the
notifications left with nothing pointing at them — in the same task that
dispatches, the way the file sweeper's two rules share one. It has to outlive the
longest digest window: a weekly digest under a daily retention is assembled from
rows that were already pruned, and presents as "the weekly mail is sometimes
empty" rather than as a configuration error.

rig ships **no transport**. Channels are an interface an application implements,
for the reason the mail notifier already gives — see
[notifications.md](notifications.md#delivery).

## `presence`

Who is here, and what they are looking at. Off by default, and what makes
`server-go` write the service, the sweeper and the routes at all — a project
without this block carries none of it, and does not name the module.

```yaml
presence:
  enabled: true
  expose: false     # also project rig_presence as a read-only resource
  ttl: 1m           # how long a session stays present after its last heartbeat
  heartbeat: 20s    # how often a browser confirms it is there
  sweep: 1m         # how often the in-process sweeper ticks
  grace: 5m         # how long past the TTL a row survives before deletion
```

`enabled` needs the migrations behind it (`rig setup-project` writes them) and
the tenancy tables they depend on, because a presence row names an account and a
tenant. Like `notifications:` it does **not** need the `auth:` block.

**`heartbeat` is a write rate, not a latency knob.** Every beat is one row
changed and every subscriber to the presence shape hears about it, so lowering it
multiplies traffic by the number of people present. The arithmetic is in
[presence.md](presence.md#scope-and-what-presence-costs); read it before
changing this number.

`ttl` has to be at least three times `heartbeat`, and rig refuses the pair
otherwise. Three beats is the floor because each one covers a different failure:
a garbage-collection pause, a slow network, and the request that was actually
lost. At two, an ordinary hiccup makes somebody vanish from everybody else's
screen. Under fifteen seconds is refused outright — presence that flickers on an
ordinary mobile connection presents as a broken feature rather than as a number
somebody chose.

`ttl` and `heartbeat` are the two values here **answered to the browser**, on
every heartbeat, rather than compiled into the front end. So changing either is a
deploy of your server and not a release of your client, and there is no copy of
the number in the browser to disagree with this one.

`grace` is what stops the two halves of expiry contradicting each other. A
subscriber stops drawing a row at `ttl`; the sweeper deletes it at `ttl + grace`.
A row is therefore always invisible before it is gone, never the other way round
— which would be a row that came back when a slow client caught up.

`sweep` is how often the in-process sweeper ticks. `api.Main` starts it for a
project with this block, and `sweep-presence` is a subcommand for an operator who
would rather it were a cron entry — running both is not a mistake, since deleting
an already-expired row is idempotent. A sweep faster than
`ttl` is a warning rather than a refusal: it works, it just spends deletes on
rows every subscriber had already stopped drawing.

`expose` is the second answer again. Without it presence is written through the
hand-written routes under `/presence` and read over its live shape, which is all
a front end needs — and no model or repository is generated for it, because
`rig/presence` is what writes the rows. With it, `rig_presence` is also
projected as a read-only `Get`/`List` resource, model and repository included.

## `throttle`

How many API calls one caller may make. Off by default, and what makes
`server-go` write the check into every route at all — a project without this
block carries no limiter and writes to no counters.

```yaml
throttle:
  enabled: true
  api_key: {max: 5000, window: 1m}    # per API key
  account: {max: 1000, window: 1m}    # per signed-in account
  tenant:  {max: 10000, window: 1m}   # per tenant, across all their accounts
  ip:      {max: 300, window: 1m}     # per address, for callers who are not signed in
  interval: 1s                        # how long a replica counts before publishing

  routes:                             # extra limits on particular routes
    - pattern: POST /api/v1/todos
      max: 60
      window: 1m

  exempt:                             # and routes nothing applies to
    - GET /api/v1/todos/{id}/live_stream
```

**This is fair-use limiting and not a defence against a flood.** A request that
reaches your handler has already cost a connection, a TLS handshake and a
goroutine; under a volumetric attack an application-level limiter is more load on
the thing that fails first, not less. Volumetric defence belongs in front of the
application — a CDN, an L7 proxy, or your provider's WAF. What this block does
buy is real and is what most people mean: a client stuck in a retry loop that
would otherwise drink the connection pool, one tenant's batch job crowding out
every other tenant, scraping and enumeration, and cost control on an API that
fans out to something metered.

The four per-caller limits are a ladder — machines, then people, then strangers.
An integration is allowed the most because that is what an integration is for; a
person clicking cannot reach a thousand a minute without something being wrong;
and an address nobody has authenticated gets the least, because addresses are the
cheapest identity there is. `tenant` sits above all of them and is a different
question: it is what stops one customer crowding out the rest, not what stops one
of their users.

A call is counted on one rung of that ladder and not several. A request made with
an API key spends `api_key` and not also the `account` the key acts as — the
tightest limit is the one that decides, so counting both would put every
integration under the account number and leave `api_key` decorative. `tenant` is
the exception and applies on top, because it is the different question.

**`ip` applies only to callers who are not signed in.** Once a request carries an
identity, that identity is the better key: an office behind one NAT is one
address, and a phone is a different address every few minutes. Leaving out `ip`
entirely means anonymous routes are unlimited.

`ip` also depends on [`auth.trusted_proxies`](#auth). Behind a load balancer
every request arrives from the balancer, so with no trusted list the per-address
limit is one budget for the entire internet. With one, `X-Forwarded-For` is
believed — but only from a peer inside it, because an address read from a header
the client controls is an address the client chooses.

**`interval` is the accuracy of every number above, stated as time.** Each
replica counts locally and publishes to the database on this interval,
reconciling sooner as a caller approaches their limit. That is not an
optimisation you can turn off by setting it to zero and forget: without it, every
API call is a write to a single contended row, and the limiter becomes the
bottleneck at exactly the traffic it was added for. The cost is that the limit is
approximate — several replicas can collectively miss up to one interval of
traffic each. A caller who is already over their limit is refused from memory, so
an attack costs at most one write per interval however fast it arrives.

`routes` are extra budgets on top of the per-caller ones, counted against the
same caller. A route listed here must give both `max` and `window`: unlike the
per-caller limits there is no default to fall back on, and a route somebody named
with no numbers is one they meant something by and did not say.

Patterns are matched against the route pattern `net/http` reports for the route
it dispatched — `POST /api/v1/todos`, including the method and the `{id}`
placeholders — and not against the request path. rig refuses a pattern with no
method or with a `*` in it.

`exempt` wins over `routes`, and rig refuses a pattern listed in both. Streaming
endpoints belong here: a live-sync connection is one long-lived request, so
counting it per request means an idle client and a reconnect loop look the same.

**The limiter fails open.** If the counters cannot be reached the request is
served and a warning is logged. This is the opposite of what the
[auth limits](auth.md) do, and deliberately: a login limiter that failed open
would be a credential-stuffing window held open by anybody who can make your
database slow, while an API limiter that failed closed would turn a database blip
into the outage it exists to prevent. The trade is worth knowing — somebody who
can degrade your database can also switch this off.

Refused calls get `429` with `Retry-After`, and every call gets `RateLimit-Limit`
and `RateLimit-Remaining` so a well-behaved client can slow down before it is
refused. Both SDKs honour all three — see
[clients.md](clients.md#seeing-the-limit-before-you-hit-it).

**The counters need sweeping.** They live in `rig_throttle`, which `rig
setup-project` writes, and it gains a row per caller per window — for the address
limit, a row per address per minute. `api.Main` merges in a `sweep-throttle`
subcommand that deletes the dead ones — turning this block on is what puts it
there, the same way `files:` puts `sweep-files` there:

```go
Tasks: map[string]serve.Task{ /* yours */ },
```

Nothing schedules it for you. Zero means twice the longest window you configured,
which is as far back as anything counts; unlike the auth log there is no lockout
to preserve here, so a bucket past that point cannot free a caller who is still
over their limit — deleting it is free.

## `cache`

Whether rig holds reads in memory between requests. Off by default.

```yaml
cache:
  enabled: true
  ttl: 30s                  # the backstop, not the guarantee
  channel: rig_cache        # the Postgres channel invalidations travel on
  max_entries: 50000        # per cache, before the whole map is dropped
```

Two kinds of read are covered and they are worth telling apart, because rig owns
one of them completely and the other one only if you let it.

**The reads rig makes for itself**, on behalf of every caller: resolving a session
token, resolving an API key, and — before that second one — the failure limit that
stops somebody grinding secrets against a key id. All three are a row read on
every authenticated request, for answers that change when somebody signs out or
gets their key wrong. They come with an [`auth`](#auth) block and there is nothing
else to say: rig makes the read and rig makes every write that invalidates it.

**The read rig makes for one of your tables**, which is `cache: true` in that
table's own configuration file — see [tables.md](tables.md#cache). That covers
`Get`, the lookup by identifier behind every `GET /resource/{id}`, and it is
opt-in per table because it is a promise as much as a setting. The promise is
below.

Either one is enough on its own. A block with neither an `auth:` block nor a
cached table is refused when the project is compiled rather than left as four
numbers nothing looks at.

**It is not a time-to-live over authentication.** That would be a revoked session
that keeps working, and rig does not offer it. Every revocation rig performs
publishes a Postgres `NOTIFY` **inside the transaction that performed it** — so
the invalidation is delivered exactly when that transaction commits, discarded if
it rolls back, and reaches every replica listening on `channel`. A session ended
on one replica stops working on all of them at the moment the revocation commits,
not when a timer runs out.

`ttl` is what remains for a replica that was not listening at that moment. It is
the honest cost of the block, stated as time: with no invalidation arriving, a
change takes effect this long after it was made. And a replica that knows it has
lost the channel does not fall back on `ttl` — it stops caching entirely and
reads through, because serving permissions nobody can withdraw is worse than
serving them slowly. That is the opposite of what [`throttle`](#throttle) does,
and for the opposite reason.

The API-key failure count is held under the same rule with one wrinkle: only the
*zero* is kept. A count can only rise inside its window and every row it counts is
one rig writes, so "no failures for this key" is safe to hold and is withdrawn by
the failure that makes it wrong. A key somebody is already grinding is counted
afresh every time, and the limit refuses on the same attempt it would have with no
cache at all. What stops costing a query is the integration that never gets its
key wrong.

**There is nothing to wire.** No map to build, no publish to add, no hook to
register. rig caches these three reads *because* it owns both halves — it makes
the read and it makes every write that invalidates it — so there is no write
path an application can forget. The shutdown is a field on `api.Parts` rather
than a call, so it is not a line to remember either:

```go
return api.Parts{Handler: mux, Auth: front}, nil
```

### Holding one of your tables: `cache: true`

Beside the three reads above, one read of your own can be held: `Get`, the lookup
by identifier. One of *your own* is the operative part — `rig_file`, the
`rig_notification_*` tables and the rest of rig's own are written by the module
that owns them rather than through a repository, so asking to hold one is refused.
It is asked for per table, in that table's configuration file:

```yaml
# services/todo/todo.yaml
table: todo
cache: true
```

That is the whole of it. The generated repository grows a map, a listener and a
withdrawal on every write it makes to the row, and `store.New` builds and starts
all of it — so there is nothing to call and no order to get right. One line in
your own wiring, and safe to leave out for the reason the auth one was: a
listener that is not running reports itself as not live, and a cache that is not
live reads through. It is a call rather than a field on `api.Parts` because the
store is yours, not rig's:

```go
app.CloseWithin("store", 5*time.Second, repos.Close)
```

**What you are promising by writing it.** rig publishes the withdrawal from the
writes *it* makes, so every write to this table has to go through the generated
repository. Two ways to break that, and both are silent:

```go
repos.Pool().Exec(ctx, "UPDATE todo SET title = $1 WHERE id = $2", ...)  // no
```

and raw SQL against the `tx` a [`dbhook`](services.md) hands you. Either one
moves the row without telling anybody, and every replica holding it goes on
serving the old one until `ttl` expires. A migration, a `psql` session and a
second deployment writing the same table are the same hole from further away.

This is the [`Grants`](#cache) argument applied to a table you own, and it is why
this is a key per table rather than something `cache.enabled` turns on for
everything: rig cannot tell from your schema whether the promise is true, so
nothing but a person can make it.

**When you have to write around it anyway**, the repository has the withdrawal on
it, and calling it is the difference between a promise you can keep and one you can
only break:

```go
repos.Todos.ForgetCached(ctx, row)   // row as it was, inside the writing transaction
```

Nothing that goes through the repository needs this. It is here because rig needed
it first: a [`files`](#files) column is written by the file service, inside the
transaction that finalizes an upload rather than through `Update`, so attaching a
cover to a held row would leave every replica saying it has none — and the download
endpoint answering 404 for something that had just been uploaded. That call is
generated; yours is the same one.

**Only `Get`.** A list, a search and the trash are a query every time, and that is
not an omission to fill in later. What a list returns depends on filters, on
paging and on rows other than the one being asked about, so any write to the table
could change any list — the invalidation would have to drop everything, on every
write, and the entry would be gone before a second caller ever hit it. `Get` is
the one read that is a pure function of a row, which is the property that makes it
cacheable at all.

**Two kinds of `Get` are never held.** One made inside a transaction, because
every write begins by reading the row it is about to change — to snapshot the
previous version, and to judge the change against it — and a held answer there
would put a version that never existed into the history. And one that widened its
scope with [`readopt`](services.md), because reading across tenants or across a
table's owners answers something the key does not describe.

Everything else in this section applies unchanged: the withdrawal is a `NOTIFY`
on the writing transaction, `ttl` is the backstop for a replica that missed one,
and a replica that has lost the channel holds nothing at all.

**The read this deliberately does not cover is your `Grants` function.** It is
the most expensive one on the path — a join over role tables, on every request —
and it is over *your* tables, written to by *your* code. rig cannot see those
writes, so caching that answer would mean publishing your own invalidations, and
one forgotten write path there is a permission you took away that goes on
working.

So it is not a key here. It is `auth.NewGrantsCache`, in Go, next to the writes
you are promising to publish from — three lines of wiring and an `Invalidate` on
each of them. [`docs/auth.md`](auth.md#verifying-without-a-row-read) has the shape
and `examples/auth` is wired that way end to end.

`channel` matters when two deployments share one database and must not share
invalidations. It has to be a plain identifier — letters, digits and
underscores — because it reaches Postgres both inside a `LISTEN` and as a
parameter to `pg_notify`, and both have to name the same channel.

`max_entries` bounds each cache. Past it the whole map is dropped rather than
swept: nothing outlives `ttl` anyway, so reaching the bound costs one window
behaving as though there were no cache — where refusing to store anything new
would leave a process answering forever out of whichever keys arrived first.

## `tracing`

Spans. Off by default, and what makes every generator emit them at all — a
project without this block imports no tracing library and carries no
OpenTelemetry in its `go.mod`.

```yaml
tracing:
  enabled: true
```

That is the whole block, and the two things you might expect beside it are
deliberately elsewhere.

The **service name** is `project.name`. You have already said what this
application is called, and a second name here could disagree with the first —
which is the kind of thing nobody notices until two deployments turn up in a
collector under names neither of them recognises.

**Where the spans go** is not generated. A collector endpoint or a file path is
a property of the deployment, not of the build: the same binary runs on your
laptop, in CI and in production, and only the last of those has anywhere to send
spans. It is `$OTEL_EXPORTER_OTLP_ENDPOINT`, `$RIG_TRACE_FILE`, or a field on
`observe.Config` in your `main`. With none of them set, nothing is recorded and
nothing is exported — see [observability.md](observability.md#where-the-spans-go)
for why that is the useful default rather than a broken one.

Turning it on changes generated code, so `rig generate` has to run: the handlers
open a span each, the repositories open one per call and one per hook, and
`store.Config` grows a `Tracer` field for your `main` to fill in.

## `monitoring`

rig's own page over those spans: the last few hundred requests, what each of
them spent its time on, and the log lines they wrote. Off by default, and off
means there is no listener rather than a port answering 404.

```yaml
monitoring:
  enabled: true
  addr: 127.0.0.1:9090
```

It is a reader over the span file `tracing:` writes and stores nothing of its
own, so **it cannot be turned on without `tracing:`** — rig refuses that
combination (RIG3005) rather than leaving you with a page that is empty forever.

The log half is a run-time arrangement rather than a key here: a sink you open,
tee into your logger and hand to the page. `max_logs` is the one part of it this
block decides. See
[observability.md](observability.md#the-logs).

| Key | Default | |
|---|---|---|
| `enabled` | `false` | Serves the page. Requires `tracing.enabled`. |
| `addr` | — | **Required.** Where the page listens, as `host:port`. `$RIG_MONITOR_ADDR` overrides it at run time. |
| `base_path` | `/_rig/monitor` | Where it is mounted on that listener. Nothing else is on it to collide with; a reverse proxy in front of the port can key on it. |
| `max_traces` | `200` | How many requests the page lists, newest first. |
| `max_logs` | `500` | How many log lines the page reads, newest first. Larger than `max_traces` because one request writes several lines. |
| `password_env` | `RIG_MONITOR_PASSWORD` | The variable the password is read from. |
| `password` | — | The password itself. It warns (RIG3006): rig.yaml is checked in. |
| `allow` | — | Addresses that may reach the page, as CIDR ranges or single addresses. Empty allows any. |

**`addr` has no default, and an enabled block without one is refused
(RIG3009).** The page gets a listener of its own inside the same binary rather
than a route on your API's mux, because the interface a socket is bound to is
the only boundary in front of it that a client cannot talk its way around —
`127.0.0.1:9090` means this machine and nothing else, in every deployment. rig
picks neither half of that for you: a default port is one two rig services on a
host would fight over, and a default interface is a decision about who can reach
a page that lists every path, request id and error cause your server has seen.

`allow` **narrows the password rather than replacing it.** An address that is
not on the list is answered 404, before the password is compared — but there is
no way to have the list without the password, because it reads the connection's
own address and never a forwarded header, and behind a load balancer that means
it matches everything or nothing. That is the case `addr` covers, and why the
two are layers rather than alternatives. See
[observability.md](observability.md#restricting-it-to-an-address).

**The password is not here by default**, and for the reason the collector
endpoint is not: it is a property of the deployment. With nothing in the
variable at run time the page does not listen at all, which is what you want on
a laptop and in CI. Writing one into this file is accepted — a throwaway staging
box and a production deployment are not the same decision — and warned about
once, because the page it guards lists every path, request id and error cause
this server has seen.

Turning it on changes generated code, so `rig generate` has to run: the API
package gains a `Monitoring()` that supplies everything above. It does **not**
add anything to `api.Server` — the page is never on the mux `Register` returns.
See [observability.md](observability.md#the-monitoring-page) for the `main` that
goes with it.

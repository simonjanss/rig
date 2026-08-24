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
three together.

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
| `fk_naming` | A foreign-key column is not named after the table it references |
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
  max_attempts: 5
  backoff_base: 1m          # doubling: 1m, 2m, 4m, 8m, 16m
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
client. Both stay, and the difference between them is the point.

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

`sweep` is how often the in-process sweeper ticks **when it is running**. Whether
it runs at all is a line in your `main.go`, not a value here, the same way the
notification engine is started rather than configured on. A sweep faster than
`ttl` is a warning rather than a refusal: it works, it just spends deletes on
rows every subscriber had already stopped drawing.

`expose` is the second answer again. Without it presence is written through the
hand-written routes under `/presence` and read over its live shape, which is all
a front end needs; with it, `rig_presence` is also projected as a read-only
`Get`/`List` resource.

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
limit, a row per address per minute. `api.ThrottleSweeper(0)` is the `serve.Task`
that deletes the dead ones, and it wants a cron entry the way
`IdempotencyPruner` does:

```go
Tasks: map[string]serve.Task{"sweep-throttle": api.ThrottleSweeper(0)},
```

Nothing schedules it for you. Zero means twice the longest window you configured,
which is as far back as anything counts; unlike the auth log there is no lockout
to preserve here, so a bucket past that point cannot free a caller who is still
over their limit — deleting it is free.

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
them spent its time on, and the log lines they wrote, at `/_rig/monitor`. Off by
default, and off means the route does not exist.

```yaml
monitoring:
  enabled: true
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
| `enabled` | `false` | Mounts the page. Requires `tracing.enabled`. |
| `base_path` | `/_rig/monitor` | Where it is mounted. It cannot sit under `api.base_path` or `auth.base_path`, where it would take a route this project owns. |
| `max_traces` | `200` | How many requests the page lists, newest first. |
| `max_logs` | `500` | How many log lines the page reads, newest first. Larger than `max_traces` because one request writes several lines. |
| `password_env` | `RIG_MONITOR_PASSWORD` | The variable the password is read from. |
| `password` | — | The password itself. It warns (RIG3006): rig.yaml is checked in. |
| `allow` | — | Addresses that may reach the page, as CIDR ranges or single addresses. Empty allows any. |

`allow` **narrows the password rather than replacing it.** An address that is
not on the list is answered 404, before the password is compared — but there is
no way to have the list without the password, because it reads the connection's
own address and never a forwarded header, and behind a load balancer that means
it matches everything or nothing. See
[observability.md](observability.md#restricting-it-to-an-address).

**The password is not here by default**, and for the reason the collector
endpoint is not: it is a property of the deployment. With nothing in the
variable at run time the page is not mounted at all, which is what you want on a
laptop and in CI. Writing one into this file is accepted — a throwaway staging
box and a production deployment are not the same decision — and warned about
once, because the page it guards lists every path, request id and error cause
this server has seen.

Turning it on changes generated code, so `rig generate` has to run: `api.Server`
grows a `Monitor` field and the API package gains a `Monitoring()` that supplies
everything above. See
[observability.md](observability.md#the-monitoring-page) for the three lines of
`main` that go with it.

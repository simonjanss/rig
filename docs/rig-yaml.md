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
| `url` | — | Connection URL of a database you manage. Set it and no container is started. |

Set `url` to point at a database you manage instead — which is what CI does,
where a service container is already running and starting another one would be
wasteful.

A `url` you wrote is used exactly as written. rig does not append parameters to
it, because quietly editing a connection string is a good way to break one that
already carries its own.

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

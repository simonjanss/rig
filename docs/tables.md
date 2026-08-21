# Table configuration

One YAML file per table. Your schema says what exists; this file says what it
means and how much of it belongs on the API.

`rig sync` creates these files and keeps them in step with the database. You
edit them; `rig sync` never rewrites what you wrote — it edits the file in
place, so comments, blank lines, and key order all survive.

Where they live is set by `layout` in [rig.yaml](rig-yaml.md). By default:

```
services/todo/todo.yaml
```

```yaml
# yaml-language-server: $schema=../../.rig/table.schema.json
table: todo
comment: One thing somebody means to do.
```

## What this file cannot do

There is no key for a column's type, its nullability, or its keys. Those are
facts, not choices, and the database already states them. This file can add
documentation, rename what something is called on the wire, forbid an update,
hide a column, or declare an endpoint beyond CRUD — but it can never contradict
the schema.

Everything is keyed by **physical names**: the table, column, and enum label as
spelled in Postgres. API names are derived and can change under you, so keying
on them would move your configuration the first time you renamed a resource.

## New entries arrive with a TODO

When `rig sync` finds a column your file does not mention, it adds it with a
TODO comment — and that fails validation until you say what the column is for.

That is deliberate. A field that reaches your public API before anybody wrote a
sentence about it is how an API ends up documented as a list of names.

---

## Table-level keys

```yaml
table: todo                      # required, must match the file's location
comment: One thing somebody means to do.

resource: Todo                   # API resource name
plural: Todos
path_segment: todos              # the URL segment

operations: [Create, Get, List, Search, Update, Delete]
public: [Get, List]
expose: true

order_by: [-created_at, id]
restore_window_days: 30
```

| Key | |
|---|---|
| `table` | The physical table name. Must match where the file is, so a renamed table is caught rather than silently ignored. |
| `resource` | API resource name. Defaults to the table name in PascalCase. Some names are rig's — see [schema.md](schema.md#names-rig-reserves). |
| `plural` | Defaults to an inflection of the resource name. |
| `path_segment` | URL segment. Defaults to the kebab-case plural. |
| `comment` | What the table is for. Becomes the Go doc comment and the API description. |
| `operations` | Which operations to expose. **Replaces** the default set. |
| `public` | Operations and endpoints that answer without a credential. |
| `expose` | Whether the table appears in the API at all. Defaults to true. |
| `order_by` | Default ordering, most significant first. `-` for descending. |
| `restore_window_days` | Days a soft-deleted row stays restorable. Required when the table has `deleted_at`. |
| `access` | How wide a read reaches by default. |
| `on_delete` | The order the tables referencing this one are told a row is going. |

### `operations`

The default set is `Create, Get, List, Search, Update, Delete`. Naming
`operations` replaces it entirely rather than merging — additive semantics could
not express removing one.

```yaml
# Read-only from the outside; rows arrive through an import job.
operations: [Get, List, Search]
```

### `expose: false`

Keeps the model and the repository, generates no endpoints. This is what a table
like a token store wants: the data layer is worth generating and a REST
interface for it is not.

```yaml
expose: false
```

### `order_by`

```yaml
order_by: [-created_at, id]
```

Newest first, then by identifier. Include something unique as the last term —
without it the order is not total, and two pages of a paginated list can show
you the same row twice or skip one.

### `access`

How wide a read reaches by default. This is the floor, not the ceiling: what a
caller may ask for beyond it is a permission.

```yaml
access:
  scope: own
  owner: account_id     # defaults to created_by_account_id
```

| Value | |
|---|---|
| `tenant` | Every row the tenant owns. The default. |
| `own` | Only the caller's own rows. |

`owner` is which column that means. It defaults to `created_by_account_id`, the
audit column every generated write already stamps, so what a read narrows to and
what a write records are the same fact.

Name another one when the row's owner is not whoever created it — an inbox line
belongs to the person it is addressed to, an assigned task to its assignee. The
column has to be a `uuid` referencing `rig_account` and it has to be `NOT NULL`:
a row with no owner is invisible to every narrow read and nothing reports it,
which is tolerable for an audit column (a row a migration created really does
have nobody behind it) and is not tolerable here.

`own` narrows **reads**, and `?scope=all` widens them for a caller who holds the
`.read.all` permission.

It narrows **writes** too, and there is no widening for those: an owner-scoped
table refuses to change somebody else's row outright. A write is a different
kind of decision from a read, and one flag answering both would be a bad answer
to two questions.

### `on_delete`

When a row here is deleted, every table pointing at it is told, inside the same
transaction, and can refuse — that is a
[parent hook](services.md#when-a-row-you-point-at-is-deleted), and it lives in
the child's service layer because the *action* is the child's.

The *sequence* is this table's, because this is the only place that can see all
its children at once. rig derives one — tables that reference each other are told
outermost-first, which is the order the rows themselves would have to go in — and
this key is for when that is wrong:

```yaml
on_delete:
  order: [fixture, player]   # anything unlisted runs after, in the derived order
```

Physical table names. Naming some of them is the ordinary case: the reason to
write this at all is one pair whose order is wrong, and a list that had to name
every child is a list that silently stops mentioning one.

The order does not affect whether the delete succeeds — everything is one
transaction, so a refusal unwinds every hook before it. It affects what one
sibling can see of another, and which error the caller gets when two of them
would both refuse.

`rig ir` prints the resolved order under each resource's `children`.

---

## `columns`

Keyed by physical column name.

```yaml
columns:
  title:
    comment: What needs doing, in a few words.
  email:
    comment: Where to reach them.
    format: EmailAddress
  slug:
    comment: The URL-safe name, fixed at creation.
    immutable: true
  internal_score:
    exclude: true
  settings:
    comment: Per-user preferences.
    go_type: UserSettings
```

| Key | |
|---|---|
| `comment` | What the column holds. Wins over a Postgres `COMMENT ON`. |
| `field` | API field name. Defaults to the column name in PascalCase. |
| `format` | Narrows a string for documentation and client validation. |
| `go_type` | A named Go type for a `jsonb` column, replacing the generic raw-message type. |
| `operations` | Which of `Create`, `Read`, `Update` this column takes part in. Replaces the default set. |
| `immutable` | Settable on create, never on update — so it is absent from the update input entirely. |
| `read_only` | Never writable through the API. |
| `exclude` | Not exposed at all. |
| `snapshot_ignore` | Keep the live value when a snapshot is restored. |
| `example` | Shown in generated documentation. |

`format` accepts: `EmailAddress`, `URL`, `PhoneNumber`, `Color`, `CountryCode`,
`LanguageCode`, `TimeZone`, `RichText`.

The difference between `immutable` and `read_only` is which door is shut.
`immutable` means you can set it once — a slug, a currency, an account type.
`read_only` means the API never writes it; something else does.

Managed columns (`id`, `tenant_id`, the audit columns, the snapshot triple) are
not listed here at all. See [schema.md](schema.md).

---

## `enums`

Keyed by the Postgres type name. The label stays the value on the wire; only the
generated identifier is yours to name.

```yaml
enums:
  todo_priority:
    name: TodoPriority
    description: How urgently a task wants attention.
    values:
      low:
        name: Low
        description: Worth doing eventually.
      normal:
        name: Normal
        description: Worth doing soon. This is the default.
      high:
        name: High
        description: Worth doing before anything else.
```

---

## `relations`

Keyed by the related table name. The relation itself comes from your foreign
key; this only configures how it appears.

```yaml
relations:
  player:
    name: Players
    embed: true
```

| Key | |
|---|---|
| `name` | The accessor on the resource. |
| `embed` | Include the related rows in read responses. |

---

## `endpoints`

Anything beyond CRUD. rig has no default for one — that is the point of
declaring it — so your service layer implements it, and the build fails until it
does.

```yaml
endpoints:
  - name: Complete
    method: POST
    path: /{id}/_complete
    summary: Mark the task as done.
    description: |
      Completing a task that is already done is a conflict rather than a
      no-op: two people ticking the same box should not both be told they
      were the one who finished it.
    request:
      path_params:
        - name: ID
          type: UUID
          description: The task to complete.
      body:
        - name: Note
          type: String
          optional: true
          description: What was done, appended to the task's notes.
    responses:
      - status: 200
        body_object: Todo
        description: The completed task.
      - status: 409
        body_object: Error
        description: The task was already done.
```

| Key | |
|---|---|
| `name` | Method name on the generated service interface. |
| `method` | `GET`, `POST`, `PUT`, `PATCH`, `DELETE`, or `QUERY`. |
| `path` | Relative to the resource, for example `/{id}/_publish`. |
| `summary`, `description` | Documentation. |
| `permission` | RBAC key required. Empty means a valid session is enough. |
| `public` | Answer without a credential. |
| `request` | What the client sends. |
| `responses` | Every status this endpoint can return. |

`request` takes `path_params`, `query_params`, and either `body` (a list of
fields) or `body_object` (the name of a whole object) — not both. A response
takes `body_object` or `body_fields`, likewise not both.

Each parameter is:

```yaml
- name: Limit        # PascalCase
  type: Int          # a primitive, or the name of an enum or object
  description: How many to return.
  optional: true
  array: false
  default: "50"
```

An endpoint named after a generated one **replaces** it. That is reported as a
note ([RIG4001](diagnostics.md)) so the shadowing is visible rather than
mysterious — it is a legitimate thing to do when the generated `Update` is not
the update you want.

The `_` prefix on a path segment (`/_complete`, `/_search`) is a convention, not
a rule: it keeps action sub-paths from ever colliding with a future field name.

---

## `electric`

Live-sync shape endpoints for this table. rig builds the tenant and lifecycle
predicates; declared params are handed to your own scoping function, which can
only narrow a shape further.

`enabled: true` is the whole configuration. Which shapes exist is decided by
your columns, not by a key here: a soft-deletable table also gets a trash shape,
and one that keeps its previous versions also gets a per-row history shape.

```yaml
electric:
  enabled: true
  auth: tenant
  params:
    since:
      type: Timestamp
      optional: true
      description: Only rows changed after this moment.
```

See [electric.md](electric.md).

---

## A complete file

From [examples/todo](../examples/todo/services/todo/todo.yaml):

```yaml
# yaml-language-server: $schema=../../.rig/table.schema.json
table: todo
comment: One thing somebody means to do.

restore_window_days: 30
order_by: [-created_at, id]

columns:
  title:
    comment: What needs doing, in a few words.
  notes:
    comment: Anything worth remembering that does not fit in the title.
  is_done:
    comment: Whether the task has been completed.
  priority:
    comment: How urgently the task wants attention.
  due_at:
    comment: When the task is due, or null if it never expires.

enums:
  todo_priority:
    name: TodoPriority
    description: How urgently a task wants attention.
    values:
      low: {name: Low, description: Worth doing eventually.}
      normal: {name: Normal, description: Worth doing soon. This is the default.}
      high: {name: High, description: Worth doing before anything else.}
```

## Next

- [schema.md](schema.md) — what the database has to say first
- [api.md](api.md) — the endpoints this produces
- [services.md](services.md) — implementing the endpoints you declared

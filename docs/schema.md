# Your schema

The columns rig recognizes by name, and what having each one turns on. This is
the page to read before you write your first migration.

rig has no schema language. Your migrations are the schema: `rig sync` applies
them to a throwaway Postgres, introspects the result, and works from what it
finds. So a behaviour is declared by **having the column**, not by setting a key
somewhere. There is no `soft_delete: true` — you add `deleted_at`, and the table
is soft-deletable.

That cuts both ways, and it is worth knowing up front: turning a behaviour off
is a migration, not an edit.

## Migrations

Goose format, in `migrations/`, named `NNNNN_snake_case.sql`:

```
migrations/00001_create_todo.sql
migrations/00002_add_todo_snapshots.sql
```

The number is a sequence, not a timestamp. Two people adding a migration on the
same day will collide on the filename — which is the point. A merge conflict is
a much better outcome than two migrations silently interleaving.

`rig migration new <name>` writes the next one for you. With `--table` it
scaffolds a whole table with the conventional columns already in place:

```bash
rig migration new create_todo --table todo --soft-delete
```

`--table` refuses a name under the `rig_` prefix and warns about one that
projects to a resource name rig keeps — see
[Names rig reserves](#names-rig-reserves) — so you find out before the file
exists rather than from the next `rig validate`. A warning rather than a refusal,
because a `resource:` key answers that half of the rule and the table name may be
the one you want.

## The two required columns

Every table rig generates for needs both of these.

```sql
id         uuid PRIMARY KEY,
tenant_id  uuid NOT NULL,
```

**`id uuid`** — a single-column uuid primary key. Not a composite key, not a
bigint. rig refuses anything else ([RIG5001](diagnostics.md),
[RIG5002](diagnostics.md)), because every generated route, every relation, and
every client method is built around a single opaque identifier, and a sequential
integer additionally leaks how many rows you have.

**`tenant_id uuid`** — every generated query filters on it, and the value comes
from the caller's credentials, never from the request. A client cannot ask for
another tenant's rows because there is no parameter that would let it.

If you are building something single-tenant, you still need this column. Use one
fixed uuid. The cost is one column and one index per table; what you buy is that
the day a second customer arrives is not a rewrite.

You also want an index that **leads** with it:

```sql
CREATE INDEX todo_tenant_created_idx ON todo (tenant_id, created_at DESC);
```

This is not an optimization. Every generated query has `tenant_id = $1` in its
WHERE clause, so without a leading-tenant index every read is a full scan. rig
warns about it by default and most projects turn it up to an error
(`tenant_id_leading_index` in [rig.yaml](rig-yaml.md)).

## Audit columns

Optional, and independent — take the pairs you want.

```sql
created_at              timestamptz NOT NULL DEFAULT now(),
created_by_account_id   uuid,
updated_at              timestamptz,
updated_by_account_id   uuid,
deleted_at              timestamptz,
deleted_by_account_id   uuid,
```

rig fills these in on every write. They never appear in a create or update
input, so a client cannot claim to be someone else or backdate a row.

There are three more, for a change that arrived through an API key:

```sql
created_by_api_key_id   uuid,
updated_by_api_key_id   uuid,
deleted_by_api_key_id   uuid,
```

The account columns say *whose* a change was; the key columns say *which
credential* it came through. Both are worth having, because an integration's
account is often a service account shared between several keys: the account
tells you the import did it, and the key tells you which integration to revoke.
A change made by a person signed in normally leaves the key columns null.

## `deleted_at` makes a table soft-deletable

Add `deleted_at` and `DELETE` stops removing rows. It sets the timestamp, and
every generated read excludes the row from then on.

You also get a trash: the row can be listed and restored within a window you
have to state.

```yaml
# services/todo/todo.yaml
restore_window_days: 30
```

This is required on a soft-deletable table and rejected on any other
([RIG5030](diagnostics.md), [RIG5031](diagnostics.md)). rig will not pick a
number for you, because "how long is a deleted invoice recoverable" is a
question about your product and often about your regulator.

## The snapshot triple keeps history

Three columns, and every update copies the row as it was before writing the
change. The history is rows in the same table, which means you can read it with
the same queries — rather than a diff nobody can reconstruct.

```sql
CREATE TYPE todo_version_type AS ENUM ('Original', 'Snapshot');

ALTER TABLE todo
    ADD COLUMN version_type          todo_version_type NOT NULL DEFAULT 'Original',
    ADD COLUMN snapshot_from_todo_id uuid REFERENCES todo(id),
    ADD COLUMN snapshot_from_todo_at timestamptz;
```

The two `snapshot_from_*` columns are named after the table. The enum must carry
exactly the labels `Original` and `Snapshot`.

All three or none: a partial triple is an error rather than a table rig quietly
treats as unversioned ([RIG5010](diagnostics.md)).

Add the CHECK constraint too. Without it, "a snapshot is immutable and an
original is unmarked" is only a convention, and one stray UPDATE turns your
history into fiction:

```sql
ALTER TABLE todo ADD CONSTRAINT todo_version_check CHECK (
    (version_type = 'Original'
        AND snapshot_from_todo_id IS NULL
        AND snapshot_from_todo_at IS NULL)
    OR
    (version_type = 'Snapshot'
        AND snapshot_from_todo_id IS NOT NULL
        AND snapshot_from_todo_at IS NOT NULL
        AND updated_at IS NULL
        AND deleted_at IS NULL)
);

CREATE INDEX todo_snapshot_idx ON todo (snapshot_from_todo_id);
```

`rig migration new <name> --table t --snapshot` writes all of this.

If a column should keep its live value when a snapshot is restored rather than
being reverted along with everything else, mark it in the table configuration:

```yaml
columns:
  view_count:
    snapshot_ignore: true
```

The value is still copied *into* the snapshot. It just is not copied back out.

## Managed columns

These are set by rig, never by a client, and so never appear in a create or
update input — and `rig sync` leaves them out of the configuration files it
writes, because there is nothing about them to configure:

```
id                       tenant_id
created_at               created_by_account_id     created_by_api_key_id
updated_at               updated_by_account_id     updated_by_api_key_id
deleted_at               deleted_by_account_id     deleted_by_api_key_id
version_type             snapshot_from_<table>_id  snapshot_from_<table>_at
```

## Enums

A Postgres enum becomes a Go type and a documented set of values. The Postgres
label stays the value on the wire; the identifier is derived, and you can
override it.

```sql
CREATE TYPE todo_priority AS ENUM ('low', 'normal', 'high');
```

```yaml
enums:
  todo_priority:
    name: TodoPriority
    description: How urgently a task wants attention.
    values:
      low:
        name: Low
        description: Worth doing eventually.
```

One constraint that catches people: an enum type must be nullable everywhere or
nowhere. If one column has it `NOT NULL` and another does not, rig cannot give
the type one Go representation ([RIG5040](diagnostics.md)).

## Relations

Declare a foreign key and rig derives the relation — the accessor on the
resource, the filter, and the option to embed the related rows in a read.

```sql
player_id uuid NOT NULL REFERENCES player(id),
```

Two rules apply.

**Index your foreign keys.** A foreign key without a covering index makes
deleting the parent row a full scan of the child table. rig reports it, and most
projects set `fk_needs_index: error`.

**`ON DELETE CASCADE` is refused** ([RIG6040](diagnostics.md)). It is an error
rather than a warning, because a cascade is a delete your application never
sees: no hook runs, nothing is notified, nothing is snapshotted, and the rows
are gone.

What to do instead is a plain foreign key and a
[parent hook](services.md#when-a-row-you-point-at-is-deleted) on the child. It
runs inside the transaction that is deleting the parent and can clear the link,
delete the rows through your own service, or refuse the delete outright — three
bodies where the keyword offered three words, and a fourth case the keyword
could not have said at all.

## Comments are documentation

A `COMMENT ON` in your migration becomes the Go doc comment on the model and the
description in the generated API documentation. A comment in the table
configuration overrides it.

Write them in the migration when the comment is about the data, and in the YAML
when it is about the API. Most projects set `missing_comment: error`, which is
what makes the generated documentation actually useful rather than a list of
field names.

## Naming rules rig checks

Each of these is a rule you can set to `off`, `warn`, or `error` in
[rig.yaml](rig-yaml.md). They are conventions, not requirements — but the
defaults exist because each one has bitten somebody.

| Rule | What it wants |
|---|---|
| `boolean_prefix` | A boolean reads as a predicate: `is_`, `has_`, `can_`, `should_`, `was_`, `allow_` |
| `timestamp_suffix` | A column ending in `_at` is a `timestamptz`, and a timestamp column is named `_at` |
| `date_suffix` | A column ending in `_date` is a `date`, and vice versa |
| `fk_naming` | A foreign-key column is named after the table it points at: `player_id` |
| `fk_needs_index` | Every foreign key is covered by an index |
| `tenant_id_leading_index` | Some index leads with `tenant_id` |
| `cascade_delete` | No foreign key declares `ON DELETE CASCADE` |
| `missing_comment` | Every table and column has a comment |
| `unmentioned_column` | Every column is mentioned in the table configuration |
| `migration_filename` | Files are named `NNNNN_snake_case.sql` |

`timestamp_suffix` is stricter than it looks, and deliberately: an `_at` column
must be `timestamptz`, not a bare `timestamp`. A name ending in `_at` claims the
column records when something *happened*, and only `timestamptz` can — a bare
`timestamp` is a clock reading with nothing to anchor it, so two of them cannot
be ordered across a daylight-saving change. `timestamp` is the right type for a
birthday or for opening hours, and the wrong one for an event.

## Names rig reserves

Everything above is a convention you can turn off. This is the one naming rule
you cannot, and there are two halves to it.

**The `rig_` prefix is rig's.** Your tables cannot use it. It is what tells you,
in psql, which tables arrived with the foundation and which you wrote — and it is
what lets rig add a table to the foundation without landing on one of yours.

Bookkeeping is under the prefix too, and is not a resource in any project:
`rig_migrations` for your own migrations, and `rig_auth_migrations`,
`rig_files_migrations` and `rig_notify_migrations` for the sets those modules
carry when
[`migrations.foundation` is `embedded`](rig-yaml.md#who-keeps-rigs-migrations).
goose writes those tables rather than a migration creating them, so rig neither
refuses them nor generates anything for them.

**The names those tables project to are reserved too**, whether or not you
expose them:

| Reserved | Taken by |
|---|---|
| `Tenant`, `Identity`, `IdentityCredential`, `IdentityVerification`, `Account` | the tenancy tables |
| `APIKey` | `rig_api_key` |
| `AccountToken`, `AuthLog`, `IdentitySession` | the session tables |
| `IdentityOAuth` | `rig_identity_oauth` |
| `File` | `rig_file` |
| `Notification`, `NotificationRecipient`, `NotificationDevice`, `NotificationSetting`, `NotificationDelivery` | the [notification](notifications.md) tables |

So you cannot have a table called `account`, or `file`, or `notification`
([RIG2004](diagnostics.md)) — nor a table under the prefix
([RIG2005](diagnostics.md)).

The list grows when rig's foundation does, and it grows on its own: a name is
reserved by a foundation table's configuration asking for it, not by an entry in
a list somebody has to remember. That is why turning `notifications.enabled` on
does not change what is reserved — those five names were already rig's the moment
the part existed.

**The trade.** You give up a handful of common table names. What you get back is
that `auth.expose: [rig_account]` stays one line in `rig.yaml` forever. Without
the reservation it works until the day you turn it on, and the fix that day is a
migration, a rename in every client that reads the resource, and a deprecation
window for anybody already calling it.

Names are sometimes reserved before the tables exist, which is the same bet made
earlier. `Notification` was reserved while the notification part was still being
designed: a name costs you one alternative today, and finding out on the day the
part lands costs you the migration.

**Keeping the table name.** Only the projected name is reserved, so a table you
genuinely want to call `file` can keep its name and answer to something else:

```yaml
# services/file/file.yaml
table: file
resource: Document
```

That is the whole escape. It is deliberately not a switch: renaming the resource
is a decision about your API, and it is visible in the file that describes it.

This one file is written by hand. `rig sync` skips it and says so, because the
name it would fill in is the reserved one; `rig migration new --table file`
warns and writes the migration anyway. Nothing here applies to the `rig_`
prefix, which no key answers.

**Turning it off.** `auth.own: true` ([rig-yaml.md](rig-yaml.md#auth)) stops
both rules. A project that has forked the foundation owns those tables and their
names, so there is nothing left to reserve them from. It is the same one-way
door it already was — you are maintaining the auth schema yourself from then on.

## Next

- [tables.md](tables.md) — the configuration file that annotates all of this
- [rig-yaml.md](rig-yaml.md) — where the validation rules are set
- [diagnostics.md](diagnostics.md) — every code these rules produce

## Files

A file attaches to a row through a column, and the column's name carries the
role:

```sql
cover_file_id uuid references rig_file (id)
```

`<role>_file_id` is the whole declaration. Everything follows from it — the path
segment `cover-file`, the endpoints, the permission keys `todo.cover_file.read`
and `todo.cover_file.write`, the Go field, the part on a form — and no
configuration key can disagree with it, because there is none. This is the same
kind of fact as `deleted_at` meaning soft-deletable.

Put the tenant inside the key:

```sql
foreign key (tenant_id, cover_file_id) references rig_file (tenant_id, id)
```

`references rig_file (id)` alone proves the file exists and says nothing about
whose it is, so attaching another tenant's file becomes something every hook has
to remember. The composite form makes it a constraint violation instead. It needs
`unique (tenant_id, id)` on `rig_file`, which the foundation migration creates.

Three endpoints appear under the row that owns the file:

```
POST   /api/v1/todos/{id}/cover-file
GET    /api/v1/todos/{id}/cover-file/{fileId}/{filename}
DELETE /api/v1/todos/{id}/cover-file
```

The nesting is what makes them safe rather than what makes them tidy: the handler
resolves the owning row through the repository before it touches a byte, so you
cannot upload to a row you cannot read, and an owner-scoped table's files are
exactly as invisible as its rows.

**A not-null file column has no delete endpoint.** Detaching means clearing the
column, and a column that cannot be null has nowhere to be cleared to — the row
goes instead. It also means the create on that table accepts a form as well as a
JSON body, because the row and its bytes have to be committed together or the
column could never be written at all.

**Many files is a table, not a key.** A gallery, an attachment list, a set of
receipts: write the table, give it a file column, and it gets its own file
endpoints along with everything else an ordinary rig table gets — a list query,
ordering, captions, soft delete. `examples/todo` has both forms.

Turn it all on with the `files:` block in [rig-yaml.md](rig-yaml.md#files); the
column does nothing without it.

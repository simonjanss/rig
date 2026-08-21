# Live sync

> **Half written.** The shapes a table gets are below. Still to come: the
> scoping function, running an ElectricSQL service alongside your application,
> and what a client does with the stream.
>
> Until it exists, the `electric:` block in [tables.md](tables.md#electric) is
> the complete configuration reference, and the `electric` generator's options
> are in [generators.md](generators.md).

A **shape** is a filtered view of one table that a client subscribes to and keeps
up to date, instead of polling. The sync service serves it;
rig generates an endpoint that stands in front of it, so that a subscription is
authenticated and tenant-scoped like every other read.

Turn it on per table:

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

rig builds the tenant and lifecycle predicates itself — a subscriber of that
endpoint cannot see another tenant's rows, or soft-deleted ones, or snapshots.
Your declared params are handed to a scoping function rig writes a stub for, and
that function can only **narrow** the shape further. There is no way for it to
widen one.

The generator writes nothing at all until some table opts in, so leaving it
configured in `rig.yaml` costs nothing.

## Three shapes, decided by your columns

The one above is the **live** shape: the rows an ordinary read returns. A table
whose schema keeps more than that gets more than one route, and you configure
none of it — `deleted_at` is what says this table has a trash, and the snapshot
columns are what say it has a history, the same way they decide whether the API
has a `GET /_deleted`. Asking you a second time would only create a way for the
two answers to disagree.

| Route | Carries | Needs |
|---|---|---|
| `GET /electric/todo` | Live rows. Not deleted, not a snapshot. | — |
| `GET /electric/todo/_deleted` | Retired rows — the trash. | `deleted_at` |
| `GET /electric/todo/{id}/_versions` | One row's previous versions. | the snapshot columns |

The columns are the whole rule, and `operations:` is not part of it. The API
needs `List` before it offers a `GET /_deleted` and `Get` before it offers a
`GET /{id}/_versions`, but live sync is its own read surface — a table with no
`operations` at all still gets shapes, which is how rig's own unexposed tables
are subscribed to. Reading `operations` here would leave such a table with a
live shape and no trash.

Every one of them is filtered to the caller's tenant, and to the caller's own
rows on an owner-scoped table, exactly like the live shape.

### Scoping them

Each gets its own field on `Handlers` and its own stub, so you can narrow a
trash stream without touching a live one. **While the field is nil, the route
uses the live shape's scope.** The trash and the history carry the same table's
rows, so a check the live shape needed — team membership, a share table, whatever
rig cannot read off a column — is almost always a check these need too, and
inheriting can only ever show a subscriber less. Setting the field replaces the
inherited scope rather than adding to it, so a stub you wire up should repeat
whatever the live one adds unless the reason for it stops applying.

That matters most on an upgrade: these routes appear the first time you
regenerate, and a new field on a struct you fill in by name is not a compile
error. If your live scope is load-bearing for who may see a row, it keeps
working on all three routes without you doing anything — but wiring the
generated no-op stubs in without reading them would switch it off for two of
them.

They are the live-sync counterparts of [`GET /_deleted` and
`GET /{id}/_versions`](api.md), and the split into routes is the point: a
subscriber chooses which rows it wants by choosing a URL, not by sending a
parameter. Nothing a client puts on the query string moves a shape from one of
these to another.

Two things differ from the endpoints they mirror, both worth knowing before you
build on them:

**`_versions` answers an unknown id with an empty stream, not a 404.** A shape
endpoint has no database handle — it is a filter in front of the sync service,
not a lookup — so an id from another tenant matches nothing rather than being
refused. Your subscriber cannot tell "no history" from "not yours". If that
distinction matters, read the row over the API first.

**`_deleted` has no restore window.** `GET /_deleted` shows only rows still
inside `restore_window_days`; this shape shows every retired row. A window is a
moving predicate, and the sync service evaluates a shape's filter when a row
*changes* — so a row that aged out would emit nothing and sit in your
subscriber's copy forever, filtered in appearance and not in fact. Nothing
hard-deletes past-window rows either, so on a table that is deleted from heavily
this stream grows without bound. Use `GET /_deleted` when you want the window.

## See also

- [tables.md](tables.md#electric) — the configuration keys
- [generators.md](generators.md) — `electric_url`, `shape_import`, `stub_dir`

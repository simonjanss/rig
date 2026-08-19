# Live sync

> **Not written yet.** This page will document shape endpoints, the scoping
> function, running an ElectricSQL service alongside your application, and what
> a client does with the stream.
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

rig builds the tenant and lifecycle predicates itself — a subscriber cannot see
another tenant's rows, or soft-deleted ones, or snapshots. Your declared params
are handed to a scoping function rig writes a stub for, and that function can
only **narrow** the shape further. There is no way for it to widen one.

The generator writes nothing at all until some table opts in, so leaving it
configured in `rig.yaml` costs nothing.

## See also

- [tables.md](tables.md#electric) — the configuration keys
- [generators.md](generators.md) — `electric_url`, `shape_import`, `stub_dir`

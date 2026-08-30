# Diagnostics

> **Not written yet.** This page will explain each code in full, with the schema
> or configuration that causes it and how to fix it.
>
> Until it exists, `rig codes` lists every code and `rig codes RIG6002` prints
> the summary and hint for one — the same text rig shows when it reports the
> problem. Nothing here is more authoritative than the binary.

Every problem rig reports carries a code, a severity, and the exact line that
caused it:

```
services/todo/todo.yaml
  20:5: error[RIG6002]: column todo.estimate_minutes has no comment
    Describe the column in its `comment:` key.

1 error
```

## The ranges

| Range | About |
|---|---|
| `RIG1xxx` | Types: a Postgres type with no Go mapping, an empty enum, a relation rig cannot project |
| `RIG2xxx` | Naming: two things projecting to the same API name, a plural that cannot be derived, a collision with a name or table prefix rig reserves |
| `RIG3xxx` | Configuration: invalid YAML, a key that does not match the schema, a key that has been replaced by another, a block that cannot work without another one, a column or enum the configuration names that no longer exists |
| `RIG4xxx` | Notes: a hand-written endpoint replacing a generated one |
| `RIG5xxx` | Structure: no primary key, a partial snapshot triple, a missing `restore_window_days`, an enum nullable in one place and not another, an `on_delete.order` rig cannot resolve, a live-sync table the database will not replicate |
| `RIG6xxx` | Conventions: missing comments, unindexed foreign keys, naming rules, `ON DELETE CASCADE`, migration filenames |
| `RIG9xxx` | Internal invariants. Seeing one of these is a bug in rig |

## Severity

`RIG6xxx` codes and a few others are configurable — set each to `off`, `warn`,
or `error` in the `validate:` block of [rig.yaml](rig-yaml.md#validate).

Structural rules are not configurable. A schema that breaks one cannot be
generated from at all, so there is no severity to set.

`rig validate --strict` treats every warning as a failure, which is what CI
wants: a warning nobody ever fails on is a warning nobody ever fixes.

`--format github` emits annotations, so a failure lands on the offending line in
a pull request. `--format json` is for anything else that has to read them.

## The three that read the database rather than your files

`RIG5090`, `RIG5091` and `RIG5092` are the only rules that report a fact about
the server instead of a fact about your schema or configuration, and they all
say the same kind of thing: this table asks for a live-sync shape, and the
database is not arranged to give it one.

| Code | What the database said | If you ignore it |
|---|---|---|
| `RIG5090` | No publication a migration wrote carries the table. Publish it in one — `ALTER PUBLICATION rig_publication ADD TABLE todo`. The publication Electric maintains for itself does not count | The sync service will try to publish it on the first subscription, and can only do so from a role that owns the table — otherwise the subscription fails as an access error |
| `RIG5091` | The table is `UNLOGGED`, so it writes no WAL | Nothing can ever follow it. The shape answers `200` with no rows, forever |
| `RIG5092` | The server runs with `wal_level` other than `logical`. Reported once, not once per table. `database.electric.enabled` sets it on the local container; a project with shapes and no local sync service writes `wal_level=logical` under `database.settings` instead | No publication on it can be decoded, so every shape in the project is empty |

Two of those are absolute and one is not, which is worth knowing before you
reach for a workaround. `RIG5091` and `RIG5092` describe streams that cannot
exist. `RIG5090` describes a stream whose existence depends on the sync service
having privileges it may not have in production — an error because "it works on
my machine and fails in the deployment with least privilege" is the worst place
to find that out, not because the stream is certainly empty.

They need a database to have been read, which means they are silent under
`rig validate --schema dump.json` when the dump was written before rig read
these facts. That is deliberate: a table reported as unpublished on evidence
nobody collected would fail a project where everything is right.

[electric.md](electric.md#the-table-has-to-be-published) has the migration and
the reason not to leave it to the sync service.

## See also

- [schema.md](schema.md#naming-rules-rig-checks) — what the convention rules want
- [schema.md](schema.md#names-rig-reserves) — the names and the table prefix that are rig's
- [rig-yaml.md](rig-yaml.md#validate) — setting severities
- [electric.md](electric.md#the-table-has-to-be-published) — what `RIG5090` wants

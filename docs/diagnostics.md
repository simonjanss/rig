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
| `RIG2xxx` | Naming: two things projecting to the same API name, a plural that cannot be derived, a collision with a reserved name |
| `RIG3xxx` | Configuration: invalid YAML, a key that does not match the schema, a column or enum the configuration names that no longer exists |
| `RIG4xxx` | Notes: a hand-written endpoint replacing a generated one |
| `RIG5xxx` | Structure: no primary key, a partial snapshot triple, a missing `restore_window_days`, an enum nullable in one place and not another, an `on_delete.order` rig cannot resolve |
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

## See also

- [schema.md](schema.md#naming-rules-rig-checks) — what the convention rules want
- [rig-yaml.md](rig-yaml.md#validate) — setting severities

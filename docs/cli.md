# The CLI

> **Not written yet.** This page will document every command, subcommand, and
> flag with worked examples.
>
> Until it exists, `rig --help` and `rig <command> --help` are complete and
> authoritative — every command carries a description explaining what it does
> and why. [tutorial.md](tutorial.md) walks through the ones you use daily.

The commands, in the order you meet them:

| | |
|---|---|
| `rig init [dir]` | Start a new project |
| `rig migration new <name>` | Write a new migration file |
| `rig db up` / `down` / `reset` / `url` / `psql` | Manage the throwaway local database — and, with `database.electric.enabled`, the sync service beside it |
| `rig sync` | Bring table configuration in step with the database |
| `rig validate` | Check the schema and its configuration |
| `rig generate` | Write the code |
| `rig check` | Report whether the generated code is up to date — the CI gate |
| `rig setup-project` | Scaffold the authentication foundation |
| `rig generators` | List the generators rig knows about |
| `rig codes [CODE]` | List or explain diagnostic codes |
| `rig ir` | Print the compiled document |
| `rig schema` | Write the JSON Schema files editors use |
| `rig completion <shell>` | Shell completion |

Global flags, on every command:

```
-C, --directory string     run as if started in this directory
    --config string        path to rig.yaml (default: search upwards)
    --format string        diagnostic format: text, json or github
    --no-color             disable colored output
    --container-bin string container engine to use (default: docker, then podman)
```

`--format github` emits diagnostics as GitHub Actions annotations, so a
validation failure shows up on the line that caused it in a pull request.

## Two checkouts of one project

rig names the throwaway database after your project and publishes it on the port
in `rig.yaml`. That pair is deliberate: a name you can type and a port your
`.env` already carries. It is also the one thing two clones of the same project
cannot share — they agree on both, so whichever runs second adopts the other's
container and applies its own branch's migrations on top of it. What you see is
not a collision. It is `rig check` reporting tables your branch never introduced,
or `apply migrations: detected 3 missing (out-of-order) migrations`.

Set `RIG_DB_ISOLATE` to anything that differs between the clones — the directory
is the obvious choice — and rig gives each one a container of its own:

```bash
export RIG_DB_ISOLATE=$PWD
rig db up          # todo-db-88c55d79, on a port the kernel picked
rig db url         # the only thing that knows which port that was
```

The name keeps them apart and the port stops them queueing for one number. Leave
it unset and nothing changes: one clone gets the name and the port it wrote down.

Two things follow from the port being the kernel's. `rig db url` starts the
database to answer, because there is no port until a container has one. And
whatever you point at it needs telling — `DATABASE_URL=$(rig db url) go test
./...` rather than a connection string with the number typed into it.

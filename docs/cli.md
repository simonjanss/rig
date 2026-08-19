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
| `rig db up` / `down` / `reset` / `url` / `psql` | Manage the throwaway local database |
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

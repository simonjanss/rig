# Working in rig

The checks, and which ones need Docker. `make help` lists every target.

## Before pushing

```bash
make hooks   # once per clone: installs the pre-push hook
make check   # what that hook runs, and what CI runs
```

`make check` is every check in this file, cheapest first:

```bash
make fmt-check   # gofmt; `make fmt` rewrites
make vet
make build
make test        # the fast suite
make deps        # go mod tidy, then fail if anything changed
make lint        # golangci-lint, pinned, installed into ./bin
make vulncheck   # govulncheck, likewise
make test-docker # needs Docker
make examples    # needs Docker; a few minutes on its own
```

So a push takes a few minutes and needs Docker or Podman running. That is the
trade: CI no longer watches branches, so this is where a break gets caught.
`git push --no-verify` skips the hook when you need it to.

`make deps` fails on any `go.mod` or `go.sum` that differs from what is
committed, untracked files included, so run it with the tree clean or it will
report your own work in progress back to you. When it does fire on a clean
tree, `make tidy` produced the change: commit it.

Each of these runs per module — the repository is a Go workspace, and `./...`
names one module's packages and nothing else. `make check` is a list of those
per-module targets and nothing more: a target that ran `./...` once and called
it "everything" would quietly skip nine modules out of ten.

## The ones that need Docker

```bash
make test-docker   # the suite behind the `docker` build tag
make examples      # regenerate all four examples and run them for real
```

`make test-docker` covers `.`, `runtime`, `auth`, `files`, `migrate` and
`rigclient`.
Most of it starts its own Postgres on a port of its own and cleans up after
itself. The `migrate` module is the exception: it expects a database at
`localhost:55440`, or wherever `DATABASE_URL` points, and **skips itself
silently** when there is none — so a green run there does not mean it ran.

`make examples` is the strongest regression test in the repository. It runs
`rig generate` and `rig check` in each example, then builds and tests it; the
examples are real projects, so a generator change that breaks one breaks it
visibly. `rig generate` starts and migrates the database each example names in
its own `rig.yaml`, so do not have something else listening on those ports
(todo 55440, fantasyfootball 55441, auth 55442, auth_oauth 55443).

Expect `make examples` to take a few minutes. It is worth it for any change
under `internal/gen/` or `internal/compile/`.

## Generated files

Anything `*.gen.go` is rewritten on every run — a fix belongs in the generator
that emitted it. When a golden file changes because the change was intended,
`make update-golden` rewrites the ones under `internal/`, and
`make update-schema` rewrites the introspection golden from a real Postgres.

## CI

`.github/workflows/rig.yaml` runs all of the above on every push to `main`, and
on nothing else. Branches and pull requests are covered by the pre-push hook in
`.githooks/`, which runs the same `make check` on the machine that wrote the
commit — so a break is caught before the push rather than an hour later in a
runner. Nothing in the workflow is unique to CI: each job is one of the make
targets, so a check that fails there fails the same way locally.

A red `main` therefore means the hook was skipped, or was never installed:
`make hooks`.

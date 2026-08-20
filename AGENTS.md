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
make godoc-check # exported symbols in the five imported modules have doc comments
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

`make test-docker` covers `.`, `runtime`, `auth`, `files`, `notify`, `migrate`
and `rigclient`.
Most of it starts its own Postgres on a port of its own and cleans up after
itself. The `migrate` module is the exception: it expects a database at
`localhost:55440`, or wherever `DATABASE_URL` points, and **skips itself
silently** when there is none — so a green run there does not mean it ran.

`make examples` is the strongest regression test in the repository. It runs
`rig generate` and `rig check` in each example, then builds and tests it; the
examples are real projects, so a generator change that breaks one breaks it
visibly. `rig generate` starts and migrates the database each example names in
its own `rig.yaml`.

Every port a suite or an example pins is named in `internal/dockerdb/ports.go`,
and a test there refuses two suites on one number. A new suite takes its port
from that file rather than by grepping for one that looks free — the examples
are listed there too, even though their own configuration is where the number
actually lives.

**Two checkouts of rig on one machine do not share a database.** The Makefile
exports `RIG_DB_ISOLATE=$(CURDIR)`, and rig answers by suffixing every container
name with a digest of it and letting the kernel pick the host port — so this
clone gets `todo-db-88c55d79` on whatever was free rather than `todo-db` on
55440. Without it, the second clone to run adopts the first's container and
migrates its own branch on top, and what arrives is not a collision: it is `rig
check` reporting tables this branch never introduced, or `apply migrations:
detected 4 missing (out-of-order) migrations`. Diagnostics about the current
branch, produced by somebody else's schema. `internal/dockerdb/isolate.go` is
the mechanism and the reasoning.

Two consequences. Run the Docker suites through `make`, because a bare `go test
-tags docker ./internal/authtest` has no `RIG_DB_ISOLATE` and goes back to
sharing. And a container per example per checkout adds up on a machine with
several workspaces open — `make db-down` stops this checkout's, and the next
command rebuilds from the migrations.

The ports in `ports.go` are what a single checkout gets, so `psql
postgres://rig:rig@localhost:55440/rig` is still the todo example when nothing
is isolated. Under isolation, ask: `cd examples/todo && ../../bin/rig db url`.

Expect `make examples` to take a few minutes. It is worth it for any change
under `internal/gen/` or `internal/compile/`.

## Generated files

Anything `*.gen.go` is rewritten on every run — a fix belongs in the generator
that emitted it. When a golden file changes because the change was intended,
`make update-golden` rewrites the ones under `internal/`, and
`make update-schema` rewrites the introspection golden from a real Postgres.

## Documentation

`docs/` is **user** documentation: how somebody builds an application with rig.
Not how rig is built — that is this file — and not what is not built yet, which
is `NEXT.md`. So `docs/` names what a user writes or imports (`rig.yaml`,
`migrations/`, `services/*/`, `rig/runtime`, `rig/auth`, `rig/migrate`,
`rig/rigclient`) and never `internal/`, `pkg/ir`, or a generator package. Second
person, organised around what somebody is trying to do, and every rule stated
with the trade it makes.

**A change to rig's user-visible surface updates the page that documents it, in
the same commit.** The pages are short and the mapping is mechanical:

| A change to | Updates |
|---|---|
| `internal/gen/*` — a generator added, removed, renamed | `docs/generators.md`, `README.md` |
| a generator's `Options` struct | `docs/generators.md` |
| `internal/project/config.go` — any `rig.yaml` key | `docs/rig-yaml.md` |
| `internal/tableconf/config.go` — any per-table key | `docs/tables.md` |
| `internal/cli/*.go` — a command, subcommand, or flag | `docs/cli.md` |
| `internal/compile/convention.go` — a recognised column or rule | `docs/schema.md` |
| `internal/compile/builtin.go` — the error codes, the pagination limits | `docs/api.md` |
| `internal/diag` — a code added, or a severity changed | `docs/diagnostics.md` |
| `auth/`, or `internal/project/auth.go` | `docs/auth.md` |
| `runtime/electric`, `internal/gen/electricgo` | `docs/electric.md` |
| `notify/`, `internal/project/notifications.go` | `docs/notifications.md` |
| `runtime/serve`, `runtime/dbhook` | `docs/services.md` |
| `rigclient/`, `internal/gen/goclient` | `docs/clients.md` |

Two rules that are easier to break than the table above:

**A removed feature is removed from every page that names it.** The failure
documentation actually has is describing something that is gone, not omitting
something new. Grep for it.

**Nothing that does not exist yet appears in `docs/`.** `README.md` names only
what `rig generators` would list. Planned work belongs in `NEXT.md`. The root
README claimed an OpenAPI document and a TypeScript client for several months
before anybody noticed neither existed.

Several pages are stubs and say so in a blockquote at the top, pointing at the
authoritative source in the meantime — `rig <cmd> --help`, `rig schema project`,
a named example. Filling one in is deleting the blockquote, not adding a file.

A checked-in `PostToolUse` hook in `.claude/` prints the matching page when one
of the files above is edited. It reminds; it does not gate.

## Godoc

`runtime/`, `auth/`, `files/`, `notify/`, `migrate/` and `rigclient/` are separate
modules
that a generated application imports. Their godoc is the only documentation
their Go surface has: `docs/` covers what somebody writes — `rig.yaml`, a
migration, a service — and never what they call. So a doc comment there is
documentation, not commentary, and `make godoc-check` fails on an exported
symbol without one.
**A change to a signature updates its doc comment in the same commit.**

The check asks only whether a symbol has a comment, never what the comment says.
Two things it deliberately leaves alone, because both are judgement:

- **The form.** Comments here are prose, not `// Foo does...` boilerplate, which
  is why `ST1000` and `ST1020`–`ST1022` are off in `.golangci.yml`. A presence
  check that also demanded the form would contradict a decision already made.
- **Struct fields.** They are documented where the name leaves a question open
  and not otherwise. Enforcing them would produce a hundred `// ID is the ID.`
  lines, and the next reader would trust the comments less, not more.

Three ways a comment that exists still fails to render, none of which a compiler
catches:

**A comment above several one-line declarations documents only the first.**
`electric.Where` had `// Gt, Gte, Lt, and Lte compare.` over four of them, and
three of the four were undocumented on pkg.go.dev for as long as it was there.
`make godoc-check` catches this one.

**A doc link to an unexported symbol renders as literal brackets.** In a comment
on an *exported* declaration, link only what is exported and name the rest in
prose. Elsewhere the brackets are harmless, because nothing renders them —
`[Service.accountFor]` in `auth/account/memory.go` sits on an unexported field
and reads fine where it is. Nothing catches this, so after a rename:

```bash
grep -rn '^\s*//.*\[[A-Z][A-Za-z0-9_]*\.[a-z][A-Za-z0-9_]*\]' --include='*.go' \
  runtime auth files migrate rigclient
```

**A doc on a `const (` block covers every name in it**, so the block is where a
closed set is explained once rather than a dozen times.

Examples are `Example` functions with an `// Output:` comment, in `example_test.go`
and the external test package. They render on pkg.go.dev *and* run in `make
test`, which is the point: an example that has to compile and produce the stated
output cannot quietly stop being true. The seven packages that have them are all
in `runtime/` and all pure — one needing Postgres or a live server has no
deterministic output, and a compile-only example is a weaker promise than none.

## CI

`.github/workflows/rig.yaml` runs all of the above on every push to `main`, and
on nothing else. Branches and pull requests are covered by the pre-push hook in
`.githooks/`, which runs the same `make check` on the machine that wrote the
commit — so a break is caught before the push rather than an hour later in a
runner. Nothing in the workflow is unique to CI: each job is one of the make
targets, so a check that fails there fails the same way locally.

A red `main` therefore means the hook was skipped, or was never installed:
`make hooks`.

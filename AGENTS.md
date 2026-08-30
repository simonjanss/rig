# Working in rig

The checks, and which ones need Docker. `make help` lists every target.

**Releasing is different from everything else in here.** It publishes tags that
cannot be moved, deleted or fixed afterwards, so it is not something to do
because main looks ready — read [Releasing](#releasing) before deciding to, and
do not cut one you were not asked for.

## Issues

Todos live in GitHub issues on `simonjanss/rig`, and nowhere else. `NEXT.md` and
`SIMPLIFICATIONS.md` are a record of what was built and why it came out the way
it did — not a queue. Do not add a todo to either of them, and do not start a
third markdown file for one.

**Search before filing.** `gh issue list --search "<terms>"`. Several workspaces
run against this repository at once, so the thing may already be there.

**File one when the work will outlive the session, or when you were asked for a
todo.** A fix you are making right now does not need an issue first, and
read-only work — answering a question, reading around the codebase — never does.

**Check nobody else has it.** An issue carrying the `claimed` label, or with an
assignee, is one another workspace is probably inside. Stop and ask rather than
starting a second branch on it.

**Claim it before the first commit**, so the claim is visible while the work is
in flight rather than after it:

```bash
gh issue edit N --add-label claimed --add-assignee @me
```

If you put the work down without finishing it, hand the claim back:
`gh issue edit N --remove-label claimed --remove-assignee @me`.

**Closing is the pull request's job.** Put `Closes #N` in the PR body and
merging closes the issue. Do not close it by hand, and keep the number out of
the commit subject — merges here are squashed, so the subject already ends in
the PR number.

## Before pushing

```bash
make hooks   # once per clone: installs the pre-push hook
make check   # what that hook runs, and what CI runs
```

`make check` is every check in this file, cheapest first:

```bash
make fmt-check   # gofmt; `make fmt` rewrites
make vet
make godoc-check # exported symbols in the imported modules have doc comments
make build
make test        # the fast suite
make deps        # go mod tidy, then fail if anything changed
make lint        # golangci-lint, pinned, installed into ./bin
make vulncheck   # govulncheck, likewise
make release-check # goreleaser's config validates and builds
make ts          # the TypeScript workspace; needs pnpm
make test-docker # needs Docker
make examples    # needs Docker; a few minutes on its own
make linearlite-web  # the one example front end anything compiles; needs pnpm
```

`make update-examples` is not in `make check`: it writes, and a check that
rewrites what it is checking is not a check.

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
it "everything" would quietly skip ten modules out of eleven.

## The ones that need Docker

```bash
make test-docker   # the suite behind the `docker` build tag
make examples      # check all five examples for drift and run them for real
```

`make test-docker` covers `.`, `runtime`, `auth`, `authmodel`, `files`,
`notify`, `observe`, `presence`, `migrate`, `rigclient` and `rigs3`. All of it
wants Postgres except `rigs3`, which wants MinIO.
Most of it starts its own Postgres on a port of its own and cleans up after
itself. The `migrate` module is the exception: it expects a database at
`localhost:55440`, or wherever `DATABASE_URL` points, and **skips itself
silently** when there is none — so a green run there does not mean it ran.

`make examples` is the strongest regression test in the repository. It runs
`rig check` in each example, then builds and tests it; the examples are real
projects, so a generator change that breaks one breaks it visibly. `rig check`
starts and migrates the database each example names in its own `rig.yaml`.

It checks and writes nothing, which is the whole point: the examples commit their
generated output so that a generator change shows up as a diff, and generating
before checking would compare the generators against what they had just written.
So a change under `internal/gen/` fails this target by design. `make
update-examples` regenerates all five with `--prune`, and the diff it leaves is
the review — the counterpart to `make update-golden` for output that is
committed rather than compared.

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
`make update-golden` rewrites the ones under `internal/`,
`make update-examples` rewrites the committed output under `examples/`, and
`make update-schema` rewrites the introspection golden from a real Postgres.

**The banner is load-bearing.** `gen.Banner` is written by the emitters and read
back by `gen.Orphans`, which is how `rig check` finds a file rig wrote and no
generator produces any more — in a checkout with no manifest, which is every
checkout CI makes. An emitter that stops writing it, or a new emitter that never
starts, silently narrows that check to the `.gen.` naming convention. Both marks
come from `pkg/gen` so they cannot drift apart.

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
| `runtime/electric`, the shape half of `internal/gen/servergo` | `docs/electric.md` |
| `internal/gen/openapigen` | `docs/api.md`, `docs/generators.md`, `README.md` |
| `notify/`, `internal/project/notifications.go` | `docs/notifications.md`, `docs/rig-yaml.md` |
| `presence/`, `internal/project/presence.go` | `docs/presence.md`, `docs/rig-yaml.md` |
| `runtime/cache`, `internal/project/cache.go` | `docs/rig-yaml.md`, `docs/auth.md` |
| `runtime/serve`, `runtime/dbhook`, `runtime/apibase` | `docs/services.md` |
| `runtime/reqlog`, `observe/`, or what a generator emits about logging or spans | `docs/observability.md` |
| `internal/project/tracing.go` — the `tracing:` block | `docs/observability.md`, `docs/rig-yaml.md` |
| `internal/project/monitoring.go` — the `monitoring:` block | `docs/observability.md`, `docs/rig-yaml.md` |
| `rigclient/`, `internal/gen/goclient` | `docs/clients.md` |
| `rigs3/`, `internal/project/files.go` | `docs/rig-yaml.md` |
| `ts/packages/*`, `internal/gen/tsclient` | `docs/clients.md` |

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

## The TypeScript workspace

`ts/` is a pnpm workspace holding the packages a front end imports —
`@rig-ts/client`, `@rig-ts/electric` and `@rig-ts/presence` — plus `typecheck-fixture`,
which is not published and exists only to be compiled.

The third one is not like the other two, and it is worth knowing why before
adding a fourth. `@rig-ts/client` retries and `@rig-ts/electric` maps; neither does
anything until it is called. `@rig-ts/presence` owns a timer, two window listeners
and a `keepalive` fetch on teardown — **it is the first thing rig ships that runs
when nobody called it**, which is why it is a package of its own rather than part
of `@rig-ts/electric`, and why it is the one place a side effect is expected. It is
also the only package with a second entry point (`@rig-ts/presence/react`, behind an
optional `react` peer dependency), so that a project which does not use React
never has `react` reachable from the module it imports.

That last point has a consequence for anything that aliases these sources rather
than consuming a published build. `examples/linearlite/web` does, and it therefore
pins `react` to its own copy by absolute path the way it already pins the sync
stack — because an import resolved out of `ts/` looks for its dependencies there,
and `make linearlite-web` never installs `ts/`. The rule the app's vite config
states is the general one: **every dependency of an aliased `ts/` source is named
in the app.** A fifth package that imports something new needs a line there.

`make ts` is the whole of it: install, Prettier, `tsc`, and the unit suite. It
needs pnpm on the machine and nothing else.

Three things about it are easy to get wrong.

**The generated output is not Prettier's.** Nothing in the Go toolchain formats
TypeScript, so `internal/gen/tsbuf` emits code already laid out — four spaces,
Prettier's import wrapping, its quote style — and Prettier runs over the
hand-written packages only. A change to the emitter's layout is checked by the
golden files and by nothing else.

**A golden test cannot notice that the output stopped compiling.** It proves the
generator emits the bytes it emitted last time, which stays true after a runtime
signature moves underneath it. `typecheck-fixture` is what closes that: its
`tsconfig.json` includes the golden directories and `examples/todo/client-ts`
directly — not copies — and maps `@rig-ts/client`, `@rig-ts/electric` and
`@rig-ts/presence` to their `src`, so it checks what the packages say now rather than
what the last build said. A new golden goes in that list, or the emitter can start
writing something that does not compile and every check will stay green.

**And a fixture that never mounts anything cannot check a package that runs on
its own.** `@rig-ts/presence` owns a timer and two window listeners, and the
mistakes that surface there are about *where in a component tree* it is built:
six of the eight bugs review found in it were in the browser half. That is why
`make linearlite-web` is in `make check` — `examples/linearlite/web` is the one
front end in the repository that anything compiles, and `web/src/presence` is
where the three decisions the package cannot make for you are written down.

**`allowBuilds` in `pnpm-workspace.yaml` is checked in on purpose.** pnpm blocks
install scripts by default and fails the install rather than warning once a
blocked one exists, so the answer belongs in the repository instead of being
approved once per clone.

## Godoc

`runtime/`, `auth/`, `authmodel/`, `files/`, `notify/`, `observe/`, `presence/`,
`migrate/`, `rigclient/` and `rigs3/` are separate modules
that a generated application imports, and `pkg/` is the root module's own
published surface — the IR and the generator interface, which is what somebody
writing a generator against rig imports. Their godoc is the only documentation
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
  runtime auth files notify observe presence migrate rigclient rigs3
```

**A doc on a `const (` block covers every name in it**, so the block is where a
closed set is explained once rather than a dozen times.

Examples are `Example` functions with an `// Output:` comment, in `example_test.go`
and the external test package. They render on pkg.go.dev *and* run in `make
test`, which is the point: an example that has to compile and produce the stated
output cannot quietly stop being true. The seven packages that have them are all
in `runtime/` and all pure — one needing Postgres or a live server has no
deterministic output, and a compile-only example is a weaker promise than none.

## Releasing

### When — read this before deciding to

**Do not cut a release unless you were asked to cut this one, now.** Everything
else in this file describes work that can be revised; this is the one procedure
that cannot. A tag the module proxy has fetched is in the checksum database
permanently: it cannot be moved, deleted, or fixed, only superseded by a higher
number. Propose a release, name the version you would use, and wait.

**What makes one necessary.** Merging to main releases nothing. Until a release,
a change to a published module's exported surface — `runtime`, `auth`,
`authmodel`, `files`, `migrate`, `notify`, `observe`, `presence`, `rigclient`,
`rigs3` —
or to what a generator emits is invisible to everyone outside this repository.
So the question is never "is main ahead of the last tag", it is "is somebody
waiting for something that is only on main".

**Which number.** rig is v0, where the rule is not semver's:

| The change | The bump |
|---|---|
| a signature moved, a config key changed meaning, generated output stopped compiling against the last release | minor — `v0.3.1` → `v0.4.0` |
| anything else — a fix, an addition, a new generator | patch — `v0.3.1` → `v0.3.2` |

There is no v1 until the Go surface is meant to be stable, and that is a
decision to raise rather than take: from v2 on, Go requires the major in the
import path, so every one of the eleven modules would need a `/v2` suffix and every
generated import in every user's project would change.

**Rehearse a first-of-anything with a prerelease.** `v0.4.0-rc.1` is not what
`@latest` selects and not what the setup-rig action installs, so it exercises
the whole mechanism at no cost. Use one whenever the release itself is the risky
part rather than the code in it.

**Four things never to do.** Move or delete a published tag. Release from a
branch. Hand-edit a version in a `go.mod` or a `package.json` — that is `make
release`'s job, and doing it by hand is how the eleven drift apart. Add an
`NPM_TOKEN`; publishing is tokenless by design and a secret appearing in the
release workflow means somebody misread it.

`internal/release` enforces what it can: it refuses to run off main, off a stale
main, on a dirty tree, onto a version that is already tagged, or from a module
that has grown a `replace` back.

### How

Eleven modules, one version, one commit. `make release VERSION=v0.1.0` rewrites
every intra-repository requirement to that version, commits, and creates eleven
tags: `v0.1.0` for the root module and `runtime/v0.1.0`, `auth/v0.1.0` and so on
for the rest, which is how Go names a version of a module in a subdirectory.
`make release-dry VERSION=v0.1.0` prints it without writing it.

**Lockstep is not tidiness.** The binary links `auth`, `files`, `migrate`,
`notify`, `presence` and `runtime` — it embeds their foundation schemas and
generates imports against them. A rig released at a different number from the
runtime it generates against is a rig that produces code nobody can build, so
the eleven numbers are one number.

**No published module may `replace` a sibling.** `go install pkg@version`
refuses a module whose `go.mod` carries one, and a consumer resolving a
dependency ignores the replace and asks the proxy for a version that was never
published. Local resolution is `go.work`'s job and only `go.work`'s job.
`internal/release` refuses to run when a replace comes back.

The examples keep theirs. They are not published, and they exist to compile
against the working tree — that is what makes `make examples` a regression test
rather than a test of the last release. Same for the temporary module
`internal/gen/gentest/compile.go` writes.

### The order, which is not the obvious one

```bash
make release VERSION=v0.1.0
make release-push VERSION=v0.1.0   # first
make tidy && git commit -am "go.sum: record the v0.1.0 sibling hashes"
make check                         # now that `make deps` can resolve them
git push origin main               # last
make release-verify VERSION=v0.1.0 # did the tag produce anything?
```

Tags before the branch, because `make deps` runs `go mod tidy`, and `go mod
tidy` is a single-module operation that resolves from the proxy and ignores the
workspace. Between the rewrite and the tag it is being asked for a version that
does not exist yet, and it fails.

**The pre-push hook skips a tags-only push**, which is what makes that order
possible at all. It used to run `make check` on one, and the failure was not the
expensive part: `go mod tidy` asks proxy.golang.org for each sibling at the new
version, the proxy caches the miss, and it keeps answering "unknown revision"
for about half an hour *after* the tags are up. v0.2.0 was uninstallable for
that long because the hook ran before the tags existed. Nothing else in the
repository can poison a cache outside it.

**`make release-push` sends the ten submodule tags, then the root tag alone.**
`git push origin --tags` is what this used to say, and it cannot work: GitHub
creates no push event for a batch of more than three tags, so all of them at
once triggers no release workflow. v0.2.0 got ten correct tags, no binaries, no
GitHub release and nothing on npm — and since a published tag cannot be moved,
the only fix was v0.2.1. The root tag goes last, so that by the time anything
reacts to `v*`, the versions its `go.mod` requires already resolve.

**`make tidy` belongs between the tags and the check**, not after a failure.
The release commit rewrites the requirements; the hashes for them cannot be
computed until the tags exist, so they land in a commit of their own every
time. `make deps` will tell you the same thing, one `make check` later.

**A tag the proxy has seen cannot be changed.** Not moved, not deleted — the
checksum database has it. A bad release is superseded by the next patch version
and never repaired. That is what `v0.1.0-rc.1` is for: a prerelease is not what
`@latest` selects, so it is a rehearsal with the same mechanics.

### What a release cannot be checked by

Nothing inside this repository. `go.work` resolves every sibling to the working
tree, so a `go.mod` that no outsider could resolve builds perfectly here and
fails the moment somebody runs `go get`. The `consumable` job in
`.github/workflows/release.yaml` is the check: after the tags are pushed it
installs rig from the proxy in an empty directory and builds a module that
imports four of the libraries. It runs on the tag, which means it runs after the
release exists — the earliest anything can.

**And a workflow that never started looks exactly like one that succeeded.**
That is what `make release-verify VERSION=v0.1.0` is for, and it is the last
step rather than an optional one: it checks every tag on origin, the GitHub
release and every file `checksums.txt` names, which release `latest` resolves
to, the three npm versions, and a real `go install` of the binary. The three
surfaces fail separately — v0.2.0 had tags and no build, v0.1.0 had a build and
an npm job that failed — and nothing said so until somebody tried to install it.

**A workflow that has not finished yet looks like one that never started, too**,
which is why the first thing verify asks is whether there is a run at all. The
answers are opposite: an unfinished run wants five more minutes, and a missing
one wants another version. Verify says which, and reports nothing as settled
while a run is still going.

It asks `checksums.txt` which archives shipped rather than listing them here,
because those names are a contract: `.github/actions/setup-rig` downloads
`rig_${version}_${os}_${arch}.tar.gz` by name, and a second copy of that
template is a second thing to keep right.

### The npm packages go out on the same tag

`ts/packages/{client,electric,presence}` publish as `@rig-ts/client`,
`@rig-ts/electric` and `@rig-ts/presence`, at the tag with its `v` stripped —
npm has no leading v. `make release` sets those three versions in the same
commit that sets the Go ones, and the `npm` job in the release workflow refuses
to publish if a package.json and the tag disagree.

Why lockstep here too: `rig generate` emits `import ... from "@rig-ts/client"`,
so the generator and the package it generates imports of have to agree, exactly
as the binary and `runtime` do.

**There is no NPM_TOKEN.** The job publishes through npm's trusted publishing,
which mints a short-lived credential from the workflow's OIDC identity — hence
`id-token: write` and no secret anywhere. Each package needs its trusted
publisher registered once on npm, pointing at this repository and
`release.yaml`.

It packs with pnpm and publishes with npm, and it takes both: only `pnpm pack`
rewrites the `workspace:^` peer dependencies into real ranges, and only `npm
publish` speaks OIDC. `client` publishes first, because the other two depend on
it.

### Pushing a tag builds binaries

`.github/workflows/release.yaml` triggers on `v*`, which matches the root tag
and none of the ten others — a glob does not cross a slash, and `runtime/v0.1.0`
does not start with a `v` anyway. It runs goreleaser for linux and darwin on
amd64 and arm64, and `.github/actions/setup-rig` is what a pipeline elsewhere
uses to download one of those instead of paying for `go install`.

## CI

`.github/workflows/rig.yaml` runs the Go half of the above on every push to
`main`, and on nothing else. Branches and pull requests are covered by the
pre-push hook in `.githooks/`, which runs the whole of `make check` on the machine
that wrote the commit — so a break is caught before the push rather than an hour
later in a runner. Nothing in the workflow is unique to CI: each job is one of the
make targets, so a check that fails there fails the same way locally.

**The two pnpm targets are the exception, and it is a real gap.** No job installs
node, so `make ts` and `make linearlite-web` run on the pushing machine and
nowhere else. A push with `--no-verify` therefore skips every TypeScript check in
the repository, including the only thing that compiles a front end.

A red `main` therefore means the hook was skipped, or was never installed:
`make hooks`.

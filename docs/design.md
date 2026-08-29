# Why rig works this way

Every decision below cost something. This page says what, so you can tell
whether the trade is one you want to make — and so that the surprising parts of
rig are surprising for a reason you can read.

If you just want to get something running, [tutorial.md](tutorial.md) is the
page you want.

---

## Your schema is the source of truth

**The decision.** rig has no schema language. Your migrations are the schema.
`rig sync` applies them to a throwaway Postgres and introspects the result.

**What it means for you.** There is no configuration key for anything the
database can state itself. You do not write `soft_delete: true` — you add a
`deleted_at` column, and that *is* the declaration. Same for versioning, for
audit trails, for relations.

**What it costs.** Changing a behaviour is a migration, not a one-line edit. You
cannot turn versioning off for an afternoon.

**The alternative.** A schema DSL that generates both the migrations and the
code, which is what most frameworks in this space do. It is genuinely nicer
until the first time the two disagree — and they always eventually disagree,
because a database accumulates things nobody's DSL emitted: an index somebody
added during an incident, a constraint from a migration written by hand, a
column type an extension changed. At that point the DSL is describing a database
that does not exist, and the code generated from it is wrong in ways nothing
catches. The database is the only thing that agrees with the database.

---

## The middle layer is yours, and rig will not touch it

**The decision.** rig generates the repository below your business logic and the
HTTP layer above it. It writes your service file exactly once, and never again.

**What it means for you.** There are no hooks-into-generated-code, no partial
classes, no "edit below this line" markers. When rig writes something with
`.gen.` in the name, it owns it completely and will overwrite it without asking.
When it writes your service stub, you own it completely and rig will not touch
it again even if the schema changes underneath.

**What it costs.** rig cannot help you with business logic at all. A framework
that generated a default "publish" endpoint from a naming convention would save
you the file.

**The alternative.** Generating everything and giving you override points. That
works right up until an override needs to see something the generator did not
think to expose, and then you are editing generated code and losing it on the
next run. A clean seam that a compiler enforces is worth more than a rich set of
hooks that mostly fit.

The compiler enforcement is the load-bearing part: declare an endpoint in your
table configuration and your service stops compiling until you implement it. Not
a 501 at runtime — a build failure.

---

## Everything is scoped to a tenant, whether you have tenants or not

**The decision.** Every table rig generates for carries `tenant_id`. Every
generated query filters on it. The value comes from the caller's credentials,
never from the request.

**What it means for you.** A client cannot read another tenant's rows because
there is no parameter that would let it ask. Not "because we remembered to check"
— because the filter is in every generated statement and there is no code path
without it.

**What it costs.** One column and one index on every table, and the mild
awkwardness of a single-tenant application carrying a constant uuid around.

**The alternative.** Adding multi-tenancy when you need it. This is the one that
never works. Retrofitting a tenant column onto a live schema means auditing
every query anybody ever wrote, and the ones you miss are the interesting ones.
The cost of carrying it from day one is small and fixed; the cost of adding it
later is unbounded.

---

## Generated code is ignored by default

**The decision.** `rig init` writes a `.gitignore` containing `*.gen.go`. Your
build runs `rig generate`.

**What it means for you.** Your diffs contain what you wrote. A schema change is
one migration and one configuration edit in review, not four thousand lines of
regenerated repository burying it.

**What it costs.** Your build needs the rig binary, and a fresh clone does not
compile until somebody has run `rig generate`.

**The other choice is legitimate.** Commit it, and the generated code becomes
reviewable — you can see exactly what a schema change did to your API, and a
generator upgrade shows up as a diff instead of a surprise. The examples in this
repository do exactly that, on purpose.

Either way, `rig check` is what you run in CI. It regenerates everything in
memory and fails on any difference, without writing anything. Committed
generated code that nobody regenerated is how a schema change quietly stops
matching the code that reads it.

**A leftover is a difference too.** rig records what it wrote in
`.rig/manifest.json`, and that file is gitignored — so the checkout CI makes has
no record at all. A gate that depended on it would pass on a tree still carrying
the generated files of a table that was renamed three commits ago. So rig also
recognizes its own output from the output: `.gen.` in the name, or its banner on
the first line. The cost is that the naming convention becomes a rule rather
than a habit, and a hand-written `*.gen.go` inside a project will be reported
and pruned. That is the trade — rig owning a namespace it already documented, in
exchange for a check that means the same thing in a clone as it does on your
machine.

---

## Configuration is keyed by physical names

**The decision.** Table configuration files are keyed by the table, column, and
enum label as spelled in Postgres — not by the names that end up in your API.

**What it means for you.** `columns: { created_by_account_id: ... }`, even
though the field is `createdBy` on the wire. Slightly uglier to read.

**What it costs.** The file does not read quite like the API it configures.

**The alternative.** Keying on API names, which reads better and breaks the
first time you rename a resource: the derived names shift, and your
configuration silently stops applying to the columns it was written for. rig
would have no way to tell "this key is for a column that was renamed" from "this
key is for a column that no longer exists". Keyed physically, a renamed column
is a loud error ([RIG3101](diagnostics.md)) instead of configuration that
quietly stopped working.

---

## Migrations are numbered, not timestamped

**The decision.** `00001_create_todo.sql`, `00002_add_snapshots.sql`. A
sequence.

**What it means for you.** Two people adding a migration on the same day collide
on the filename and have to resolve it.

**What it costs.** That merge conflict.

**The alternative.** Timestamps, which never collide. Which is exactly the
problem: two migrations written in parallel branches merge cleanly, interleave
in an order neither author considered, and apply fine in development because
they happened to run in the order they were written. A merge conflict on a
filename is a cheap, early, unmissable version of a conversation those two
authors needed to have.

---

## Every list is limited, whether you asked or not

**The decision.** Every generated list and search applies a limit. 50 by
default, 500 at most.

**What it means for you.** You cannot fetch a whole table in one call, even
deliberately. Bulk export is a job you write, not a query parameter.

**What it costs.** The occasional inconvenience of paginating something you know
is small.

**The alternative.** Unbounded by default. That works for the entire life of a
project, right up until one tenant's table gets large, and then it is an
incident: memory, the connection pool, and the response all fail at once, in
production, on the table that grew.

---

## The set of error codes is closed

**The decision.** There are exactly eight machine-readable error codes, and no
endpoint can add a ninth.

```
BadRequest 400   Unauthorized 401   Forbidden 403         NotFound 404
Conflict 409     UnprocessableEntity 422   RateLimited 429   Internal 500
```

**What it means for you.** Your clients switch on a fixed enum. A client written
today handles every failure any rig endpoint will ever produce; adding an
endpoint never breaks it.

**What it costs.** You cannot mint `INSUFFICIENT_CREDITS` as a top-level code.
It goes in the message, or in the validation fields, or in your own body.

**The alternative.** Per-endpoint error codes, which are more expressive and
turn every client into a growing switch statement with a `default:` branch that
nobody has thought carefully about.

Status codes alone are too coarse for this — three unrelated failures all return
400 — and codes alone leave clients guessing about retry behaviour. Both, from a
fixed set, is the combination that lets a client be written once.

---

## Permissions are on by default

**The decision.** `api.permissions` defaults to `derived`. Every endpoint gets a
permission from its resource and operation, and the check is generated.

**What it means for you.** Turn rig on and an authenticated caller holding no
grants reaches nothing. For a project that had no authorization, that is a real
behaviour change and it will look like everything is broken.

**What it costs.** You have to grant things before anything works.

**The alternative.** Defaulting to open, and letting people opt in. Which means
the projects that never got round to it are unprotected and nobody wrote that
down anywhere. `permissions: none` is one line, and having to write it is the
point: being unprotected should be a decision in a file, not the absence of one.

---

## A token is a database read, not a signature

**The decision.** rig's credentials are opaque strings. Verification is one
indexed row read. There are no signing keys, no key rotation, no JWKS.

**What it means for you.** Revocation takes effect on the **next request**. Not
at the end of a token lifetime, not when a denylist propagates — immediately,
because there is no signed document to wait out.

**What it costs.** One indexed read per request, where a JWT costs a signature
verification and no I/O.

**The alternative.** JWTs, which are faster and stateless and cannot be
un-issued. Every real system built on them eventually grows a revocation list,
at which point it is doing the database read anyway, plus the key management.

[auth.md](auth.md) has the whole story, including what to do if that read ever
actually shows up in a profile.

---

## `ON DELETE CASCADE` is refused

**The decision.** A foreign key declaring `ON DELETE CASCADE` is an error
([RIG6040](diagnostics.md)), not a warning.

**What it means for you.** You write the deletion of children yourself, or you
soft-delete them.

**What it costs.** More code for a genuinely simple parent-child cleanup.

**The alternative.** Allowing it. A cascade is a delete your application never
sees: no lifecycle hook runs, nothing is notified, no snapshot is taken, no
audit column is written, and the rows are simply gone. Everything else in rig is
built on the premise that a write is observable. A cascade is a hole straight
through that, punched by the database, on a code path nobody is looking at.

---

## One document, many generators

**The decision.** `rig generate` compiles your schema and configuration into a
single intermediate document, then hands the same document to every generator.

**What it means for you.** The router, the client, and the documentation cannot
disagree about what your API looks like, because all three are reading the same
description of it. You can also look at that description:

```bash
rig ir
```

The encoding is canonical, so the output is stable enough to commit and diff.
Doing that across a refactor is the most direct way to see exactly what a
migration did to your API.

**What it costs.** A generator can only express what the document can carry.
Adding something genuinely new usually means teaching the document about it
first.

---

## rig is not in your deployment

**The decision.** Generated code depends on `rig/runtime` (and optionally
`rig/auth`, `rig/migrate`). It never depends on rig itself.

**What it means for you.** rig is a build-time tool. No rig binary runs in
production, and nothing your server does at runtime is version-coupled to the
tool that generated it.

**What it costs.** The runtime modules are a real dependency you are taking on,
and they have to stay small and stable because of it.

This is also why `rig db up` starts a container and `runtime/serve` does not.
Container handling lives in the CLI, where it cannot end up in your deployment —
a server that boots its own Postgres is the wrong thing to copy into a real
project.

---

## Related

- [concepts.md](concepts.md) — what the above adds up to
- [schema.md](schema.md) — the conventions the first decision produces
- [auth.md](auth.md) — the same style of reasoning, applied to credentials

# Live sync

> **Partly written.** The shapes a table gets are below, running the sync
> service is [its own section](#running-electric-alongside-your-application),
> deploying it is [the one after
> that](#deploying-the-sync-service), and what a client does with the shapes is
> in [clients.md](clients.md#live-sync). Still to come: the scoping function.
>
> Until it exists, the `electric:` block in [tables.md](tables.md#electric) is
> the complete configuration reference, and `server-go`'s `electric_url` and
> `stub_dir` options are in [generators.md](generators.md).

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

`server-go` writes nothing about live sync until some table opts in, so leaving
`electric_url` configured in `rig.yaml` costs nothing. The shapes are not a
generator of their own: a shape route is an API route, and it is mounted on the
same mux, identifies its caller with the same claims lookup, and refuses with
the same error mapper as every other route this project answers.

That last part is worth reading twice if you have a `throttle:` block, because
these routes go through it too. A subscription is a long poll — held open, then
answered, then reissued — so one open shape is roughly three requests a minute
per subscriber, and four open shapes about a dozen. Against the standard limits
that is nothing (they are a thousand a minute per account), but a limit you
tightened for your own API now applies here as well, and a refused poll is a
subscriber that stops receiving rather than one slow response.

## Three shapes, decided by your columns

The one above is the **live** shape: the rows an ordinary read returns. A table
whose schema keeps more than that gets more than one route, and you configure
none of it — `deleted_at` is what says this table has a trash, and the snapshot
columns are what say it has a history, the same way they decide whether the API
has a `GET /_deleted`. Asking you a second time would only create a way for the
two answers to disagree.

| Route | Carries | Needs |
|---|---|---|
| `GET /api/v1/todo/_stream` | Live rows. Not deleted, not a snapshot. | — |
| `GET /api/v1/todo/_deleted/_stream` | Retired rows — the trash. | `deleted_at` |
| `GET /api/v1/todo/{id}/_versions/_stream` | One row's previous versions. | the snapshot columns |

`_stream` is always the last segment, so a shape's route is the read surface it
streams plus one marker, and nothing ahead of the marker changes meaning because
it is there. What comes first is the **table** name — not the resource's plural
path segment, which is what keeps a shape from colliding with the endpoints
beside it (`/api/v1/todos/...`). The `/api/v1` is your `api.base_path`: shapes
are served by the same mux as the rest of your API and sit under the same
prefix.

The columns are the whole rule, and `operations:` is not part of it. The API
needs `List` before it offers a `GET /_deleted` and `Get` before it offers a
`GET /{id}/_versions`, but live sync is its own read surface — a table with no
`operations` at all still gets shapes, which is how rig's own unexposed tables
are subscribed to. Reading `operations` here would leave such a table with a
live shape and no trash.

Every one of them is filtered to the caller's tenant, and to the caller's own
rows on an owner-scoped table, exactly like the live shape.

### Scoping them

Each gets its own field on `api.Shapes` and its own stub — beside the service
stub for the same table — so you can narrow a trash stream without touching a
live one. **While the field is nil, the route uses the live shape's scope.** The
trash and the history carry the same table's rows, so a check the live shape
needed — team membership, a share table, whatever rig cannot read off a column —
is almost always a check these need too, and inheriting can only ever show a
subscriber less. Setting the field replaces the
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

That rule about moving predicates is not only about the trash. It is the reason
[presence](presence.md) puts its freshness test in the subscriber rather than in
SQL: "seen in the last minute" is the same kind of predicate, and a row that
simply stopped being written would never fire the filter again. Anything you were
thinking of expiring inside a shape belongs on that page first.

## Running Electric alongside your application

The shapes are served by ElectricSQL — a separate service that follows your
database over logical replication — with rig's generated endpoints standing in
front of it. Locally, `rig db up` runs it for you:

```yaml
database:
  port: 55440
  electric:
    enabled: true      # image, container_name and port all have defaults
```

That brings up a second container beside the database, pointed at it and
waiting until its replication stream reports active. `rig db down` stops both
and `rig db reset` rebuilds both — the sync service holds a replication slot
*in* the database, so a database thrown away takes the slot with it, and rig
recreates the service rather than leaving it following nothing. Enabling the
block also adds `wal_level=logical` to the database's settings, which logical
replication requires and which cannot be turned on after Postgres has started.

Four settings in four places have to agree, and each names a different thing:

| Where | What it says |
|---|---|
| `database.electric.port` in rig.yaml | where the local sync service answers |
| `server-go`'s `electric_url` | where the generated proxy forwards |
| `electric: {enabled: true}` in a table's yaml | that the table has shapes at all |
| `ALTER PUBLICATION` in a migration | that Postgres will replicate the table |
| the role in the sync service's `DATABASE_URL` | who it connects to Postgres as |

So a project on port 55445 writes `electric_url: http://localhost:55445`, or
overrides it at runtime the way the examples do, with an environment variable
read in `main.go`. `rig db url --electric` prints the address rather than making a
Makefile repeat the port — and it is the only correct answer under
`RIG_DB_ISOLATE`, where the port is assigned when the container starts. The
fourth is [the next section](#the-table-has-to-be-published), and the fifth is
[deploying it](#deploying-the-sync-service).

What happens to a subscription while that service is unreachable is
[its own section](#when-the-sync-service-is-down).

**The server says at boot whether it found it.** The generated `Mount` asks the
sync service's health endpoint once, before the first connection is accepted,
and logs what it found:

```
INFO  the sync service is answering
WARN  the sync service is not answering  error="…connection refused"  hint="run `rig db up` to start the sync service, or set $ELECTRIC_URL"  cost="a shape with a fallback serves a snapshot; the rest answer 502"
```

A warning, and the server starts — which is the right default, because a shape
with a fallback serves a snapshot through an outage and every route that is not
a shape never touched the sync service at all. For an application whose pages
*are* shapes, say so and it refuses to start instead:

```yaml
  - name: server-go
    options:
      electric_required: true
```

That also puts the sync service in `ReadinessPath`, so an instance that loses it
is taken out of the load balancer rather than left in it answering nothing. Both
halves are off by default for the same reason: coupling readiness to a
dependency you degrade gracefully without turns one sync outage into every
replica dropping out at once.

A project with the [monitoring page](observability.md) gets the same answer as a
status pill beside the request list, without the flag and without waiting for a
subscriber to discover it — the page holds `Proxy.Health` and asks it on every
poll. `Proxy.SyncReachable` is the cheaper, different question: what the requests
that actually happened found. See [Rig stops asking](#rig-stops-asking).

**Mounting them is one field on `api.Handlers`.**

```go
mux := api.Register(api.Handlers{
	Server: api.Server{Auth: front, DB: pool, Logger: log},
	Todo:   todos,

	Shapes: api.Shapes{
		App:   app,     // what the drain registers on
		Proxy: proxy,   // nil mounts no shape route at all
		Todo:  todo.Shape,
	},
})
```

`api.Shapes` has a field per shape, plus the proxy the routes forward through
and the `serve.App` their ending registers on. Setting `Proxy` mounts every
stream endpoint and registers the drain in the same call; leaving it nil mounts
none of them, which is what a project that generated its shapes and has not
written the front end yet wants.

`App` may be nil when you own the ending yourself — a test building this handler
from a bare pool, which drains the proxy in its own cleanup. rig says so on the
way past rather than assuming you meant it.

A live subscription is a request the server is deliberately not answering yet.
That makes it an in-flight request, so `http.Server.Shutdown` waits for it — and
waits, because nothing in the poll is late and `Shutdown` does not cancel a
request's context. Without the drain one open browser tab is a shutdown that
spends its whole budget waiting for the sync service to have news, and a sync
service that hangs rather than refuses is a shutdown that spends all of it. What
goes without in either case is everything registered after: the trace flush, the
presence sweep, and the notification engine's close, which is where its claims
are handed back.

`Register` registers `Proxy.Drain` as a drain step, so the polls end at the
*start* of the shutdown, once readiness is already false and there is nothing
left to gain from holding a subscription open. A subscriber gets a 503 with a
`Retry-After` and resumes from the same offset against whichever replica is still
serving — deliberately not a fallback snapshot, since this process is going away
and the next attempt is somebody else's to answer. Nothing is lost, because a
poll that had not answered had nothing in it yet.

Its five seconds are already in `api.ShutdownBudget()`, and
`serve.Config.Shutdown: api.Shutdown{Shapes: ...}` is how a deployment asks for
another — see [services.md](services.md).

In a deployment, run Electric as a service of its own against your real
database and point the proxy's URL at it. Nothing else changes: the proxy is
the only thing a browser ever talks to, so the sync service itself stays
unreachable from outside — which is the point of the proxy, and why
`ELECTRIC_INSECURE=true` on the local container is not the setting it sounds
like.

`examples/linearlite` is the worked example: a table with shapes, the proxy
wired in `api/internal/app`, and a front end subscribed from a browser. It is
also the one place to read a **filled-in scope stub** —
`api/internal/services/rig_presence` — because presence is the shape where
narrowing is not an optimization: a heartbeat is a row change delivered to every
subscriber, so the scope is what decides whether the feature is affordable at
all. See [presence.md](presence.md#scope-and-what-presence-costs).

## The table has to be published

Logical replication carries only what a publication names, and a shape is a
filter in front of that stream. The sync service will publish a table for you —
that part is covered below, and it is why this is easy to never think about — but
it can only do so where its database role owns the table, which is exactly the
privilege a production deployment tends not to grant it. Say it in a migration
and the answer stops varying by environment.

rig writes that migration, because rig is what knows which tables stream:

```
$ rig migration new publish_shapes --publish-shapes
created migrations/00007_publish_shapes.sql
  publishing:        rig_notification_recipient, rig_presence, todo
  identity to FULL:  rig_notification_recipient, rig_presence, todo
```

What it writes is the two statements Postgres gates on ownership, for every table
with a shape:

```sql
-- Postgres has no CREATE PUBLICATION IF NOT EXISTS, hence the block.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_publication WHERE pubname = 'electric_publication_default') THEN
        CREATE PUBLICATION electric_publication_default;
    END IF;
END
$$;

ALTER PUBLICATION electric_publication_default ADD TABLE todo;

-- The second half of the same job. Electric wants the whole old row on an
-- update or a delete, and Postgres gates this on ownership exactly as it gates
-- the line above — so a migration that publishes and stops has moved only one
-- of the two things a least-privilege deployment cannot do for itself.
ALTER TABLE todo REPLICA IDENTITY FULL;
```

**It reads the database, so running it twice is not writing the same file
twice.** A table that gains a shape six months later gets a migration adding that
one table to the publication that is already there — not a rewrite of the one that
is already applied, which goose would refuse anyway. It says what it left alone,
on the terminal and in the file. And when nothing is missing it writes nothing:

```
$ rig migration new publish_shapes --publish-shapes
every streaming table is already published with REPLICA IDENTITY FULL; nothing to write
```


The publication is the sync service's own — `electric_publication_default` — and
that is deliberate rather than a collision: it reads only that one. [Deploying the
sync service](#the-publication-has-to-be-the-sync-services-own) is why, and it is
worth reading before the first deployment rather than after.

`rig validate` refuses a table that streams and is in no publication
([`RIG5090`](diagnostics.md#the-four-that-read-the-database-rather-than-your-files)),
one whose replica identity is not `full` (`RIG5093`), one that is `UNLOGGED` and
therefore writes no WAL at all (`RIG5091`), and a server whose `wal_level` is not
`logical` (`RIG5092`). All four read the
database `rig generate` already reads — after your migrations have been applied,
which is why the migration is where the answer belongs.

**What the sync service does, and where it stops.** Electric maintains a
publication of its own — `electric_publication_default` — and adds a table to it
the first time somebody requests a shape on that table. So on your machine, with
the superuser `rig db up` hands it, this mostly just works and `RIG5090` reads
like bureaucracy.

It stops in two places, both of them somebody else's environment. Postgres wants
ownership of a table both to add it to a publication and to set `REPLICA
IDENTITY FULL`, and Electric does both — so a least-privilege role cannot, and
the way you find out is a subscription failing with an error about access rather
than anything about replication. And a deployment running with
`ELECTRIC_MANUAL_TABLE_PUBLISHING=true` has told Electric not to try: there,
membership is entirely yours to maintain and Electric only checks it.

A publication your migrations own is true the moment they run, under any role,
in every environment. Electric keeps its own alongside it — a table can be in
both — so declaring it costs nothing where the automatic path would have worked
anyway. That is the whole argument for the rule: not that your stream is empty,
but that whether it is empty depends on a privilege you probably have locally and
probably will not have in production.

Which is why `RIG5090` does not accept `electric_publication_default` as an
answer. Once you have opened the app on your machine, Electric has published the
table there, and a rule that counted it would go quiet on every developer's
database and speak up only where nobody has run anything — the exact opposite of
what it is for. A table carried by that publication and no other is still
reported, and the message says so. Adding a table to it by hand is not the fix
either: it is Electric's to maintain, and it does not exist in the deployment you
are being warned about.

**rig's own tables need it too.** `notifications:` gives
`rig_notification_recipient` a shape and `presence:` gives `rig_presence` one,
neither of which you asked for — a client reads its inbox and who else is here
as streams, or not at all. So a project with either block has a table to publish
even if none of its own tables mention live sync. `examples/auth` is that case
and its migration publishes exactly one table.

## Deploying the sync service

Everything above is true on a laptop, where `rig db up` hands the sync service the
superuser of a throwaway database. A deployment is the same three facts and one
more: **who the sync service connects to Postgres as.**

It should not be the owner of your tables, and it certainly should not be the
master user. What it needs is narrow — replication, `CONNECT`, `CREATE` on the
database, and `SELECT` on what it streams — and rig carries the migration that
creates exactly that role:

```
electric      LOGIN, REPLICATION      (or a member of rds_replication)
              CONNECT, CREATE ON DATABASE current_database()
              USAGE ON SCHEMA public
              SELECT ON ALL TABLES, and on every table added after
```

You do not write it. A project with a table that streams gets
`rig/runtime/electric`'s migration set alongside `rig/auth` and the rest — the
generated `MigrationSources` names it, so `rig generate` is the whole of adopting
it. `CREATE` is on the list because the sync service creates its own publication
and replication slot on first connection, and `CREATE PUBLICATION` is a
database-level privilege.

Two things about that migration are worth knowing rather than rediscovering:

- **It branches on the database, not on an environment variable.** Setting the
  `REPLICATION` attribute needs a true superuser, and a managed Postgres gives
  nobody one — on RDS the master user has `CREATEROLE` and not `rolsuper`. So the
  migration grants `rds_replication` where that role exists and sets the attribute
  where it does not. Nothing to remember to configure.
- **The `SELECT` grant has to be a migration.** `ALTER DEFAULT PRIVILEGES` is
  recorded per grantor and per schema: it covers tables created by the role that
  ran it and no others. Run it from anywhere but the role that runs your
  migrations and it covers what existed at the time and nothing added later — and
  the way that fails is a shape that comes back **empty** rather than one that says
  why.

### The role has no password

Deliberately: the migration is a file in git, so nothing in it can be a
credential. Give it one at boot, out of the same secret the sync service itself
reads as `DATABASE_URL` — one secret with two readers rather than a second holding
a duplicate that drifts:

```go
Migrate: func(ctx context.Context, pool *pgxpool.Pool) error {
	if err := migrate.RequireAll(srcs, migrate.Options{})(ctx, pool); err != nil {
		return err
	}
	// Empty everywhere there is no sync service, where this does nothing.
	_, err := electric.SetRolePassword(ctx, pool,
		os.Getenv("ELECTRIC_DATABASE_URL"), electric.Role)
	return err
},
```

It sends a SCRAM-SHA-256 verifier rather than the password, and that is the point
rather than an optimisation. A managed Postgres commonly logs DDL and captures
statement text for performance insight, and Postgres redacts a `PASSWORD` literal
in neither — which is why `psql` has `\password`. A verifier holds `StoredKey`,
which lets a server check a proof but not produce one, so unlike an MD5 hash it is
not password-equivalent and cannot be replayed to log in. No error it returns
carries the connection string, the password or the verifier.

The password has to be ASCII alphanumeric. SCRAM normalisation is the identity map
on that alphabet and not in general, so anything outside it would need normalising
before hashing or the verifier would not match — and rather than normalise
silently, this refuses. Whatever generates the password stays inside the alphabet.

### The secret goes on the proxy, not on the browser

A deployed sync service takes a secret, and it takes it as a **query parameter** —
which is why Electric's own documentation says it must never be sent from a
client, and why the sync service does not belong on a public load balancer at all.
The thing that holds the secret is rig's proxy:

```go
proxy, err := electric.New(electric.Config{
	URL:    os.Getenv("ELECTRIC_URL"),
	Secret: os.Getenv("ELECTRIC_SECRET"),
	DB:     pool,
}.Defaults())
```

So the browser calls your API, your API calls the sync service on a private
address, and shape data comes back as your API's response. A client's own
`Authorization` header is never forwarded upstream.

`Secret` is a field rather than an entry in `Extra` because the proxy has to know
the value in order to take it back out of what it reports. The credential is on
the URL of every upstream request, a transport failure is an `*url.Error`, and
that renders the URL it was given — so without this the secret reaches your
`OnError` and then your log the first time the sync service is unreachable. With
it, every error the proxy reports has both the plain and the percent-encoded
spelling replaced by `[redacted]`, and `errors.Is` still reaches what failed.

A secret you already had in `Extra["secret"]` — the only place there was before
this field — is redacted the same way, so nothing has to change for the leak to
stop. Setting both is refused rather than reconciled: one of the two would be
silently dropped, and it may be the one you meant.

### The publication has to be the sync service's own

This is the part of least privilege that is not finished, and it is worth stating exactly
because none of the errors say it.

**The sync service reads only its own publication.** It is named
`electric_publication_<replication stream id>` — `electric_publication_default` unless the
deployment sets one — and it creates it on first connection if nothing else has. A
publication under a name of your own is *never consulted*, so a table published only there
satisfies nothing at run time however green `rig validate` is.

The two ways that fails, verified against a real server, since neither mentions a
publication the project never named:

```
must be owner of table lesson
```

with the default settings — the service trying to add the table to its own publication and
lacking the ownership Postgres requires. And:

```
Database table "public.lesson" is missing from the publication
"electric_publication_default" and the ELECTRIC_MANUAL_TABLE_PUBLISHING setting
prevents Electric from adding it
```

with `ELECTRIC_MANUAL_TABLE_PUBLISHING=true`, which does not mean "use the publication my
migrations wrote". It means "the table must already be in *mine*, and I will not add it".

**What works,** end to end, with the least-privileged role and no table ownership anywhere:
have the migration create the publication the service is going to look for, before it ever
runs. That is what `rig migration new --publish-shapes` writes, and it is why the ordering
inside `rig db up` matters — migrations first, then the sync service.

```sql
CREATE PUBLICATION electric_publication_default;
ALTER PUBLICATION electric_publication_default ADD TABLE todo;
ALTER TABLE todo REPLICA IDENTITY FULL;
```

The publication is then owned by the role that runs migrations rather than by the sync
service. That is not a detail: adding a table needs the publication's owner, and it is also
the one fact that tells this apart from the service having built the publication itself.
`RIG5090` reads exactly that — it accepts an Electric publication your migrations own, and
still refuses the same publication owned by the service, because a table in *that* one got
there on privileges the service will not have elsewhere.

**The deployment has to set `ELECTRIC_MANUAL_TABLE_PUBLISHING=true`,** and the reason is
sharper than "otherwise it tries and fails". Left on its default the service reconciles its
publication on boot and on every subscription — adding tables a shape needs, and *dropping
every table no shape currently needs*. So a migration that published three tables has its
work silently undone the moment the service starts. `rig db up` sets it on the local
container for the same reason, which is what makes the local arrangement the same one rather
than a second one that happens to agree.

**One state to hand over first.** A database where the service created the publication
before any migration did — which is every deployment that was running before the project had
this migration. `--publish-shapes` refuses rather than writing an `ALTER PUBLICATION` that
Postgres would deny, and says how to get out of it. On a managed database, one statement:

```sql
ALTER PUBLICATION electric_publication_default OWNER TO <the role that runs migrations>;
```

It loses nothing — the sync service keeps running, rebuilds its slot if it has to, and a
publication it no longer owns is one it will not empty. Then run the command again. On a
throwaway database `rig db reset` does the same thing by dropping the publication with the
database and letting the migration create it first.

### Two traps that are not rig's to fix

Worth writing down because both cost an afternoon:

- **`/v1/health` answers `202` while it is starting**, with
  `{"status":"starting"}`. A container health check using `curl -f` calls that
  healthy and routes traffic to a service that will hang the first shape request.
  Match on `200` specifically. rig's own probe does — it requires a 200 *and*
  `active` in the body — which is what `CheckSyncService` reports at boot.
- **`ELECTRIC_TCP_READ_TIMEOUT` must exceed any load balancer's idle timeout.** A
  live shape request is a long poll that deliberately hangs; rig's own
  `PollDeadline` is five minutes.

## When the sync service is down

A shape endpoint is a filter in front of the sync service, so by default a sync
service that cannot be reached is a **502** and a subscriber with no rows. On a
page whose list *is* a shape, that is a blank page.

Give the proxy a database and it answers every shape from the shape's own
filter:

```go
proxy, err := electric.New(electric.Config{
    URL: os.Getenv("ELECTRIC_URL"),
    DB:  pool,   // the whole of it
})
```

That is the whole of it — one field, and there is nothing to write per shape and
nothing to keep in step. **A shape is a `SELECT`**: rig already built the filter
that decides which rows it holds, sent it to the sync service, and has it in
hand. Answering from the database is running that same predicate somewhere else.

Which is also why there is nothing here that can go quietly wrong. Whatever your
[scope](#scoping-them) narrowed, the snapshot narrows, because it is the same
`WHERE` clause — not a second description of the shape that somebody has to
remember to change twice. The rows go out in the sync protocol's own format, so
**a subscriber needs no change and cannot tell** — `X-Rig-Sync-Fallback` on the
response is the only sign, and it is there so a browser's network tab and your
logs have one. `snapshot` on the rows themselves, and `must-refetch` on the 409
that sends a resuming subscriber to fetch them, which is what tells that 409
apart from the sync service's own.

Settings on the proxy, none of them with defaults — `electric.New` refuses a
config that left one empty, because each governs what a subscriber sees while the
sync service is away and a value the package chose would be one nobody chose. The
`Default…` constants beside each field are what to write.
`electric.Config.InitialTimeout` (10s)
is how long a first read waits for the sync service to *begin* answering before it
counts as unreachable — the answer itself is then copied out however long it
takes. `MaxSnapshotRows` (20,000) is how large a snapshot may be, applied as a
`LIMIT` so the rows past the bound are never read, and `SnapshotTimeout` (5s) is
how long one read may take. The response's own clock is not one of these
settings: `serve.Config.WriteTimeout` (30s) is set once for every route in the
application and would otherwise cut a live poll's answer off on the way out, so
the proxy replaces it for the request it is serving with `electric.PollDeadline`
(5m) — the same mechanism a file transfer uses, and the reason neither needs a
field on `serve.Config`. `OnError` is worth setting too — it is the only way
the reason for a 502 on a shape route reaches your log, and every error it is
handed names the shape's table, so an outage across four shapes is four lines you
can tell apart rather than four copies of one.

**A refused shape route answers the same error envelope as every other route** —
flat JSON with `code` and `message`, which is what `@rig-ts/client`'s error
predicates read. Setting `OnError` on the generated `Server` to your API
server's error writer adds the request identifier and puts the failure in the
same log line as the rest; leaving it nil still answers the envelope, without
the identifier.

### Rig stops asking

A sync service that is down is down for every shape and every subscriber, so
asking it once per request means each of them paying `InitialTimeout` to learn
what the request before it learned: a held goroutine, a held connection, and a
spinner for ten seconds in front of a snapshot that was ready immediately.

So the proxy counts. After `BreakerThreshold` failures **in a row** (5) it stops
asking for `BreakerCooldown` (5s), and every request in that window is answered
from here — the snapshot where the shape has a fallback, and the status it always
had where it does not. When the cooldown is up, *one* request is let through to
find out; if it succeeds the circuit closes and everything is forwarded again. A
single failure among successes never counts, and nothing polls in the background,
so a service that comes back is found by the next subscriber through the door.

Two ways to know, and neither is the error log:

```go
proxy, err := electric.New(electric.Config{
    URL: os.Getenv("ELECTRIC_URL"),
    OnSyncState: func(ctx context.Context, reachable bool) {
        // Twice per outage rather than once per request. This is the line to
        // alert on.
        log.WarnContext(ctx, "live sync", slog.Bool("reachable", reachable))
    },
})
...
proxy.SyncReachable() // the same answer, for a health endpoint or a banner
```

A third, when the question is "is it there *now*" rather than "what did the last
requests find":

```go
err := proxy.Health(ctx) // one GET at the sync service's own health endpoint
```

It is a probe and touches the circuit in neither direction — a status page
polling every few seconds must not be what stops the proxy serving live shapes,
and a probe that succeeded must not put a subscriber back onto a service it has
not tried itself. Answering HTTP is not enough for it: the sync service comes up
before it has connected to Postgres and reports its own state in the body, and a
shape request in that gap hangs rather than fails. This is what the boot check
and the monitoring page's pill both ask.

The trade is a sync service that is fine behind a network that briefly is not:
those requests are answered from a fallback, or refused, for as long as one
cooldown — where without the circuit they would have waited and then usually
succeeded. `BreakerThreshold: -1` turns it off and every request goes on asking.

`examples/linearlite` has a button in its header that stops the sync service's
container and starts it again, so all of this is something to watch rather than
read: the board surviving on a snapshot, the lag in both directions between the
container's state and `SyncReachable`, and the subscription recovering onto real
sync. Its README calls the walkthrough "Take the sync service down".

### What a subscriber actually gets

**A snapshot, not a stream.** Correct at the moment it was read and not updated
afterwards. What follows is:

| Request | While the sync service is gone |
|---|---|
| a read from the beginning | the snapshot, with a handle of rig's own |
| **a poll resuming from the sync service's own handle** | **`409 must-refetch`, so the subscriber reads from the beginning and lands on the snapshot — or `502` and its rows kept, if the fallback would have refused** |
| the poll after the snapshot | `503` and a `Retry-After`. The subscriber keeps its rows |
| that poll, once the service is back | `409 must-refetch` — the sync service's own answer to a handle it never issued, so the subscriber starts again on real sync |

**The second row is the one to understand, because it is the difference between
a degraded board and a board that was only ever saved by a reload.** A tab that
was already streaming when the outage began is not asking to read the shape; it
is asking what changed since an offset, and a snapshot is not a smaller answer
to that question. So the answer for as long as the outage lasted used to be a
502 per poll, forever — on a page nobody reloads, because the rows are still on
the screen. Being told to start again is what gets it somewhere: the request
after a `must-refetch` *is* a read from the beginning, which is the one request a
snapshot answers.

It is `must-refetch` in both directions and that is not a coincidence — it is the
same mechanism used twice, because the protocol already has exactly one way to
say "the handle you are holding is no good, start over". Only a subscription
that has somewhere to start again *to* is told it. For anything else the 502
stands, since resetting one that does not would cost it the rows it is holding
and then refuse the request anyway.

"Somewhere to start again to" is checked rather than assumed, and it is a
stronger condition than the shape having a fallback: a read that fails — or one
past `MaxSnapshotRows` — leaves the rows where they are instead of taking them
and then refusing the read it sent the subscriber to make. Asked as one row
rather than by building the snapshot and discarding it, since the question is
whether there is one and not what is in it.

A subscription therefore survives an outage in both directions without a reload,
and rig arranges only the first half of that: the recovery 409 is the sync
service's own.

**A snapshot is read per request, not once per outage.** Every read from the
beginning reads the table again, so what comes back is the table as it is at that
moment. The sync service was the thing that was down, not the API, so a
write during an outage commits — and the next read from the beginning has it.
What is frozen is not the data; it is one subscription's copy of it. A tab that
reloads, a tab that opens, and a tab that was just told to start again all see
current rows, while a tab holding a snapshot taken a minute ago holds a
minute-old one until something makes it read again.

**Which means the thing to tell a front end is that a write will not echo.** A
subscriber that renders a change optimistically and clears the overlay when the
stream confirms it is waiting for a confirmation that cannot arrive, so what a
person sees is whatever that code does on its timeout — usually the change
appearing and then reverting, over a row that held the new value the whole time.
It corrects itself when the sync service comes back, and a reload before then
shows the new value too, which is the confusing part: the board is more correct
after a reload than the tab that was watching. `examples/linearlite`'s
`usePendingMoves` is exactly this shape of code and step 3 of its README is what
it looks like. A page with optimistic writes has more reason to ask
`Proxy.SyncReachable` than a read-only one does.

It is not a repository read, and that is deliberate: it goes to the table with
the shape's own projection and the shape's own `WHERE`. So each of the three
shapes answers with its own generation of the row without anything being said
about it, and the trash no longer differs from its stream over
`restore_window_days` — a repository applies that window and
[a shape does not](#three-shapes-decided-by-your-columns), and this is the shape.

### Two things to get right

**A sync outage becomes database load.** Every subscriber falls back at the same
moment, because what they have in common is the service being gone — so an
outage is one read per shape per subscriber against the database the sync
service was shielding. Every subscriber, not only the ones arriving: a tab that
was already streaming is told to start again and reads too, which is the price of
it not needing a reload.

Two settings bound it, and one shape of query keeps it from being worse.
`MaxSnapshotRows` refuses rather than truncates, because a subscriber cannot tell
a short answer from a complete one — as a `LIMIT`, so the rows past the bound are
never read. `SnapshotTimeout` (5s) is how long one read may take before it gives
up and lets the next request through. And the read that decides whether a
resuming tab has a snapshot to start again *to* asks for one row rather than all
of them, so a tab that was already streaming costs a little more than the tab
beside it rather than twice as much.

Leaving `Config.DB` nil is still available, and is what a deployment that would
rather answer 502 than take that load should do.

**Some shapes should not have one, and rig knows about the main one.**
[Presence](presence.md) is the clearest case: a snapshot of who was here a moment
ago, that then stops updating, is worth less than an empty list, because the
feature *is* the freshness. rig gives its own presence shape no fallback, so
that is not a line you write. For one of your own, say so on the table:

```yaml
electric:
  enabled: true
  fallback: false
```

One key covers all three of the table's shapes.

## Subscribing to one

`ts-client` writes a factory per shape, and a subscriber chooses which rows it
wants by choosing a factory. One thing to know before you write against them: a
streamed row carries column names where the API sends its own keys, so the
generated types are `TodoRow` and `Todo` rather than one type used twice.

Both are in [clients.md](clients.md#live-sync).

### A stream pauses in a hidden tab

The most surprising thing about subscribing from a browser, and the first thing to
check when live sync looks broken: **the sync client stops polling while
`document.visibilityState` is `hidden`.** A backgrounded tab, a tab behind another
window, a minimised window — none of them receive, and each resumes where it left
off when it comes back.

That is the right behaviour and it costs nothing to somebody using your
application. It costs a great deal to somebody testing one, because the obvious
way to test live sync is two tabs of one window — where only one of them is ever
visible, so the other looks dead. Two windows side by side, both visible, is the
arrangement that works; so is spoofing `visibilityState` from a browser driver.

Nothing in rig changes this and nothing should. A tab nobody is looking at asking
for updates every twenty seconds is a cost with no reader — which is also why
[presence](presence.md) stops heartbeating when a tab is hidden rather than
working around it.

## See also

- [clients.md](clients.md#live-sync) — subscribing from a browser
- [presence.md](presence.md) — the other thing a shape is used for, and what the
  moving-predicate rule decides there
- [tables.md](tables.md#electric) — the configuration keys
- [api.md](api.md) — what a shape route answers with, including the two failures
- [generators.md](generators.md) — `electric_url`, `stub_dir`
- [cli.md](cli.md) — `rig db url --electric`, `rig migration new --publish-shapes`
- [diagnostics.md](diagnostics.md#the-four-that-read-the-database-rather-than-your-files) —
  `RIG5090` to `RIG5093`, the four rules that read the database

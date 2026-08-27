# Live sync

> **Partly written.** The shapes a table gets are below, running the sync
> service is [its own section](#running-electric-alongside-your-application),
> and what a client does with the shapes is in
> [clients.md](clients.md#live-sync). Still to come: the scoping function.
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

rig builds the tenant and lifecycle predicates itself — a subscriber of that
endpoint cannot see another tenant's rows, or soft-deleted ones, or snapshots.
Your declared params are handed to a scoping function rig writes a stub for, and
that function can only **narrow** the shape further. There is no way for it to
widen one.

The generator writes nothing at all until some table opts in, so leaving it
configured in `rig.yaml` costs nothing.

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

Each gets its own field on `Handlers` and its own stub, so you can narrow a
trash stream without touching a live one. **While the field is nil, the route
uses the live shape's scope.** The trash and the history carry the same table's
rows, so a check the live shape needed — team membership, a share table, whatever
rig cannot read off a column — is almost always a check these need too, and
inheriting can only ever show a subscriber less. Setting the field replaces the
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

Three settings in three places have to agree, and each names a different thing:

| Where | What it says |
|---|---|
| `database.electric.port` in rig.yaml | where the local sync service answers |
| the `electric` generator's `electric_url` | where the generated proxy forwards |
| `electric: {enabled: true}` in a table's yaml | that the table has shapes at all |

So a project on port 55445 writes `electric_url: http://localhost:55445`, or
overrides it at runtime the way the examples do, with an environment variable
read in `main.go`.

What happens to a subscription while that service is unreachable is
[its own section](#when-the-sync-service-is-down).

In a deployment, run Electric as a service of its own against your real
database and point the proxy's URL at it. Nothing else changes: the proxy is
the only thing a browser ever talks to, so the sync service itself stays
unreachable from outside — which is the point of the proxy, and why
`ELECTRIC_INSECURE=true` on the local container is not the setting it sounds
like.

`examples/linearlite` is the worked example: a table with shapes, the proxy
wired in `internal/app`, and a front end subscribed from a browser. It is also the one
place to read a **filled-in scope stub** — `services/rig_presence` — because
presence is the shape where narrowing is not an optimization: a heartbeat is a row
change delivered to every subscriber, so the scope is what decides whether the
feature is affordable at all. See [presence.md](presence.md#scope-and-what-presence-costs).

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

Settings on the proxy, all with defaults. `electric.Config.InitialTimeout` (10s)
is how long a first read waits for the sync service to *begin* answering before it
counts as unreachable — the answer itself is then copied out however long it
takes. `MaxSnapshotRows` (20,000) is how large a snapshot may be, applied as a
`LIMIT` so the rows past the bound are never read, and `SnapshotTimeout` (5s) is
how long one read may take. `OnError` is worth setting too — it is the only way
the reason for a 502 on a shape route reaches your log, and every error it is
handed names the shape's table, so an outage across four shapes is four lines you
can tell apart rather than four copies of one.

**A refused shape route answers the same error envelope as every other route** —
flat JSON with `code` and `message`, which is what `@rig/client`'s error
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
- [generators.md](generators.md) — `electric_url`, `shape_import`, `stub_dir`

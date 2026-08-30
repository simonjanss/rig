# Presence

Who is here, and what they are looking at.

Turn it on and your application can show a teammate's avatar on the card they
have open, and a ring on the field they are typing into. There is no socket to
run and no service to add: presence is a table, a heartbeat is a write, and a
[live shape](electric.md) is how it reaches everybody else.

```yaml
presence:
  enabled: true
```

That gives you `rig_presence` in the [foundation](schema.md#names-rig-reserves),
three routes under `/presence`, a live shape at
`/api/v1/rig_presence/_stream`, a sweeper, and the wiring for all of it in your
generated API package. The browser half is
[`@rig-ts/presence`](clients.md#presence).

## What a presence row is

**One browser tab.** Not one person — the identity is the tab, because reading in
one and editing in another is the ordinary case, and a row keyed by account alone
would have two tabs overwrite each other on every heartbeat. The person would
appear to teleport between the two things they were doing.

So a tab names itself — `@rig-ts/presence` mints a key per tab — and the server
stores one row per `(tenant, account, session key)`. Two tabs are two presences,
and it is the reader's job to collapse them if it wants one avatar per person.
`others()` answers freshest first, so the first sighting of an account is the tab
that spoke most recently — which is the one whose activity a tooltip should
describe, because it is the one they are actually using.

## Where somebody is, in three levels

A target narrows, and each level is optional:

| | |
|---|---|
| `scope` | which part of the application — a board, a document |
| `targetTable` + `targetId` | which row |
| `targetField` | which control on it |

`scope` is required and the rest are not. A null `targetId` is somebody on a list
rather than on a row; a null `targetField` is somebody looking at a row rather
than typing into it. Neither gets a flag beside it saying so, because a column
that answers a question a nullability already answers is a second answer waiting
to disagree with the first.

`activity` is `viewing` or `editing`, separately, because a client may know that
somebody is editing before it knows which control has focus.

**`targetTable` is checked against your own tables** — the generated
`PresenceTargets()` is written from the compiled document — and that is a typo
boundary rather than a security one. The column reaches no SQL statement, so
there is nothing to inject through it; what the check buys is that a reader can
trust the value means a table rather than treating it as free text.

## Why a subscriber decides who is here

This is the one thing to understand about presence, and it is not a caveat — it
is the reason the feature is shaped the way it is.

[electric.md](electric.md) states the rule, about the trash shape: **the sync
service evaluates a shape's filter when a row _changes_.** A predicate that moves
on its own — "seen in the last minute" — would never fire again for a row that
simply stopped being written, and the row would sit in every subscriber's copy
forever, filtered in appearance and not in fact.

So the row carries `seen_at`, the last heartbeat, and nothing filters on it
server-side. Whoever is reading does the arithmetic:

> **Whoever is reading decides who is here. The sweeper decides how much of the
> past a new subscriber has to download.**

That is the inverse of [the inbox](notifications.md), where the durable row is
the truth and the engine is only latency. Here the reader's own comparison is the
truth: it is correct within a second, it costs nothing, and it works on the day
your project was generated, before anybody has wired a cron.

`@rig-ts/presence` does this for you, and there is one trick in it worth knowing
because it is what makes it correct. A browser cannot compare `seenAt` against
its own clock — a laptop five minutes fast would show an empty room. **The
freshest `seenAt` in the collection is itself a reading of the server's clock**,
taken at most one heartbeat ago by whichever tab beat most recently, so comparing
every row against the newest one cancels the skew entirely. No offset, no header,
no extra request.

## The sweeper, and why it is not the guarantee

Expired rows are deleted, and the delete is what converges every subscriber's
copy — a DELETE is a change, so it fires the filter that a moving predicate
could not. Without it a tab open for eight hours would hold a thousand dead rows.

Two ways to run it, and unlike the notification dispatcher **neither of them is
the guarantee**:

Both are already wired, and neither is a line you write. `api.Main` starts a
ticker before your own wiring runs — it builds the sweeper over `app.Pool`,
starts it, and registers its shutdown — and merges a `sweep-presence` subcommand
into `Tasks` for an operator who would rather it were a cron job than a
goroutine. Running both is not a mistake; see below. `api.StartPresenceSweeper`
is the call behind the first, exported for a project keeping the sequence itself.

Skipping both costs space and a slower first fetch, and nothing else — who is
present is still right.

**It is a goroutine at all, and that is a decision.** The dispatcher is a
subcommand because resolving an audience twice costs a read and sending twice
costs somebody a duplicate mail. `DELETE … WHERE seen_at < $1` is idempotent and
commutative: two replicas sweeping at once agree, and the loser deletes nothing.
There is no lease here because there is nothing racing can get wrong.

`grace` is what keeps the two mechanisms from contradicting each other. A
subscriber stops drawing a row at `ttl` and the sweeper deletes it at
`ttl + grace`, so a row is always invisible **before** it is gone — never the
other way round, which would be a row that came back when a slow client caught
up.

## Scope, and what presence costs

Every heartbeat is one row changed, and every subscriber to that shape hears
about it. So `heartbeat` is not a latency knob: **it is the write rate and the
fan-out rate, multiplied by tabs.** Read this before lowering it.

Fifty people with two tabs each, at the defaults:

| | per second |
|---|---|
| heartbeat writes | ~5 |
| focus-change writes | ~3 |
| WAL | ~2.5 KB/s |
| **shape messages, one tenant-wide scope** | **~800** |
| shape messages, scoped per screen (~8 people) | ~64 |

Eight single-row primary-key updates a second is nothing for Postgres. **The cost
is the fan-out, two orders of magnitude larger than the writes** — 800 messages a
second means every one of a hundred tabs is parsing eight changes a second and
re-running its live query.

Which is why the shape declares `scope` and `targetId`: a subscriber narrows to
the screen it is on, and the reduction is roughly the ratio of the tenant to that
screen. Narrow it in the scope stub rig writes for you, which arrives as a no-op
and is the one stub worth filling in before this reaches a real tenant:

```go
if p.HasScope {
    w.Eq("scope", p.Scope)
}
if p.HasTargetID {
    w.Eq("target_id", p.TargetID.String())
}
```

Both conditions stay optional, because a subscriber that wants the whole tenant —
a diagnostic page, a header bar showing everybody signed in — should not have to
invent a scope to ask for it.

Two more things about the cost, both intended:

**A hidden tab ages out, and that is the right answer.** The sync service pauses
a stream in a hidden tab, so it is not receiving; `@rig-ts/presence` stops beating
and sends a leave, so it is not broadcasting either. "Simon's tab is behind his
mail client, he is not editing your title" is true. It also stops the browser's
background-timer throttling from clamping a heartbeat below the TTL and making
that tab flicker for everybody else.

**The TTL can be generous _because_ the leave exists.** An ordinary close, a
navigation or a tab going to the background sends one, so the TTL only has to
cover a crashed tab and a dead network. That is what makes twenty seconds a
sensible heartbeat rather than five.

**And a sync outage empties the room rather than freezing it.** Every other shape
answers from the database while the sync service is unreachable — see
[electric.md](electric.md#when-the-sync-service-is-down) — and this one
deliberately does not, because a snapshot of who was here a moment ago that then
stops updating is worth less than an empty list: the feature *is* the freshness.
It is also the one that would come out right anyway. The heartbeat is a REST call
an outage does not touch, so it goes on supplying a fresh reading of the server's
clock while the streamed rows sit still, and `@rig-ts/presence` ages them out at the
TTL. A fallback here would buy a minute of ghosts and then the empty room it
already shows.

rig decides this for its own presence table, so it is not a line you write and
not one you can forget. For a table of your own,
[`electric: {fallback: false}`](tables.md#electric) says the same thing.

## From the browser

In full in [clients.md](clients.md#presence). The shape of it:

```tsx
const presence = createPresence({
    runtime: client.runtime,
    scope: "board",
    stream: createPresenceStream(client.runtime, { scope: "board" }),
});

// Everybody else, narrowed to what is on the screen.
const others = usePresence(presence, { table: "todo", id });

// And where this tab is. On focus, never on onChange — typing a
// two-hundred-character title is one presence write.
presence.focus({ table: "todo", id, field: "title" }, "editing");
```

### Three things the package deliberately does not do for you

`@rig-ts/presence` owns the timer, the throttle, the clock and the teardown. What it
cannot own is where in *your* component tree those things live, and each of the
three is a bug the first time somebody writes it.
`examples/linearlite/web/src/presence` gets all three right, and is worth copying
rather than rediscovering.

**Build the handle in an effect, not during render.** React StrictMode mounts,
unmounts and mounts again on the first commit. Built during render that leaves
the first loop beating forever with nobody holding it, and calls `close()` — which
is final — on the one that is kept. It works in a production build and is dead
under a dev server, which is the worst way round. Answer the one commit before
the effect with an idle handle whose `others()` returns a *stable* empty array:
`usePresence` gives that array to `useSyncExternalStore`, which compares
snapshots by identity.

**Report a spot from one effect and end it in another.** Reporting needs the
target in its dependency list, so moving between rows writes the move. Ending
needs an empty one, because it is about the component's lifetime — and without it
the last target reported stays reported, so closing a detail panel leaves that
tab on that card for everybody else. One effect cannot have both dependency
lists.

**Mount it behind whatever gates a session.** There is nothing for an anonymous
visitor to be present in, and the heartbeat has no credential to send.

## Wiring it up

`presence.enabled` makes `server-go` write `internal/api/presence.gen.go`. One
line joins your wiring: `Presence` on `api.Handlers`.

```go
api.Register(mux, api.Handlers{ /* ... */ Presence: api.NewPresence(pool)})
```

The other two are `api.Main`'s. It starts the sweeper before your own wiring runs
— the service it sweeps through is its own, over `app.Pool`, so it needs nothing
from you — and merges `sweep-presence` into `Tasks`. `api.StartPresenceSweeper`
stays exported for a project that keeps the sequence itself.

[`examples/linearlite`](../examples/linearlite) is the worked one: one line in
`api/internal/app`, a filled-in scope stub in
`api/services/rig_presence`, and
`web/src/presence` — which is where the parts a package cannot own for you live,
and [the section above](#three-things-the-package-deliberately-does-not-do-for-you)
is about those.

Nothing checks a permission on `/presence`, deliberately: everybody who may look
at a screen may say they are looking at it. And there is no account field in the
request — who is present is read from the credential — so "you may only write
your own presence" is not a rule a handler enforces, it is a sentence a client
cannot phrase.

`presence.expose` additionally projects `rig_presence` as a read-only `Get`/`List`
resource, for the filter grammar and a typed client. The routes and the shape
exist either way, and that is the ordinary case.

## What rig does not do here

**No live cursors.** Sixty writes a second per person, each a durable transaction
through a single-writer WAL, a replication slot, a decoding plugin, a long poll
and a JSON parse. The disqualifying part is not the latency: it is that every
cursor position would be a committed row somebody backs up. Cursors need an
ephemeral fan-out that forgets, and rig has none.

**No collaborative text editing**, and not because of throughput. Co-editing
needs causal ordering per document, merge semantics, and a way to send an
_operation_ rather than a row. A shape is a set of rows with no ordering
guarantee across writers, and `rig_presence` has no log. A CRDT in a jsonb column
under last-write-wins is how somebody loses a paragraph.

> Presence over live sync is a durable, authenticated, tenant-scoped **state**
> channel with about a second of resolution, whose cost is one WAL record per
> change multiplied by every subscriber. It is not a transport, and the moment
> you want it to be one you want a different mechanism.

**No per-table `presence:` key.** A client names its target at runtime, so a
per-table switch would be a second answer to a question the request already
answers — and the useful half of it, refusing a table this API has never heard
of, comes free from the compiled document.

**No `state` column.** There was going to be one — an escape hatch for a colour
or a selection range — and it cannot work: a generated streamed row is what
parameterises a live collection, and that type does not admit an opaque value. So
the hatch would have been reachable from nothing. An application that needs more
than the three target columns and `activity` says it in a table of its own.

**No `UNLOGGED` table**, which is the first thing an experienced reader suggests.
It writes no WAL, so logical replication sees nothing and the shape streams
empty forever.

**Nothing prunes for you if you skip the sweeper.** The table is bounded by
people rather than by time, so it does not grow without limit — but it does keep
whoever was here last week until something deletes them.

## See also

- [electric.md](electric.md) — the shape this is read over, and the rule that
  decides its design
- [clients.md](clients.md#presence) — `@rig-ts/presence` in full
- [rig-yaml.md](rig-yaml.md#presence) — every key and its default
- [notifications.md](notifications.md) — the other feature built on a table and a
  shape, with the opposite answer about where the truth lives

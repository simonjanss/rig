# linearlite

The full-stack example: a Linear-style board where every piece of rig works
together. The other examples each hold one thing up to the light —
[todo](../todo) is the lifecycle features with no authentication,
[auth](../auth) is the accounts story with no live sync — and this is the
answer to "what does it look like assembled": accounts, tenants, invitations,
personal API keys, uploads, soft delete, version history, notifications and the
mail they would have sent, presence — whose avatar is on which card, and whose
cursor is in which field — a custom endpoint that a PATCH could not do
correctly, and a React front end kept current by live sync — with spans and
rig's own monitoring page over all of it, and an import job filling the board
through the generated Go client.

## Run it

```bash
make demo
```

That is the whole setup: it builds rig, starts Postgres *and* the sync
service, applies the migrations, seeds the demo tenant, builds the front end
(skipped with a note if pnpm is not installed), and runs the server. Run it
again tomorrow and it reuses all of it. `make down` stops the containers.

The steps it is made of, for when you want one of them alone or want to see
where each piece comes from:

```bash
rig db up          # Postgres AND the sync service — see database.electric in rig.yaml
go run . seed      # the demo tenant, two people, the roles, a board
go run .           # the API at :8084

cd web
pnpm install
pnpm build         # the server serves web/dist; use `pnpm dev` for a dev loop
```

`make demo` also sets `$RIG_LOG_FILE`, `$RIG_TRACE_FILE` and
`$RIG_MONITOR_PASSWORD` — the three things rig's monitoring page needs and no
build can know — into a throwaway `.run/`. Without them no port is opened for
the page at all, which is the right default everywhere else and useless on a
tour, so the Makefile says so out loud rather than the configuration file doing
it quietly. Where it listens is in rig.yaml: `127.0.0.1:9084`, its own port
beside the API's 8084. `$RIG_DEMO_SYNC_CONTAINER` is a fourth of the same kind,
and the one with teeth: it names the container the sync service runs in, and
without it the routes that stop and start that container are not registered. See
[Take the sync service down](#take-the-sync-service-down).

Open [localhost:8084](http://localhost:8084) and sign in as
`demo@linearlite.dev` / `correct horse battery staple` — or register a fresh
account and watch requirement two happen: the picker you land in already lists
an invitation to the demo workspace, left there by the `OnRegistered` hook in
`internal/app` inside the very transaction that created you.

For the full effect, open a second browser (or a private window) as
`alex@linearlite.dev` and put the two side by side: each window's header shows
the other person, a card dragged in one moves in the other, opening an item puts
a ring on that card in the other window, and if the item belongs to the other
person, a toast slides in — no socket code, no polling, nothing in `web/` asking
twice.

## The pieces, and where each one lives

| What you see | What it is |
|---|---|
| The board updates without a reload | `electric: {enabled: true}` in `services/todo/todo.yaml`; the generated shape routes on the API's own mux (`internal/api/*_shape.gen.go`, wired through `api.Shapes` in `internal/app` — the proxy authenticates every subscriber and builds the tenant filter); `createTodoStream` + `useLiveQuery` in `web/src/board/` |
| Who else is here, on which card, in which field | `presence: {enabled: true}` in rig.yaml and three lines across `internal/app` and `main.go`; `services/rig_presence/rig_presence_shape.go` narrows the shape to a scope, which is the one thing that makes the fan-out affordable; `web/src/presence/` is the browser half — one loop for the whole app, built in an effect because StrictMode would otherwise orphan it, and a `useSpot` that ends where the panel does |
| Register → invited to the demo tenant | `auth.allow_registration` in rig.yaml, and `autoInvite()` in `internal/app`: the `OnRegistered` hook provisions the newcomer with an invitation and attaches the member role, all in the registration transaction |
| Create your own workspace | `auth.allow_tenant_creation`, with `authz.SeedFor` as `TenantOptions.OnCreated` — a new tenant gets its roles in the transaction that made it |
| The item panel's History, and Revert | the snapshot triple in `migrations/00002` — every update keeps the version it replaced, and `/todo/{id}/_versions/_stream` makes the panel grow while you edit |
| The Trash, and Restore | `deleted_at` + `restore_window_days: 30`; the trash is a live shape too, so a delete visibly moves a card between windows |
| Attachments | `files: {enabled}` in rig.yaml and the `attachment_file_id` column — the multipart create commits the row and the bytes together |
| The bell, and the toasts | the `todo_notification` link table; `NotifyAt`/`NotifyWho` in `services/todo/todo.go` (stakeholders minus whoever made the change, resolved at send time); the inbox reaches the browser over the `rig_notification_recipient` shape |
| Personal API keys on the settings page | `POST /auth/api-keys`, kind `Personal` — gated on `apikey.own`, which the member role grants because a personal key can never do more than its owner |
| The import job | `go run ./import -key rig_sk_…` — the generated Go client (`client/`), a todo per CSV row, a deliberate delay so the board fills card by card, and idempotency keys so a rerun creates nothing |
| **Claim it**, and its 409 | the `endpoints:` block in `services/todo/todo.yaml` and `Claim` in `services/todo/todo.go` — the one control that is not CRUD, because the rule is about the value already in the column |
| **Outbox** | `services/outbox` implements both interfaces rig ships no transport for: `account.Notifier` for the links auth mints, and `notify.Sender` for the email copy of an inbox line. `/_demo/outbox` reads it, and the reset and invite flows end there |
| **Monitor ↗** | `tracing:` and `monitoring:` in rig.yaml, wired in `main.go` — the last few hundred requests, what each spent its time on, and the log lines it wrote, at `http://localhost:9084/_rig/monitor`. Its own port, not the API's: which interface it is bound to is the only boundary in front of it a client cannot talk its way around |
| **Security**: sessions and the sign-in trail | `GET /auth/sessions` and `GET /auth/audit`, both rig's own, neither generated from this schema. The trail is written whether or not anybody reads it. The **Just me / Everybody** switch is `?scope=all`, refused without `authlog.read.all` — which the Owner holds and a member does not |
| Pending invitations, and changing your password | `GET /auth/invitations` + `DELETE`, and `POST /auth/password/change` — which answers with a fresh pair, because setting a password revokes every session the identity had, including the one that asked |
| The workspace menu in the header | `GET /auth/tenants`, `POST /auth/tenants/{id}/switch`, and `POST /auth/tenants` to start one — three endpoints behind one control, in `web/src/shell/TenantSwitcher.tsx`. A switch answers with a pair and nothing else and then reloads, because the live-sync collections are cached by runtime rather than by credential |
| **How you are told**, on the settings page | `notifications: expose: true` and nothing else — two generated resources with no hand-written HTTP and no `services/` directory between them, because how rig's own tables project is rig's answer. Both are owner-scoped on `account_id`, so a read is narrowed to your own rows before any of this code runs, and a create that names somebody else is refused in the generated writer |
| Desktop notifications | a second `notify.Sender` in `services/outbox`, and the device rows it is handed. Email has an address on the account; a push has to be told where, and that is the whole difference. rig ships no Web Push, so the channel records what a real transport would have been given |

## What the front end is

`web/` is a React application, deliberately plain: Vite, react-router,
dnd-kit for the drag, hand-written CSS, and no state library — the state is
the database, and TanStack DB (which `@rig/electric` is built on) keeps the
board a live view of it. The generated client lives in `web/src/api`, written
by `rig generate` like everything else, and imports `@rig/client`,
`@rig/electric` and `@rig/presence` from this repository's `ts/` packages.

Two boundaries are worth reading before copying anything:

- **Streamed rows are not API rows.** A stream carries what Postgres printed
  (`created_at`); the API sends JSON keys (`createdAt`). `web/src/lib/rows.ts`
  is the one file allowed to know that.
- **Writes never touch a collection.** Every change goes through the REST
  client and arrives back over the stream like anybody else's change. The one
  optimistic concession is `usePendingMoves`: a dragged card holds its column
  until the echo lands, because a card snapping back for half a second reads
  as a failed drop. Its ten-second timeout is the one place a sync outage is
  visible in code that knows nothing about sync — the echo cannot arrive, so
  the overlay always expires. See [Take the sync service
  down](#take-the-sync-service-down), step 3.
- **Presence is the one thing with a timer in it.** `web/src/presence` builds
  exactly one loop for the whole application, in an effect and not during
  render — StrictMode mounts twice on the first commit, and building it in the
  body leaves one loop beating with nobody holding it while `close()`, which is
  final, is called on the one that is kept. It works in `pnpm build` and is dead
  under `pnpm dev`, which is the worst way round for a bug to be.

The `/auth/*` calls are hand-written in `web/src/auth/` — deliberately, since
those routes belong to rig and not to this schema, the generated client does
not cover them; the wire shapes are mirrored from `runtime/authwire`. The
`/_demo/*` calls in `web/src/outbox/outboxApi.ts` and `web/src/sync/syncApi.ts`
are hand-written for a different reason: none is about a table. The outbox is a
ring buffer in the server's memory, the tour is a fact about how the binary was
started, and the sync switch operates a container — so generating a client, a
filter grammar and an OpenAPI entry for any of them would be generating them for
something that vanishes on restart.

The **Outbox** and **Monitor ↗** items, and the sync pill in the header, appear
only when the server says they will work. `GET /_demo/tour` is one handler, and
it is there because a nav item that leads nowhere is worse than no nav item — no
password means no port for the monitoring page, which is the ordinary case for
anybody running `go run .` by hand. It hands back the monitor's URL rather than
a boolean, because the page is on a port of its own and a relative href no
longer reaches it.

One behavior to know before demonstrating on a projector: the sync client
pauses streams while a tab is hidden and resumes on focus, so a background
window catches up the moment it becomes visible rather than burning
connections while nobody looks.

## The demo script

**The first board fills in rather than arriving.** Sign in and the columns show
pulsing placeholders for a moment before the cards land, and the wait is round
trips rather than rows. A shape's read from the beginning ends with
`snapshot-end` and **no** `Electric-Up-To-Date` header; it takes a second request
to get one, and that header is what makes a subscriber ready rather than still
loading. Three shapes start at once on this screen — the board, the bell and
presence — so that is six requests before the board is furnished, over three of
the six connections a browser gives an origin.

On a laptop each of those is single-digit milliseconds, so what you see is mostly
the front end starting: one 736 kB bundle, parsed and run once. The placeholders
are there because without them an unfurnished column is indistinguishable from an
empty one — it renders "Drop items here" and a count of zero, which is a wrong
answer confidently given.

Two things that are *not* what is slow, in case you go looking. A shape the sync
service has never seen is built on first request, and for this board that is tens
of milliseconds, not seconds. And a sync service that is still starting does not
hold the board up either: the read is answered from this application's own
database instead, which is the section at the end. `X-Rig-Sync-Fallback: snapshot`
on `GET /api/v1/todo/_stream?offset=-1` is how to tell which of the two answered,
and the log line beside it names the shape.

1. Sign in as demo. Drag a card between columns.
2. Second window, alex. Watch the card sit where demo left it; drag it back.
3. As alex, open an item demo created and change its status — demo's window
   gets a toast and a badge. Open **Outbox** in either window: the same
   notification is there a second time, as the mail a channel was handed.
4. Look at the two headers: each shows the other person. Open an item as alex
   and watch that card grow a ring in demo's window; click into alex's title
   field and the ring turns accent-coloured and demo's panel says "Alex is
   editing" beside the same control. Type a long title and count the presence
   writes in the network panel: **one**, on focus. Then close alex's panel and
   watch demo's card go quiet while alex stays in the header — presence
   distinguishes "in the workspace" from "on this row", and a cleanup that fired
   on every move between rows could not.

   Switch alex's window behind another one. Alex leaves demo's header within a
   second, on a `pagehide`/visibility leave rather than after the TTL — a hidden
   tab is not receiving the stream either, so "alex is not editing your title" is
   the truth rather than a workaround.
5. Open the item, edit the description twice, and watch History grow. Revert
   to the first version; the revert itself becomes a version.
6. In demo's window, press **Claim it** on an item alex holds. It is refused —
   409, because the rule is about the value already in the column and the
   endpoint decides one statement before the write. Then **Take it anyway**,
   and alex gets a toast about their item changing hands.
7. Delete it. Check the Trash — then restore it and watch it rejoin the board.
8. Settings → create a personal key → copy the printed command:

   ```bash
   go run ./import -key rig_sk_…
   ```

   The board fills, card by card, while the job prints its report. Run it
   again: nothing duplicates — each row carries an idempotency key, and the
   server replays the recorded answers.
9. Sign out, **Forgot your password?**, then sign back in as alex and open
   **Outbox** for the link. Set a new password with it, and send the same link
   twice: single-use means the second one is refused, not ignored.
10. Settings → **How you are told** → **Register this browser**, then have alex
   change something of demo's. The Outbox now shows the same notification three
   ways: the inbox line demo can see in the bell, the mail the email channel was
   handed, and the devices a push transport would have addressed. Set Desktop to
   **Off** and do it again — the inbox line is still written, because the inbox
   is not a channel.
11. **Security** → the sessions demo is signed in on, and every sign-in,
    refusal and key mint rig recorded. Press **Everybody** as demo, then as
    alex: the second is a 403, and that is `?scope=all` meeting a permission
    the member role does not hold.
12. **Monitor ↗** — everything above, as requests: what each spent its time on,
    the log lines it wrote, and the trace id that ties an error body to both.
    The import job is the interesting one to look at.
13. Press **Stop** on the sync pill and keep working. The section below is what
    to watch.

## Take the sync service down

The pill in the header is the whole point of `DB: pool` on `electric.Config` in
`internal/app`, and it is the one thing in this example you cannot see by reading it.
With both windows open:

1. The pill reads **Live sync**. Press **Stop**. The container goes down
   immediately; the pill stays green for a moment and then turns amber and reads
   **Sync stopped**, with a strip under the header.

   That moment is rig's circuit breaker. The proxy does not probe the sync
   service — it counts failures, and it takes five in a row before it stops
   asking. Until then it is still forwarding and still paying the timeout.

2. **The board is still there, and it is not the browser holding what it had.**
   Watch the network tab, because the four requests in it are the whole
   mechanism:

   | | |
   |---|---|
   | `409` | the live poll, on the handle real sync gave it. rig cannot extend that stream and cannot answer it with a snapshot, so it says start again |
   | `200` | the client does, from `offset=-1` — and *that* is a request a snapshot answers. `X-Rig-Sync-Fallback: snapshot` |
   | `503` | the poll after it, carrying rig's own handle. Keep your rows and come back |
   | `503` | again, every five seconds, for as long as it lasts |

   No reload anywhere in that, and no 502. The rows on screen came out of this
   application's own database, under the shape's own filter, in the sync
   protocol's own format — so nothing in `web/` knows the difference.

   **Trash**, the bell, and an item's **History** all keep working too, each
   under its own filter: not deleted, deleted, narrowed to your account, one
   row's snapshots. Four screens, and not a line of code here for any of them —
   each shape's filter was already written, and answering from it is running the
   same predicate against Postgres instead of sending it upstream.

   The one thing that does go is presence — **and the headers empty out about a
   minute in, not at once.** Their rows freeze the moment the stream dies, but
   the heartbeat is a REST `PUT` that never stopped answering, so it keeps
   supplying a fresh reading of the server's clock while the rows sit still.
   `ts/packages/presence/src/clock.ts` compares one against the other, so they
   age out at the TTL — a minute here — and the one-second tick makes them
   disappear with no event to carry it.

   That is why presence is the one shape rig gives no fallback, and the reason
   is not squeamishness: a snapshot of who was here a moment ago, that then
   stops updating, is worth less than an empty list, because the feature *is*
   the freshness. It is also the shape where one would change the least — it
   would buy a minute of ghosts and then the empty room you already get. rig
   decides that for its own presence table, so it is not a line anybody here
   wrote and not one anybody can forget.

3. Drag a card. The write lands — the API never went anywhere — and then the
   card snaps back after ten seconds, because a snapshot does not update and
   `usePendingMoves` gives up waiting for an echo that cannot arrive.

   **The row is in its new column the whole time, and the reload is what shows
   that.** A snapshot is read per request, not once per outage: reloading is a
   read from the beginning, the shape is read again, and the card comes back
   where you dropped it. The other window, still holding the snapshot it
   took before the drag, does not see it until sync is back. So the board a
   reload gets is more correct than the board that was watching — which is the
   confusing part, and the reason the strip under the header says a write will
   not appear rather than leaving it to be discovered.

   This is the cost, stated exactly: an outage costs live updates, and it does
   not cost the board.

4. Press **Start**. The container is running before the pill turns green — that
   gap is the breaker's cooldown, one request let through to find out. Then the
   card from step 3 appears in both windows, in its new column, without a
   reload. The network tab shows the same shape of thing in reverse: `409`, then
   `offset=-1`, then live polling again. This half rig arranges nothing about —
   the subscription is carrying a handle rig invented, and the real sync service
   refuses a handle it never issued all by itself. One mechanism, used at both
   ends of the outage, because the protocol only has one way to say "that handle
   is no good, start over".

5. `.run/linearlite.log` has two lines and not two hundred — `OnSyncState` fires
   when it went and when it came back, where `OnError` is one per subscriber.
   **Monitor ↗** has the same two.

The switch itself is three routes in `internal/app/demo.go` shelling out to
`docker`, and it exists only when `$RIG_DEMO_SYNC_CONTAINER` names a container.
Not a route that answers 403 — no route at all, which is what keeps a scan from
learning this process can reach a container engine.

**Inside a checkout of rig, step 4 does not work, and the pill says so.** rig's
own Makefile sets `RIG_DB_ISOLATE` so two clones cannot adopt each other's
containers, and under it the sync service is published on a port the kernel
chooses rather than on 55445. A container published that way gets a *different*
port every time it starts — the pill reads **Sync moved** and names both numbers
— and since the proxy's URL is fixed when the process starts, the board stays on
a snapshot until the server is restarted. `make demo` from this directory sets
neither variable, so the port is 55445, `docker start` restores the same mapping,
and the switch round-trips. Under isolation, pass the name `docker ps` shows and
expect one direction:

```bash
make demo DEMO_SYNC_CONTAINER=linearlite-electric-1a2b3c4d
```

## The tests

They are all in `integration/`, a package of their own rather than files beside
`main.go`: the suite builds the server through `internal/app`, and no test can
import a `main`. The example root is the application.

The docker suite drives the same `app.New` the binary serves, over a real
database: the register → invitation → accept → board flow, a fresh tenant
whose Owner can write immediately, one item's whole life (create, refuse,
version, revert, trash, restore, assign), the multipart attachment round trip,
the import job with a minted key and a rerun that grows nothing, the
notification rule with its actor exclusion, claiming an item and the 409 a
second person gets, two people claiming at once and the one 200 between them, a
steal reaching the person it was taken from, a password reset walked end to end
through the outbox, a preference and a device being nobody's business but their
owner's, and one notification arriving on both channels at once.
`presence_docker_test.go` covers the half of presence a browser writes — a beat,
the two numbers every answer carries, two tabs of one account staying two rows, a
leave that takes one of them, and a target table this API has never heard of.
`monitor_test.go` needs no database: whether the page exists at all, and which
port it would be on, is decided by the environment and rig.yaml before anything
is served, so that is where it is asserted — including the absolute URL the nav
link needs now that the page is on an origin of its own.
`electric_docker_test.go` covers the outage without needing an outage: the board
loading from a snapshot with `$ELECTRIC_URL` aimed at a closed port, the `503`
the poll after it gets, the `502` presence keeps because it has no fallback, and
the switch answering `404` when no container is named — which is the security
property, so it is the one asserted rather than assumed.
`make examples` runs it all — `go test -tags docker ./...` reaches
`integration/` like any other package; the shape-route test runs its live half —
the todo shapes and the presence one — only when `$ELECTRIC_URL` points at the
sync service `rig db up` started. `monitor_test.go` carries no build tag, so
`go test ./...` runs it and nothing else there.

The browser half is not in `make examples`, which has Go and Docker and
deliberately not pnpm. It is in `make linearlite-web` at the repository root,
which `make check` now runs: presence is the first rig feature whose interesting
half is a browser one, and it is worth knowing that six of the eight bugs review
found in it were in code nothing else here compiles.

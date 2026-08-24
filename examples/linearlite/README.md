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
build can know — into a throwaway `.run/`. Without them the page is not mounted
at all, which is the right default everywhere else and useless on a tour, so
the Makefile says so out loud rather than the configuration file doing it
quietly.

Open [localhost:8084](http://localhost:8084) and sign in as
`demo@linearlite.dev` / `correct horse battery staple` — or register a fresh
account and watch requirement two happen: the picker you land in already lists
an invitation to the demo workspace, left there by the `OnRegistered` hook in
`main.go` inside the very transaction that created you.

For the full effect, open a second browser (or a private window) as
`alex@linearlite.dev` and put the two side by side: each window's header shows
the other person, a card dragged in one moves in the other, opening an item puts
a ring on that card in the other window, and if the item belongs to the other
person, a toast slides in — no socket code, no polling, nothing in `web/` asking
twice.

## The pieces, and where each one lives

| What you see | What it is |
|---|---|
| The board updates without a reload | `electric: {enabled: true}` in `services/todo/todo.yaml`; the generated shape routes on the API's own mux (`internal/electric/`, wired in `main.go` — the proxy authenticates every subscriber and builds the tenant filter); `createTodoStream` + `useLiveQuery` in `web/src/board/` |
| Who else is here, on which card, in which field | `presence: {enabled: true}` in rig.yaml and three lines in `main.go`; `services/rig_presence/rig_presence_shape.go` narrows the shape to a scope, which is the one thing that makes the fan-out affordable; `web/src/presence/` is the browser half — one loop for the whole app, built in an effect because StrictMode would otherwise orphan it, and a `useSpot` that ends where the panel does |
| Register → invited to the demo tenant | `auth.allow_registration` in rig.yaml, and `autoInvite()` in `main.go`: the `OnRegistered` hook provisions the newcomer with an invitation and attaches the member role, all in the registration transaction |
| Create your own workspace | `auth.allow_tenant_creation`, with `authz.SeedFor` as `TenantOptions.OnCreated` — a new tenant gets its roles in the transaction that made it |
| The item panel's History, and Revert | the snapshot triple in `migrations/00009` — every update keeps the version it replaced, and `/todo/{id}/_versions/_stream` makes the panel grow while you edit |
| The Trash, and Restore | `deleted_at` + `restore_window_days: 30`; the trash is a live shape too, so a delete visibly moves a card between windows |
| Attachments | `files: {enabled}` in rig.yaml and the `attachment_file_id` column — the multipart create commits the row and the bytes together |
| The bell, and the toasts | the `todo_notification` link table; `NotifyAt`/`NotifyWho` in `services/todo/todo.go` (stakeholders minus whoever made the change, resolved at send time); the inbox reaches the browser over the `rig_notification_recipient` shape |
| Personal API keys on the settings page | `POST /auth/api-keys`, kind `Personal` — gated on `apikey.own`, which the member role grants because a personal key can never do more than its owner |
| The import job | `go run ./import -key rig_sk_…` — the generated Go client (`client/`), a todo per CSV row, a deliberate delay so the board fills card by card, and idempotency keys so a rerun creates nothing |
| **Claim it**, and its 409 | the `endpoints:` block in `services/todo/todo.yaml` and `Claim` in `services/todo/todo.go` — the one control that is not CRUD, because the rule is about the value already in the column |
| **Outbox** | `services/outbox` implements both interfaces rig ships no transport for: `account.Notifier` for the links auth mints, and `notify.Sender` for the email copy of an inbox line. `/_demo/outbox` reads it, and the reset and invite flows end there |
| **Monitor ↗** | `tracing:` and `monitoring:` in rig.yaml, wired in `main.go` — the last few hundred requests, what each spent its time on, and the log lines it wrote, at `/_rig/monitor` |
| **Security**: sessions and the sign-in trail | `GET /auth/sessions` and `GET /auth/audit`, both rig's own, neither generated from this schema. The trail is written whether or not anybody reads it. The **Just me / Everybody** switch is `?scope=all`, refused without `authlog.read.all` — which the Owner holds and a member does not |
| Pending invitations, and changing your password | `GET /auth/invitations` + `DELETE`, and `POST /auth/password/change` — which answers with a fresh pair, because setting a password revokes every session the identity had, including the one that asked |
| The workspace menu in the header | `GET /auth/tenants`, `POST /auth/tenants/{id}/switch`, and `POST /auth/tenants` to start one — three endpoints behind one control, in `web/src/shell/TenantSwitcher.tsx`. A switch answers with a pair and nothing else and then reloads, because the live-sync collections are cached by runtime rather than by credential |
| **How you are told**, on the settings page | `notifications: expose: true` and an `operations:` line each in `services/rig_notification_setting/` and `services/rig_notification_device/` — two generated resources with no hand-written HTTP between them. Both are `access: {scope: own}`, so a read is narrowed to your own rows before any of this code runs |
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
  as a failed drop.
- **Presence is the one thing with a timer in it.** `web/src/presence` builds
  exactly one loop for the whole application, in an effect and not during
  render — StrictMode mounts twice on the first commit, and building it in the
  body leaves one loop beating with nobody holding it while `close()`, which is
  final, is called on the one that is kept. It works in `pnpm build` and is dead
  under `pnpm dev`, which is the worst way round for a bug to be.

The `/auth/*` calls are hand-written in `web/src/auth/` — deliberately, since
those routes belong to rig and not to this schema, the generated client does
not cover them; the wire shapes are mirrored from `runtime/authwire`. The two
`/_demo/*` calls in `web/src/outbox/outboxApi.ts` are hand-written for a
different reason: neither is about a table. The outbox is a ring buffer in the
server's memory and the tour is a fact about how the binary was started, so
generating a client, a filter grammar and an OpenAPI entry for either would be
generating them for something that vanishes on restart.

The **Outbox** and **Monitor ↗** items appear only when the server says they
will work. `GET /_demo/tour` is one handler and one boolean each, and it is
there because a nav item that leads to a 404 is worse than no nav item — the
monitoring page is unmounted without a password, which is the ordinary case
for anybody running `go run .` by hand.

One behavior to know before demonstrating on a projector: the sync client
pauses streams while a tab is hidden and resumes on focus, so a background
window catches up the moment it becomes visible rather than burning
connections while nobody looks.

## The demo script

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

## The tests

The docker suite drives the same `newAPI` the binary serves, over a real
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
`monitor_test.go` needs no database: whether the page exists at all is decided
by the environment before anything is served, so that is where it is asserted.
`make examples` runs it all; the
shape-route test runs its live half — the todo shapes and the presence one — only
when `$ELECTRIC_URL` points at the sync service `rig db up` started.

The browser half is not in `make examples`, which has Go and Docker and
deliberately not pnpm. It is in `make linearlite-web` at the repository root,
which `make check` now runs: presence is the first rig feature whose interesting
half is a browser one, and it is worth knowing that six of the eight bugs review
found in it were in code nothing else here compiles.

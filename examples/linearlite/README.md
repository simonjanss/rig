# linearlite

The full-stack example: a Linear-style board where every piece of rig works
together. The other examples each hold one thing up to the light —
[todo](../todo) is the lifecycle features with no authentication,
[auth](../auth) is the accounts story with no live sync — and this is the
answer to "what does it look like assembled": accounts, tenants, invitations,
personal API keys, uploads, soft delete, version history, notifications, and a
React front end kept current by live sync, with an import job filling the
board through the generated Go client.

## Run it

```bash
rig db up          # Postgres AND the sync service — see database.electric in rig.yaml
go run . seed      # the demo tenant, two people, the roles, a board
go run .           # the API at :8084

cd web
pnpm install
pnpm build         # the server serves web/dist; use `pnpm dev` for a dev loop
```

Open [localhost:8084](http://localhost:8084) and sign in as
`demo@linearlite.dev` / `correct horse battery staple` — or register a fresh
account and watch requirement two happen: the picker you land in already lists
an invitation to the demo workspace, left there by the `OnRegistered` hook in
`main.go` inside the very transaction that created you.

For the full effect, open a second browser (or a private window) as
`alex@linearlite.dev` and put the two side by side: a card dragged in one
window moves in the other, and if the item belongs to the other person, a
toast slides in — no socket code, no polling, nothing in `web/` asking twice.

## The pieces, and where each one lives

| What you see | What it is |
|---|---|
| The board updates without a reload | `electric: {enabled: true}` in `services/todo/todo.yaml`; the generated shape routes on the API's own mux (`internal/electric/`, wired in `main.go` — the proxy authenticates every subscriber and builds the tenant filter); `createTodoStream` + `useLiveQuery` in `web/src/board/` |
| Register → invited to the demo tenant | `auth.allow_registration` in rig.yaml, and `autoInvite()` in `main.go`: the `OnRegistered` hook provisions the newcomer with an invitation and attaches the member role, all in the registration transaction |
| Create your own workspace | `auth.allow_tenant_creation`, with `authz.SeedFor` as `TenantOptions.OnCreated` — a new tenant gets its roles in the transaction that made it |
| The item panel's History, and Revert | the snapshot triple in `migrations/00009` — every update keeps the version it replaced, and `/todo/{id}/_versions/_stream` makes the panel grow while you edit |
| The Trash, and Restore | `deleted_at` + `restore_window_days: 30`; the trash is a live shape too, so a delete visibly moves a card between windows |
| Attachments | `files: {enabled}` in rig.yaml and the `attachment_file_id` column — the multipart create commits the row and the bytes together |
| The bell, and the toasts | the `todo_notification` link table; `NotifyAt`/`NotifyWho` in `services/todo/todo.go` (stakeholders minus whoever made the change, resolved at send time); the inbox reaches the browser over the `rig_notification_recipient` shape |
| Personal API keys on the settings page | `POST /auth/api-keys`, kind `Personal` — gated on `apikey.own`, which the member role grants because a personal key can never do more than its owner |
| The import job | `go run ./import -key rig_sk_…` — the generated Go client (`client/`), a todo per CSV row, a deliberate delay so the board fills card by card, and idempotency keys so a rerun creates nothing |

## What the front end is

`web/` is a React application, deliberately plain: Vite, react-router,
dnd-kit for the drag, hand-written CSS, and no state library — the state is
the database, and TanStack DB (which `@rig/electric` is built on) keeps the
board a live view of it. The generated client lives in `web/src/api`, written
by `rig generate` like everything else, and imports `@rig/client` and
`@rig/electric` from this repository's `ts/` packages.

Two boundaries are worth reading before copying anything:

- **Streamed rows are not API rows.** A stream carries what Postgres printed
  (`created_at`); the API sends JSON keys (`createdAt`). `web/src/lib/rows.ts`
  is the one file allowed to know that.
- **Writes never touch a collection.** Every change goes through the REST
  client and arrives back over the stream like anybody else's change. The one
  optimistic concession is `usePendingMoves`: a dragged card holds its column
  until the echo lands, because a card snapping back for half a second reads
  as a failed drop.

The `/auth/*` calls are hand-written in `web/src/auth/` — deliberately, since
those routes belong to rig and not to this schema, the generated client does
not cover them; the wire shapes are mirrored from `runtime/authwire`.

One behavior to know before demonstrating on a projector: the sync client
pauses streams while a tab is hidden and resumes on focus, so a background
window catches up the moment it becomes visible rather than burning
connections while nobody looks.

## The demo script

1. Sign in as demo. Drag a card between columns.
2. Second window, alex. Watch the card sit where demo left it; drag it back.
3. As alex, open an item demo created and change its status — demo's window
   gets a toast and a badge.
4. Open the item, edit the description twice, and watch History grow. Revert
   to the first version; the revert itself becomes a version.
5. Delete it. Check the Trash — then restore it and watch it rejoin the board.
6. Settings → create a personal key → copy the printed command:

   ```bash
   go run ./import -key rig_sk_…
   ```

   The board fills, card by card, while the job prints its report. Run it
   again: nothing duplicates — each row carries an idempotency key, and the
   server replays the recorded answers.

## The tests

The docker suite drives the same `newAPI` the binary serves, over a real
database: the register → invitation → accept → board flow, a fresh tenant
whose Owner can write immediately, one item's whole life (create, refuse,
version, revert, trash, restore, assign), the multipart attachment round trip,
the import job with a minted key and a rerun that grows nothing, and the
notification rule with its actor exclusion. `make examples` runs it all; the
shape-route test runs its live half only when `$ELECTRIC_URL` points at the
sync service `rig db up` started.

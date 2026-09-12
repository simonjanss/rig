# auth

The other examples have no authentication and say so. This one is the
counterpart: nothing here is about notes, and everything is about what
`rig setup-project` writes and what you connect it to.

## How it was made

```bash
rig init auth --module github.com/you/auth
rig setup-project          # migrations only: tenants, identities, credentials,
                           # accounts, API keys, sessions, roles, providers, log
rig migration new create_note
rig db up && rig sync && rig validate && rig generate
```

`setup-project` wrote migrations 1 to 5 and nothing else — no table
configuration, because rig generates no code for those tables. Migration 6 is the
application: one `note` table, so that there is something to protect.

The order is deliberate: tenants, people and accounts, then **API keys**, then
sessions, roles and provider links. Keys come second because everything after
them can then record which key changed a row — including `rig_identity` and `rig_account`
themselves, and the session and log tables, which name the key a request arrived
with where they are created rather than by altering themselves afterwards.

## Running it

```bash
rig db up && go run . seed && go run .
```

Then open **http://localhost:8082** — there is an interface, and it is the fastest
way to see all of this working. Every button in it makes a real HTTP request to
this application's own API, with a real `Authorization` header, and the panel down
the right shows the request that was actually sent as a `curl` you can paste into
a terminal:

```
┌ Notes ──────────────┬ API keys ───────────┐ ┌ Requests ─────────────────┐
│ Written by the      │ Nightly import      │ │ 201 POST /api/v1/notes    │
│ nightly import      │  note.write         │ │             ↑ api key     │
│  by Nightly import  │ Personal automation │ │ 200 GET  /auth/tenants    │
│  [through Nightly…] │  note.write         │ │ 200 POST /auth/email-code │
├ People here ────────┼ Active sessions ────┤ │                           │
│ Ada       Owner you │ 019fc785  Web  this │ │ curl -i -X POST \         │
│ Grace     Admin     │  started 2m ago     │ │   …/api/v1/notes \        │
│ Nightly…  service   │                     │ │   -H 'Authorization: …'   │
└ Auth log ───────────┴─────────────────────┘ └───────────────────────────┘
```

What it demonstrates, in the order it is worth clicking:

| | |
|---|---|
| **Sign in with a code** | Type an address, read the code out of the outbox panel, type it back. There is no password anywhere and no screen for one. |
| **Create a tenant** | A tenant, an Owner account and a role, in one transaction, from the picker you land in. |
| **Write a note** | `note.write`, checked in the service's own hook. |
| **Mint an API key** | Integration or personal, with scopes. The secret appears once. |
| **Write a note with the key** | And watch the row say *by Nightly import, through Nightly import* — the account it acted as and the credential it came through. |
| **Invite somebody** | A single-use link lands in the outbox panel, because there is no mail server. |
| **Look at it first** | `GET /auth/invitations/preview` with no credential at all: who invited you and where, with the address masked. It spends nothing. |
| **Accept the invitation** | Unauthenticated, in a fresh browser. **Accepting is what creates the account** — until then nobody was a member. |
| **Or withdraw it** | Kills the link, and removes nothing, because nothing was created. |
| **Be in two tenants** | Make a tenant of your own and the switcher appears — Owner in one, Admin in the other, one address. |
| **Read the auth log** | Every attempt, rotation, invitation and lockout, newest first. |
| **Rotate the tokens** | The pair is consumed and replaced; replaying the old refresh token revokes the family. |
| **Type the code wrong** | Three guesses and the code is dead; ask for another. Enough wrong sign-ins and the *address* is locked with a `Retry-After`, counted in the database. |
| **Sign in with a provider** | Nothing on the page names a tenant. A new address lands *in the picker*: an identity, a provider link, and an account nowhere. |

The panels are honest about failure, which is the part worth watching: an Admin
without `apikey.manage` sees *Refused: 403 — this action requires the
"apikey.manage" permission* where the key list would be, rather than an empty
panel. A 403 is the most interesting answer an authentication demonstration can
give.

`seed` makes a tenant, an Owner to sign in as, a role that may write, and an
integration with a service account and a key of its own:

```
seeded tenant 00000000-0000-0000-0000-000000000001 (addresses in example.com)
with ada@example.com — Owner, holding note.write
ask for a code at /auth/email-code and read it off the page: this example's
notifier prints what it would have mailed.
integration key for nightly-import@example.com: rig_sk_ZB4ZBXDD…_HVR6TXAM…
```

Nothing is set up for Ada to sign in *with*, because there is nothing to set up:
she asks for a code at the address above and types it back.

The key is printed once, because only its hash is stored.

There is still no *registration* endpoint — whether anybody may sign themselves
up is a product decision the foundation does not make for you. What it does give
you is provisioning, below.

### The provider button, and why there is no tenant on it

`/start` is an anonymous browser GET: no session, no token, and nobody having
proved they own an address. So there is nobody to have tenants yet, and asking
which one they meant is asking a question the visitor cannot answer. The
resolver answers `uuid.Nil`, the callback settles who they are, and where they
go comes from their own memberships — the same three answers a code sign-in
gives: the tenant they were last in, their oldest, or nowhere yet and here is
the picker.

The provider is a stand-in from [`examples/idp`](../idp), served by this
application, so the button works with nothing registered anywhere. Its consent
screen lets you choose what it claims about you, which is how both branches of
the linking rule are reachable: sign in with the address you registered with and
the two accounts become one — **if** it says the address is verified. Turn that
off and watch it refuse, which is the check the whole OAuth package turns on.
Linking also records the verification, so somebody who asked for a code once and
never used it is confirmed from then on.

Two keys in `rig.yaml` are worth reading together:

```yaml
    allow_provisioning: true
    allow_joining: false
```

The second one is load-bearing here, and the reason is this example's
`tenant.from: [query, header]`. The generated resolver reads `?tenant=`, and
`/start` is a link anybody can write — so
`/auth/oauth/demo/start?tenant=<somebody-else's-tenant>` would otherwise make
whoever clicked it an account somewhere they never chose. With joining off it
cannot: the callback resolves the identity and refuses with `no_tenant_access`.
An ordinary sign-in names no tenant and is unaffected. `examples/auth_oauth`
leaves both on, because there the tenant comes from the host and a link cannot
lie about it.

Set `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET` and a real Google button
appears with nothing else changing.

## The flow

```bash
T=00000000-0000-0000-0000-000000000001

# ask for a code — always 204, whatever address you type
curl -s -i localhost:8082/auth/email-code -H "X-Tenant-Id: $T" \
  -d '{"emailAddress":"ada@example.com"}'
# 204, and the code is in the outbox panel at /ui — there is no mail server

# type it back. The tenant is a header here, because a sign-in happens before
# there is a session to read it from
curl -s localhost:8082/auth/email-code/verify -H "X-Tenant-Id: $T" \
  -d '{"emailAddress":"ada@example.com","code":"123456"}'
# {"accessToken":"rig_at_AGP4…","refreshToken":"rig_rt_AGP4…",
#  "expiresAt":"…","refreshExpiresAt":"…","sessionId":"019fc785-…"}

# and use it on the generated API
curl -s localhost:8082/api/v1/notes -H "Authorization: Bearer rig_at_AGP4…" \
  -d '{"title":"Signed in"}'
# 201, with tenantId and createdByAccountId stamped from the token

curl -s localhost:8082/api/v1/notes            # 401, no credential
curl -s localhost:8082/auth/refresh  -d '{"refreshToken":"rig_rt_AGP4…"}'
curl -s localhost:8082/auth/logout   -H "Authorization: Bearer rig_at_AGP4…"
```

Tokens are opaque rather than JWTs: `rig_at_<id>.<secret>`, where the database
stores only a hash of the secret. Revoking one is a row update, so it takes
effect on the next request — there are no signing keys to rotate and no window
where a revoked token still works.

Every refresh rotates: the presented token is consumed and a new pair issued.
Replaying a consumed one outside a 30-second leeway revokes the whole family and
writes a `TokenReuseDetected` entry, which turns a stolen token from a permanent
problem into a detectable event.

## Configure it, then hand it a pool

The foundation's settings are one block in [rig.yaml](rig.yaml) — this example's
whole block, and most of it is turning two routes on:

```yaml
auth:
  enabled: true
  email_code:
    enabled: true
    allow_provisioning: true
  allow_tenant_creation: true
  tenant:
    from: [query, header]
```

`rig generate` writes the assembly into `internal/api`, beside the routes and the
handlers, and wiring it up is one call. This is all of the authentication code in
[main.go](main.go):

```go
front, err := api.New(pool, api.Hooks{
	Grants:   authz.Grants(pool),   // who holds which permission
	Notifier: mail,                 // where a reset or an invitation goes
	Tenants:  account.TenantOptions{…},
})
if err != nil {
	return nil, err
}

mux := api.Register(api.Handlers{
	Server: api.Server{Auth: front},
	Note:   note.New(repos.Notes),
	…
})
```

That is the mailed-code sign-in, logout, refresh, email verification,
invitations, the session list and the API keys — with mandatory rotation and
reuse detection, an attempt ceiling on each code, and lockout counted in the
database. Nothing is generated and nothing is yours to maintain: `internal/` in
this project holds `note` and no more.

`GetClaims` is the whole integration. Every generated handler identifies its
caller with the same verification that issued the token, so a session token, an
API key, and the permissions attached to either mean the same thing everywhere —
and the tenant scoping the repositories enforce comes from the same claims.

The defaults are the ones a project would have written anyway: `/auth` for the
paths, the `X-Tenant-Id` header for the tenant, 10-minute access tokens, 12-hour
sessions, 30 days for "remember me", six-digit codes that die after three wrong
guesses, and the documented rate limits. Every one of them is a key under `auth:` in rig.yaml —
`rig schema project` lists them, and [docs/auth.md](../../docs/auth.md) says what
each one costs. They live in the file rather than in a Go literal because the
reference documentation and the client libraries are generated from it: a token
lifetime written in code is a lifetime nothing else can read.

What stays in Go is what a file cannot hold — a function, and a secret. This
example passes three functions and no secrets. Even the error mapper needs no
saying: the wiring is generated into this API's own package, so an authentication
failure goes through the same mapper as everything else and a 401 from the
sign-in endpoint is shaped like a 401 from anywhere else.

**The parts are still there.** `api.New` calls `auth.New`, which holds no
logic of its own — it
assembles `authpg`, `session`, `account`, `apikey`, `authhttp` and `throttle`,
every one of them exported and separately usable. `front.Parts()` returns what it
built, for the things an endpoint cannot do for you: issuing a session after
somebody signs in another way, minting a key from an admin screen, resolving a
caller's grants. This example reaches for it in its seed and its tests:

```go
front.Parts().Accounts.Invite(ctx, account.InviteInput{ /* … */ })
```

Going halfway is a supported route rather than a rewrite: build the parts
yourself and skip the façade.

## Nothing about the foundation is generated

`setup-project` writes eleven tables and rig generates **no code for any of
them**. Everything they would provide already exists in the `rig/auth` module:

| what | where it comes from |
|---|---|
| the types — `account.Identity`, `apikey.Key`, `session.Token` | `rig/auth`, imported |
| the queries | `authpg`, hand-written SQL over the same tables |
| the endpoints | `api.Server{Auth: front}`, mounted by Register |
| the permission checks | `tenancy.Require(claims, …)`, over keys rig derived |
| who holds which permission | **yours** — `services/tenant/grants.go`, over this example's own role tables |
| tenancy | `tenancy.Claims`, and the `tenant_id` on your own tables |

The difference this makes to a project, measured on this one:

```
generated for the foundation, before   27,604 lines across 65 files
generated for the foundation, now                0 lines
generated for note, the application      3,157 lines across 11 files
```

None of those 27,604 lines were ever called: the auth package reaches its tables
through its own queries, so a projected model and repository beside them were a
second door onto the same rows and a second thing to read.

Every table the foundation creates is named `rig_…`, so a `\dt` says which
tables arrived with it and which are yours. rig knows which are the foundation's
from **your own migration files** — `00001_rig_tenancy.sql` and its siblings —
not from the names, so a project with a `rig_account` table nobody scaffolded is
an ordinary table and still gets a model, a repository and an API like any
other.

They are still ordinary tables in every other respect. They follow the same
column conventions, `rig sync` can read them, and `rig validate` holds them to
the same rules — which is why the migrations carry `COMMENT ON` for every column
and a freshly set-up project validates clean.

### When you do want CRUD over one

An administration screen listing the people in a tenant is a fair thing to want:

```bash
rig setup-project --expose rig_account   # writes services/rig_account/rig_account.yaml
```

```yaml
# rig.yaml
auth:
  expose: [rig_account]
```

That table is then projected like any other — model, repository, routes — and its
neighbours stay out. The physical name keeps its prefix and the API does not: the
scaffolded configuration asks for `resource: Account`, so what arrives is
`Account` on `/api/v1/accounts`. It also keeps the table free of a `Create`,
because an account created through plain CRUD would have no identity behind it
and nobody would have been asked.

`auth.own: true` goes further and generates for all of it, for a project that has
forked the migrations and stopped importing `rig/auth`. It is a one-way door in
practice: a generated repository does not enforce what the auth package enforces,
so a row reaching `rig_identity_verification` through one is a credential nobody
minted.

## One person, many tenants

The one structural decision worth reading before the rest. There are two tables
where most schemas have one:

| table | scope | holds |
|---|---|---|
| `rig_identity` | global | the address, whether it is confirmed, the linked providers. Who somebody *is*. |
| `rig_account` | one tenant | their role there, their display name there, whether they are still there. Who they are *here*. |

Somebody who works at two of your customers is one identity and two accounts, and
one address reaches both. They can be an `Owner` in one and `Basic` in the other,
which the single-table version cannot express at all.

The reason it is split this way round — rather than making membership a join table
and leaving `rig_account` global — is that `rig_account` keeps its `tenant_id`. Every
generated query is still scoped automatically, `created_by_account_id` still
points at a row belonging to exactly one tenant, and `Claims{TenantID, AccountID}`
is unchanged. Nothing downstream has to remember anything. The global half is
not generated from at all, so the only thing a client can reach is the
tenant-scoped one.

```sql
-- the person, once
INSERT INTO rig_identity (id, email_address, display_name) VALUES (…, 'ada@example.com', 'Ada');
-- and an account per tenant they work in
INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name, role)
VALUES (…, :first,  :ada, 'ada@example.com', 'Ada', 'Owner'),
       (…, :second, :ada, 'ada@example.com', 'Ada', 'Basic');
```

Three consequences worth knowing:

- **The address is globally unique**, not unique per tenant. A second identity
  with the same address is refused by the database.
- **"Sign me out everywhere" reaches every tenant.** A person is global and
  their sessions are not, so anything narrower would leave a thief signed in to
  the tenant the person was not looking at. It is
  `accounts.RevokeEverySession`, and the decision to use it is the
  application's.
- **A service account has no identity at all** — `identity_id` is null and a CHECK
  requires it, because nobody signs in as an integration. Its address resolves to
  nobody, so a login attempt gets the same 401 an unknown address does.

Login still takes one tenant: it verifies the person globally and then resolves
their account in the tenant the request named, answering **403** if they do not
belong to it. Choosing between tenants at sign-in, and switching between them
afterwards, is not built yet — see the end of this file.

## Accounts, levels, and time zones

An account carries three things beyond an address and a name:

| column | why |
|---|---|
| `kind` | `Person` or `Service`. A service account is what an integration's key acts as; it has no identity, so there is nothing to sign in with and nothing to phish. |
| `role` | `Owner`, `Admin` or `Basic`, **in this tenant**. The coarse level, because almost every product ends up with these three and inventing a permission taxonomy on day one is how a project ends up with fourteen. |
| `time_zone` | An IANA name. It belongs to the person rather than to a request: a report scheduled for 9am means 9am where they are, and no browser offset is available when nobody is looking at one. Null means UTC, and `account.Location()` falls back to UTC rather than failing on an unknown zone. |

The level and this example's role tables are not two systems to keep in step: one
query resolves both, and the level arrives in a caller's claims as a role name. So
`claims.Can("note.write")` is unchanged, and `slices.Contains(claims.Roles,
"Owner")` answers the coarse question without a join. Who may raise somebody to
`Owner` is a rule for your own service to enforce — the column is `[Read, Update]`
and rig will not invent one.

## API keys: integration and personal

Every key belongs to a tenant and acts as an account, which is what makes an
integration's writes attributable to something. The kinds differ in whose account
that is:

```go
// The nightly import: its own service account, minted by the person who
// connected it.
keys.Mint(ctx, apikey.MintInput{
	TenantID: tenant, AccountID: serviceAccountID,
	Kind: apikey.KindIntegration, Name: "Nightly import",
	Scopes: []string{"note.write"},
	CreatedByAccountID: &ada,
})

// Ada automating her own work.
keys.Mint(ctx, apikey.MintInput{
	TenantID: tenant, AccountID: ada,
	Kind: apikey.KindPersonal, Name: "Ada's own scripts",
	CreatedByAccountID: &ada,
})
```

- **Integration.** Acts as a service account of its own, so `created_by_account_id`
  on every row it writes names the integration, and the audit trail distinguishes
  "the import did it" from "Ada did it". Deactivating Ada does not stop it — an
  integration outlives the employee who connected it. Its scopes *are* its
  permissions: enriching them from the service account's roles would make a
  machine credential grow new powers whenever somebody edited a role.
- **Personal.** Acts as its owner. What it can reach is exactly what they can
  reach — the claims come from their grants, narrowed by the key's scopes when it
  has any, so a key can never exceed its owner and reducing their role reduces
  the key with it.

A personal key must act as the account that created it, and both the manager and
the database say so — `api_key_personal_is_its_own` is a CHECK, so an insert that
goes around the manager is refused too.

`rig_api_key` has no generated repository, like the rest of the foundation: keys are minted through
`/auth/api-keys`, because the secret exists only in the response that creates it
and a generic POST could not return it.

## Auditing: who, and through what

Every table with the audit columns records who made a change. A table that also
carries the key columns records the credential it came through:

| column | value |
|---|---|
| `created_by_account_id` | whose change it is — a person, or an integration's service account |
| `created_by_api_key_id` | the key it arrived with, or null when a person did it in the product |

Both, not one: a service account may have several keys, so the account says *the
nightly import did this* and the key says *through this credential* — which is
the one you revoke. `updated_by_*` and `deleted_by_*` work the same way, and a
restore clears the deletion pair together.

```json
{ "id": "019fc7…", "title": "Imported overnight",
  "createdByAccountId": "8f2b…",   // the nightly import's service account
  "createdByApiKeyId":  "c41a…" }  // the key it used
```

The columns are opt-in by existing, exactly like the account ones: add them to a
table and rig stamps them. They are managed, so they never appear in a create or
update body, and `rig sync` does not ask you to describe them.

**Leaving them out has a consequence, on purpose.** A table of your own that
records *who* changed a row and not *through what* refuses a write from an API
key:

```
403  a Report cannot be changed with an API key: the table does not record
     which key made a change
```

A key must be no less traceable than a person, and the rule is generated into the
repository rather than checked in a handler — the repository is the floor every
write stands on, so a custom endpoint reaching for the writer passes through it
too.

The foundation holds itself to it. `rig_api_key` is created directly after the
accounts it acts as, which is what lets `rig_account` carry the key columns as well —
so an integration can provision accounts, and the change says which credential
did it. Nothing `setup-project` writes refuses a key.

Two things are deliberately exempt. A table that records nobody at all — a lookup
table, a join table — is left alone: nothing there is any less traceable for a key
than for a person. And a project without the API key part gets no check emitted at
all, since no request can arrive with one.

Under the hood the key travels in the claims — `tenancy.Claims.APIKeyID`, set
when the credential was a key — so a hook, a service rule and the repository all
see the same thing.

## Adding somebody, and asking somebody

Two verbs and deliberately not one, because they do different things and a flag
on one function could never say which.

`POST /auth/accounts` adds a member, now:

```bash
curl -s localhost:8082/auth/accounts -H "Authorization: Bearer rig_sk_TMDR…" \
  -d '{"emailAddress":"grace@example.com","displayName":"Grace","role":"Admin"}'
# 201 {"id":"019fc7…","kind":"Person","role":"Admin", …}
```

It finds the person by their address or creates them, then gives them an account
here. An address that already belongs to somebody is **reused rather than
refused** — a person who works at two of your customers is one person, and a
second identity would mean a second of everything. It honours the tenant's
allowed domains, refuses a second account *in the same tenant* with a **409**,
and records who asked. It mails nothing: telling them is the application's, which
is the argument for the other verb when the mail is the point.

`POST /auth/invitations` asks somebody to join, and **creates nothing here**:

```bash
curl -s localhost:8082/auth/invitations -H "Authorization: Bearer rig_at_AGP4…" \
  -d '{"emailAddress":"grace@example.com","displayName":"Grace","role":"Admin"}'
# 201 {"id":"019fc7…","emailAddress":"grace@example.com","role":"Admin", …}
```

What it writes is an identity, if this installation has never seen the address,
and a row that says which tenant, what role, what to call them and who asked.
**Accepting is what creates the account** — so Grace is not in the people list,
is not counted, and has nothing scoped to her until she follows the link. Because
the account then comes into existence inside rig's own handler, the roles that
used to be granted by whoever clicked Invite are granted in `OnJoined` instead.

An **API key may call either**, which is what the audit columns were for: the new
row names both the integration's service account and the key it came through, so
"who added this person" has an answer months later. Both need the
`account.provision` permission — a role grant for a human, a scope for a key, one
vocabulary either way.

## Allowed email domains

`rig_tenant.allowed_email_domains` is a `text[]`, empty by default:

```sql
UPDATE rig_tenant SET allowed_email_domains = ARRAY['example.com'] WHERE id = …;
```

Empty allows any address, which is right for a tenant that is not one company.
When it is set, an address outside the list is a **422** — and a subdomain of a
listed domain counts, because somebody who allows `example.com` means the company
and `mail.example.com` is the company.

It is enforced wherever an account comes into existence, not just at this
endpoint: **a first sign-in through an OAuth provider honours it too** — which
the provider button on this page reaches, and
[`examples/auth_oauth`](../auth_oauth) reaches from the other direction. That one matters more than it looks — a provider will authenticate anybody with a Google account, so "sign
in with Google" plus provisioning is an open door until something says which
addresses belong here.

## Permissions

The note service checks one, in its own hooks
([services/note/note.go](services/note/note.go)):

```go
func (s *rules) mayWrite(_ context.Context, claims tenancy.Claims, _ *model.NoteCreateInput) error {
	return tenancy.Require(claims, PermissionWrite)
}
```

A hook rather than middleware, and that is the point worth taking: the check runs
inside the transaction that does the write, on the claims the request arrived
with, so every path to a write goes through it — the generated endpoint, a custom
endpoint, and anything the service layer calls itself. A route-level check only
covers the route.

Reading a note needs a valid session and nothing more; writing one without the
permission is a **403** — the caller is known and simply not allowed, which is a
different thing to fix from a 401.

## Rate limits

Counted over `rig_auth_log` with a sliding window rather than in memory, so two
replicas cannot disagree and a restart does not clear somebody's lockout. Five
wrong codes for one address and the next attempt is a **429** with `Retry-After`
— including for the *correct* code, because the lockout is about the address and
not about whether this particular guess was right.

Beside it, and not the same thing: a code dies after **three** wrong guesses.
That is a ceiling on one secret rather than a limit on an address, and a rolling
window cannot express it — a fresh code would arrive with the old one's failures
still against it. The ceiling is deliberately well under the lockout, so that
mistyping is "ask for another" rather than fifteen minutes out.

## What this example does not do

- **The outbox is not a mailbox.** `services/outbox` keeps the last twenty
  secrets in memory so that signing in and being invited can both be
  demonstrated without a mail server — the code on the page *is* the code in the
  mail. A live code is a credential for the ten minutes it lasts, so a real
  `Notifier` sends it and keeps nothing.
- **Half of the provider sign-in is a different example.** The button here names
  no tenant, which is one of the two shapes: the callback resolves who somebody
  is, and where they go is settled from their own memberships. The other shape —
  a tenant per subdomain, so the host names it before the redirect and the state
  cookie carries it — is [`examples/auth_oauth`](../auth_oauth). A deployment has
  a host to read or it does not; there is no third answer.
- **`permission:` in a table's YAML is not enforced yet.** The key is read into
  the IR and no generator consumes it, so the check has to be a hook today. That
  is why the one above is a hook rather than a line of configuration.
- **There is no administration API.** Listing the people in a tenant, or the
  roles, means `auth.expose` and a generated resource — or your own handler. The
  foundation ships authentication, not a back office.

## Tests

`auth_docker_test.go` drives the wiring this example ships — `newAPI` is a
function taking a pool precisely so a test can build the same thing — over a real
database: the session flow end to end, the 403 without a permission, one person
in two tenants signing in to both with one address and seeing different notes,
and the lockout. It reaches the foundation's tables with plain SQL, because there
is no generated repository for them — which is also what a project's own
migrations and seeds will do.

```bash
rig db up && go test -tags docker ./...
```

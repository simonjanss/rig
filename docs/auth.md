# Authentication

Four credentials, one door, and a rule for which is which.

Everything below is served by `rig/auth` over tables named `rig_…`, so a project
can tell them from its own. Nothing about it is generated, and nothing about it is
yours to maintain.

Where those tables come from is a choice: `rig setup-project` writes their
migrations into your own `migrations/` directory, or `rig/auth` carries them and
applies them itself. That is `migrations.foundation`, and
[rig-yaml.md](rig-yaml.md#who-keeps-rigs-migrations) has the trade. Either way the
tables are the same and everything on this page reads the same.

Both the prefix and the names those tables project to — `Account`, `Tenant`,
`File` and the rest — are reserved, so your own schema cannot land on either.
[schema.md](schema.md#names-rig-reserves) says what that costs and what it buys.

What *is* yours is one block in `rig.yaml` — see
[What you configure](#what-you-configure) — and the two or three functions a file
cannot hold, under [What you decide](#what-you-decide). `rig generate` writes the
assembly between them, into the same package as your routes and handlers, so the
lifetimes the documentation quotes are the lifetimes the server enforces.

New to rig? [concepts.md](concepts.md) and [tutorial.md](tutorial.md) come first;
this page assumes you have an application already. The rest of the documentation
is indexed in [README.md](README.md).

---

## The four credentials

Every credential is an opaque string with a prefix. The prefix is not decoration:
it makes a leaked token identifiable on sight in a log or a paste, and it means a
credential presented at the wrong door is refused **by shape** rather than by a
check somebody has to remember to write.

| prefix | what it is | resolves to | lifetime |
|---|---|---|---|
| `rig_at_` | access token | a caller **inside one tenant** | 10 minutes |
| `rig_rt_` | refresh token | nothing — it only buys a new pair | 12 hours, or 30 days with "remember me" |
| `rig_sk_` | API key | a machine, or a person automating themselves | until revoked or expired |
| `rig_it_` | identity session | a person, with **no tenant** | 30 minutes |

The format is the same for all of them:

```
<prefix><base32(id)>.<base32(secret)>      tokens
rig_sk_<key_id>_<base32(secret)>           API keys
```

The id half is the lookup key; the database stores only `sha256(secret)`. So
verification is one indexed row read, which is what makes **revocation take effect
on the next request** — there is no signed document to wait out. There are no
signing keys, no key rotation and no JWKS. The cost is that read; see
[Tuning](#tuning) if it ever matters.

> sha256 rather than argon2id, and the difference is deliberate. A password is
> short and human-chosen and worth grinding for; a token secret is 256 bits from
> the system's random source, and running a memory-hard function on every request
> would be a denial of service aimed at ourselves.

### `rig_sk_` covers both kinds of key

There is one prefix for both, because both go to the same door and take the same
verification path. What differs is where the permissions come from, and that is
read from the row rather than from the wire:

- **Integration** — the key's `scopes` *are* its permissions, full stop. No role
  lookup. A machine credential that inherited a person's roles would grow new
  powers whenever somebody edited a role.
- **Personal** — resolved as its owner, then **intersected** with the key's
  scopes. It can never exceed what its owner currently holds, and it shrinks when
  their access does.

That intersection is why minting the two needs different permissions:
`apikey.own` grants nothing its holder does not already have, and
`apikey.manage` creates authority.

---

## Headers

Two, and only one of them is ever required.

```http
Authorization: Bearer <credential>
```

Required on everything except the endpoints listed as *none* below. The scheme is
matched case-insensitively; anything else is a 401 saying so.

```http
X-Tenant-Id: <uuid>
```

Read **only** where a tenant cannot be known any other way: `POST /auth/login` and
`POST /auth/password/reset`. Once there is a session the tenant comes from the
token, and this header is ignored.

Absent is not an error — it means *unspecified*, and login treats that as "sign me
in wherever I belong". That is what a single sign-in page needs: a visitor cannot
say which tenants an address belongs to before the password has been checked. A
header that is present and malformed is still refused, because that is a caller
getting it wrong rather than leaving it out.

Replace the resolver entirely with `auth.Config.Tenant` — a subdomain and a path
segment are both ordinary, and rig will not guess:

```go
auth.New(auth.Config{
    Pool: pool,
    Tenant: func(r *http.Request) (uuid.UUID, error) {
        return tenantFromSubdomain(r.Host)
    },
})
```

`X-Forwarded-For` is read only when the immediate peer is a network named in
`auth.Config.TrustedProxies`. Empty means none, which is the safe default: an
address out of a header a client controls is an address a client chooses, and a
rate limit keyed on one is a rate limit an attacker walks around.

---

## Endpoints, and what each accepts

`none` means no `Authorization` header — the request body carries its own proof, or
there is nothing to prove yet.

### Signing in and out

| | credential | notes |
|---|---|---|
| `POST /auth/login` | none | Answers with a pair, an identity token, and your tenants |
| `POST /auth/logout` | `rig_at_` | Revokes the whole family |
| `POST /auth/refresh` | `rig_rt_` in the body | The only endpoint that takes one |
| `POST /auth/register` | none | Only when `AllowRegistration` |
| `GET /auth/oauth/{provider}/start` | none | Only when providers are configured |
| `GET /auth/oauth/{provider}/callback` | none | The provider sends the browser here |

### Before you are in a tenant

Mounted only when identity sessions are enabled, which `auth.New` always does.
All four take `rig_it_`, and none of them can read application data.

| | notes |
|---|---|
| `GET /auth/me/tenants` | Where you could go |
| `GET /auth/me/invitations` | Invitations addressed to you, across tenants |
| `POST /auth/me/invitations/accept` | Takes the invitation's **id**, not its token |
| `POST /auth/tenants` | Only when `AllowTenantCreation` |
| `DELETE /auth/me/session` | Sign out of the picker |

### Inside a tenant

All take `rig_at_`.

| | permission |
|---|---|
| `GET /auth/tenants` | — |
| `POST /auth/tenants/{id}/switch` | — |
| `GET /auth/sessions`, `DELETE /auth/sessions/{id}` | — for your own |
| `GET /auth/sessions?scope=all` | `session.read.all` |
| `DELETE /auth/sessions/{id}?scope=all` | `session.revoke.all` |
| `GET /auth/audit` | — for your own events |
| `GET /auth/audit?scope=all` | `authlog.read.all` |
| `POST /auth/accounts` | `account.provision` |
| `GET /auth/invitations`, `DELETE /auth/invitations/{id}` | `account.provision` |
| `GET /auth/api-keys`, `DELETE /auth/api-keys/{id}` | `apikey.own` (yours) or `apikey.manage` (anybody's) |
| `POST /auth/api-keys` | `apikey.own` for a personal key, `apikey.manage` for a service one |
| `POST /auth/impersonate`, `DELETE /auth/impersonate` | `account.impersonate` |

### Passwords and addresses

| | credential |
|---|---|
| `POST /auth/password/reset` | none |
| `POST /auth/password/reset/confirm` | none — the emailed token is in the body |
| `POST /auth/password/change` | `rig_at_` |
| `POST /auth/email/verify` | none — the emailed token is in the body |
| `POST /auth/email/verify/resend` | `rig_at_` |
| `POST /auth/invitations/accept` | none — the emailed token is in the body |

---

## Flows

### A stranger arrives

The four steps, and the second one is the part that does not exist in most
frameworks.

```
1  POST /auth/register            {emailAddress, displayName, password}
   → 201  identityToken, tenants: []                     ← no session yet

2  GET  /auth/me/invitations      Bearer rig_it_…
   GET  /auth/me/tenants          Bearer rig_it_…

3  POST /auth/me/invitations/accept   Bearer rig_it_…  {invitationId}
   or
   POST /auth/tenants                 Bearer rig_it_…  {name}
   → 200  accessToken, refreshToken, tenants: [{…, current: true}]

4  GET  /api/v1/notes            Bearer rig_at_…
```

Step 1 creates a person and **nothing else** — no tenant, no session. That state
has to exist, because somebody with an invitation waiting has an account and
belongs nowhere, and accepting an invitation requires being signed in.

An application that wants every newcomer to land somewhere answers step 2
itself: `OnRegistered` (under [What you decide](#what-you-decide)) runs inside
the registration transaction, and its ordinary body is `accounts.Provision`
with `Invite` set — so the picker the stranger lands in already lists an
invitation to a starter tenant. The transaction is the point: a hook error
rolls the whole sign-up back, so there is never an account that half-joined.

Accepting sends the invitation's **identifier**, not the token that was emailed.
Being signed in as the person invited is the *stronger* claim of the two: a token
proves somebody reached the address, a session proves who they are. That is why a
listing can hand out identifiers and never tokens.

### Somebody who already has an account

```
POST /auth/login   {emailAddress, password}          no X-Tenant-Id
→ 200 {
    accessToken, refreshToken, sessionId, expiresAt,
    identityToken,                    ← always, even alongside a session
    tenants: [ … ]                    ← so a picker needs no second call
  }
```

Belonging to one or more tenants lands you in the oldest — predictable beats
clever, and the rest are one `POST /auth/tenants/{id}/switch` away. Belonging to
**none** is a 200 with no `accessToken` and an empty `tenants`, not a 403: that
used to be a refusal and it made the flow above impossible.

Naming a tenant you are not in *is* still a 403. You asked for somewhere specific.

The identity token comes back even when a session does, because switching tenant
later is the same flow and the picker's endpoints are what answer "where else could
I go".

### Staying signed in

```
POST /auth/refresh   {refreshToken: "rig_rt_…"}
→ 200  a new pair; the presented token is now spent
```

**Rotation is mandatory.** Every refresh consumes the token it was given and mints
a new pair, in one transaction. A stolen refresh token is therefore useful exactly
until the real client refreshes — at which point the theft becomes visible instead
of permanent.

Replaying a spent token:

- **within 30 seconds** → you get the existing child pair back. A dropped response
  is not an attack, and revoking a family over a network blip would make every
  flaky connection a logout.
- **after that** → the **entire family is revoked**, an auth-log entry records the
  original and current IP and user-agent, and the answer is 401. Either the token
  was stolen or the legitimate holder's copy was, and there is no way to tell from
  inside the request — so the only safe move is to end it for both.

A session does not get longer by being refreshed: the child inherits the parent's
expiry. Otherwise a left-open tab would be immortal.

### A machine

```
POST /auth/api-keys   Bearer rig_at_…
  {"name": "Nightly import", "kind": "Integration", "scopes": ["note.read", "note.write"]}
→ 201 {"key": {…}, "secret": "rig_sk_…"}       ← the only time the secret exists
```

Then every request:

```http
Authorization: Bearer rig_sk_…
```

No login, no refresh, no session. A key is a credential on its own.

Two things worth knowing when minting:

- **A key can never be given authority its creator does not hold.** Every
  requested scope is checked against the caller's own permissions first, so
  "manage API keys" is not quietly "grant yourself anything".
- **Rotation is create-new then revoke-old**, with an overlap window, because a
  key in a deployment somewhere cannot be swapped atomically.

`last_used_at` is updated on a throttled write, so key auth stays a single read on
the hot path.

### Signing in with a provider

Two routes, mounted only when `auth.Config.OAuth.Providers` is non-empty.

```
GET /auth/oauth/google/start?returnTo=/dashboard
→ 302 to Google

GET /auth/oauth/google/callback?code=…&state=…
→ whatever your OnSignIn writes
```

They sit **under the auth base**, so a custom `BasePath` moves them with everything
else: `/api/auth` puts them at `/api/auth/oauth/{provider}/start`.

Built in: `oauth.Google(id, secret)`, `oauth.Microsoft(id, secret, tenant)`,
`oauth.GitHub(id, secret)`. The redirect URI is **built, not configured** — derived
from `BaseURL` and the route that was mounted, because two places to write one URL
is one place to get it wrong. It has to match what the provider has registered,
exactly.

#### The round trip carries no server state

`start` mints a random `state` and a PKCE verifier and seals both into one
HMAC-signed cookie:

```
__Host-rig_oauth   {state, verifier, provider, returnTo, expires}
```

The `__Host-` prefix is a browser-enforced promise: secure, path-scoped to `/`, and
not settable by a subdomain. `Insecure: true` drops to a plain name for local HTTP
and is for nothing else.

`callback` opens it, compares `state` against the query — a state that came back
without a matching cookie is somebody else's sign-in being finished in this browser
— and clears the cookie **whatever happens next**, because it is single-use and one
left behind is a state somebody could replay.

No table of pending sign-ins to clean up. `SigningKey` must be at least 32 bytes and
`New` refuses without one. PKCE on a confidential client is belt and braces and it
is free: a stolen authorization code is useless without the verifier, which never
left the server.

#### Then two questions, in order

**Who is this?** Answered globally, with no tenant involved:

1. `FindLink(provider, subject)` — the **subject**, always.
2. Failing that, `FindIdentityByEmail` — so "sign in with Google" reaches the person
   who already signed up with a password, rather than making a second one beside
   them.
3. Failing that, `ProvisionIdentity` — but only under `AllowProvisioning`.

**Do they belong here?** Answered per tenant from `FindAccount`, and answered **no**
unless `AllowProvisioning` is on and `JoinTenant` accepts them — which must honour
the tenant's allowed email domains.

#### Three decisions worth more than the rest of the package

**Matching is on the provider's `subject`, never on the address.** Subjects are
stable for the life of an account; addresses change, and providers hand a released
address to somebody else. Matching on the address is how one person ends up signed
in as another.

**Linking an existing person requires `EmailVerified`.** Anybody can register any
address at some provider; only a verified one is evidence. Without this check,
whoever registers your address anywhere owns your account here. GitHub does not
report verification on `/user`, so its provider fetches `/user/emails` and reads the
primary address's flag.

**`AllowProvisioning` is off by default.** A provider will authenticate anybody with
a Google account. An open sign-in endpoint on a business application is a way for a
stranger to appear inside a customer's tenant — rarely what anyone wants and never
what they expect. One switch gates both doors: creating the identity, and joining
the tenant.

#### The tenant is decided before the redirect

A provider sign-in has to know which tenant it is for **at the start**, because
that is what the callback joins somebody to. `start` resolves it with
`Config.Tenant` and seals it into the state cookie; `callback` reads it from there
rather than asking again.

That is not an optimisation. The callback URL is registered with the provider and
fixed, so it carries nothing an application's resolver could read — a header or a
query parameter that was there on the way out is gone on the way back. Only a
**host** survives, which is why a subdomain deployment
(`acme.example.com/auth/oauth/google/start`) never has to think about this and
anything else does.

Carrying it also means a callback cannot be replayed against a different tenant
than the one it started for.

**A host per tenant needs `OAuth.Origin`.** The callback URL is built from
`BaseURL`, which is one origin — but the state cookie is host-only, so a sign-in
started at `beta.example.com` and sent back to `acme.example.com` arrives without
the cookie and is refused. `Origin` answers per request instead:

```go
Origin: func(r *http.Request) string { return "https://" + r.Host },
```

The constraint it lives inside is the provider's: a redirect URI is registered
exactly, and few providers accept a wildcard — so every origin it can return has to
be registered. A deployment with more subdomains than a console can hold keeps the
callback on one canonical host and has `OnSignIn` hand the finished session on to
the tenant's own host.

Google and Microsoft also refuse plain `http` for anything but `localhost` and
`127.0.0.1`, so a subdomain over plain http cannot be registered with either at all
— which makes https the only shape a tenant-per-host deployment can use with them.
`examples/auth_oauth` documents both ways round it for local work.

#### How it ends is yours

`OnSignIn` is required; `New` refuses without it.

```go
OnSignIn: func(w http.ResponseWriter, r *http.Request, in oauth.SignIn) error {
    // in.Link, in.TenantID, in.AccountID, in.Profile, in.Provider
    // in.New       — this sign-in created the account
    // in.ReturnTo  — already checked against the allow-list
}
```

It writes the response: set a cookie, redirect with a token, render a page. rig does
not choose, because the choice depends on whether the client is a browser, a
single-page application or a native app catching a deep link.

Through `auth.New` it defaults to `authhttp.Handler.SignIn`, which issues the same
pair a password login does — so a provider sign-in and a password sign-in produce
the same credential and everything downstream is identical.

`New` is true for somebody joining their **second** tenant as well as their first:
the account is new either way, which is what onboarding is about.

`returnTo` is bounded. A relative path on this origin always passes; anything else
has to be in `AllowedReturnTo`. An unchecked `returnTo` is an open redirect, and an
open redirect on a sign-in endpoint is how a phishing link gets to wear your domain.

Cancelling at the provider comes back as `?error=…` and is answered 400 with an
`OAuthSignIn` / `Failed` log entry — not a 500, because nobody's server failed.

### Acting as somebody else

```
POST   /auth/impersonate   Bearer rig_at_…   {accountId}     needs account.impersonate
DELETE /auth/impersonate   Bearer rig_at_…
```

The issued session carries `ImpersonatedByAccountID`, which **propagates through
every rotation** — a session that began as impersonation cannot quietly become an
ordinary one. Both ends are in the auth log.

---

## What the caller becomes

Whatever the credential, it resolves to one value:

```go
type Claims struct {
    TenantID    uuid.UUID
    AccountID   uuid.UUID
    Subject     Subject          // Account | ApiKey | System
    Roles       []string
    Permissions []string
    APIKeyID    *uuid.UUID       // which credential, not whose change
    Extra       json.RawMessage  // your session context
    ImpersonatedByAccountID *uuid.UUID
}
```

`AccountID` is *whose change this is*; `APIKeyID` is *which credential it came
through*. One service account can have several keys, and when something has gone
wrong the useful question is which key to revoke.

**Permissions are resolved per request, never baked into the token.** After the
credential resolves, your `Grants` function fills them in — so revoking a role
bites on the next call rather than whenever the session happens to refresh. That
is also why `Extra` is documented as never for authorization: it is only as fresh
as the last refresh.

`TenantID` is never zero. That is the invariant every generated query relies on,
and it is why "signed in with no tenant" is a *different credential* rather than
claims with a hole in them.

---

## Status codes

Decided in one place, which is what stops them drifting per endpoint.

| | meaning |
|---|---|
| **401** | Identity could not be established — missing, malformed, expired, revoked or replayed token; unknown key; wrong password. *Never* a permission failure. |
| **403** | Identity is known and not permitted — missing permission, disabled account, a widening you do not hold. |
| **404** | The row belongs to another tenant, or to another person on an owner-scoped table. Not 403: a distinct "you may not see this" turns every identifier into an existence oracle. |
| **429** | Any throttle or lockout, always with `Retry-After` and `RateLimit-*`. |

**The 404 survives the widening.** `DELETE /auth/sessions/{id}` answers 404 for a
session that is not yours, and it keeps answering 404 for a caller who holds
`session.revoke.all` and names a session in another tenant — the same 404 an
invented identifier gets. What the permission changes is which sessions you may
end, never which ones you can find out about. Asking for `scope=all` without
holding it is refused before the identifier is looked at, so even that 403 says
nothing about whether the session exists.

---

## Rate limits

Counted over `rig_auth_log` with a sliding window. No Redis, no in-memory state,
correct across replicas, and self-healing — counters age out.

| limit | `auth.limits` key | keyed on | default | cleared by a success |
|---|---|---|---|---|
| Failed login | `login_by_email` | `lower(email_address)` | 5 / 15 min → lockout | yes |
| Failed login | `login_by_ip` | IP address | 50 / 15 min | **no** |
| Password reset request | `password_reset` | `lower(email_address)` | 5 / hour | no |
| Verification resend | `verification_resend` | account | 5 / hour | no |
| Refresh | `refresh` | session root | 60 / min | no |
| API key auth failures | `api_key_failures` | `key_id` | 20 / min | yes |

The configuration sets `max` and `window`. Which event a limit counts, and what
clears it, stays rig's: a limit counting something else under the same name would
not be the same limit.

Two keys for login, deliberately: an email-only limit lets one attacker lock a
victim out, and an IP-only limit lets a botnet spray. The IP limit is *not* cleared
by a success — one valid login from a shared address would otherwise wipe the
record of a thousand failures from the same place, which is the thing it exists to
notice.

The lockout check runs **before** password verification, so a locked request
neither burns an argon2 hash nor extends its own window. Login is padded to a
configurable floor (750ms) so response time does not reveal whether an account
exists.

---

## Verifying without a row read

Every authenticated request resolves a session token or an API key, and that is a
row read. An API-key request makes a second one before it — the failure limit that
stops somebody grinding secrets against a key id, counted out of the
authentication log. Turning on [`cache:`](rig-yaml.md#cache) in `rig.yaml` holds
both answers in memory instead:

```yaml
cache:
  enabled: true
```

**This is not a time-to-live over authentication.** A cache over authorization
with only a timer on it is a revoked session that keeps working, which is why rig
never shipped one. What this switches on is a Postgres `NOTIFY` channel: every
revocation the foundation performs — a logout, an administrative revoke, a
password change ending every session, reuse detection killing a family, an API
key revoked or rotated — publishes **inside the transaction that performed it**.
Postgres delivers a notification when its transaction commits and throws it away
if that transaction rolls back, so the invalidation is atomic with the change,
reaches every replica, and needs no outbox, no trigger and no second piece of
infrastructure.

So a session ended on one replica stops working on all of them at the moment the
revocation commits. `ttl` is only the backstop, for a replica that was not
listening at that moment — and a replica that knows it has lost the channel stops
caching altogether rather than serving what it can no longer withdraw.

The failure limit works the same way and for the same reason, with one wrinkle
worth knowing. Only the *zero* is held — "this key id has no recent failures" —
because inside a window a count can only rise, and every row it counts is one rig
writes. A key that somebody is already grinding is counted afresh on every
attempt, so the limit bites on exactly the attempt it would have without a cache;
what stops costing a query is the integration that has never once got its key
wrong. That is the opposite of a process-local tally, which would let each replica
wave through an interval of traffic it could not see.

Nothing here is yours to wire, including the shutdown. There is no map to build
and no invalidation to publish, because rig caches exactly the reads it owns on
both sides — it makes the read and it makes every write that withdraws it. What
holds a connection is the invalidation channel, and closing it is a field rather
than a line to remember:

```go
return api.Parts{Handler: mux, Auth: front}, nil
```

`api.Main` closes it, within the five seconds `api.ShutdownBudget` already counts
for it — `serve.Config.Shutdown: api.Shutdown{Auth: ...}` is how a deployment
asks for another, see [services.md](services.md). It used to be `app.CloseWithin("auth", 5*time.Second, front.Close)` in
every `main.go`, which was exactly the wrong shape for something that costs a
connection rather than correctness: leave it out with no cache configured and
nothing happens at all, until the day somebody turns the cache on.

The same block also covers one read per table that asks for it — `cache: true` in
a table's configuration file holds its `Get`. That one *is* a promise you are
making, for the reason the next paragraph gives about `Grants`: the writes are
through rig's repository or they are invisible to it. See
[tables.md](tables.md#cache).

**Your `Grants` function is not cached, and that is deliberate.** It is the
expensive read on this path — a join over role tables, per request — but the
tables are yours and so are the writes, and rig cannot see them. Caching it would
mean you publishing your own invalidations, and a write path left out there is a
permission you revoked that goes on working with nothing to say so. rig will not
make that promise on your behalf, so turning this on is a call rather than a key
in `rig.yaml`.

If you decide to take it on, `auth.NewGrantsCache` is the map and the obligation
that comes with it. Three lines, in this order, because the generated wiring needs
the function before it can hand back the bus:

```go
grants := auth.NewGrantsCache(auth.GrantsCacheConfig{})

front, err := api.New(pool, api.Hooks{
	Grants: grants.Wrap(authz.Grants(pool)),
})

grants.Serve(front.Parts().Cache)
```

And then the half that is yours: every write that changes what somebody may do
publishes on the transaction that made it.

```go
func AttachRole(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID, ...) error {
	if _, err := tx.Exec(ctx, `INSERT INTO account_role ...`); err != nil {
		return err
	}
	return grants.Invalidate(ctx, tx, tenantID, accountID)
}
```

`Invalidate` withdraws one account's answer in one tenant. `InvalidateAll` is for
the writes that change what a role *means* rather than who holds it — seeding a
tenant's roles, editing the grants on one. Both take the transaction that made the
change, so the invalidation commits with it and is thrown away if it rolls back.

What the helper is doing for you is the six things that are the same for
everybody and easy to get wrong once: the key is the tenant *and* the account,
the two slices are copied on the way out, an error is never held, an empty answer
is, a replica that has lost the channel reads through, and the publish rides your
transaction. `examples/auth` is wired this way end to end —
`services/authz/authz.go` is the whole of the obligation, three call sites.

Every part of it is fail-safe: a cache that was never served holds nothing, a bus
that is not running reports itself as not live, and a map that is not live reads
through. Leaving out `Serve`, or the `cache:` block, costs latency rather than
correctness. What is *not* fail-safe is the half rig cannot check — a role write
you forgot to publish from is a permission that goes on working until `ttl`
expires. That is the trade, and it is why this is opt-in.

**Watch for the write paths that are not yours.** rig's own configuration for
`rig_account` is read-only, and this is one of the reasons: if your `Grants` reads
`rig_account.role` and you widen that resource to `Update`, a `PATCH` on it
changes the answer without going anywhere near your role tables. A `dbhook`
`BeforeCommit` on that update is where the `Invalidate` goes, inside the
transaction rather than after it.

---

## The authentication trail

Every sign-in, failure, lockout, logout, refresh, replay, key use, impersonation,
invitation and tenant switch is a row in `rig_auth_log` — twenty-two events,
written by the foundation as they happen. Two endpoints read them, and which one
you get depends on what you ask for:

```
GET /auth/audit                your own events         no permission
GET /auth/audit?scope=all      the tenant's events     authlog.read.all
```

One endpoint and a parameter, not two endpoints. `scope` works here the way it
works on every generated read: the caller says how wide an answer it wants, the
response says what it got, and asking for more than you hold is a **403** rather
than a quietly smaller result. A narrower answer would leave a client unable to
tell "you may not see that" from "there is nothing else."

Your own trail costs nothing to reach, because it is a screen every product
eventually wants: *where have I signed in from, and did anything fail.* Without
`scope=own` falling out of the same endpoint that would be a second route with a
second shape and its own bugs.

Filters, all optional, all refusing a value they do not understand rather than
answering with fewer rows — a misspelled event that returned an empty page would
read as "that never happened":

| | |
|---|---|
| `accountId` | one person's events. Only with `scope=all`; naming somebody else without it is a 400 |
| `event` | one of the recorded events. An unknown name is a 400 |
| `outcome` | `Succeeded` or `Failed` |
| `since`, `until` | RFC 3339 instants, `since` inclusive and `until` exclusive |
| `limit`, `offset` | 50 by default, 500 at most |

This is the one `/auth/*` endpoint that pages. The rest answer `{"data": […]}`,
because a tenant's keys, invitations and tenants are a handful of rows; a trail is
millions, so it answers `{"data": […], "pagination": {…}}` with the same three
members and the same bounds every generated list uses. The generated Go client
walks it with `Auth.AuditLogAll`.

### What it does not show

**The entries that resolved to no tenant.** A sign-in that named none, and an
attempt against an address with no account anywhere, are both recorded with a null
`tenant_id` — they are exactly what the rate limiter most needs to count — and no
tenant can read them. The query is `tenant_id = $1` and nothing else. The tempting
widening is to match on the email address instead, so a tenant sees failed attempts
against its own people's addresses even when nobody named the tenant; that hands
tenant A a record of tenant B's people typing their own addresses into a login
form. A global view of those attempts is a real need and it is an operator's need:
query the table.

**Anything about a row.** This is authentication only — what happened to a
credential. What happened to a row is [snapshots](schema.md), which replaced the
audit log rig used to have.

### Or expose the table instead

`auth: {expose: [rig_auth_log]}` gives you the log as an ordinary resource: a
model, a repository, and `Get`, `List` and `Search` with the generated filters and
live sync. That one line is the whole of it — the configuration saying which
operations belong on rig's own table is rig's, and there is no file to write. See
[rig's own tables need no file](tables.md#rigs-own-tables-need-no-file). Both answers stay, and the difference between them is the
point. A generated read filters by tenant, so it cannot see the tenant-less rows
**and has nowhere to explain that it cannot**. The endpoint excludes them
deliberately, and this page is where it says so. Take the resource if you want the
log as data; take the endpoint if you want the trail.

### Retention

Nothing prunes `rig_auth_log` unless you say so:

```yaml
auth:
  log_retention: 90d
```

That writes an `AuthLogPruner` task into your API package. Register it in
`serve.Config.Tasks` and it becomes a subcommand for a cron job — a job rather
than a goroutine, because housekeeping that schedules itself inside the server is
housekeeping every replica does at once, to the table the whole authentication
path is writing to.

> **The window has a floor, and rig refuses to go under it.** The rate limits are
> counted from this table. A retention window shorter than the longest limit
> window deletes the failures a lockout is adding up to — so the limiter goes on
> answering "allowed" with nothing to say it has stopped working. `rig check`
> refuses such a window naming the limit it would break, and `auth.New` refuses to
> start on one assembled in Go. It is refused rather than quietly raised, because
> a number changed behind your back is a number you cannot reason about later.

---

## What you configure

Everything with a fixed answer is in `rig.yaml`, and that is not a stylistic
preference: the reference documentation and the client libraries are generated
from the same file, so a token lifetime written in a Go literal is a lifetime
nothing else can read. A block that says `enabled: true` and nothing more is a
working configuration.

```yaml
auth:
  enabled: true            # nothing below it is read without this

  base_path: /auth

  # Where a request says which tenant it is for. Tried in order; the first that
  # names one wins, and a request that names none is an ordinary answer.
  #
  #   header  a header, X-Tenant-Id by default
  #   host    the leftmost label of the Host, looked up as a tenant slug
  #   query   a query parameter, for a local demonstration
  #   hook    your own resolver, passed to the generated wiring
  tenant:
    from: [host]
    default_slug_env: DEFAULT_TENANT   # host only: the slug to fall back to

  session:
    access_ttl: 10m
    refresh_ttl: 12h
    remember_ttl: 30d
    rotation_leeway: 30s
    identity_ttl: 30m

  password:
    min_length: 12
    max_length: 1024
    breach_check: false     # Have I Been Pwned, hash prefix only, fails open

  # Whether the routes exist at all. Off means no route, rather than a 403 to
  # something probeable.
  allow_registration: false
  allow_tenant_creation: false
  require_verified_email: false

  # Only the numbers. Which event each limit counts is rig's — see Rate limits.
  limits:
    login_by_email: {max: 5, window: 15m}
    login_by_ip: {max: 50, window: 15m}

  trusted_proxies: [10.0.0.0/8]        # empty believes no X-Forwarded-For

  # How long an entry in the authentication trail is kept. Absent keeps
  # everything, which is the default because how long a trail has to survive is
  # a compliance question. Setting it writes a `prune-auth-log` subcommand; it
  # cannot be shorter than the longest window above. See The authentication trail.
  log_retention: 90d

  # Foundation tables to generate a model, a repository and an API for anyway —
  # for an administration screen listing the people in a tenant, most often. It
  # changes nothing about authentication: rig/auth still reaches these tables
  # through its own queries, and a generated repository beside them is a second
  # door into the same rows. rig_account's row comes from rig/authmodel, so its
  # JSON keys are camelCase whatever naming.json_case says — RIG3260.
  expose: [rig_account]

  # Take the schema over: generate for every foundation table, and stop
  # importing rig/auth. It also stops rig reserving the `rig_` prefix and the
  # names its tables project to, because from here they are yours. A one-way
  # door — everything on this page becomes code you maintain.
  own: false

  oauth:
    base_url: https://app.example.com  # a provider compares this exactly
    base_url_env: BASE_URL             # and the environment gets the last word
    origin_from_host: false            # or derive it per request, see below
    signing_key_env: OAUTH_SIGNING_KEY # >= 32 bytes, the same in every replica
    state_ttl: 10m
    allow_provisioning: false
    allowed_return_to: [https://app.example.com]
    insecure: false                    # never set this in a deployment
    providers:
      - name: google                   # GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET
      - name: microsoft
        required: true                 # refuse to start without its credentials
        client_id_env: MS_ID           # when the defaulted names do not suit
        client_secret_env: MS_SECRET
        tenant_env: MICROSOFT_TENANT   # Microsoft's tenant, not rig's
```

A client secret is never in the file. The configuration names the environment
variable and the generated code reads it, which is also what lets one binary
offer Google in a deployment and nothing at all on a laptop: a provider whose
pair is absent is skipped rather than mounted broken, unless it says `required`.

`client_id_env` and `client_secret_env` default to the provider's name in upper
case, so `- name: google` already reads `GOOGLE_CLIENT_ID` and
`GOOGLE_CLIENT_SECRET` and most projects never set either. `tenant_env` is
Microsoft's own idea of a tenant, which has nothing to do with rig's: `common`
accepts any account, `organizations` excludes personal ones, and a directory id
restricts sign-in to one organization.

`origin_from_host` derives the callback origin from each request's `Host`
instead of from `base_url`. It is what an application serving a tenant per
subdomain needs: the state cookie carrying the PKCE verifier is set on the host
the sign-in started at, and a browser will not send it to a sibling subdomain —
so the callback URL is per host, and every one of them has to be registered with
the provider.

`insecure` allows that state cookie over plain HTTP, because a browser refuses a
`__Host-` prefixed cookie without TLS and a laptop rarely has any. It is for
local development and nowhere else.

There is no generator to add. `server-go` already writes your API package, and
`rig generate` writes the assembly into it as one more file — `auth.gen.go`,
beside the routes and the handlers. That is why an authentication failure is
shaped like every other failure this API returns: the error mapper it reaches
for is the same package's, with no import path in a configuration file to say
where to find it.

A project with no `auth:` block gets no such file, and so never depends on
rig/auth — `examples/todo` serves a list of chores without pulling in argon2.

---

## What you decide

What is left in Go is what a file cannot hold: a function, and a secret. Three
functions, and each is optional until the configuration makes it necessary — the
generated `Config` refuses at construction rather than failing on a request.

```go
front, err := api.New(pool, api.Hooks{
    // Who holds which permission. rig derives the keys from your schema and
    // generates the check; where the strings come from is yours. A role table, a
    // switch on the account's level, a claim on a federated token — every product
    // answers it differently, so rig ships no answer.
    //
    // Required when your handlers check one, which they do by default.
    Grants: myGrants(pool),

    // Where a reset link, a verification link or an invitation goes. Nil sends
    // none, and is refused outright when require_verified_email is set: a link
    // nobody sends is an account nobody can ever use.
    Notifier: mail,

    // Whether those links are queued rather than sent inside the request. Off by
    // default; see "Mail that survives a provider outage" below, and note that
    // turning it on without a cron entry turns mail off.
    Mail: auth.MailOptions{Queue: true, Retention: 30 * 24 * time.Hour},

    // What a tenant is beyond its row and its first account. The configuration
    // says the endpoint exists; this says what it does.
    Tenants: account.TenantOptions{
        Allow:     func(ctx, by account.Creator) error { … },      // who may
        Validate:  func(ctx, *account.TenantDraft) error { … },    // what a name may be
        Slug:      func(name string, id uuid.UUID) string { … },
        OnCreated: func(ctx, made account.NewTenant) error { … },  // what else it needs
    },

    // What happens to a stranger who just signed themselves up, inside the
    // transaction that created them — an error rolls the sign-up back. The
    // ordinary body is Provision with Invite set, so the picker they land in
    // already has an invitation to a starter tenant waiting. Present only when
    // allow_registration is set; nil registers the person and nothing else.
    OnRegistered: func(ctx context.Context, accounts *account.Service, in account.Registered) error {
        _, err := accounts.Provision(ctx, account.ProvisionInput{
            TenantID:     starterTenant,
            EmailAddress: in.EmailAddress,
            DisplayName:  in.DisplayName,
            Invite:       true,
        })
        return err
    },

    // How a provider sign-in ends, for a browser that wants a cookie and a
    // redirect rather than JSON. Nil issues the same session a password login does.
    OAuth: api.OAuthHooks{OnSignIn: nil},

    // Where the cause of a failed auth request is recorded. Nil uses
    // slog.Default(). Pass the same logger you give Server.Logger below: these
    // routes answer on the same mux and in the same shape, and a 500 from
    // signing in should not be the one line that lands somewhere else.
    Logger: app.Logger,
})

mux := api.Register(api.Handlers{
    Server: api.Server{
        Auth:   front,      // GetClaims and /auth/* in one field
        DB:     pool,       // where a write carrying an Idempotency-Key is recorded
        Logger: app.Logger, // where the cause of a 500 goes
    },
    Note: note.New(repos.Notes),
})
```

They are two fields rather than one because of the order: the configuration is
built first, and what it produces is what `Server.Auth` is then set to. See
[observability.md](observability.md).

`OnCreated` runs **inside** the transaction that made the tenant — reach it with
`dbx.Tx(ctx)`, the same way a generated repository does. That is what makes seeding
roles safe: a tenant whose roles failed to seed is a tenant whose Owner can do
nothing, and it rolls back with them.

`api.Config` returns the same configuration without assembling it, for a
project that needs one field this generator cannot express: take it, change that
field, and call `auth.New` yourself rather than abandoning the generated wiring.

---

## Mail that survives a provider outage

Every link rig mints — a password reset, an address confirmation, an invitation —
goes out through your `Notifier`. By default that call happens **inside the
request that asked for it**, which is the simplest thing and has one bad
afternoon in it: when your provider is down, the request fails, the caller's
rate-limit budget is already spent, and the token that was just minted is dead.
The person asks again and it costs them another attempt against the limiter.

Set `Mail.Queue` and the link is written to `rig_identity_verification_delivery`
in the same transaction instead, and sent later:

```go
front, err := api.New(pool, api.Hooks{
    Notifier: mail,
    Mail:     auth.MailOptions{Queue: true, Retention: 30 * 24 * time.Hour},
    Grants:   myGrants(pool),
})
```

**Then register the dispatcher, in the same change.** Nothing runs it for you.

```go
serve.Config{Tasks: map[string]serve.Task{
    "dispatch-auth-mail": api.AuthMailDispatcher(front, slog.Default()),
}}
```

```cron
*/1 * * * *  /srv/app dispatch-auth-mail
```

With the queue on and no such entry, links are queued and never sent, which is
the one way turning this on is worse than leaving it off. Register the task
first; it claims nothing and returns while the queue is off.

The schedule is notify's, deliberately: doubling from a minute up to an hour,
giving up after about eight hours, each wait spread upward so a provider refusing
a batch does not meet the whole batch again at one instant. Your `Notifier` can
say more than "it failed" — `account.PermanentMailError` stops a delivery on this
attempt when the provider refuses the *recipient*, and `account.RetryMailAfter`
honours a `Retry-After`. Both are optional and a plain error keeps working.

**The trade is latency.** A queued reset mail arrives up to one dispatch interval
late where inline it went out inside the request. That is the whole cost, and it
is why this is off by default.

### The token changes on every attempt

This is the part to know before you write your `Notifier`.

A queued row does **not** carry the token. rig stores only a SHA-256 of it and
the plaintext is never written down, so a queue that held one would put live
bearer tokens at rest. Instead the row holds the *intent*, and the dispatcher
generates the secret immediately before each send and rotates it into the link.

Three things follow:

- **Do not give your provider an idempotency key for these.** It is good advice
  everywhere else in rig and wrong here: each attempt carries a different token
  and the previous one has stopped working, so a provider that suppresses the
  second mail as a duplicate delivers a link that does not work.
- **A link's expiry runs from the send, not from the request.** A mail that
  waited out an outage arrives with its full window rather than the remains of
  one.
- **A link consumed or withdrawn before the mail went out is never sent.** The
  delivery is marked `Skipped`. Inline, withdrawing an invitation cannot recall a
  mail that has already gone; queued, it can.

Deliveries that are done are removed after `Mail.Retention` by the same task, and
zero keeps them forever. The link rows themselves are never pruned by it — those
are the record of who was invited and when.

## Tuning

Every one of these is a key under `auth:` in `rig.yaml`. The defaults are what
you get by writing none of them.

| | default | what it costs |
|---|---|---|
| `session.access_ttl` | 10m | Shorter is safer and refreshes more |
| `session.refresh_ttl` / `remember_ttl` | 12h / 30d | How long a stolen refresh token is worth having |
| `session.identity_ttl` | 30m | How long somebody has to pick a tenant |
| `session.rotation_leeway` | 30s | Longer forgives more retries and widens the replay window |
| `oauth.state_ttl` | 10m | How long a sign-in round trip may take. Generous for a redirect, short enough that a stolen state is useless. |
| `password.min_length` | 12 | Length is what helps; composition rules push people toward `Password1!` |

Durations are Go's syntax with `d` for days: `250ms`, `45s`, `15m`, `12h`, `30d`.

rig refuses a combination that would behave as nobody intended — an access token
that outlives its session, a "remember me" shorter than an ordinary session, a
rotation leeway a consumed token never leaves — rather than adjusting it quietly.

---

## See also

- `examples/auth` — every flow above except the provider sign-in, driven from a
  browser, with a transcript panel showing the actual requests.
- `examples/auth_oauth` — the provider sign-in, and the deployment shape it needs: a
  tenant per subdomain, so the host names the tenant a sign-in is for. It works
  with no credentials at all, because `services/idp` is a stand-in provider the
  example serves itself — and not a mock: single-use authorization codes, PKCE
  verified at the token endpoint, and a consent screen that lets you choose
  whether it says the address is verified, so both branches of that check are
  reachable from a browser. Setting `GOOGLE_CLIENT_ID` and
  `GOOGLE_CLIENT_SECRET` replaces it with nothing else changing.
- `examples/auth/services/authz` — a worked authorization model, if a starting
  point beats a blank page.

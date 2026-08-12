# Authentication

Four credentials, one door, and a rule for which is which.

Everything below is served by `rig/auth` over the tables `rig setup-project`
wrote — every one of them named `rig_…`, so a project can tell them from its own. Nothing about it is generated, and nothing about it is yours to maintain —
what *is* yours is three optional functions, listed under
[What you decide](#what-you-decide).

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
| `GET /auth/sessions`, `DELETE /auth/sessions/{id}` | — |
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

---

## Rate limits

Counted over `rig_auth_log` with a sliding window. No Redis, no in-memory state,
correct across replicas, and self-healing — counters age out.

| limit | keyed on | default | cleared by a success |
|---|---|---|---|
| Failed login | `lower(email_address)` | 5 / 15 min → lockout | yes |
| Failed login | IP address | 50 / 15 min | **no** |
| Password reset request | `lower(email_address)` | 5 / hour | no |
| Verification resend | account | 5 / hour | no |
| Refresh | session root | 60 / min | no |
| API key auth failures | `key_id` | 20 / min | yes |

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

## What you decide

Three functions and two flags. Everything else has a default that works.

```go
front, err := auth.New(auth.Config{
    Pool: pool,

    // Who holds which permission. rig derives the keys from your schema and
    // generates the check; where the strings come from is yours. A role table, a
    // switch on the account's level, a claim on a federated token — every product
    // answers it differently, so rig ships no answer.
    Grants: myGrants(pool),

    // Whether a stranger may sign themselves up. Off means the route does not
    // exist, rather than answering 403 to something probeable.
    AllowRegistration: true,

    // Whether POST /auth/tenants exists at all.
    AllowTenantCreation: true,

    // Sign-in through a provider. Leave Providers empty and the two routes are
    // not mounted.
    OAuth: auth.OAuth{
        Providers:  []oauth.Provider{oauth.Google(id, secret)},
        BaseURL:    "https://app.example.com",   // must match the provider exactly
        SigningKey: signingKey,                  // >= 32 bytes
        // Off by default: a provider authenticates anybody, so joining a tenant
        // stays a decision.
        AllowProvisioning: false,
        AllowedReturnTo:   []string{"https://app.example.com"},
        // Nil issues the same session a password login does.
        OnSignIn: nil,
    },

    // And what a tenant is, beyond its row and its first account.
    Tenants: account.TenantOptions{
        Allow:     func(ctx, by account.Creator) error { … },      // who may
        Validate:  func(ctx, *account.TenantDraft) error { … },    // what a name may be
        Slug:      func(name string, id uuid.UUID) string { … },
        OnCreated: func(ctx, made account.NewTenant) error { … },  // what else it needs
    },
})

mux := api.Register(api.Handlers{
    Server: api.Server{Auth: front},   // GetClaims and /auth/* in one field
    Note:   note.New(repos.Notes),
})
```

`OnCreated` runs **inside** the transaction that made the tenant — reach it with
`dbx.Tx(ctx)`, the same way a generated repository does. That is what makes seeding
roles safe: a tenant whose roles failed to seed is a tenant whose Owner can do
nothing, and it rolls back with them.

---

## Tuning

| | default | what it costs |
|---|---|---|
| `AccessTTL` | 10 min | Shorter is safer and refreshes more |
| `RefreshTTL` / `RememberTTL` | 12 h / 30 d | How long a stolen refresh token is worth having |
| `IdentitySessionTTL` | 30 min | How long somebody has to pick a tenant |
| `RotationLeeway` | 30 s | Longer forgives more retries and widens the replay window |
| `OAuth.StateTTL` | 10 min | How long a sign-in round trip may take. Generous for a redirect, short enough that a stolen state is useless. |
| `CacheTTL` | `0` — off | Setting it trades immediate revocation for throughput. A revoked session keeps working for up to this long. Do not set it above a few seconds, and do not set it at all unless the read is measurably a problem. |

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

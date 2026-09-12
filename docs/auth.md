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

> sha256 rather than something memory-hard, and the difference is deliberate. A
> token secret is 256 bits from the system's random source, so no amount of
> guessing will find it, and running a memory-hard function on every request
> would be a denial of service aimed at ourselves. The one secret here that *is*
> guessable is a sign-in code, and what keeps that safe is an attempt ceiling
> rather than a hashing cost — see [Signing in with a code](#signing-in-with-a-code).

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

Read **only** where a tenant cannot be known any other way: `POST /auth/email-code` and
`POST /auth/email-code`. Once there is a session the tenant comes from the
token, and this header is ignored.

Absent is not an error — it means *unspecified*, and login treats that as "sign me
in wherever I belong". That is what a single sign-in page needs: a visitor cannot
say which tenants an address belongs to before the code has come back. A
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

Both SDKs cover this whole surface, so a path per call site is not the intended
way to reach it in either language: `client.Auth` in Go
([clients.md](clients.md#asking-for-the-whole-tenant)), `client.auth` in
TypeScript ([clients.md](clients.md#signing-in)). Each method already knows
which credential its route takes and that the route sits beside the API's base
path rather than inside it, which are the two things a caller cannot see from
the table below.

### Signing in and out

There are two ways in and neither is a password. rig stores none, and there is
no endpoint that would take one.

| | credential | notes |
|---|---|---|
| `POST /auth/email-code` | none | Always 204. Only when `auth.email_code.enabled` |
| `POST /auth/email-code/verify` | none | Answers with a pair, an identity token, and your tenants |
| `POST /auth/logout` | `rig_at_` | Revokes the whole family |
| `POST /auth/refresh` | `rig_rt_` in the body | The only endpoint that takes one |
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
| `POST /auth/invitations` | `account.provision` |
| `GET /auth/invitations`, `DELETE /auth/invitations/{id}` | `account.provision` |
| `GET /auth/api-keys`, `DELETE /auth/api-keys/{id}` | `apikey.own` (yours) or `apikey.manage` (anybody's) |
| `POST /auth/api-keys` | `apikey.own` for a personal key, `apikey.manage` for a service one |
| `POST /auth/impersonate`, `DELETE /auth/impersonate` | `account.impersonate` |

### Addresses and invitations

| | credential |
|---|---|
| `POST /auth/email/verify` | none — the emailed token is in the body |
| `POST /auth/email/verify/resend` | `rig_at_` |
| `GET /auth/invitations/preview` | none — the token is in the query string |
| `POST /auth/invitations/accept` | none — the emailed token is in the body |

`GET /auth/invitations/preview` is the only unauthenticated `GET` rig serves, and
it is worth knowing why. Somebody following an invitation is, more often than
not, signed out and on a device this deployment has never seen — so a landing
page holds a token and nothing that can interpret it. This answers what the token
is *for*, without consuming it and without extending it, so the page can say
"Anna invited you to Skolan i Solna" instead of showing a bare sign-in box.

One 404 for every way the token can be wrong — unknown, used, withdrawn, expired,
the wrong kind — so it cannot be used to probe the table, and it is rate-limited
by source address.

---

## Flows

### Signing in with a code

The way in for anybody with no account at a configured provider, which in most
deployments is most people. Two calls, and a mailbox between them.

```
1  POST /auth/email-code          {emailAddress}
   → 204                                                 ← always, whatever you type

2  POST /auth/email-code/verify   {emailAddress, code}
   → 200  accessToken, refreshToken, identityToken, tenants: [ … ]
```

**The first answers 204 for an address nobody has, and for one somebody does.**
Any difference in status, body or timing would make it a list of who has an
account here, which is what the endpoint would then be used for. The second is
padded for the same reason: with no password to verify there is no expensive
operation making the two paths cost the same, so a floor over both is the whole
of what keeps them indistinguishable.

**A code dies of being guessed at, not just of being slow.** Six digits is a
million values, and a rate limit counts failures over a rolling window — it
cannot kill one secret. `auth.email_code.max_attempts` can: three wrong guesses
and the code is revoked, and the way out is to ask for another. That default is
deliberately well under the five wrong sign-ins that lock an address, so that
mistyping a code is recoverable rather than a fifteen-minute lockout.

**One live code per person.** Asking again revokes the last one, so "the newest
code is the one that works" is true rather than probable, and the table cannot be
filled by asking repeatedly.

**Typing the code back confirms the address**, because the code went there and
came back. That is the same evidence an invitation link is, and it is why
`auth.require_verified_email` does not deadlock a code-only deployment: the
first sign-in satisfies it.

`auth.email_code.allow_provisioning` is the other decision. Off — the default —
a code only reaches an address that already has an identity, which makes the
deployment invite-only. On, an address rig has never seen gets a person created
when the code is asked for, and `OnRegistered` runs in that transaction. It is
the analogue of `auth.oauth.allow_provisioning`, and both doors are shut by
default for the same reason.

> **There is no registration endpoint, and that is not an omission.** With no
> password there is nothing for one to take: "create an identity for this
> address" is what asking for a code already does, and an endpoint that minted
> an identity token for anybody who typed an address would be a door rather than
> a form.

### A stranger arrives

The four steps, and the second one is the part that does not exist in most
frameworks.

```
1  POST /auth/email-code          {emailAddress}
   POST /auth/email-code/verify   {emailAddress, code}
   → 200  identityToken, tenants: []                     ← no session yet

2  GET  /auth/me/invitations      Bearer rig_it_…
   GET  /auth/me/tenants          Bearer rig_it_…

3  POST /auth/me/invitations/accept   Bearer rig_it_…  {invitationId}
   or
   POST /auth/tenants                 Bearer rig_it_…  {name}
   → 200  accessToken, refreshToken, tenants: [{…, current: true}]

4  GET  /api/v1/notes            Bearer rig_at_…
```

Step 1 creates a person and **nothing else** — no tenant, no session. That state
has to exist, because somebody with an invitation waiting has an identity and
belongs nowhere, and accepting an invitation requires being signed in.

An application that wants every newcomer to land somewhere answers step 2
itself: `OnRegistered` (under [What you decide](#what-you-decide)) runs inside
the transaction that creates the person, and its ordinary body is one of two
calls. The transaction is the point: a hook error rolls the whole thing back, so
there is never an identity that half-arrived.

**`Provision` adds a member; `Invite` asks somebody to join.** That is the whole
difference and the two now answer differently, which is what a flag on one
function could never express.

`accounts.Provision` writes the account immediately. The newcomer is in the
tenant's people list from that call whether or not they ever come back, and step
1 then answers the way a sign-in does — the tenant list, the one they landed in
marked `current`, and a session for it:

```
   POST /auth/email-code/verify   {emailAddress, code}
   → 200  accessToken, refreshToken, identityToken,
          tenants: [{…, current: true}]                  ← the hook put them there
```

`accounts.Invite` writes no account at all — an identity if the address is new,
and a row that says which tenant, what role, what to call them and who asked.
**Accepting is what creates the account**, so somebody who was invited and has
not replied is not in the people list, is not counted as a member, and has
nothing scoped to them. Step 1 then answers with an identity token and an empty
list, and the invitation is waiting in step 2.

The trade is that there is one more step between deciding somebody should be
here and their being here, and that the step is theirs rather than yours. What
it buys is that "invited" and "a member" are different states in the database
rather than the same state with a mail attached.

Because the account comes into existence inside rig's own accept handler, there
is no application code in that request — which is what `OnJoined` is for. It runs
in the same transaction, after the account exists and before the session is
issued, and it is where a new member's roles and rows are seeded. Under
`Provision` that work was simply the caller's next line.

Accepting sends the invitation's **identifier**, not the token that was emailed.
Being signed in as the person invited is the *stronger* claim of the two: a token
proves somebody reached the address, a session proves who they are. That is why a
listing can hand out identifiers and never tokens — and why the preview below
hands one out too.

### An invitation arrives

The mail carries a link, and whoever clicks it is usually signed out on a device
this deployment has never seen. Both branches start the same way.

```
GET /auth/invitations/preview?token=…
→ 200 {"tenantName": "Skolan i Solna", "emailAddress": "b***@school.example",
       "invitedBy": "Anna Svensson", "role": "Admin", "expiresAt": "…", "id": "…"}
```

**Signed out**, the page can now say who invited them and where, offer the ways
in this deployment has — a provider, or a code — and accept afterwards. Without
it the page has a token it cannot interpret and can only show a bare sign-in box,
which throws the mail away and is the page most likely to be mistaken for
phishing.

**Signed in**, the `id` in that answer goes straight to
`POST /auth/me/invitations/accept`. That is the answer to the fourth case —
somebody holding the link *and* a session: prefer the stronger of the two claims.
`AcceptAsMe` refuses an invitation that is not the caller's own, which the token
door cannot, because there the token is the whole of the claim.

What it reveals is chosen rather than convenient. The tenant name is the point.
The address is **masked**, because mail gets forwarded and holding a link does
not prove you are its addressee — the unmasked form would turn a leaked link into
a confirmed address. `invitedBy` is a display name, and it is what makes the page
trustworthy rather than phishing-shaped: it is a name the recipient was about to
read in the mail anyway.

One consequence to know rather than to fix: Anna, signed in as herself, opening
Bo's forwarded link and taking the *token* door gets a session for Bo's account.
The token is the credential, and holding it means having reached Bo's mailbox.
The masked address is what lets a page notice the mismatch and offer to sign out
first.

### Somebody who already has an account

```
POST /auth/email-code/verify   {emailAddress, code}      no X-Tenant-Id
→ 200 {
    accessToken, refreshToken, sessionId, expiresAt,
    identityToken,                    ← always, even alongside a session
    tenants: [ … ]                    ← so a picker needs no second call
  }
```

Belonging to one or more tenants lands you in **the one you were last in**, and
the rest are one `POST /auth/tenants/{id}/switch` away. Belonging to **none** is
a 200 with no `accessToken` and an empty `tenants`, not a 403: that used to be a
refusal and it made the flow above impossible.

Naming a tenant you are not in *is* still a 403. You asked for somewhere specific.

The identity token comes back even when a session does, because switching tenant
later is the same flow and the picker's endpoints are what answer "where else could
I go".

**"Where you were last" is the most recent session for any of your accounts**,
read from `rig_account_token`'s family roots — one row per sign-in, indexed by
auth's `last_tenant` migration. A project upgrading from an older rig picks that
migration up the way it picks up any of them: `rig setup-project` writes the ones
you do not have at the next free number, or nothing at all if you keep the
foundation [`embedded`](rig-yaml.md#who-keeps-rigs-migrations). Without it the
answer is still right and the query sorts every token an account has ever held.
It changes
only because you changed it, which is the property that makes it predictable
rather than clever: switching tenant is a decision, and the next sign-in
honouring it is the decision still holding. Somebody signing in for the first
time has no last, and falls back to the tenant they joined first.

Which one you landed in is **marked** rather than sorted first: `tenants` is
ordered by name, because that is how somebody scans a list, and the one the
session is for carries `current: true`. So the ordering and the landing have
nothing to disagree about.

**Every one of these answers is the same for a provider sign-in.** Not by
coincidence — the code flow and an OAuth callback go through one method to answer
them, which is the only way two paths stay agreed about where somebody goes. See
[Signing in with a provider](#signing-in-with-a-provider).

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
GET /auth/oauth/google/start?returnTo=/dashboard&remember=1
→ 302 to Google

GET /auth/oauth/google/callback?code=…&state=…
→ whatever your OnSignIn writes
```

They sit **under the auth base**, so a custom `BasePath` moves them with everything
else: `/api/auth` puts them at `/api/auth/oauth/{provider}/start`.

`remember=1` is the "stay signed in" box a provider sign-in has nowhere to draw:
`/start` is a link, the callback is a redirect, and there is no form in between.
It buys what it buys a code sign-in — `session.remember_ttl` instead of
`session.refresh_ttl` — and needs no allow-list, because both of those are
lengths you configured. Absent, empty, or unreadable all mean no; unlike
`returnTo`, a value that cannot be read is not refused, because a `text/plain`
dead end in front of somebody who has just clicked a button is a bad trade for a
checkbox.

A TypeScript front end gets the provider list from
`client.auth.profile.oauthProviders` and the URL from
`client.auth.oauthStartUrl("google", {returnTo: "/dashboard", remember: true})`,
rather than writing either down: which providers exist is configuration, and a
page that hardcodes one is a page that has to be edited to add a second.

Built in: `oauth.Google(id, secret)`, `oauth.Microsoft(id, secret, tenant)`,
`oauth.GitHub(id, secret)`. The redirect URI is **built, not configured** — derived
from `BaseURL` and the route that was mounted, because two places to write one URL
is one place to get it wrong. It has to match what the provider has registered,
exactly.

#### The round trip carries no server state

`start` mints a random `state` and a PKCE verifier and seals both into one
HMAC-signed cookie:

```
__Host-rig_oauth   {state, verifier, provider, tenant, returnTo, remember, expires}
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

#### Then two questions, and only one of them is always asked

**Who is this?** Answered globally, with no tenant involved, and always answered
here:

1. `FindLink(provider, subject)` — the **subject**, always.
2. Failing that, `FindIdentityByEmail` — so "sign in with Google" reaches the person
   who already exists here, rather than making a second one beside
   them.
3. Failing that, `ProvisionIdentity` — but only under `AllowProvisioning`.

**Do they belong here?** Asked only when something named a tenant. Then it is
answered per tenant from `FindAccount`, and answered **no** unless
`AllowJoining` is on and `JoinTenant` accepts them — which must honour the
tenant's allowed email domains.

The two questions refuse differently, and the difference matters to whoever
reads it: `no_account` is "we have never heard of you", `no_tenant_access` is
"we know you, you are not in this one" — which is the same sentence
`POST /auth/email-code/verify` answers with, and the only one of the two a person can act
on.

When nothing named one, the callback has nothing to answer it with and does not
try: it hands on a sign-in with the first question answered and `tenantID` nil,
and where that person goes is settled the way a code sign-in settles it —
from their own memberships. See [When nobody knows the tenant
yet](#when-nobody-knows-the-tenant-yet).

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

Because it is evidence, it is **recorded**: linking a verified provider address
marks the identity's address verified, the same way provisioning through a
provider does. So somebody who signed in with a code once, never opened the
confirmation mail, and later signed in with Google is verified from then on —
and `require_verified_email` applies to a provider sign-in exactly as it applies
to a login. An address the provider has *not* verified is not recorded as
anything, which is the same rule from the other side.

It is recorded on **every** sign-in that carries a verified address, not only
the one that made the link. That is what a link made before rig recorded any of
this depends on: a repeat sign-in matches on the subject and never takes the
linking branch, so a first-link-only stamp would leave everybody already using a
provider unverified, and `require_verified_email` would start refusing them the
day it was turned on.

**Both doors are shut by default.** A provider will authenticate anybody with a
Google account. An open sign-in endpoint on a business application is a way for
a stranger to appear inside a customer's tenant — rarely what anyone wants and
never what they expect. There are two doors and a key for each:
`allow_provisioning` creates the identity, `allow_joining` puts them in the
tenant a sign-in named.

`allow_joining` is unset by default and then follows `allow_provisioning`, which
is what one key meant when it gated both. Set them apart when the answers
differ, and the ordinary case is `allow_provisioning: true` with
`allow_joining: false` — "a provider may create a person, but only an invitation
may put them in a tenant". That is worth reaching for wherever the tenant comes
from a **request** rather than from a host: `/start` is an anonymous browser
GET, so a crafted link can name any tenant, and the join is the half of the
sign-in that would act on it. An identity on its own reaches nothing.

The reverse is ordinary too — `allow_joining: true` with provisioning off — for
a deployment whose people come from a directory elsewhere: admit them to the
tenant the host named, and never invent one.

The second door only exists when a tenant was named. So for a deployment that
settles the tenant after the callback, `allow_provisioning` is the whole switch:
"may a stranger become somebody here at all". With it off, a provider sign-in
works only for an address that already has an identity, and joining a tenant is
the picker's job rather than the callback's — `allow_joining` is not consulted
at all.

#### The tenant is decided before the redirect, when it can be

`start` resolves it with `Config.Tenant` and seals it into the state cookie;
`callback` reads it from there rather than asking again.

That is not an optimisation. The callback URL is registered with the provider and
fixed, so it carries nothing an application's resolver could read — a header or a
query parameter that was there on the way out is gone on the way back. Only a
**host** survives, which is why a subdomain deployment
(`acme.example.com/auth/oauth/google/start`) never has to think about this and
anything else does.

Carrying it also means a callback cannot be replayed against a different tenant
than the one it started for.

#### When nobody knows the tenant yet

The paragraph above is also the reason a deployment with no host to read has no
answer to give at `/start`. It is an anonymous browser GET: no session, no token,
no profile, and nobody has proved they own an address yet — so the visitor cannot
be asked which of their tenants they meant, because until the callback there is
nobody to have tenants.

`Config.Tenant` may therefore be nil, or answer `uuid.Nil`, and that means
**settle it at the callback**. `auth.TenantFromHeader` — the default — already
answers `uuid.Nil` for a request with no header, so a header-based deployment
gets this without configuring anything.

What the callback then hands `OnSignIn` is a sign-in with the identity resolved
and `TenantID` nil. The default answers it from the person's own memberships,
which is [the same three answers a code sign-in
gives](#somebody-who-already-has-an-account): the tenant they were last in, or
their oldest, or a 200 with an identity token and an empty `tenants` and the
picker taking over.

`examples/auth` is this, in a browser: a provider button that names nothing, and
the page it comes back to drawing the picker.

The three answers a deployment used to have to pick between, all of them wrong,
are worth naming because a project pinned to an older release is probably using
one:

| It answered | What happened |
|---|---|
| a fixed tenant | everybody signed into one tenant regardless of where they belonged — and with `allow_provisioning` on, an account created there for them |
| a request-derived tenant (a query parameter) | the tenant became caller-supplied, so a crafted `/start` link joined a victim to a tenant of the attacker's choosing |
| `uuid.Nil` | the round trip completed, an identity and a provider link were written, and then it died in `JoinTenant` with `no such tenant` |

Note the last one used to *write* before it failed. If you are moving off it,
that is what the rows are.

**A host per tenant needs `OAuth.Origin`.** The callback URL is built from
`BaseURL`, which is one origin — but the state cookie is host-only, so a sign-in
started at `beta.example.com` and sent back to `acme.example.com` arrives without
the cookie and is refused. `Origin` answers per request instead:

```go
Origin: func(r *http.Request) string { return "https://" + r.Host },
```

`Origin` and `BaseURL` on those hooks are two different questions. `Origin`
answers per request; `BaseURL` is the one origin rig/auth builds its routes from
and requires to exist, so setting `Origin` does not remove the need for it.

The constraint it lives inside is the provider's: a redirect URI is registered
exactly, and few providers accept a wildcard — so every origin it can return has to
be registered. A deployment with more subdomains than a console can hold keeps the
callback on one canonical host and has `OnSignIn` hand the finished session on to
the tenant's own host.

Google and Microsoft also refuse plain `http` for anything but `localhost` and
`127.0.0.1`, so a subdomain over plain http cannot be registered with either at all
— which makes https the only shape a tenant-per-host deployment can use with them.
`examples/auth_oauth` documents both ways round it for local work.

#### How it ends

There are two built-in endings and an escape hatch. `oauth.New` still requires
an `OnSignIn` — it decides nothing — but through `auth.New` you get one of these
unless you write your own.

**Without a `web:` block** it is `authhttp.Handler.SignIn`, which answers with
the same **body** a code sign-in does: the token pair, the identity token and
the tenant list. Right for curl, for a native client, and for a front end served
from this same origin.

**With one** it is `authhttp.Handler.SignInToBrowser`, which leaves the tokens in
a short-lived cookie and redirects to the front end. That is the [next
section](#a-front-end-on-another-origin).

**Your own `OnSignIn`** replaces whichever of those applied.

```go
OnSignIn: func(w http.ResponseWriter, r *http.Request, in oauth.SignIn) error {
    // in.Link, in.Profile, in.Provider
    // in.TenantID, in.AccountID  — the session to issue, or BOTH NIL
    // in.New          — this sign-in created the account in that tenant
    // in.NewIdentity  — this sign-in created the person
    // in.ReturnTo     — already checked against the allow-list
}
```

It writes the response: set a cookie, redirect with a token, render a page.

**Whatever it does, it has to handle `TenantID` being nil.** That is a sign-in
whose tenant was not knowable before the redirect, and calling `Sessions.Issue`
straight from those two fields would issue a session into a tenant that does not
exist. The work of handling it is
`account.Service.SignInIdentity` — pass it `in.Link.IdentityID` and `in.TenantID`
and it answers all three cases, which is what the default does.

`New` is true for somebody joining their **second** tenant as well as their first:
the account is new either way, which is what onboarding is about. It is therefore
always false when no tenant was named, because there was no tenant to make an
account in — `NewIdentity` is the half of the question that still has an answer
there, and it is the half onboarding usually means.

`returnTo` is bounded. A relative path always passes; anything else has to be an
origin in `allowed_return_to`. An unchecked `returnTo` is an open redirect, and
an open redirect on a sign-in endpoint is how a phishing link gets to wear your
domain.

`allowed_return_to` holds **origins** — `https://app.example.com`, scheme and
host and nothing after. Not paths: a relative path is never matched against the
list, so an entry that is one does nothing at all, and neither does one with a
trailing slash. Both are refused when the file loads rather than accepted and
quietly ignored.

Which origin a *relative* path resolves against is the ending's decision. The
JSON ending hands it back untouched and the client decides. The browser ending
resolves it against `web.origin`, because that is the origin the person is
looking at.

#### A front end on another origin

The ordinary deployment: the API on `api.example.com`, a single-page application
on `app.example.com`. `SignIn`'s JSON body is useless there — the browser has
just followed a redirect from the provider, so whatever comes back is rendered as
a document, and a page of JSON is a dead end.

Name the front end and you get the other ending:

```yaml
web:
  origin: https://app.example.com   # or origin_env: APP_ORIGIN
  callback_path: /auth/callback     # the default
```

`web:` is top-level rather than under `auth:` because it is a deployment fact,
like `servers:`, and because the cross-origin half of it is wanted by projects
with no provider sign-in at all.

A finished sign-in then answers **303** to
`https://app.example.com/auth/callback` with one cookie:

```
Set-Cookie: rig_handoff=<base64url JSON>; Path=/auth/callback; Domain=example.com;
            Max-Age=60; Secure; SameSite=Lax
```

The value decodes to `authwire.Handoff` — a `SignInResponse` **without** the
tenant list, because a cookie holds about four kilobytes and a tenant list has no
bound. `identityToken` fetches the list from `GET /auth/me/tenants`, which is the
call the picker makes anyway. The token pair is absent entirely for somebody who
belongs to no tenant yet, exactly as it is in the JSON body.

It is deliberately **not** `HttpOnly`: a script on the landing page is the only
thing that can act on these tokens. What stands in for `HttpOnly` is the minute
it lives and the single path it is sent to.

`Domain` comes from the public suffix list — the registrable domain the two
hosts share, so `api.example.com` and `app.example.com` give `example.com`, and
`api.example.co.uk` and `app.example.co.uk` give `example.co.uk` rather than the
`co.uk` a label-counting guess would produce. Two hosts sharing no registrable
domain is refused: **RIG3013** when both are written in `rig.yaml`, and at
startup otherwise. That refusal is the point — a browser drops a cookie whose
`Domain` it does not accept without telling anybody, so the alternative is a
deployment that boots and loses every sign-in.

One host serving both — `localhost:8080` and `localhost:3000` in development —
gets a host-only cookie, which is correct: cookies ignore the port.

A **failure** goes to the same destination with no cookie and a reason on the
query string:

```
303 https://app.example.com/auth/callback?error=cancelled
```

So the landing page has one job and two branches: a cookie to take, or an
`error` to show. The values are the `Reason` table [below](#how-it-fails-is-yours-too),
and they cover the refusals raised *before* any ending runs as well — a cancelled
consent screen, an address no account has. Your own `OnError` still wins.

What the page does is one call each way:

```ts
import { handoffError, takeHandoff } from "@rig-ts/client";

const handoff = takeHandoff();
if (handoff) session.reset(handoff);
else showError(handoffError());
```

`takeHandoff` reads the cookie, deletes it, and gives back a
[`Handoff`](clients.md#finishing-a-provider-sign-in-in-the-browser). It is one
shot — the cookie goes whether or not it decoded, because a value nothing could
read is still a credential sitting in the browser for the rest of its minute.

`session.reset`, or `new Session(handoff)` for a session object you do not
already have. Not `session.replace`: a handoff is a **new** person's tokens, and
`replace` keeps a refresh token the answer did not carry — right for a refresh,
and here it would leave the client able to refresh back into whoever was signed
in before.

The reason that is in the SDK rather than in your callback page is the deletion.
A cookie set with a `Domain` is only removed by a `Set-Cookie` carrying the
**same** `Domain`: a bare `Max-Age=0` creates and expires a *different*,
host-only cookie and leaves the real one alive and readable. That mistake passes
every test on `localhost`, where the server sets no domain at all, and fails
only where there are two subdomains — which is only ever a deployment. Only the
server knows which `Domain` it chose, so `takeHandoff` names the host-only form
and every suffix down to two labels.

Two things stay yours. The origin can come from Go instead of the file, with
`Hooks.OAuth.WebOrigin` — and a deployment that does that answers the rest of its
cross-origin configuration in Go too, since `WebOrigin()` reads the environment
and knows nothing about what a caller passed. And setting your own `OnSignIn`
beside `web:` replaces the ending while keeping the failure redirect, because a
consent screen somebody cancelled never reaches an ending at all.

#### How it fails is yours too

Cancelling at the provider comes back as `?error=…` and is answered 400 — not a
500, because nobody's server failed. Every refusal writes an `OAuthSignIn` /
`Failed` entry, whichever of them it is.

But a 400 is a **document** here, not a body. These two routes are the only ones
rig serves that somebody reaches with their address bar, and the default answer
is a bare `text/plain` page on the API's own origin:

```
Google did not complete the sign-in: access_denied
```

That is a dead end. `OnError` replaces it:

```go
OnError: func(w http.ResponseWriter, r *http.Request, f *oauth.Failure) {
    http.Redirect(w, r, "/login?error="+string(f.Reason), http.StatusSeeOther)
},
```

`f.Reason` is the point. Nine of these failures are a 400, so a status cannot
tell them apart and neither can `rigerr.CodeOf` — and deciding between them by
matching the prose of rig's messages is a test that passes until somebody
rewords a sentence.

| `Reason` | what happened | worth suggesting a retry? |
|---|---|---|
| `cancelled` | pressed cancel at the consent screen | it is not a failure; say so |
| `provider_refused` | the provider reported anything else | yes |
| `unknown_provider` | no such provider in this deployment | no |
| `tenant` | your `Tenant` resolver refused | no |
| `return_to` | a `returnTo` that is not allowed | no — a caller's mistake |
| `state` | the state cookie was missing, expired, or another sign-in's | yes, and it works |
| `no_code` | a callback with no authorization code | yes |
| `exchange` | the provider refused the code — often a wrong secret | yes |
| `profile` | no profile came back, or one rig could not read | yes |
| `no_address` | the provider shared no email address | no |
| `unverified_address` | the provider has not verified the address | no |
| `no_account` | nobody here has this address, and provisioning is off | no |
| `no_tenant_access` | this application knows them; they are not in the tenant this sign-in named, and joining is off | no, but they can ask for an invitation |
| `ending` | `OnSignIn` refused — a tenant they do not belong to | no |
| `internal` | a failure on this side | yes |

Three rules:

**Never render `f.ProviderError`.** It is the provider's raw error value off a
query string — text anybody can write — and putting it in a redirect to your own
origin reflects a stranger's input into your application. It is on the `Failure`
for a log line. `Reason` is already the answer, from a set rig chose.

**`f.Error()` is not safe to show on `internal`.** Every other reason carries a
message written for the person who tried to sign in, but an `internal` one
carries a seal or a store failure — which is why rig's own default answers
`something went wrong` there instead of showing it. A hook that renders the
message has to make the same exception, or `Reason` is the only thing it renders.

**`f.ReturnTo` is where they were going**, so a failure can send somebody back to
the page that started the sign-in rather than to a sign-in page's default. It
lives in the sealed state cookie, so it is empty for `unknown_provider`, for
`state`, and for every failure at the start.

A `Failure` is an error, and it wraps the one rig would have answered with — so
`rigerr.CodeOf(f)`, an `errors.As` for a `*rigerr.Error` and
`httpx.WriteError(w, "", f)` all behave exactly as they would without the hook.
An API that wants these routes answering in its own envelope passes its error
writer and gets it.

The hook does not cost the audit trail anything: the entry is written before it
is called, so a redirect cannot lose the record that a sign-in was attempted and
refused.

`auth.Config.OnError` is deliberately **not** used for this. That one is the shape
your API answers failures in, and it answers them in JSON — which is no more
readable in an address bar than the plain page it would replace. Two questions,
two fields.

**A refused sign-in is in your log as well as your audit table.** It used not to
be: these two routes were the only ones rig serves that wrote no line at any
level, so a failed provider sign-in existed in `authlog` and nowhere else. A
generated server hands them the same error writer every other route reports
through, so the line reads like every other failure and carries the same request
id.

That writer is `auth.OAuth.Fail`, and it is behind both of the fields above:
`OnError` wins, and the front-end redirect a `web:` block installs wins with it.
It answers when neither is there, which is a headless deployment — where the
`text/plain` page becomes your API's own envelope, since nobody is reading it
with their eyes.

`auth/oauth` used on its own, without a generated server, still answers
`text/plain` and still writes nothing. Set `oauth.Config.Fail` to your own error
writer to change that.

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
| **401** | Identity could not be established — missing, malformed, expired, revoked or replayed token; unknown key; wrong sign-in code. *Never* a permission failure. |
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
| Failed sign-in | `login_by_email` | `lower(email_address)` | 5 / 15 min → lockout | yes |
| Failed sign-in | `login_by_ip` | IP address | 50 / 15 min | **no** |
| Code requested | `email_code_request` | `lower(email_address)` | 5 / hour | no |
| Code requested | `email_code_ip` | IP address | 100 / hour | no |
| Verification resend | `verification_resend` | account | 5 / hour | no |
| Refresh | `refresh` | session root | 60 / min | no |
| API key auth failures | `api_key_failures` | `key_id` | 20 / min | yes |
| Invitation previewed | `invitation_preview` | IP address | 60 / hour | no |

**`auth.email_code.max_attempts` is not in this table, and that is the point.**
It is a ceiling on one code rather than a limit on an address: a limit counts
failures over a rolling window, so a fresh code would arrive with the old code's
failures still against it, and five mistypes would lock the address rather than
killing one code. The two are not substitutes and neither replaces the other.

With the standard numbers the *request* limit is what somebody actually hits
first: reaching five failed sign-ins takes five codes, because a code dies after
three guesses, and the sixth request is refused before the sixth sign-in can be
tried. So `login_by_email` is defence in depth on this door rather than the thing
doing the work — worth knowing before tuning either.

The configuration sets `max` and `window`. Which event a limit counts, and what
clears it, stays rig's: a limit counting something else under the same name would
not be the same limit.

Two keys for a sign-in, deliberately: an email-only limit lets one attacker lock
a victim out, and an IP-only limit lets a botnet spray. The IP limit is *not*
cleared by a success — one valid sign-in from a shared address would otherwise
wipe the record of a thousand failures from the same place, which is the thing it
exists to notice. `email_code_ip` is loose for the same reason `login_by_ip` is:
a shared office is one address, and it is a ceiling on how fast one source can
make identities rather than a per-person limit.

The lockout check runs **before** the code is compared, so a locked request
neither does the work nor extends its own window. A sign-in is padded to a
configurable floor (750ms) so response time does not reveal whether an account
exists — and with no password to verify, that floor is the whole of what keeps
the two paths indistinguishable rather than a belt over braces.

**A provider sign-in counts as a success for `login_by_email`**, so signing in
with Google lifts a lockout somebody earned mistyping their code. That is
safe rather than a hole: clearing it takes control of the provider account, which
is not something somebody guessing a code has. It has no lockout of its own —
there is no credential being guessed, and the round trip to the provider is the
bound.

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
an address being confirmed, reuse detection killing a family, an API
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
written by the foundation as they happen. **Every** way a provider sign-in can be
refused is one of them, including the ones nobody is watching for: an expired
state cookie, a code exchange the provider would not honour, a callback naming a
provider this deployment does not have. `detail.reason` is the
[`oauth.Failure` reason](#how-it-fails-is-yours-too), and `detail.error` is the
message rig answered with. Two endpoints read them, and which one
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
walks it with `Auth.AuditLogAll` — or `client.auth.auditLogAll(…)` from a
browser, which is the same walk.

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

  # The way in for anybody with no account at a configured provider. Off by
  # default; on, it mounts two routes and off means they do not exist.
  email_code:
    enabled: true
    length: 6            # 6 to 10. Fewer is guessable whatever the ceiling
    ttl: 10m
    max_attempts: 3      # a ceiling on one code, not a limit on the address
    # Whether a code may go to an address rig has never seen, creating the
    # person. Off is invite-only; on is self-registration, and OnRegistered is
    # what decides where a newcomer lands.
    allow_provisioning: false

  # Whether the route exists at all. Off means no route, rather than a 403 to
  # something probeable.
  allow_tenant_creation: false
  # Refuses a provider sign-in until the address has been confirmed. The code
  # flow is never refused by it, because typing the code back *is* confirming
  # the address. See Signing in with a provider for what else counts.
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
    base_url_env: BASE_URL             # where a deployment says which one
    origin_from_host: false            # or derive it per request, see below
    signing_key_env: OAUTH_SIGNING_KEY # >= 32 bytes, the same in every replica
    state_ttl: 10m
    allow_provisioning: false          # may a provider create a person
    # allow_joining: false             # may it put one in the tenant a sign-in
                                       # named. Unset follows allow_provisioning
    allowed_return_to: [https://app.example.com]  # origins, never paths
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

Beside `auth:` rather than inside it, when the front end is served somewhere
other than this API:

```yaml
web:
  origin: https://app.example.com   # or origin_env: APP_ORIGIN
  callback_path: /auth/callback
```

That is what selects the browser ending — see [A front end on another
origin](#a-front-end-on-another-origin) — and it is top-level because a project
with a separate front end and no provider sign-in still has the fact.

**And the environment is a fallback, not the only way in.** Three of these
values are things a deployment supplies rather than states — the origin, the
signing key, and each provider's pair — so the generated `OAuthHooks` carries a
field for each, and the field wins:

| Field | Falls back to |
|---|---|
| `BaseURL` | `$BASE_URL`, then `base_url` |
| `SigningKey` | `$OAUTH_SIGNING_KEY` |
| `Credentials.Google` | `$GOOGLE_CLIENT_ID` / `$GOOGLE_CLIENT_SECRET` |

Set none of them and nothing changes: the reads are the same reads. Set one and
your `main.go` decides where that value came from — a secret manager, a mounted
file, a test on an ephemeral port that would rather not write to the process it
is running in. It is the same answer `serve.Config` gives for `DatabaseURL` and
`Addr`, and it is what lets an application's own configuration struct name the
values it needs rather than leaving them to a README.

`Credentials` has one field per provider you configured and no others, so a
provider you do not offer is one there is nowhere to name, and a misspelling is
a compile error rather than a value nothing reads. `BaseURL()` beside it answers
what `base_url` and `$BASE_URL` resolved to — and refuses, naming the variable,
where they resolve to nothing.

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

    // What happens to somebody rig has never seen — asking for a code with a
    // new address, where email_code.allow_provisioning allows it — inside the
    // transaction that created them. An error rolls the whole thing back. Nil
    // creates the person and nothing else, which leaves them in the picker
    // with nothing in it.
    //
    // The two bodies answer differently, which is the whole reason there are
    // two verbs. Invite leaves them outside with a door to knock on; Provision
    // puts them in the tenant and the sign-in answers with a session for it.
    OnRegistered: func(ctx context.Context, accounts *account.Service, in account.Registered) error {
        _, err := accounts.Invite(ctx, account.InviteInput{
            TenantID:     starterTenant,
            EmailAddress: in.EmailAddress,
            DisplayName:  in.DisplayName,
        })
        return err
    },

    // What a new member needs beyond their account row, in the transaction
    // that created it. Accepting an invitation is called by rig's own handler,
    // so this is the only application code in that request — under Provision
    // the same work was simply your next line.
    OnJoined: func(ctx context.Context, in account.Joined) error {
        return grantRolesFor(ctx, in.TenantID, in.AccountID, in.Role)
    },

    // How a provider sign-in ends and how it fails, plus the values rig.yaml
    // can only name a variable for. Written out like this they are read where
    // every other read in a main function already is; left out entirely, the
    // generated code reads the same variables itself, which is the ordinary
    // deployment.
    OAuth: api.OAuthHooks{
        // Nil takes one of the two built-in endings: the JSON body a login
        // answers with, or — with a `web:` block — a cookie and a redirect to
        // the front end. Setting one replaces the ending and keeps the failure
        // redirect below.
        OnSignIn: nil,

        // And how one fails, for the same browser. Nil answers a bare
        // text/plain page on this API's origin, which is a dead end for
        // somebody mid-navigation. f.Reason says which of the ways it was.
        OnError: func(w http.ResponseWriter, r *http.Request, f *oauth.Failure) {
            http.Redirect(w, r, "/login?error="+string(f.Reason), http.StatusSeeOther)
        },

        // The values a file cannot hold. Each prefers what you set and reads
        // the variable rig.yaml names when you set nothing, so a project that
        // writes none of them behaves exactly as it did before they existed.
        // Fill one in when the value lives where the environment cannot reach
        // it — a secret manager, a mounted file, or a test on an ephemeral port
        // whose address does not exist until it is listening.
        BaseURL:    cfg.OAuthBaseURL,
        SigningKey: cfg.OAuthSigningKey,
        // Present only for a project with a `web:` block, and answering this
        // here means answering the rest of the cross-origin configuration here
        // too: WebOrigin() reads the environment and cannot see this field.
        WebOrigin: cfg.AppOrigin,
        Credentials: api.OAuthCredentials{
            Google: api.OAuthClient{ID: cfg.GoogleID, Secret: cfg.GoogleSecret},
        },
    },

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

Every secret rig mints — a sign-in code, an address confirmation, an invitation —
goes out through your `Notifier`. By default that call happens **inside the
request that asked for it**, which is the simplest thing and has one bad
afternoon in it: when your provider is down, the request fails, the caller's
rate-limit budget is already spent, and the secret that was just minted is one
nobody ever received. The person asks again and it costs them another attempt
against the limiter.

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
| `email_code.length` | 6 | More digits buy more attempts; fewer than six is guessable whatever the ceiling |
| `email_code.ttl` | 10m | Longer is a live credential sitting in a mailbox; shorter is somebody who went to make tea |
| `email_code.max_attempts` | 3 | Higher is friendlier and closer to the address lockout beside it, which is the one that costs fifteen minutes |
| `email_code.allow_provisioning` | off | On, a stranger typing an address creates a person and runs `OnRegistered` before proving anything |
| `limits.invitation_preview` | 60 / hour | Lower and a shared office stops being able to read invitation links |

Durations are Go's syntax with `d` for days: `250ms`, `45s`, `15m`, `12h`, `30d`.

rig refuses a combination that would behave as nobody intended — an access token
that outlives its session, a "remember me" shorter than an ordinary session, a
rotation leeway a consumed token never leaves — rather than adjusting it quietly.

---

## See also

- `examples/auth` — every flow above, driven from a browser, with a transcript
  panel showing the actual requests. Including the provider sign-in **with no
  tenant named anywhere**: one button on one page, and the picker taking over
  afterwards, which is the state described under [When nobody knows the tenant
  yet](#when-nobody-knows-the-tenant-yet).
- `examples/auth_oauth` — the same sign-in with the other answer: a tenant per
  subdomain, so the host names the tenant before the redirect. Between them the
  two cover both, which is the only choice a deployment actually has.
- `rig/auth/oauthtest` — the stand-in provider both of them serve, which is why
  either works with no credentials at all, and the one to reach for in your own
  tests. Not a mock: single-use authorization codes, PKCE verified at the token
  endpoint, and a consent screen that lets you choose whether it says the address
  is verified, so both branches of that check are reachable from a browser.
  Setting `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET` replaces it with nothing
  else changing.
- `examples/auth/services/authz` — a worked authorization model, if a starting
  point beats a blank page.

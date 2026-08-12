# examples/oauth

Signing in with a provider, at a tenant the **host** decided.

```bash
rig db up
go run .
open http://acme.localhost:8083
```

Two tenants are seeded, and each answers at its own address:

| | |
|---|---|
| `http://acme.localhost:8083` | Acme, where `ada@acme.test` already has a password |
| `http://beta.localhost:8083` | Beta, where nobody is yet |

`*.localhost` resolves to `127.0.0.1` without touching `/etc/hosts`, which is the
whole reason this runs on a laptop.

Out of the box the provider is a stand-in this example serves itself, so there is
nothing to register. To use your own Google or Microsoft credentials, see
[below](#using-your-own-google-or-microsoft-credentials) — including why a
subdomain of `localhost` is not something either of them will accept.

## Why this is its own example

`examples/auth` covers the rest of authentication: sessions, refresh rotation,
invitations, API keys, permissions, the log. It does not cover this one, because a
provider sign-in is as much about deployment shape as about authentication.

A sign-in has to know **which tenant** it is for before the redirect, because that
is what the callback joins somebody to. And the callback URL is registered with the
provider and fixed, so it carries nothing an application could read on the way
back:

- a header does not survive the round trip,
- a query parameter does not survive it,
- a **host** does.

So rig resolves the tenant once, at the start, and seals it into the signed state
cookie. A subdomain per tenant is what makes that natural — and it is the only
shape where "sign in with Google" needs no list of tenants on the page. Which is
the other half: `examples/auth` could only offer a provider button per tenant by
enumerating every tenant in the database to a stranger, which no real deployment
would do.

## What to try

Three sign-ins reach three different branches. The stand-in provider's consent
screen lets you say what it will claim about you, which is how.

1. **A stranger joins.** At `beta.localhost`, sign in as any new address. An
   identity, an account in Beta, and a session — `AllowProvisioning` is on here.
   A business application leaves it off, and a provider sign-in then only works
   for somebody already invited.
2. **An existing account is linked.** At `acme.localhost`, sign in as
   `ada@acme.test` with **verified** on. Same identity, same account, second way
   in. The header will say *Owner of Acme*: the account they already had.
3. **The same address, unverified.** Refused. Without that check, whoever
   registers your address at any provider owns your account here.

Then save a bookmark at one host and look for it at the other. It is not there:
every generated query ANDs in the tenant, and the tenant came from the host.

## The part worth copying

Three lines of resolver, and one field:

```go
// Which tenant this request is for. The leftmost label of the host.
Tenant: tenantFromHost(pool),

OAuth: auth.OAuth{
	// And which origin a callback comes back to. The state cookie is host-only,
	// so a sign-in that started at beta.localhost has to finish there.
	Origin: func(r *http.Request) string { return "http://" + r.Host },
	…
}
```

Everything else — the state cookie, PKCE, matching on the provider's subject
rather than the address, refusing to link an unverified one — is `auth/oauth`, and
none of it is in this directory.

The one thing the provider's side constrains: a redirect URI is registered
exactly, and few providers accept a wildcard. Every origin `Origin` can return has
to be registered — which `services/idp` enforces too, because a stand-in that
skipped it would be teaching the wrong lesson. A deployment with more subdomains
than a console can hold keeps the callback on one canonical host instead, and has
`OnSignIn` hand the finished session on to the tenant's own host.

## Using your own Google or Microsoft credentials

Set a pair and that provider appears; set none and the stand-in appears instead.
Nothing else changes — same routes, same state cookie, same subject matching.

```bash
export GOOGLE_CLIENT_ID=…            # Google Cloud console → Credentials
export GOOGLE_CLIENT_SECRET=…

export MICROSOFT_CLIENT_ID=…         # Entra ID → App registrations
export MICROSOFT_CLIENT_SECRET=…
export MICROSOFT_TENANT=common       # or organizations, or a directory id
```

`MICROSOFT_TENANT` is Microsoft's idea of a tenant and has nothing to do with
rig's: `common` accepts any account, `organizations` excludes personal ones, and a
directory id restricts sign-in to one organization.

Startup prints the redirect URIs to register, one per provider per origin, derived
from the routes the auth package actually mounts:

```
sign-in providers: Google, Microsoft
  register these redirect URIs, exactly:
    http://acme.localhost:8083/auth/oauth/google/callback
    http://beta.localhost:8083/auth/oauth/google/callback
    …
```

### The catch, and it is theirs not rig's

**Google and Microsoft accept plain `http` only for `localhost` and `127.0.0.1`.**
`http://acme.localhost:8083` is neither, so neither console will take it — which is
the same wildcard problem a real deployment has, arriving early. Two ways through:

**One host, tenant from the environment.** Register `http://localhost:8083/…`,
which they do accept, and name the tenant it serves:

```bash
BASE_URL=http://localhost:8083 DEFAULT_TENANT=acme go run .
open http://localhost:8083
```

A real sign-in with your own credentials, landing in Acme. What it cannot show is
the host deciding anything — for that, sign in at the two subdomains with the
stand-in. `DEFAULT_TENANT` exists for this and for nothing else; a deployment on
https never sets it.

**Or a domain you own, over https.** A wildcard record and a tunnel, then
`BASE_URL=https://acme.yourdomain.dev`, and register both origins. That is the
real thing, subdomains and all — and Microsoft will not take `http` here even for
a domain you control, so it has to be https either way.

Google's consent screen needs the `openid`, `email` and `profile` scopes, which is
what `oauth.Google` asks for. Nothing needs configuring beyond that: the provider
is three URLs and a way to read a profile, and the ones for Google, Microsoft and
GitHub ship in `auth/oauth`.

### What linking means with a real provider

`ada@acme.test` has a password here, and a provider sign-in as an address that
already has an account links the two — but only when the provider says the address
is **verified**. Google reports that per address. Microsoft's userinfo does not
report it at all, and `auth/oauth` treats an address in a directory Microsoft
controls as established, because treating it as unverified would make linking
impossible for every Entra customer.

Neither will let you *choose*, which is exactly why the stand-in exists: it is the
only way to see the refusal.

## What is a prop

`services/idp` is a provider this example serves itself, so the flow works without
registering an application with anybody. It is not a mock: it hands back a
single-use authorization code and verifies the PKCE challenge before exchanging
it, so the path exercised is rig's real one. Nothing in `auth/oauth` knows it is
not Google.

Set `GOOGLE_CLIENT_ID` and `GOOGLE_CLIENT_SECRET` and the real one replaces it
with no other change. Delete the package in a project of your own and pass
`oauth.Google(id, secret)`.

The interface is two pages and no build step. `examples/auth` has the dashboard;
building a second one here would bury the one thing this example is about.

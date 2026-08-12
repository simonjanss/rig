package main

// pageHTML is the whole interface. No build step, no framework, no CDN.
const pageHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{if .Tenant}}{{.Tenant}} — {{end}}oauth — rig</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #fbfbfd; --panel: #fff; --ink: #14161a; --dim: #6b7280;
    --line: #e5e7eb; --accent: #3b5bdb; --ok: #0f7b3f; --bad: #b42318;
  }
  @media (prefers-color-scheme: dark) {
    :root { --bg: #0d0f13; --panel: #14171d; --ink: #e8eaed; --dim: #9aa3af;
            --line: #262b34; --accent: #8ba2ff; --ok: #4ade80; --bad: #f87171; }
  }
  * { box-sizing: border-box; }
  body { margin: 0; background: var(--bg); color: var(--ink);
         font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, sans-serif; }
  code { font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
         background: color-mix(in srgb, var(--ink) 7%, transparent);
         padding: .05rem .3rem; border-radius: 4px; font-size: .92em; }
  .wrap { max-width: 44rem; margin: 0 auto; padding: 2.5rem 1.25rem 4rem; }

  header { display: flex; align-items: baseline; gap: .75rem; flex-wrap: wrap;
           padding-bottom: 1rem; border-bottom: 1px solid var(--line); }
  header h1 { font-size: 1.15rem; margin: 0; letter-spacing: -.01em; }
  header .host { color: var(--dim); font-size: .85rem; }
  header .spacer { flex: 1; }

  .panel { background: var(--panel); border: 1px solid var(--line);
           border-radius: 12px; padding: 1.1rem 1.25rem; margin-top: 1.25rem; }
  .panel h2 { font-size: .72rem; text-transform: uppercase; letter-spacing: .07em;
              color: var(--dim); margin: 0 0 .8rem; }
  .note { color: var(--dim); font-size: .88rem; }
  .flash { margin-top: 1.25rem; padding: .6rem .8rem; border-radius: 8px;
           border: 1px solid color-mix(in srgb, var(--accent) 40%, var(--line));
           background: color-mix(in srgb, var(--accent) 10%, transparent); font-size: .9rem; }
  .bad { border-color: color-mix(in srgb, var(--bad) 40%, var(--line));
         background: color-mix(in srgb, var(--bad) 10%, transparent); }

  a.button, button { font: inherit; cursor: pointer; text-decoration: none;
    display: inline-block; padding: .4rem .85rem; border-radius: 8px;
    border: 1px solid var(--ink); background: var(--ink); color: var(--bg); }
  a.button.ghost, button.ghost { background: transparent; color: var(--ink);
    border-color: var(--line); }
  .row { display: flex; gap: .5rem; flex-wrap: wrap; align-items: flex-end; }
  label { display: block; font-size: .8rem; color: var(--dim); }
  input { font: inherit; color: inherit; padding: .4rem .55rem; width: 100%;
          border: 1px solid var(--line); border-radius: 7px; background: var(--panel); }
  ul { list-style: none; margin: 0; padding: 0; }
  li { padding: .5rem 0; border-bottom: 1px solid var(--line); }
  li:last-child { border-bottom: 0; }
  .empty { color: var(--dim); font-size: .9rem; }
</style>
</head>
<body>
<div class="wrap">

<header>
  <h1>{{if .Tenant}}{{.Tenant}}{{else}}No tenant{{end}}</h1>
  <span class="host">{{.Host}}</span>
  <span class="spacer"></span>
  {{if .SignedIn}}
    <form method="post" action="/sign-out"><button class="ghost">Sign out</button></form>
  {{end}}
</header>

{{with .Flash}}<div class="flash">{{.}}</div>{{end}}
{{with .Refused}}<div class="flash bad">{{.}}</div>{{end}}

<div class="panel">
  <h2>The tenant comes from the host</h2>
  <p class="note">
    You are at <code>{{.Host}}</code>, which is
    {{if .Tenant}}the tenant <b>{{.Tenant}}</b>{{else}}<b>no tenant at all</b>{{end}}.
    The resolver is three lines and reads the leftmost label of the host — no
    header, no query parameter, nothing a client chooses.
  </p>
  <p class="note">
    That is not a style preference. A provider sign-in has to know the tenant
    <em>before</em> the redirect, because it is what the callback joins you to — and
    the callback URL is registered with the provider and fixed, so it carries
    nothing on the way back. A header does not survive it. A host does. It is why
    rig seals the tenant into the state cookie when a sign-in starts.
  </p>
  {{with .OtherHost}}
    <p><a class="button ghost" href="//{{.}}/">Go to {{.}}</a></p>
  {{end}}
</div>

{{if not .SignedIn}}
  <div class="panel">
    <h2>Sign in with a provider</h2>
    {{if .Tenant}}
      <p class="note">
        No tenant to pick, because the host already said. This is the same link a
        real deployment renders.
      </p>
      <p>
        {{range .Providers}}
          <a class="button" href="/auth/oauth/{{lower .}}/start?returnTo=/">
            Continue with {{.}}</a>
        {{end}}
      </p>
      {{if .Demo}}
        <p class="note">
          <b>Demo</b> is a stand-in this example serves itself, so the flow works with
          no credentials — but it is the real flow: a single-use authorization code,
          and PKCE verified at the token endpoint. Its consent screen lets you choose
          the address and whether it is <b>verified</b>, which is how you reach both
          branches of the check that matters.
        </p>
        <p class="note">
          Try it three ways. Sign in as a new address and you join this tenant. Sign
          in as <code>ada@acme.test</code> — who already has a password here — with
          <b>verified</b> on, and the provider is linked to that account. Turn
          verified off and it is refused, because otherwise whoever registers your
          address anywhere owns your account here.
        </p>
        <p class="note">
          Your own credentials in <code>GOOGLE_CLIENT_ID</code> and
          <code>GOOGLE_CLIENT_SECRET</code>, or the <code>MICROSOFT_</code> pair,
          replace the stand-in with nothing else changing — see the README for which
          redirect URIs to register, and why plain <code>http</code> on a subdomain
          is not one of them.
        </p>
      {{else}}
        <p class="note">
          Real credentials, and a real provider. Everything above still holds: you
          are matched on the provider's <b>subject</b> rather than the address, and
          an existing account is only linked when the provider says the address is
          <b>verified</b> — Google reports that per address, and Microsoft vouches
          for every address in a directory it controls.
        </p>
        <p class="note">
          The addresses this run expects a provider to send somebody back to were
          printed at startup. They are registered exactly, one per origin, because
          the callback returns to the host the sign-in began at.
        </p>
      {{end}}
    {{else}}
      <p class="note">
        There is nothing to sign in to at this host. A sign-in belongs to a tenant,
        and the host is what names one — or, for a host with no tenant label in it,
        <code>DEFAULT_TENANT</code>.
      </p>
    {{end}}
  </div>
{{else}}
  <div class="panel">
    <h2>Signed in{{with .Who}} — {{.}}{{end}}</h2>
    <p class="note">
      An ordinary session: the same pair a password login issues, so everything
      downstream is identical. The bookmarks below come from this application's own
      API, called with a real <code>Authorization</code> header.
    </p>

    <form method="post" action="/bookmarks">
      <div class="row">
        <div style="flex:1 1 10rem"><label>Title
          <input name="title" required placeholder="The Go blog"></label></div>
        <div style="flex:1 1 12rem"><label>URL
          <input name="url" required placeholder="https://go.dev/blog"></label></div>
        <button>Save</button>
      </div>
    </form>
  </div>

  <div class="panel">
    <h2>Bookmarks in {{.Tenant}}</h2>
    <ul>
      {{range .Bookmarks}}
        <li><b>{{.Title}}</b><br><span class="note">{{.URL}}</span></li>
      {{else}}
        <li class="empty">None yet.</li>
      {{end}}
    </ul>
    {{with .OtherHost}}
      <p class="note">
        Save one, then go to <code>{{.}}</code> and sign in there: the list is
        empty, because every generated query ANDs in the tenant and the tenant came
        from the host.
      </p>
    {{end}}
  </div>
{{end}}

</div>
</body>
</html>
`

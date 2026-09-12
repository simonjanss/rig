package oauthtest

import "html/template"

// consent is the page a browser lands on, and the form is the demonstration.
//
// It says on itself that it is a prop. A page that looked like a real provider's
// would be teaching somebody to type an address into whatever looks like one,
// which is the habit the whole of this is meant not to build.
var consent = template.Must(template.New("consent").Parse(`
<!doctype html>
<title>Stand-in provider</title>
<style>
  body { font: 15px/1.6 system-ui, sans-serif; max-width: 34rem; margin: 4rem auto;
         padding: 0 1.25rem; color: #14161a; }
  @media (prefers-color-scheme: dark) { body { background: #0d0f13; color: #e8eaed; } }
  .prop { border: 1px solid currentColor; border-radius: 8px; padding: .6rem .8rem;
          font-size: .85rem; opacity: .75; }
  label { display: block; margin: .8rem 0; }
  label span { display: block; font-size: .8rem; opacity: .7; }
  input[type=text], input[type=email] { width: 100%; padding: .4rem .55rem; font: inherit; }
  button { font: inherit; padding: .45rem 1rem; margin-top: .5rem; cursor: pointer; }
  code { font-family: ui-monospace, Menlo, monospace; }
</style>

<h1>Stand-in provider</h1>
<p class="prop">
  This is not a real identity provider. The application you came from serves it
  itself, so the OAuth sign-in works without registering an application with
  anybody — but the flow is the real one: this page hands back a single-use
  authorization code, and the token endpoint verifies the PKCE challenge before
  exchanging it.
</p>

<p>
  Choose what this provider will say about you. Signing in as an address that
  already has an identity links the two — <em>if</em> the provider says the address
  is verified. Turn that off to see the refusal, which is the check the whole OAuth
  package turns on.
</p>

<form method="post" action="` + BasePath + `/approve">
  <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
  <input type="hidden" name="state" value="{{.State}}">
  <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">

  <label><span>Subject — the stable identifier rig matches on</span>
    <input type="text" name="subject" value="{{.Subject}}" required></label>
  <label><span>Email address</span>
    <input type="email" name="email" value="ada@example.com" required></label>
  <label><span>Display name</span>
    <input type="text" name="name" value="Ada"></label>
  <label><input type="checkbox" name="verified" checked>
    This provider has verified the address</label>

  <button>Approve</button>
</form>
`))

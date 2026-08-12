// Package idp is a stand-in identity provider, so this example's OAuth sign-in
// works without registering an application with anybody.
//
// It is a prop, and it says so on the page it serves. What it is not is a mock:
// it implements the authorization-code flow properly — an authorization endpoint
// that hands back a single-use code, a token endpoint that verifies the PKCE
// challenge before exchanging it, and a userinfo endpoint behind the access token.
// So the code path exercised is the real one. Nothing in rig/auth knows this
// provider is not Google.
//
// The one thing it does that a real provider cannot is let you choose what it says
// about you. That is the interesting part of a demonstration: sign in as an address
// that already has a password and watch it link; turn "verified" off and watch it
// refuse to, which is the check the whole OAuth package turns on.
//
// Delete this package in a real project and pass oauth.Google(id, secret).
package idp

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/simonjanss/rig/auth/oauth"
)

// Name is the provider label. It has to be a value of the oauth_provider enum,
// which migration 00007 adds.
const Name = "Demo"

// Server is the stand-in.
type Server struct {
	base string
	// allowed are the redirect URIs registered with this provider.
	allowed []string

	mu    sync.Mutex
	codes map[string]grant
	// tokens are the access tokens userinfo will answer for.
	tokens map[string]oauth.Profile
}

// grant is an authorization waiting to be exchanged.
type grant struct {
	profile   oauth.Profile
	challenge string
	expires   time.Time
}

// New builds a server. origins are the redirect URIs it will accept, and the
// first is also where this provider serves itself.
//
// A real provider has a list too, typed into a console one line at a time, and it
// is the constraint a tenant-per-subdomain deployment runs into: wildcards are
// rarely allowed, so every host that can start a sign-in has to be registered.
// This stand-in is honest about that — approve refuses an origin it was not given.
func New(origins ...string) *Server {
	allowed := make([]string, 0, len(origins))
	for _, o := range origins {
		allowed = append(allowed, strings.TrimRight(o, "/"))
	}
	return &Server{
		base:    allowed[0],
		allowed: allowed,
		codes:   map[string]grant{},
		tokens:  map[string]oauth.Profile{},
	}
}

// Mount registers the three endpoints a provider has.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /idp/authorize", s.authorize)
	mux.HandleFunc("POST /idp/approve", s.approve)
	mux.HandleFunc("POST /idp/token", s.token)
	mux.HandleFunc("GET /idp/userinfo", s.userinfo)
}

// Provider is what to hand rig.
//
// A real provider is three URLs and a way to read a profile, which is exactly what
// oauth.Provider holds — so a stand-in is a literal rather than a subclass of
// anything. The Parse function reads the same shape Google's userinfo returns,
// because that is what this server answers with.
func (s *Server) Provider() oauth.Provider {
	return oauth.Provider{
		Name:         Name,
		ClientID:     "demo-client",
		ClientSecret: "demo-secret",
		Scopes:       []string{"openid", "email", "profile"},
		Endpoint: oauth2.Endpoint{
			AuthURL:   s.base + "/idp/authorize",
			TokenURL:  s.base + "/idp/token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
		UserInfoURL: s.base + "/idp/userinfo",
		Parse: func(body []byte) (oauth.Profile, error) {
			var v struct {
				Sub           string `json:"sub"`
				Email         string `json:"email"`
				EmailVerified bool   `json:"email_verified"`
				Name          string `json:"name"`
			}
			if err := json.Unmarshal(body, &v); err != nil {
				return oauth.Profile{}, err
			}
			return oauth.Profile{
				Subject:       v.Sub,
				EmailAddress:  v.Email,
				EmailVerified: v.EmailVerified,
				DisplayName:   v.Name,
			}, nil
		},
	}
}

// authorize is the consent screen, and the form is the demonstration.
//
// A real provider knows who you are and what it will say about you. This one asks,
// so both branches of the interesting check are reachable from a browser: an
// address that already has a password links when the provider vouches for it, and
// is refused when it does not.
func (s *Server) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("code_challenge") == "" {
		// rig always sends one. A provider that accepted a request without it
		// would be letting a stolen code be exchanged by whoever stole it.
		http.Error(w, "this provider requires PKCE", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = consent.Execute(w, map[string]any{
		"RedirectURI":   q.Get("redirect_uri"),
		"State":         q.Get("state"),
		"CodeChallenge": q.Get("code_challenge"),
		"Subject":       "demo-subject-1",
	})
}

// approve mints the authorization code and sends the browser back.
func (s *Server) approve(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}

	redirectURI := r.FormValue("redirect_uri")
	if !s.registered(redirectURI) {
		// A real provider checks the redirect URI against what was registered.
		// Skipping it is how an authorization code ends up at somebody else's
		// server, so the stand-in checks too.
		http.Error(w, "unregistered redirect_uri", http.StatusBadRequest)
		return
	}

	code := random()
	s.mu.Lock()
	s.codes[code] = grant{
		profile: oauth.Profile{
			Subject:       strings.TrimSpace(r.FormValue("subject")),
			EmailAddress:  strings.TrimSpace(r.FormValue("email")),
			EmailVerified: r.FormValue("verified") == "on",
			DisplayName:   strings.TrimSpace(r.FormValue("name")),
		},
		challenge: r.FormValue("code_challenge"),
		expires:   time.Now().Add(2 * time.Minute),
	}
	s.mu.Unlock()

	back, err := url.Parse(redirectURI)
	if err != nil {
		http.Error(w, "bad redirect_uri", http.StatusBadRequest)
		return
	}
	q := back.Query()
	q.Set("code", code)
	q.Set("state", r.FormValue("state"))
	back.RawQuery = q.Encode()

	http.Redirect(w, r, back.String(), http.StatusFound)
}

// registered reports whether a redirect URI is one of ours.
func (s *Server) registered(uri string) bool {
	return slices.ContainsFunc(s.allowed, func(origin string) bool {
		return strings.HasPrefix(uri, origin+"/")
	})
}

// token exchanges a code, and verifies the PKCE challenge doing it.
func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}

	code := r.FormValue("code")

	s.mu.Lock()
	g, ok := s.codes[code]
	// Single use, whatever happens next. A code that survived its exchange is a
	// code somebody can replay.
	delete(s.codes, code)
	s.mu.Unlock()

	switch {
	case !ok:
		s.oauthError(w, "invalid_grant", "no such authorization code")
		return
	case time.Now().After(g.expires):
		s.oauthError(w, "invalid_grant", "that authorization code has expired")
		return
	}

	// The verifier proves the exchange is being made by whoever started the
	// sign-in. rig keeps it in a signed cookie and never sends it to the browser,
	// so a stolen code is useless without it.
	sum := sha256.Sum256([]byte(r.FormValue("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		s.oauthError(w, "invalid_grant", "the code_verifier does not match the challenge")
		return
	}

	access := random()
	s.mu.Lock()
	s.tokens[access] = g.profile
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": access,
		"token_type":   "Bearer",
		"expires_in":   300,
	})
}

// userinfo answers for an access token, in the shape Google's does.
func (s *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	access := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	s.mu.Lock()
	p, ok := s.tokens[strings.TrimSpace(access)]
	s.mu.Unlock()

	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"sub":            p.Subject,
		"email":          p.EmailAddress,
		"email_verified": p.EmailVerified,
		"name":           p.DisplayName,
	})
}

// oauthError answers the way a provider does, so rig's handling of a refusal is
// exercised rather than a plain 500.
func (s *Server) oauthError(w http.ResponseWriter, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": code, "error_description": description,
	})
}

func random() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), time.Now().Unix())
}

var consent = template.Must(template.New("consent").Parse(`
<!doctype html>
<title>Demo provider</title>
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

<h1>Demo provider</h1>
<p class="prop">
  This is not a real identity provider. It is served by this example so the OAuth
  sign-in works without registering an application with Google — but the flow is
  the real one: this page hands back a single-use authorization code, and the token
  endpoint verifies the PKCE challenge before exchanging it.
</p>

<p>
  Choose what this provider will say about you. Signing in as an address that
  already has a password links the two accounts — <em>if</em> the provider says the
  address is verified. Turn that off to see the refusal, which is the check the
  whole OAuth package turns on.
</p>

<form method="post" action="/idp/approve">
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

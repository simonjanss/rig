// Package oauthtest is a stand-in identity provider, so a provider sign-in can be
// driven in a test or a demonstration without registering an application with
// anybody.
//
// It is not a mock. It implements the authorization-code flow properly — an
// authorization endpoint that hands back a single-use code, a token endpoint that
// verifies the PKCE challenge before exchanging it, and a userinfo endpoint behind
// the access token — so the code path exercised is the real one.
//
// What makes it worth rig owning rather than every project writing is
// [Server.Wear]. It takes rig's own [oauth.Google], [oauth.Microsoft] or
// [oauth.GitHub] and points the URLs here, leaving Name, Scopes, Parse and Extra
// exactly as production has them. So the double cannot drift from the thing it
// stands for, and a project never has to know how a given provider spells a
// profile — which is the mistake that does not fail loudly: a camelCase key parses
// to an empty profile and arrives as [oauth.ReasonInternal] two layers away.
//
// The one thing it does that a real provider cannot is let you choose what it says
// about you. In a test that is [Server.Expect]; in a browser it is the consent
// page, which is where the interesting check is reachable by hand — sign in as an
// address that already has an identity and watch it link, turn "verified" off and
// watch it refuse to.
//
// A consumer takes on one obligation. [oauth.Provider.Name] is written into
// rig_identity_oauth.provider, which is a Postgres enum in every project's own
// migrations — so wearing Google needs nothing, and [Server.Custom] needs a
// migration adding that label.
//
// This package imports testing, the way net/http/httptest's callers do. Import it
// from a test, a seed or a development-only branch, and never from a path a
// deployment takes.
package oauthtest

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/simonjanss/rig/auth/oauth"
)

// BasePath is where this provider's own endpoints sit. It is under a path of its
// own so the stand-in can be mounted on an application's mux beside the routes it
// is standing in for, which is what the examples do: one origin, one listener, and
// a browser that never leaves the host it started on.
const BasePath = "/idp"

// CodeTTL is how long an authorization code lasts. Generous for a redirect, short
// enough that a code left in a log is useless.
const CodeTTL = 2 * time.Minute

// Server is the stand-in.
type Server struct {
	base    string
	allowed []string
	own     *httptest.Server

	mu     sync.Mutex
	codes  map[string]grant
	tokens map[string]oauth.Profile
}

// grant is an authorization waiting to be exchanged.
type grant struct {
	profile oauth.Profile
	// refuse makes the token endpoint answer the way it does when the client
	// secret is wrong — see [Server.ExpectRefusal].
	refuse bool
	// challenge is the PKCE challenge the authorization request carried. Empty
	// means the code was staked by [Server.Expect] rather than issued to a
	// browser, so there was no authorization request to carry one — see there.
	challenge string
	expires   time.Time
}

// New builds a server that the caller will serve. origins are the redirect URIs
// it will accept, and the first is also where this provider is reachable.
//
// A real provider has that list too, typed into a console one line at a time, and
// it is the constraint a tenant-per-subdomain deployment runs into: wildcards are
// rarely allowed, so every host that can start a sign-in has to be registered.
// This stand-in is honest about it and refuses an origin it was not given.
func New(origins ...string) *Server {
	if len(origins) == 0 {
		panic("oauthtest: New needs at least one origin — the one this provider is reachable at")
	}
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

// Start builds a server with a listener of its own, closed when the test ends.
//
// This is the form for a test that only needs a provider. An application serving
// the stand-in itself — a demonstration, or a suite driving a browser across
// several hosts — wants [New] and [Server.Mount] instead, so that everything stays
// on one origin.
//
// extraOrigins are redirect URIs to accept besides this server's own. The
// application under test is always one of them, so pass its origin.
func Start(tb testing.TB, extraOrigins ...string) *Server {
	tb.Helper()

	// The address has to be known before the provider is built, because a
	// redirect URI is absolute and there is no port until a listener has one.
	own := httptest.NewUnstartedServer(nil)
	s := New(append([]string{"http://" + own.Listener.Addr().String()}, extraOrigins...)...)

	mux := http.NewServeMux()
	s.Mount(mux)
	own.Config.Handler = mux
	own.Start()
	tb.Cleanup(own.Close)

	s.own = own
	return s
}

// URL is where this provider is reachable.
func (s *Server) URL() string { return s.base }

// Mount registers the endpoints a provider has, plus the one GitHub's Extra
// reaches — see [Server.Transport].
func (s *Server) Mount(mux oauth.Router) {
	mux.HandleFunc("GET "+BasePath+"/authorize", s.authorize)
	mux.HandleFunc("POST "+BasePath+"/approve", s.approve)
	mux.HandleFunc("POST "+BasePath+"/{provider}/token", s.token)
	mux.HandleFunc("GET "+BasePath+"/{provider}/userinfo", s.userinfo)
	mux.HandleFunc("GET "+BasePath+"/{provider}/emails", s.emails)
}

// Wear returns one of rig's own providers with its three URLs pointed here.
//
// Name, Scopes, Parse and Extra are untouched, which is the point: the sign-in
// under test runs production's code for reading that provider's answer, and the
// profile is served in that provider's own spelling. Pass credentials or do not —
// the stand-in does not check them, because a wrong secret is the provider's
// refusal to model and [oauth.ReasonExchange] already has a test.
//
// The provider must be one this package knows how to answer as. An unknown name
// is served OpenID Connect's spelling, which is Google's; see [Server.Custom].
func (s *Server) Wear(p oauth.Provider) oauth.Provider {
	p.Endpoint = oauth2.Endpoint{
		AuthURL:  s.base + BasePath + "/authorize",
		TokenURL: s.base + BasePath + "/" + url.PathEscape(p.Name) + "/token",
		// In the parameters rather than the header, because that is what every
		// provider rig ships uses and a stand-in that accepted both would hide a
		// mismatch rather than surface it.
		AuthStyle: oauth2.AuthStyleInParams,
	}
	p.UserInfoURL = s.base + BasePath + "/" + url.PathEscape(p.Name) + "/userinfo"
	return p
}

// Custom is a provider of this deployment's own, named whatever you like.
//
// It is what an in-house identity server looks like to rig, and what an example
// mounts so that the enum obligation is visible: name is written into
// rig_identity_oauth.provider, so this needs a migration adding that label and
// [Server.Wear] over a built-in one does not. That is the point worth taking from
// it — adding Okta is a migration, not a line of configuration.
//
// The profile is served in OpenID Connect's spelling, which is what an in-house
// server would most likely answer with and what [oauth.Google] reads.
func (s *Server) Custom(name string) oauth.Provider {
	return s.Wear(oauth.Provider{
		Name:         name,
		ClientID:     "stand-in-client",
		ClientSecret: "stand-in-secret",
		Scopes:       []string{"openid", "email", "profile"},
		Parse:        oauth.Google("", "").Parse,
	})
}

// Expect stakes a profile and returns the authorization code that will produce it.
//
// This is the form for a test that drives the callback directly: start a sign-in
// to collect the state, then deliver this code to it. Every call returns a code of
// its own, so a suite can run in parallel without holding a lock of its own —
// which is the reason it returns the code rather than setting a field.
//
// A code from here carries no PKCE challenge, because no authorization request was
// made to carry one, so the token endpoint accepts whatever verifier rig sends.
// The browser path through the consent page does check it, and
// TestThePKCEVerifierIsChecked covers that; a test that wants to prove PKCE wants
// that path rather than this one.
func (s *Server) Expect(p oauth.Profile) string {
	code := random()
	s.mu.Lock()
	s.codes[code] = grant{profile: p, expires: time.Now().Add(CodeTTL)}
	s.mu.Unlock()
	return code
}

// ExpectRefusal returns an authorization code the token endpoint will refuse.
//
// This is the branch that only appears in failure and that no project can reach
// on its own: a provider that authenticated somebody and then would not trade the
// code — a wrong client secret, most often, which refuses every sign-in
// identically and says nothing anywhere about why. rig answers it with
// [oauth.ReasonExchange].
//
// Like [Server.Expect], every call returns a code of its own.
func (s *Server) ExpectRefusal() string {
	code := random()
	s.mu.Lock()
	s.codes[code] = grant{refuse: true, expires: time.Now().Add(CodeTTL)}
	s.mu.Unlock()
	return code
}

// authorize is the consent screen, and the form is the demonstration.
//
// A real provider knows who you are and what it will say about you. This one asks,
// so both branches of the interesting check are reachable from a browser.
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
		"Subject":       "stand-in-subject-1",
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
		expires:   time.Now().Add(CodeTTL),
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

	s.mu.Lock()
	g, ok := s.codes[r.FormValue("code")]
	// Single use, whatever happens next. A code that survived its exchange is a
	// code somebody can replay.
	delete(s.codes, r.FormValue("code"))
	s.mu.Unlock()

	switch {
	case !ok:
		oauthError(w, "invalid_grant", "no such authorization code")
		return
	case time.Now().After(g.expires):
		oauthError(w, "invalid_grant", "that authorization code has expired")
		return
	case g.refuse:
		// The shape a provider refusing the exchange actually has, rather than a
		// 500: rig reads the error document, and a stand-in that answered
		// anything else would exercise the wrong branch.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "invalid_client", "error_description": "the client secret is wrong",
		})
		return
	}

	verifier := r.FormValue("code_verifier")
	if verifier == "" {
		oauthError(w, "invalid_grant", "this provider requires a code_verifier")
		return
	}
	// The verifier proves the exchange is being made by whoever started the
	// sign-in. rig keeps it in a signed cookie and never sends it to the browser,
	// so a stolen code is useless without it. A staked code has no challenge to
	// check against — see [Server.Expect].
	if g.challenge != "" {
		sum := sha256.Sum256([]byte(verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
			oauthError(w, "invalid_grant", "the code_verifier does not match the challenge")
			return
		}
	}

	access := random()
	s.mu.Lock()
	s.tokens[access] = g.profile
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": access, "token_type": "Bearer", "expires_in": 300,
	})
}

// userinfo answers for an access token, in the spelling of the provider it is
// being asked as.
func (s *Server) userinfo(w http.ResponseWriter, r *http.Request) {
	p, ok := s.bearer(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	body, err := spelling(r.PathValue("provider"), p)
	if err != nil {
		// 500 and the sentence, rather than an empty profile: this is a mistake
		// in the test, and the failure it would otherwise produce names the
		// provider rather than the line that made it.
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

// emails is the second call GitHub's Extra makes.
//
// It sits under [BasePath] like everything else here, even though the URL it
// stands in for does not: that one is the single thing about a provider which is
// not data — [oauth.GitHub] hardcodes api.github.com — so reaching it at all
// means pointing the HTTP client somewhere else, and something that is already
// rewriting the host may as well rewrite the path. [Server.Transport] is what
// does both, and it is why this server needs no route outside its own prefix.
func (s *Server) emails(w http.ResponseWriter, r *http.Request) {
	p, ok := s.bearer(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(emails(p))
}

// bearer resolves the access token a request carries.
func (s *Server) bearer(r *http.Request) (oauth.Profile, bool) {
	access := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.tokens[access]
	return p, ok
}

// oauthError answers the way a provider does, so rig's handling of a refusal is
// exercised rather than a plain 500.
func oauthError(w http.ResponseWriter, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": code, "error_description": description,
	})
}

// random is a secret nobody is meant to guess, which a code and an access token
// both are — a counter would let one test's code be exchanged by another's.
func random() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("oauthtest: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

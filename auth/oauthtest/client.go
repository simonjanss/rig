package oauthtest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"sync"
	"testing"

	"golang.org/x/oauth2"

	"github.com/simonjanss/rig/runtime/authwire"
)

// Transport sends a request meant for somebody else here instead.
//
// Two of the three URLs on an [oauth.Provider] are data and [Server.Wear] repoints
// them. The third is not: [oauth.GitHub]'s Extra hardcodes api.github.com, because
// its user endpoint returns null for anybody who kept their address private and
// only the emails endpoint says whether one is verified. So the only way to reach
// a stand-in for that call is to point the HTTP client somewhere else.
//
// A request already addressed to this server is left alone, so the token exchange
// and the userinfo read still go where [Server.Wear] sent them.
func (s *Server) Transport() http.RoundTripper {
	here, err := url.Parse(s.base)
	if err != nil {
		panic("oauthtest: " + err.Error())
	}
	return &rewrite{to: here}
}

type rewrite struct{ to *url.URL }

func (t *rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host == t.to.Host {
		return http.DefaultTransport.RoundTrip(r)
	}
	clone := r.Clone(r.Context())
	clone.URL.Scheme, clone.URL.Host = t.to.Scheme, t.to.Host
	clone.Host = ""
	return http.DefaultTransport.RoundTrip(clone)
}

// Middleware puts [Server.Transport] where rig's OAuth handler will find it.
//
// This is the non-obvious half, and the one that costs an afternoon. The token
// exchange and the userinfo read are made by the *server*, not by the browser, and
// golang.org/x/oauth2 takes its HTTP client from the context — which for these two
// routes is the request's. So one wrapper around the application's handler covers
// the exchange, the profile read and GitHub's second call, and without it a
// sign-in fails as "GitHub refused the authorization code": the provider blamed
// for what is a name resolution.
//
// Wrap the application under test, not this server.
func (s *Server) Middleware(next http.Handler) http.Handler {
	client := &http.Client{Transport: s.Transport()}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(
			context.WithValue(r.Context(), oauth2.HTTPClient, client)))
	})
}

// Jar is a cookie jar that ignores every rule about hosts and security.
//
// The state cookie rig writes is Secure and, unless [oauth.Config.Insecure] says
// otherwise, __Host- prefixed. net/http/cookiejar is right to drop both over the
// plain HTTP an httptest server speaks — and the result is that every provider
// sign-in in the suite fails for a reason no deployment has. Carrying the cookies
// by hand is what every project ends up doing; this is that, once.
type Jar struct {
	mu      sync.Mutex
	cookies []*http.Cookie
}

// SetCookies records what a response set, and forgets what it cleared.
func (j *Jar) SetCookies(_ *url.URL, cookies []*http.Cookie) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, c := range cookies {
		j.cookies = slices.DeleteFunc(j.cookies, func(had *http.Cookie) bool { return had.Name == c.Name })
		if c.MaxAge >= 0 {
			j.cookies = append(j.cookies, c)
		}
	}
}

// Cookies answers with everything it holds, for any URL.
func (j *Jar) Cookies(*url.URL) []*http.Cookie {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]*http.Cookie(nil), j.cookies...)
}

// Client is a browser that stops at every redirect and keeps every cookie.
//
// Stopping matters more than it sounds: every ending on the browser paths is a
// 303, and a client that followed them would test the page it landed on rather
// than the redirect that is the actual answer. The jar is [Jar], for the reason
// given there.
//
// It does not carry [Server.Transport], deliberately. That one exists to
// redirect calls the *server* makes to a provider; putting it on the browser
// would send the application's own requests to the stand-in as well.
func Client() *http.Client {
	return &http.Client{
		Jar: &Jar{},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Handoff reads the tokens a browser sign-in left in its cookie.
//
// The value is base64url of [authwire.Handoff]'s JSON, which is a contract with
// the front end rather than an implementation detail — and decoding it by hand is
// the last thing between a suite and asserting on a provider sign-in.
func Handoff(tb testing.TB, res *http.Response) authwire.Handoff {
	tb.Helper()

	for _, c := range res.Cookies() {
		if c.Name != authwire.HandoffCookie {
			continue
		}
		body, err := base64.RawURLEncoding.DecodeString(c.Value)
		if err != nil {
			tb.Fatalf("the %s cookie is not base64url: %v", authwire.HandoffCookie, err)
		}
		var out authwire.Handoff
		if err := json.Unmarshal(body, &out); err != nil {
			tb.Fatalf("the %s cookie is not a handoff: %v", authwire.HandoffCookie, err)
		}
		return out
	}
	tb.Fatalf("no %s cookie on a %d; the sign-in did not reach the browser ending",
		authwire.HandoffCookie, res.StatusCode)
	return authwire.Handoff{}
}

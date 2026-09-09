package handoff_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/auth/handoff"
	"github.com/simonjanss/rig/runtime/authwire"
)

// The two hosts of the deployment this package exists for.
const (
	api = "api.example.com"
	web = "https://app.example.com"
)

func must(t *testing.T, cfg handoff.Config) *handoff.Handoff {
	t.Helper()
	h, err := handoff.New(cfg)
	if err != nil {
		t.Fatalf("New(%+v): %v", cfg, err)
	}
	return h
}

// The Domain attribute is the one thing here a browser will not complain
// about, so it is the one thing worth a table.
func TestTheCookieDomainIsTheSharedRegistrableDomain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		apiHost    string
		origin     string
		wantDomain string
		wantErr    bool
	}{
		{
			name:       "two subdomains of one domain",
			apiHost:    api,
			origin:     web,
			wantDomain: "example.com",
		},
		{
			name: "a multi-label public suffix is not a registrable domain",
			// The case a label-counting heuristic gets wrong: co.uk is a public
			// suffix, so Domain=co.uk would be dropped by every browser and
			// example.co.uk is the answer.
			apiHost:    "api.example.co.uk",
			origin:     "https://app.example.co.uk",
			wantDomain: "example.co.uk",
		},
		{
			name:    "two registrable domains under one public suffix",
			apiHost: "api.foo.co.uk",
			origin:  "https://app.bar.co.uk",
			wantErr: true,
		},
		{
			name:    "unrelated domains",
			apiHost: api,
			origin:  "https://app.other.com",
			wantErr: true,
		},
		{
			name: "one host on two ports is host-only",
			// Development, and the reason equal hostnames are checked before
			// the public suffix list: localhost has no registrable domain, and
			// Domain=localhost is dropped.
			apiHost:    "localhost:8080",
			origin:     "http://localhost:3000",
			wantDomain: "",
		},
		{
			name:       "the same host is host-only",
			apiHost:    api,
			origin:     "https://" + api,
			wantDomain: "",
		},
		{
			name:    "localhost against a loopback address shares nothing",
			apiHost: "127.0.0.1:8080",
			origin:  "http://localhost:3000",
			wantErr: true,
		},
		{
			name:       "an API origin rather than a bare host",
			apiHost:    "https://" + api + "/",
			origin:     web,
			wantDomain: "example.com",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h, err := handoff.New(handoff.Config{
				Origin: tc.origin, APIHost: tc.apiHost,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("New accepted %s and %s", tc.apiHost, tc.origin)
				}
				// The refusal has to name both hosts, because the reader is
				// looking at a process that will not start and the two values
				// are in different files.
				for _, want := range []string{"registrable domain"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not mention %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			cookie, err := h.Cookie(authwire.Handoff{IdentityToken: "t"}, h.Destination(""))
			if err != nil {
				t.Fatalf("Cookie: %v", err)
			}
			if cookie.Domain != tc.wantDomain {
				t.Errorf("Domain = %q, want %q", cookie.Domain, tc.wantDomain)
			}
		})
	}
}

func TestNewRefusesAnOriginThatIsNotOne(t *testing.T) {
	t.Parallel()

	for name, cfg := range map[string]handoff.Config{
		"no origin":   {APIHost: api},
		"no api host": {Origin: web},
		"no scheme":   {Origin: "app.example.com", APIHost: api},
		"a scheme rig cannot redirect to": {
			Origin: "ftp://app.example.com", APIHost: api,
		},
		"no host":             {Origin: "https://", APIHost: api},
		"a path":              {Origin: "https://app.example.com/app", APIHost: api},
		"a query":             {Origin: "https://app.example.com?a=b", APIHost: api},
		"a relative callback": {Origin: web, APIHost: api, CallbackPath: "auth/callback"},
		"a protocol-relative callback": {
			Origin: web, APIHost: api, CallbackPath: "//evil.example",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := handoff.New(cfg); err == nil {
				t.Fatalf("New accepted %+v", cfg)
			}
		})
	}
}

// A trailing slash on the origin is the commonest way a deployment writes one,
// and it must not turn into a double slash in every redirect.
func TestNewNormalisesTheOrigin(t *testing.T) {
	t.Parallel()

	h := must(t, handoff.Config{Origin: "https://APP.example.com/", APIHost: api})
	if got, want := h.Origin(), "https://app.example.com"; got != want {
		t.Errorf("Origin() = %q, want %q", got, want)
	}
	if got, want := h.Destination("").String(), web+"/auth/callback"; got != want {
		t.Errorf("Destination(\"\") = %q, want %q", got, want)
	}
}

func TestDestinationResolvesAgainstTheFrontEnd(t *testing.T) {
	t.Parallel()

	h := must(t, handoff.Config{Origin: web, APIHost: api})
	callback := web + "/auth/callback"

	for _, tc := range []struct{ name, returnTo, want string }{
		{"nothing asked for", "", callback},
		{
			// The bug this whole ending exists to fix: oauth's checkReturnTo
			// lets a bare path through before the allow-list, and that path is
			// a route on the front end rather than on this API.
			name: "a relative path", returnTo: "/projects/7",
			want: web + "/projects/7",
		},
		{
			name: "a relative path with a query", returnTo: "/auth/callback?redirect=%2Fx",
			want: web + "/auth/callback?redirect=%2Fx",
		},
		{
			name: "an absolute URL the cookie reaches", returnTo: web + "/welcome",
			want: web + "/welcome",
		},
		{
			// The cookie's Domain is example.com, so it is sent here too.
			name: "a sibling subdomain the cookie reaches", returnTo: "https://admin.example.com/x",
			want: "https://admin.example.com/x",
		},
		{
			// Honouring this would redirect to a page that can never read the
			// cookie just written, which looks to the person like a sign-in
			// that did nothing.
			name: "an origin the cookie does not reach", returnTo: "https://app.other.com/x",
			want: callback,
		},
		{"a protocol-relative URL", "//evil.example/x", callback},
		{"not a path at all", "projects/7", callback},
		{"not a URL", "http://[::1", callback},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := h.Destination(tc.returnTo).String(); got != tc.want {
				t.Errorf("Destination(%q) = %q, want %q", tc.returnTo, got, tc.want)
			}
		})
	}
}

// Reachability is the cookie's own rule rather than the origin's, and the two
// differ in both directions: a cookie ignores the port, so a sibling port on
// the same host is reachable — but a host-only cookie reaches no other host at
// all, whatever it shares with one.
func TestReachabilityIsTheCookieScopeRatherThanTheOrigin(t *testing.T) {
	t.Parallel()

	h := must(t, handoff.Config{Origin: "http://localhost:3000", APIHost: "localhost:8080"})

	// A project that listed this port in allowed_return_to gets it, because a
	// cookie set for localhost is genuinely sent to localhost:4000.
	if got, want := h.Destination("http://localhost:4000/x").String(),
		"http://localhost:4000/x"; got != want {
		t.Errorf("Destination = %q, want %q", got, want)
	}
	// Another host is another matter: this cookie is host-only.
	if got, want := h.Destination("http://app.localtest.me/x").String(),
		"http://localhost:3000/auth/callback"; got != want {
		t.Errorf("Destination = %q, want %q", got, want)
	}
}

func TestTheCookieCarriesTheHandoffAndNothingElse(t *testing.T) {
	t.Parallel()

	h := must(t, handoff.Config{Origin: web, APIHost: api, TTL: 30 * time.Second})
	in := authwire.Handoff{IdentityToken: "identity", IdentityExpiresAt: time.Now().UTC()}
	in.AccessToken = "access"

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/google/callback", nil)
	if err := h.Write(rec, req, in, "/auth/callback?redirect=%2Fx"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got, want := rec.Code, http.StatusSeeOther; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := rec.Header().Get("Location"),
		web+"/auth/callback?redirect=%2Fx"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}

	cookies := (&http.Response{Header: rec.Header()}).Cookies()
	if len(cookies) != 1 {
		t.Fatalf("%d cookies, want 1", len(cookies))
	}
	got := cookies[0]
	if got.Name != authwire.HandoffCookie {
		t.Errorf("name = %q, want %q", got.Name, authwire.HandoffCookie)
	}
	if got.Path != "/auth/callback" {
		t.Errorf("Path = %q, want the destination's path", got.Path)
	}
	if got.Domain != "example.com" {
		t.Errorf("Domain = %q, want example.com", got.Domain)
	}
	if !got.Secure {
		t.Error("not Secure, and the front end is https")
	}
	if got.HttpOnly {
		t.Error("HttpOnly, so the page it is written for cannot read it")
	}
	if got.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", got.SameSite)
	}
	if got.MaxAge != 30 {
		t.Errorf("MaxAge = %d, want 30", got.MaxAge)
	}

	body, err := base64.RawURLEncoding.DecodeString(got.Value)
	if err != nil {
		t.Fatalf("the value is not base64url: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatalf("the value is not JSON: %v", err)
	}
	if back["identityToken"] != "identity" || back["accessToken"] != "access" {
		t.Errorf("decoded to %v", back)
	}
	if _, ok := back["tenants"]; ok {
		t.Error("the handoff carries a tenant list, which is what it leaves out")
	}
}

// http never sends a Secure cookie over http, so a development deployment on
// http would lose the handoff if this were not read off the front end's scheme.
func TestAnHTTPFrontEndGetsANonSecureCookie(t *testing.T) {
	t.Parallel()

	h := must(t, handoff.Config{Origin: "http://localhost:3000", APIHost: "localhost:8080"})
	cookie, err := h.Cookie(authwire.Handoff{IdentityToken: "t"}, h.Destination(""))
	if err != nil {
		t.Fatalf("Cookie: %v", err)
	}
	if cookie.Secure {
		t.Error("Secure, and the front end is http")
	}
}

func TestAHandoffTooBigForACookieIsRefused(t *testing.T) {
	t.Parallel()

	h := must(t, handoff.Config{Origin: web, APIHost: api})
	in := authwire.Handoff{IdentityToken: strings.Repeat("t", 5000)}
	if _, err := h.Cookie(in, h.Destination("")); err == nil {
		t.Fatal("Cookie accepted a value a browser would drop")
	}
}

func TestFailRedirectsWithAReasonAndNoCookie(t *testing.T) {
	t.Parallel()

	h := must(t, handoff.Config{Origin: web, APIHost: api})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/auth/oauth/google/callback", nil)
	h.Fail(rec, req, "/auth/callback?redirect=%2Fx", "cancelled")

	if got, want := rec.Code, http.StatusSeeOther; got != want {
		t.Errorf("status = %d, want %d", got, want)
	}
	if got, want := rec.Header().Get("Location"),
		web+"/auth/callback?error=cancelled&redirect=%2Fx"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
	if set := rec.Header().Values("Set-Cookie"); len(set) != 0 {
		t.Errorf("a failure wrote %v", set)
	}
}

func TestFailWithNoReasonStillSaysSomethingWentWrong(t *testing.T) {
	t.Parallel()

	h := must(t, handoff.Config{Origin: web, APIHost: api})
	rec := httptest.NewRecorder()
	h.Fail(rec, httptest.NewRequest(http.MethodGet, "/x", nil), "", "")

	if got, want := rec.Header().Get("Location"),
		web+"/auth/callback?error=unknown"; got != want {
		t.Errorf("Location = %q, want %q", got, want)
	}
}

package cors_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/cors"
)

const front = "https://app.example.com"

// policy is the shape a generated one has: every list filled, so a test can
// tell "the header was omitted" from "the list was empty".
func policy() cors.Policy {
	return cors.Policy{
		AllowedOrigins: []string{front},
		AllowedMethods: []string{"GET", "POST", "QUERY", "OPTIONS"},
		AllowedHeaders: []string{"Authorization", "Idempotency-Key", "X-Tenant-Id"},
		ExposedHeaders: []string{"Retry-After", "RateLimit-Limit", "electric-handle"},
		MaxAge:         10 * time.Minute,
	}
}

// reached wraps a handler that records whether it ran and answers 200.
func reached() (http.Handler, *bool) {
	var ran bool
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}), &ran
}

func preflight(origin, method string) *http.Request {
	r := httptest.NewRequest(http.MethodOptions, "/api/v1/accounts", nil)
	r.Header.Set("Origin", origin)
	r.Header.Set("Access-Control-Request-Method", method)
	return r
}

func get(origin string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/accounts", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

func do(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// The mux below has no OPTIONS pattern for any of its paths, so a preflight it
// saw would be refused — and a refused preflight is a real request that is
// never made. Nothing below the policy may run.
func TestAPreflightIsAnsweredHereAndNeverReachesTheHandler(t *testing.T) {
	t.Parallel()

	next, ran := reached()
	w := do(policy().Wrap(next), preflight(front, "QUERY"))

	if *ran {
		t.Error("the preflight reached the handler below")
	}
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != front {
		t.Errorf("allow-origin = %q, want %q", got, front)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "600" {
		t.Errorf("max-age = %q, want 600", got)
	}
	for _, want := range []string{"Origin", "Access-Control-Request-Method", "Access-Control-Request-Headers"} {
		if !strings.Contains(strings.Join(w.Header().Values("Vary"), ","), want) {
			t.Errorf("Vary = %v, want it to name %s", w.Header().Values("Vary"), want)
		}
	}
}

// The lists are echoed as given. QUERY is the entry worth checking: the client
// sends a search as QUERY and falls back to POST only on a 405 or 501, and a
// preflight that omits a method fails as a network error — so there is no
// status for the fallback to read, and search fails silently.
func TestAPreflightEchoesTheListsVerbatim(t *testing.T) {
	t.Parallel()

	next, _ := reached()
	w := do(policy().Wrap(next), preflight(front, "QUERY"))

	if got := w.Header().Get("Access-Control-Allow-Methods"); got != "GET, POST, QUERY, OPTIONS" {
		t.Errorf("allow-methods = %q", got)
	}
	if got := w.Header().Get("Access-Control-Allow-Headers"); got != "Authorization, Idempotency-Key, X-Tenant-Id" {
		t.Errorf("allow-headers = %q", got)
	}
}

// None of the exposed headers is CORS-safelisted, so without the list a
// cross-origin caller cannot read a single one of them.
func TestAnAllowedRequestIsMarkedReadableAndPassedOn(t *testing.T) {
	t.Parallel()

	next, ran := reached()
	w := do(policy().Wrap(next), get(front))

	if !*ran {
		t.Fatal("the request never reached the handler")
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != front {
		t.Errorf("allow-origin = %q, want %q", got, front)
	}
	if got := w.Header().Get("Access-Control-Expose-Headers"); got != "Retry-After, RateLimit-Limit, electric-handle" {
		t.Errorf("expose-headers = %q", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}
	// The credential is a bearer token in a header, so no cookie has to cross
	// an origin and an unlisted origin carries no ambient authority to abuse.
	if got := w.Header().Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("allow-credentials = %q, want it absent", got)
	}
}

// Varied anyway: the answer depends on the header whether or not it was
// allowed, and a cache that stored one origin's answer for another is the
// failure nobody reproduces.
func TestAnUnlistedOriginGetsNothingButVary(t *testing.T) {
	t.Parallel()

	next, ran := reached()
	h := policy().Wrap(next)

	w := do(h, get("https://evil.example.com"))
	if !*ran {
		t.Error("a plain request from an unlisted origin should still be served; the browser refuses to read it")
	}
	for _, name := range []string{"Access-Control-Allow-Origin", "Access-Control-Expose-Headers"} {
		if got := w.Header().Get(name); got != "" {
			t.Errorf("%s = %q, want it absent", name, got)
		}
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}

	*ran = false
	w = do(h, preflight("https://evil.example.com", "POST"))
	if *ran {
		t.Error("a preflight from an unlisted origin reached the handler")
	}
	if w.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d: the refusal is the missing headers, not the status", w.Code, http.StatusNoContent)
	}
	for _, name := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Methods", "Access-Control-Allow-Headers", "Access-Control-Max-Age"} {
		if got := w.Header().Get(name); got != "" {
			t.Errorf("%s = %q, want it absent", name, got)
		}
	}
}

// No origin named is no headers at all, rather than headers allowing nothing —
// which would read as a misconfiguration rather than as an answer. The handler
// comes back untouched, the same value in and out.
func TestAnEmptyPolicyIsTheHandlerUntouched(t *testing.T) {
	t.Parallel()

	mux := http.NewServeMux()
	for _, origins := range [][]string{nil, {}, {""}, {"  "}, {"/"}} {
		p := cors.Policy{AllowedOrigins: origins, AllowedMethods: []string{"GET"}}
		if p.Enabled() {
			t.Errorf("origins=%q: Enabled, want not", origins)
		}
		if got := p.Wrap(mux); got != http.Handler(mux) {
			t.Errorf("origins=%q: Wrap returned a different handler, want the same one", origins)
		}
	}
}

// A trailing slash is how an origin arrives by accident, and "https://x/" is not
// a value any browser ever sends as Origin. Case and whitespace likewise.
func TestOriginsAreNormalisedBeforeTheyAreCompared(t *testing.T) {
	t.Parallel()

	p := cors.Policy{AllowedOrigins: []string{" HTTPS://App.Example.com/ ", "http://localhost:3000/"}}
	for _, origin := range []string{front, "http://localhost:3000"} {
		if !p.Allows(origin) {
			t.Errorf("Allows(%q) = false, want true", origin)
		}
	}
	if p.Allows("https://app.example.com:443") {
		t.Error("a port is part of an origin, and one that was not listed is another origin")
	}
}

// One label in place of the star, and only one: app.example.com is the tenant
// per subdomain this exists for, and a.b.example.com is a different trust
// decision that nobody made.
func TestAWildcardMatchesExactlyOneLabel(t *testing.T) {
	t.Parallel()

	p := cors.Policy{AllowedOrigins: []string{"https://*.example.com", "https://*.example.org:8443"}}
	for origin, want := range map[string]bool{
		"https://app.example.com":         true,
		"https://beta.example.com":        true,
		"https://a.b.example.com":         false,
		"https://example.com":             false,
		"http://app.example.com":          false,
		"https://app.example.com:8443":    false,
		"https://app.example.org:8443":    true,
		"https://app.example.org":         false,
		"https://evil.com?x=.example.com": false,
	} {
		if got := p.Allows(origin); got != want {
			t.Errorf("Allows(%q) = %t, want %t", origin, got, want)
		}
	}
}

// An origin is scheme://host, so a bare star — or a hostname with no scheme —
// is not an entry at all. This package would rather refuse everybody than
// allow everybody by accident, and a list made only of such entries is no list.
func TestWhatIsNotAnOriginIsNotAnEntry(t *testing.T) {
	t.Parallel()

	p := cors.Policy{AllowedOrigins: []string{"*", "app.example.com", "https://"}}
	for _, origin := range []string{front, "*", "null", "app.example.com"} {
		if p.Allows(origin) {
			t.Errorf("Allows(%q) = true, want false: nothing in the list is an origin", origin)
		}
	}
	if p.Enabled() {
		t.Error("Enabled with no origin in the list, want not")
	}
}

// The function is for origins that are rows rather than configuration, and it
// is asked only once the list has said no.
func TestAllowOriginIsAskedAfterTheList(t *testing.T) {
	t.Parallel()

	var asked []string
	p := cors.Policy{
		AllowedOrigins: []string{front},
		AllowOrigin: func(origin string) bool {
			asked = append(asked, origin)
			return origin == "https://tenant.custom.example"
		},
	}

	if !p.Allows(front) {
		t.Error("the listed origin should be allowed")
	}
	if len(asked) != 0 {
		t.Errorf("the function was asked %v about an origin the list already allowed", asked)
	}
	if !p.Allows("https://tenant.custom.example") {
		t.Error("the function said yes and was not believed")
	}
	if p.Allows("https://evil.example.com") {
		t.Error("the function said no and was not believed")
	}
	if len(asked) != 2 {
		t.Errorf("asked = %v, want the two the list refused", asked)
	}

	// A function alone is enough to enable the policy: the list may be empty
	// when every origin is a row.
	only := cors.Policy{AllowOrigin: func(string) bool { return true }}
	if !only.Enabled() {
		t.Error("a policy with only an AllowOrigin should be enabled")
	}
}

// OPTIONS without an Access-Control-Request-Method is not a preflight, and a
// request with no Origin was not sent by a page on another origin. Neither is
// this package's business beyond the Vary header.
func TestWhatIsNotACrossOriginRequestPassesThrough(t *testing.T) {
	t.Parallel()

	next, ran := reached()
	h := policy().Wrap(next)

	options := httptest.NewRequest(http.MethodOptions, "/api/v1/accounts", nil)
	options.Header.Set("Origin", front)
	w := do(h, options)
	if !*ran {
		t.Error("OPTIONS without a request method should reach the handler")
	}
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want the handler's own", w.Code)
	}

	*ran = false
	w = do(h, get(""))
	if !*ran {
		t.Error("a request with no Origin should reach the handler")
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("allow-origin = %q for a request that named no origin", got)
	}
}

// Zero is the browser's own default, not zero seconds.
func TestAZeroMaxAgeSendsNoHeader(t *testing.T) {
	t.Parallel()

	p := policy()
	p.MaxAge = 0
	next, _ := reached()
	w := do(p.Wrap(next), preflight(front, "GET"))

	if got := w.Header().Get("Access-Control-Max-Age"); got != "" {
		t.Errorf("max-age = %q, want it absent", got)
	}
}

// strings.Split on an empty string returns one empty string, which downstream
// would be an origin rather than no origins.
func TestSplitReadsAnEnvironmentVariable(t *testing.T) {
	t.Parallel()

	for raw, want := range map[string][]string{
		"":                                     nil,
		"  ":                                   nil,
		"https://a.example":                    {"https://a.example"},
		"https://a.example, https://b.example": {"https://a.example", "https://b.example"},
		",https://a.example,,":                 {"https://a.example"},
	} {
		got := cors.Split(raw)
		if strings.Join(got, "|") != strings.Join(want, "|") || (got == nil) != (want == nil) {
			t.Errorf("Split(%q) = %q, want %q", raw, got, want)
		}
	}
}

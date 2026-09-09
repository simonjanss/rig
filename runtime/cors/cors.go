// Package cors answers a browser's question about another origin: may a page
// served from there call this API, and what may it send and read.
//
// It exists because a front end is not always served from the API's own origin.
// app.example.com and api.example.com are one deployment and, to a browser, two
// origins — and the browser will not let a page on the first read an answer
// from the second until the second has said it may. Every sign-in redirect
// works without this, because a top-level navigation has no origin to check;
// the first token refresh after it is an XHR, and without the headers this
// package writes it fails before it is sent.
//
// The answer is a [Policy] wrapped around the handler, and it wraps the whole
// handler rather than a route: which origins may read this API is a decision
// about the API, not about any one endpoint, and a policy applied to some
// routes and not others is a policy with a hole in it that only a browser
// finds. rig's own probes are answered outside whatever is wrapped, so a
// readiness check every second does not pay for it.
//
// What a policy has to say is mostly not the application's to know. A search
// is sent as QUERY, and a preflight that omits the method fails as a network
// error rather than a status — so the client's fallback to POST never fires,
// and search fails with nothing in any log. Every unsafe request carries an
// Idempotency-Key. The rate-limit headers, the replayed-write marker and the
// sync cursor all travel in response headers a browser hides until they are
// exposed. Those facts belong to the server that reads and writes the headers,
// which is why the lists here are plain slices for a generator to fill rather
// than defaults this package guesses.
//
// One thing it will never write is Access-Control-Allow-Credentials. rig's
// credential is a bearer token in a header, so no cookie and no client
// certificate ever has to cross an origin, and an origin not on the list
// carries no ambient authority to abuse.
package cors

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Policy is which origins may call from a browser, and the methods and headers
// the exchange may use.
//
// The zero Policy allows nothing, and [Policy.Wrap] on one returns the handler
// it was given: no headers at all, rather than headers that allow nothing and
// read as a misconfiguration.
type Policy struct {
	// AllowedOrigins are compared exactly with the Origin a browser sends, after
	// case, surrounding whitespace and a trailing slash have been forgiven — a
	// trailing slash is how an origin arrives by accident, and "https://x/" is
	// not a value any browser ever sends.
	//
	// One wildcard form is understood: "https://*.example.com" matches exactly
	// one label in place of the star, so app.example.com is in and
	// a.b.example.com and example.com are not. That is the deployment with a
	// tenant per subdomain.
	//
	// An entry that is not scheme://host — a bare "*", a hostname with no
	// scheme — is not an origin and is dropped: this package would rather
	// refuse everybody than allow everybody by accident, and a list holding
	// only such entries leaves the handler untouched.
	AllowedOrigins []string

	// AllowOrigin is asked after the list, and only when the list said no. It is
	// for the origins that are rows rather than configuration — a custom domain
	// per tenant, read from a table — which no list in a file can name. Nil asks
	// nobody.
	AllowOrigin func(origin string) bool

	// AllowedMethods is what a preflight is told it may ask for.
	AllowedMethods []string

	// AllowedHeaders is what a caller may send beyond the ones every request may
	// carry anyway.
	AllowedHeaders []string

	// ExposedHeaders is what a caller may read off a response. Without it a
	// cross-origin page sees only the handful of headers the specification
	// safelists, and nothing an API says about limits, revisions or cursors is
	// among them.
	ExposedHeaders []string

	// MaxAge is how long a browser may keep a preflight's answer. Zero sends no
	// Access-Control-Max-Age at all, which leaves the browser's own default.
	MaxAge time.Duration
}

// Enabled reports whether [Policy.Wrap] would add anything: at least one origin
// that could be allowed, by the list or by [Policy.AllowOrigin].
func (p Policy) Enabled() bool {
	return p.matcher().enabled(p.AllowOrigin)
}

// Allows reports whether an Origin header value is one this policy accepts.
func (p Policy) Allows(origin string) bool {
	return p.matcher().allows(origin, p.AllowOrigin)
}

// Wrap answers cross-origin requests for the origins the policy names, and
// leaves every other request exactly as it found it.
//
// A preflight — OPTIONS with an Access-Control-Request-Method — is answered
// here and never reaches next. The generated mux has no OPTIONS pattern for
// any of its paths, so anything below would refuse it, and a refused preflight
// is a real request that is never made. Any other request from an allowed
// origin is marked as readable and passed on.
//
// Every response carries Vary: Origin, allowed or not. The answer depends on the
// header either way, and a cache that stored one origin's answer and served it
// to another is the failure nobody manages to reproduce.
func (p Policy) Wrap(next http.Handler) http.Handler {
	m := p.matcher()
	if !m.enabled(p.AllowOrigin) {
		return next
	}

	var (
		methods = strings.Join(p.AllowedMethods, ", ")
		headers = strings.Join(p.AllowedHeaders, ", ")
		exposed = strings.Join(p.ExposedHeaders, ", ")
		maxAge  string
	)
	if seconds := int64(p.MaxAge / time.Second); seconds > 0 {
		maxAge = strconv.FormatInt(seconds, 10)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		w.Header().Add("Vary", "Origin")

		ok := origin != "" && m.allows(origin, p.AllowOrigin)
		if ok {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}

		if r.Method == http.MethodOptions && origin != "" && r.Header.Get("Access-Control-Request-Method") != "" {
			w.Header().Add("Vary", "Access-Control-Request-Method")
			w.Header().Add("Vary", "Access-Control-Request-Headers")
			if ok {
				if methods != "" {
					w.Header().Set("Access-Control-Allow-Methods", methods)
				}
				if headers != "" {
					w.Header().Set("Access-Control-Allow-Headers", headers)
				}
				if maxAge != "" {
					w.Header().Set("Access-Control-Max-Age", maxAge)
				}
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if ok && exposed != "" {
			w.Header().Set("Access-Control-Expose-Headers", exposed)
		}
		next.ServeHTTP(w, r)
	})
}

// Split reads a comma-separated list of origins the way a deployment writes
// one into an environment variable: each entry trimmed, empty ones dropped, so
// an unset variable is no origins rather than one empty origin.
func Split(list string) []string {
	var out []string
	for part := range strings.SplitSeq(list, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// matcher is the allow-list, normalised once.
type matcher struct {
	exact map[string]struct{}
	// wild holds the suffix after the star, scheme included: "https://" and
	// ".example.com" for "https://*.example.com".
	wild []wildcard
}

type wildcard struct{ scheme, suffix string }

func (p Policy) matcher() matcher {
	m := matcher{exact: make(map[string]struct{}, len(p.AllowedOrigins))}
	for _, raw := range p.AllowedOrigins {
		o := normalize(raw)
		if o == "" {
			continue
		}
		scheme, host, found := strings.Cut(o, "://")
		if !found || scheme == "" || host == "" {
			// Not an origin, so not an entry. A bare "*" lands here.
			continue
		}
		if strings.HasPrefix(host, "*.") && len(host) > 2 {
			m.wild = append(m.wild, wildcard{scheme: scheme + "://", suffix: host[1:]})
			continue
		}
		m.exact[o] = struct{}{}
	}
	return m
}

func (m matcher) enabled(fn func(string) bool) bool {
	return len(m.exact) > 0 || len(m.wild) > 0 || fn != nil
}

func (m matcher) allows(origin string, fn func(string) bool) bool {
	o := normalize(origin)
	if o == "" {
		return false
	}
	if _, ok := m.exact[o]; ok {
		return true
	}
	for _, w := range m.wild {
		host, found := strings.CutPrefix(o, w.scheme)
		if !found || !strings.HasSuffix(host, w.suffix) {
			continue
		}
		// Exactly one label in place of the star: non-empty, and not itself
		// dotted. "*.example.com" is app.example.com and not a.b.example.com,
		// because the second is a different trust decision.
		label := strings.TrimSuffix(host, w.suffix)
		if label != "" && !strings.Contains(label, ".") {
			return true
		}
	}
	return fn != nil && fn(origin)
}

// normalize forgives the three ways one origin gets spelled two ways: case,
// surrounding whitespace, and a trailing slash.
func normalize(origin string) string {
	return strings.ToLower(strings.TrimRight(strings.TrimSpace(origin), "/"))
}

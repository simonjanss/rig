// Package handoff hands a browser its tokens when a provider sign-in ends
// somewhere else.
//
// A sign-in that finishes in a redirect cannot answer with a body. The browser
// followed the provider's callback to this API and has to end up on the front
// end, which in a deployment worth the name is served on another host: the API
// on api.example.com, the application on app.example.com. So the tokens travel
// in a short-lived cookie the landing page reads and deletes, and the redirect
// carries nothing but a destination.
//
// Everything here is arithmetic over URLs and cookie attributes, with no
// provider machinery behind it: this package does not import
// [github.com/simonjanss/rig/auth/oauth], so the rules can be read and tested
// on their own. What uses it is
// [github.com/simonjanss/rig/auth/authhttp.Handler.SignInToBrowser], which
// [github.com/simonjanss/rig/auth.New] selects when a project names a web
// origin and writes no OnSignIn of its own.
//
// The two things that are easy to get wrong and fail silently are both decided
// in [New] rather than left to a caller:
//
//   - The cookie's Domain. A browser drops a Set-Cookie whose Domain is not a
//     registrable domain of the host that sent it, and says nothing about it.
//     [New] derives it from the public suffix list and refuses at startup when
//     the two hosts share none, so the deployment fails to boot rather than
//     failing one sign-in at a time.
//   - Where a returnTo resolves. A relative path is resolved against the front
//     end, not against this API, because that is the origin the person is
//     looking at — and an absolute one is only honoured when the cookie will
//     actually reach it.
package handoff

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/simonjanss/rig/runtime/authwire"
	"golang.org/x/net/publicsuffix"
)

// DefaultTTL bounds how long the handoff cookie lives.
//
// It is a value in flight between two page loads that happen back to back, so
// the window only has to cover a redirect and a script. A minute is generous
// for that and short enough that a cookie left behind by a browser that never
// arrived is not a credential lying around.
const DefaultTTL = time.Minute

// DefaultCallbackPath is where the front end is expected to receive a handoff.
const DefaultCallbackPath = "/auth/callback"

// maxCookieBytes is the smallest limit in wide use for one cookie's value.
//
// Browsers are not required to store a larger one and do not report dropping
// it, so a value over this is refused here instead: a sign-in that fails with
// nothing written anywhere is the worst outcome available.
const maxCookieBytes = 4096

// Config says where the front end is and how to reach it from here.
type Config struct {
	// Origin is where the front end is served, as scheme://host[:port] with no
	// path — https://app.example.com. Required.
	Origin string
	// CallbackPath is the path on Origin that receives a handoff. Empty is
	// [DefaultCallbackPath].
	CallbackPath string
	// APIHost is the host this API is served on, which is the other half of the
	// cookie's Domain. Required, and a host rather than a URL: a scheme is
	// accepted and ignored.
	APIHost string
	// TTL is how long the cookie lives. Zero is [DefaultTTL].
	TTL time.Duration
}

// Handoff writes the end of a browser sign-in. Build one with [New].
type Handoff struct {
	origin   *url.URL
	callback string
	// domain is the cookie's Domain, or empty for a host-only cookie — which is
	// the right answer when the API and the front end are the same host, and
	// the only answer a browser will keep.
	domain string
	secure bool
	ttl    time.Duration
}

// New checks the two hosts against each other and returns the writer.
//
// It fails when the front end's origin is not an origin, when the callback path
// is not a path, or when a cookie written here could never be read there. That
// last one is the point of doing this at startup: the alternative is a
// deployment that boots, redirects, and loses every sign-in.
func New(cfg Config) (*Handoff, error) {
	origin, err := parseOrigin(cfg.Origin)
	if err != nil {
		return nil, err
	}

	callback := cfg.CallbackPath
	if callback == "" {
		callback = DefaultCallbackPath
	}
	if !strings.HasPrefix(callback, "/") || strings.HasPrefix(callback, "//") {
		return nil, fmt.Errorf(
			"handoff: callback path %q is not a path on %s; it must begin with "+
				"a single /", cfg.CallbackPath, origin)
	}

	apiHost := hostOf(cfg.APIHost)
	if apiHost == "" {
		return nil, fmt.Errorf(
			"handoff: no API host, so the domain the cookie needs cannot be "+
				"worked out; it is the host %s is served on", cfg.Origin)
	}

	domain, err := cookieDomain(apiHost, origin.Hostname())
	if err != nil {
		return nil, err
	}

	ttl := cfg.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}

	return &Handoff{
		origin:   origin,
		callback: callback,
		domain:   domain,
		secure:   origin.Scheme == "https",
		ttl:      ttl,
	}, nil
}

// Origin is the front end's origin, normalised — which is also an entry the
// sign-in's returnTo allow-list needs, since the redirects below all land there.
func (h *Handoff) Origin() string { return h.origin.String() }

// Destination is where this sign-in should send the browser.
//
// Empty is the configured callback. A relative path is resolved against the
// front end's origin rather than this API's, which is the whole difference
// between the two endings: the person is looking at the front end, and a path
// they asked to return to is a route there.
//
// An absolute URL is honoured only when the cookie written for it would
// actually be sent to that host; anything else — another origin, a scheme-less
// //host, a URL that does not parse — falls back to the callback. There is no
// error return because there is nothing useful a caller could do with one: a
// returnTo has already been checked against the allow-list by the time it gets
// here, and the callback is always a correct answer.
func (h *Handoff) Destination(returnTo string) *url.URL {
	fallback := func() *url.URL {
		dest := *h.origin
		dest.Path = h.callback
		return &dest
	}
	if returnTo == "" {
		return fallback()
	}

	u, err := url.Parse(returnTo)
	if err != nil {
		return fallback()
	}
	if u.Scheme == "" && u.Host == "" {
		if !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
			return fallback()
		}
		return h.origin.ResolveReference(u)
	}
	if !h.reaches(u.Hostname()) {
		return fallback()
	}
	return u
}

// Cookie is the handoff itself, addressed to one destination.
//
// Path is the destination's, so the cookie is sent to the page that reads it
// and to nothing else on the front end. It is deliberately not HttpOnly: a
// script on that page is the only thing that can act on these tokens, which is
// the same reason they are in a cookie at all rather than in a fragment.
func (h *Handoff) Cookie(in authwire.Handoff, dest *url.URL) (*http.Cookie, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("handoff: encoding the handoff: %w", err)
	}
	value := base64.RawURLEncoding.EncodeToString(body)
	if len(value) > maxCookieBytes {
		return nil, fmt.Errorf(
			"handoff: the handoff is %d bytes encoded and a cookie holds about "+
				"%d, so a browser would drop it without a word",
			len(value), maxCookieBytes)
	}

	path := dest.Path
	if path == "" {
		path = "/"
	}
	return &http.Cookie{
		Name:   authwire.HandoffCookie,
		Value:  value,
		Path:   path,
		Domain: h.domain,
		// The landing page has to read this, so it cannot be HttpOnly. What
		// stands in for that is the minute it lives and the one path it is sent
		// to.
		HttpOnly: false,
		Secure:   h.secure,
		// Lax rather than None: the cookie is read by a page on its own site,
		// and None without Secure is dropped outright.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(h.ttl.Seconds()),
	}, nil
}

// Write sets the cookie and redirects, which is the whole of a successful
// browser sign-in's last step.
func (h *Handoff) Write(
	w http.ResponseWriter, r *http.Request, in authwire.Handoff, returnTo string,
) error {
	dest := h.Destination(returnTo)
	cookie, err := h.Cookie(in, dest)
	if err != nil {
		return err
	}
	http.SetCookie(w, cookie)
	http.Redirect(w, r, dest.String(), http.StatusSeeOther)
	return nil
}

// Fail redirects to the same destination with no cookie and an error on the
// query string.
//
// One front-end route handles both outcomes, and it tells them apart by whether
// there is a cookie to take. The reason is a short stable token rather than a
// sentence, because it is going into a URL somebody may screenshot and into a
// switch the application writes — the wording of a message is neither.
//
// The rest of the query survives, so a sign-in that started at a particular
// page still comes back to it with its own parameters intact.
func (h *Handoff) Fail(
	w http.ResponseWriter, r *http.Request, returnTo, reason string,
) {
	if reason == "" {
		// Nothing is worse than a redirect that looks like success and then has
		// no tokens on it.
		reason = "unknown"
	}
	dest := h.Destination(returnTo)
	q := dest.Query()
	q.Set("error", reason)
	dest.RawQuery = q.Encode()
	http.Redirect(w, r, dest.String(), http.StatusSeeOther)
}

// reaches reports whether the cookie this writes would be sent to a host.
func (h *Handoff) reaches(host string) bool {
	host = strings.ToLower(host)
	if host == h.origin.Hostname() {
		return true
	}
	if h.domain == "" {
		return false
	}
	return host == h.domain || strings.HasSuffix(host, "."+h.domain)
}

// cookieDomain is the Domain attribute that gets the cookie from one host to
// the other, or a refusal saying why nothing would.
func cookieDomain(apiHost, webHost string) (string, error) {
	if apiHost == webHost {
		// Host-only, which is both correct and the only thing a browser will
		// keep for a single-label host: Domain=localhost is dropped.
		return "", nil
	}

	api, apiErr := publicsuffix.EffectiveTLDPlusOne(apiHost)
	web, webErr := publicsuffix.EffectiveTLDPlusOne(webHost)
	if apiErr != nil || webErr != nil || api != web {
		return "", fmt.Errorf(
			"handoff: this API is served on %s and the front end on %s, which "+
				"share no registrable domain, so no cookie set here can be read "+
				"there; serve them under one domain, or end the sign-in yourself "+
				"with an OnSignIn",
			apiHost, webHost)
	}
	return api, nil
}

// parseOrigin reads scheme://host and refuses anything more.
func parseOrigin(raw string) (*url.URL, error) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return nil, fmt.Errorf("handoff: no web origin, and it is required")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("handoff: web origin %q is not a URL: %w", raw, err)
	}
	switch {
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf(
			"handoff: web origin %q needs an http:// or https:// scheme", raw)
	case u.Host == "":
		return nil, fmt.Errorf("handoff: web origin %q names no host", raw)
	case u.Path != "" || u.RawQuery != "" || u.Fragment != "":
		return nil, fmt.Errorf(
			"handoff: web origin %q is an origin rather than a URL, so it "+
				"carries no path, query or fragment", raw)
	}
	u.Host = strings.ToLower(u.Host)
	return u, nil
}

// hostOf takes a host out of whatever the caller had — a bare host, a
// host:port, or a whole origin, since the value usually comes from the same
// place the provider's redirect URI does.
func hostOf(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "//") {
		if u, err := url.Parse(strings.TrimRight(raw, "/")); err == nil {
			return strings.ToLower(u.Hostname())
		}
		return ""
	}
	if u, err := url.Parse("//" + raw); err == nil {
		return strings.ToLower(u.Hostname())
	}
	return strings.ToLower(raw)
}

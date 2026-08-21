package observe

import (
	"cmp"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

// PasswordEnv is where the monitoring page's password comes from unless the
// project named another variable.
//
// It is an environment variable and not a key in rig.yaml for the reason the
// collector endpoint is not one either: it is a property of the deployment. A
// password is also a secret, and rig.yaml is checked in — a project may write
// one there anyway, and rig says so once rather than refusing.
const PasswordEnv = "RIG_MONITOR_PASSWORD"

// The page's defaults, applied by [Provider.Page] to whatever it is given.
const (
	// DefaultMonitorPath is where the page is mounted. Under a prefix that
	// starts with an underscore so that it cannot collide with a table: rig
	// reserves the rig_ prefix in the database and nothing generates a route
	// beginning /_rig.
	DefaultMonitorPath = "/_rig/monitor"
	// DefaultMaxTraces is how many requests the page lists.
	DefaultMaxTraces = 200
	// MinPasswordLength is the shortest password the page will start with. It
	// is the one guard against a brute force, because there is no lockout and
	// no rate limit here — see the package documentation.
	MinPasswordLength = 12
)

//go:embed page.html
var pageHTML []byte

// PageConfig is what the monitoring page needs that the generated Monitoring()
// can know.
//
// Where the spans are is deliberately not in it: [Provider.Page] fills that in
// from what [Setup] resolved, so the page reads the file this process writes
// rather than a second path somebody has to keep in step.
type PageConfig struct {
	// ServiceName is what the page calls this application, and it comes from
	// project.name in rig.yaml.
	ServiceName string

	// BasePath is where the page is mounted. Empty means
	// [DefaultMonitorPath]. It must be absolute and must not end in a slash.
	BasePath string

	// MaxTraces is how many requests the page lists, newest first. Zero means
	// [DefaultMaxTraces].
	MaxTraces int

	// Password is a literal from rig.yaml, for a project that wants the page
	// working without an environment to set. Empty — the ordinary case — falls
	// back to PasswordEnv.
	Password string

	// PasswordEnv names the variable the password is read from. Empty means
	// [PasswordEnv].
	PasswordEnv string

	// Allow is the addresses that may reach the page, as CIDR ranges or single
	// addresses — "10.0.0.0/8", "127.0.0.1", "::1". Empty, the default, allows
	// any address and leaves the password as the only check.
	//
	// It narrows; it does not replace. An address that is not on the list is
	// answered 404 before the password is looked at, so a scan learns nothing
	// and a leaked password is still not enough on its own.
	//
	// It is matched against the connection's own address and never against a
	// forwarded header, for the reason auth.trusted_proxies exists: an address
	// read from a header a client controls is an address a client chooses, and
	// an allowlist keyed on one is an allowlist anybody walks around.
	//
	// The cost of that choice is worth knowing before you rely on this: behind
	// a load balancer every request arrives from the balancer, so the list
	// matches everything or nothing and is no boundary at all. There, restrict
	// at the proxy and let the password be the check here.
	Allow []string
}

// Page is rig's monitoring page: the last requests this server served, and the
// spans underneath each one.
//
// It is a reader over the span file and holds nothing. A restart loses no
// history, because the history is the file; a second replica shows its own,
// because a file is per process.
type Page struct {
	cfg      PageConfig
	file     string
	password string
	// allow is Allow, parsed. Nil means every address, which is what an empty
	// list means: this is a narrowing and not a default-deny.
	allow []netip.Prefix
	// unarmed is why this page will mount nothing, or empty when it will.
	unarmed string
}

// Page builds the monitoring page over the span file this provider is writing.
//
// It returns an error only for a configuration that is wrong — a base path that
// is not one, a password too short to be worth having. Having no password at
// all is not wrong: it is how a project that generated the page decides not to
// serve it in this environment, and [Page.Mount] then registers nothing.
// [Page.Unarmed] says which, in one line a main can log.
//
//	page, err := tracing.Page(api.Monitoring())
//	if err != nil {
//	    return nil, err
//	}
//	if why := page.Unarmed(); why != "" {
//	    app.Logger.Info("monitoring page not mounted", "reason", why)
//	}
func (p *Provider) Page(cfg PageConfig) (*Page, error) {
	cfg.BasePath = cmp.Or(cfg.BasePath, DefaultMonitorPath)
	cfg.MaxTraces = cmp.Or(cfg.MaxTraces, DefaultMaxTraces)
	cfg.PasswordEnv = cmp.Or(cfg.PasswordEnv, PasswordEnv)

	if !strings.HasPrefix(cfg.BasePath, "/") || strings.HasSuffix(cfg.BasePath, "/") {
		return nil, fmt.Errorf("observe: monitoring base path %q must start with / and not end with one", cfg.BasePath)
	}

	allow, err := parseAllow(cfg.Allow)
	if err != nil {
		return nil, err
	}

	pg := &Page{cfg: cfg, password: cmp.Or(cfg.Password, os.Getenv(cfg.PasswordEnv)), allow: allow}
	if p != nil {
		pg.file = p.cfg.File
	}

	switch {
	case pg.password == "":
		pg.unarmed = "no password: set $" + cfg.PasswordEnv
	case len(pg.password) < MinPasswordLength:
		return nil, fmt.Errorf("observe: the monitoring password is %d characters, and %d is the minimum",
			len(pg.password), MinPasswordLength)
	}
	return pg, nil
}

// Unarmed is why this page will serve nothing, or empty when it will serve.
//
// A reason rather than a boolean, because the answer is worth logging: a page
// that is configured in rig.yaml and absent at run time should say which of the
// two ends is missing.
func (pg *Page) Unarmed() string {
	if pg == nil {
		return "no page"
	}
	return pg.unarmed
}

// Mount registers the page's routes, and registers nothing when it is unarmed.
//
// Nothing rather than a handler that refuses: a route that answers 401 tells
// anybody scanning that there is a page here, and a route that does not exist
// tells them nothing. It is the same argument that leaves the registration
// endpoint unmounted rather than answering 403.
//
// It goes on the same mux as the API, after it, so that a pattern collision is
// a panic naming this page rather than a route the project owns. The page is
// not traced and not logged, and that is not arranged here: rig opens its spans
// and writes its request lines inside each generated handler, so anything else
// on the mux is already invisible to both. Looking at the page does not appear
// on the page.
func (pg *Page) Mount(mux *http.ServeMux) {
	if pg == nil || pg.unarmed != "" {
		return
	}

	mux.Handle("GET "+pg.cfg.BasePath, pg.guard(pg.serveHTML))
	mux.Handle("GET "+pg.cfg.BasePath+"/{$}", pg.guard(pg.serveHTML))
	mux.Handle("GET "+pg.cfg.BasePath+"/traces.json", pg.guard(pg.serveTraces))
}

// guard is the address list and then the password, in that order.
//
// The order is the point. An address that is not allowed is answered 404, the
// same as a page that was never mounted, so nothing about this server's shape
// leaks to an address that has no business here — and the password is not even
// compared, so it cannot be guessed from off the list.
//
// The password is HTTP Basic rather than a form and a cookie because it is the
// whole of what this page needs: one secret, no session to store, no sign-out,
// and a browser that already knows how to ask. The user name is not checked —
// there is one credential here, and pretending otherwise would invent an
// account nobody created.
//
// The comparison is constant-time. There is deliberately no lockout and no rate
// limit: that would mean this module depending on rig/runtime for the throttle.
// What stands in for one is [MinPasswordLength], [PageConfig.Allow], and
// whatever TLS the deployment already has.
func (pg *Page) guard(next http.HandlerFunc) http.Handler {
	want := []byte(pg.password)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !pg.allowed(r) {
			http.NotFound(w, r)
			return
		}

		_, got, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="rig monitor", charset="UTF-8"`)
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	})
}

// allowed reports whether this connection's own address is on the list.
//
// [http.Request.RemoteAddr] and nothing else — no X-Forwarded-For, no
// X-Real-IP. See [PageConfig.Allow] for why. An address that cannot be parsed
// is refused rather than allowed: the list is a narrowing, and a narrowing that
// opens up when it cannot tell who is calling is not one.
func (pg *Page) allowed(r *http.Request) bool {
	if len(pg.allow) == 0 {
		return true
	}

	addr, err := remoteAddr(r.RemoteAddr)
	if err != nil {
		return false
	}
	for _, prefix := range pg.allow {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// remoteAddr is the address half of a RemoteAddr, unmapped.
//
// Unmapped because a dual-stack listener reports an IPv4 client as
// ::ffff:127.0.0.1, and an allowlist that said 127.0.0.1/32 would then refuse
// the machine it is running on. The port is dropped, and a RemoteAddr with no
// port at all is still read — httptest and a Unix socket both produce one.
func remoteAddr(s string) (netip.Addr, error) {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap(), nil
	}
	addr, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, err
	}
	return addr.Unmap(), nil
}

// parseAllow reads the list, accepting a range or a single address.
//
// A single address is the common case — one bastion, one office — and making
// somebody write /32 for it is the kind of ceremony that produces a typo, which
// on this list means a page nobody can reach.
func parseAllow(entries []string) ([]netip.Prefix, error) {
	if len(entries) == 0 {
		return nil, nil
	}

	out := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		prefix, err := parsePrefix(entry)
		if err != nil {
			return nil, err
		}
		out = append(out, prefix)
	}
	return out, nil
}

// parsePrefix reads one entry of [PageConfig.Allow]: a CIDR range like
// "10.0.0.0/8", or a single address like "127.0.0.1" standing for itself.
//
// rig's own configuration check parses the list a second time, in
// internal/project, so that a typo is a diagnostic when rig.yaml is read rather
// than a server that will not start. Sharing this would mean the rig binary
// importing this module, and with it OpenTelemetry, for eight lines over
// net/netip — the same trade that keeps the page's defaults spelled twice.
func parsePrefix(entry string) (netip.Prefix, error) {
	if strings.Contains(entry, "/") {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("observe: %q is not an address range: %w", entry, err)
		}
		// Masked so that "10.1.2.3/8" is stored as the range it means rather
		// than as a host inside it, which is what makes String() readable when
		// somebody prints one back.
		return prefix.Masked(), nil
	}

	addr, err := netip.ParseAddr(entry)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("observe: %q is not an address or an address range: %w", entry, err)
	}
	addr = addr.Unmap()
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// serveHTML answers the page itself, which is one embedded file and no
// templating: everything it shows it fetches from traces.json.
func (pg *Page) serveHTML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(pageHTML)
}

// traceList is what traces.json answers.
//
// Reason is why there is nothing to show, and it is the difference between a
// page that is empty and a page that looks broken: a server with tracing on and
// no file configured has no traces and never will, and saying so is the only
// way anybody finds out.
type traceList struct {
	Service string        `json:"service,omitempty"`
	File    string        `json:"file,omitempty"`
	Reason  string        `json:"reason,omitempty"`
	Traces  []TraceRecord `json:"traces"`
}

// serveTraces answers the traces, filtered by the query.
func (pg *Page) serveTraces(w http.ResponseWriter, r *http.Request) {
	out := traceList{Service: pg.cfg.ServiceName, File: pg.file, Traces: []TraceRecord{}}

	if pg.file == "" {
		out.Reason = "No span file. Set $" + FileEnv + ", or observe.Config.File, and restart."
		writeJSON(w, http.StatusOK, out)
		return
	}

	limit := pg.cfg.MaxTraces
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, pg.cfg.MaxTraces)
	}

	traces, err := ReadTraces(pg.file, limit)
	if err != nil {
		out.Reason = "Cannot read " + pg.file + ": " + err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}

	q := r.URL.Query()
	for _, t := range traces {
		if q.Get("status") == "error" && t.Status != "error" {
			continue
		}
		if term := q.Get("q"); term != "" && !matches(t, term) {
			continue
		}
		out.Traces = append(out.Traces, t)
	}

	// A filter that matched nothing is not a server that has served nothing,
	// and saying the second when the first is true contradicts the list
	// somebody was looking at a keystroke earlier.
	if len(out.Traces) == 0 {
		out.Reason = "No request here matches that filter."
		if len(traces) == 0 {
			out.Reason = "Nothing yet. This server has served no request since the span file was written."
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// matches is the search box: a trace id somebody pasted, or any substring of
// any span's name or attributes.
//
// Over the records already read rather than through a query language. The
// budget is a few hundred traces held in memory for the length of one response,
// and anything more than that is a tracing backend.
func matches(t TraceRecord, term string) bool {
	term = strings.ToLower(term)
	if strings.Contains(strings.ToLower(t.ID), term) {
		return true
	}
	for i := range t.Spans {
		span := &t.Spans[i]
		if strings.Contains(strings.ToLower(span.Name), term) ||
			strings.Contains(strings.ToLower(span.Error), term) {
			return true
		}
		for k, v := range span.Attributes {
			if strings.Contains(strings.ToLower(k), term) ||
				strings.Contains(strings.ToLower(fmt.Sprint(v)), term) {
				return true
			}
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

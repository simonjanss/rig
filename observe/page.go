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

// AddrEnv is where the monitoring page's listen address comes from when the
// deployment would rather say it than take what rig.yaml resolved.
//
// It overrides [PageConfig.Addr] rather than filling it in, which is the
// opposite of how [PasswordEnv] works and is deliberate. The password has no
// sensible value in a checked-in file, so rig.yaml is the fallback and the
// environment is the ordinary answer. A listen address does have one — the
// project decided it once, and `rig validate` refuses a project that did not —
// so the environment is the exception, for the deployment that has to move the
// port off whatever the project picked.
const AddrEnv = "RIG_MONITOR_ADDR"

// The page's defaults, applied by [Provider.Page] to whatever it is given.
const (
	// DefaultMonitorPath is where the page is mounted. Under a prefix that
	// starts with an underscore so that it cannot collide with a table: rig
	// reserves the rig_ prefix in the database and nothing generates a route
	// beginning /_rig.
	DefaultMonitorPath = "/_rig/monitor"
	// DefaultMaxTraces is how many requests the page lists.
	DefaultMaxTraces = 200
	// DefaultMaxLogs is how many log lines the page reads. Larger than
	// [DefaultMaxTraces] because one request writes several lines, and the
	// request line alone means there is at least one per request listed.
	DefaultMaxLogs = 500
	// MinPasswordLength is the shortest password the page will start with. It
	// is the one guard against a brute force, because there is no lockout and
	// no rate limit here — see the package documentation.
	MinPasswordLength = 12
)

// The page's assets: markup, style and behaviour, one file each rather than one
// file with three languages in it. go:embed takes three as easily as one, they
// are served through the same guard as everything else here, and a deployment
// with a strict content security policy can serve them at all — which an inline
// <style> and <script> would not be.
var (
	//go:embed page.html
	pageHTML []byte
	//go:embed page.css
	pageCSS []byte
	//go:embed page.js
	pageJS []byte
)

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

	// Addr is where the page listens, as host:port — "127.0.0.1:9090", or
	// ":9090" for every interface. $RIG_MONITOR_ADDR overrides it; see
	// [AddrEnv].
	//
	// The page gets a listener of its own rather than a route on the API's mux,
	// and this is the whole reason: [PageConfig.Allow] is matched against the
	// connection's own address, so behind a load balancer it matches everything
	// or nothing, and the only boundary left that a client cannot talk its way
	// around is which interface the socket is bound to. Loopback is a boundary
	// the kernel keeps.
	//
	// Empty is not defaulted. Nothing here knows what port is free on the
	// machine this runs on, and a page reachable from an interface nobody chose
	// is the failure this field exists to prevent — so an empty one is a page
	// that does not listen, the same as a missing password, and [Page.Unarmed]
	// says which.
	//
	// Serving it is [Page.Handler] and the application's own server; this
	// package opens no listeners.
	Addr string

	// BasePath is where the page is mounted on that listener. Empty means
	// [DefaultMonitorPath]. It must be absolute and must not end in a slash.
	//
	// Nothing else is on the listener to collide with. It is kept because it is
	// the URL projects already have, and because a reverse proxy in front of
	// [PageConfig.Addr] needs a prefix to key on.
	BasePath string

	// MaxTraces is how many requests the page lists, newest first. Zero means
	// [DefaultMaxTraces].
	MaxTraces int

	// MaxLogs is how many log lines the page reads, newest first. Zero means
	// [DefaultMaxLogs].
	MaxLogs int

	// Logs is the log file this process is writing, and nil is a page with no
	// log half — which is what a project that never wired one gets, and it says
	// so rather than showing an empty list.
	//
	// It is the sink itself and not a path, for the reason the span file is not
	// a field here either: the page and the writer agreeing on a file should not
	// be something a main arranges twice. Open it with [OpenLogs], tee
	// [Logs.Handler] into the logger the application already has, and set the
	// same object here.
	//
	// It is not in rig.yaml and cannot be, because it is an object rather than a
	// value. A generated Monitoring() therefore leaves it empty — but a
	// generated NewProcess() fills it, because that constructor is where the
	// sink is opened. A main that builds its own page is the caller that still
	// sets this by hand.
	Logs *Logs

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
	// matches everything or nothing and is no boundary at all. There,
	// [PageConfig.Addr] is the boundary — bind the page somewhere the balancer
	// is not — and this list is what narrows the addresses on that network.
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
	logs     *Logs
	password string
	// allow is Allow, parsed. Nil means every address, which is what an empty
	// list means: this is a narrowing and not a default-deny.
	allow []netip.Prefix
	// unarmed is why this page will serve nothing, or empty when it will.
	unarmed string
}

// Page builds the monitoring page over the span file this provider is writing.
//
// It returns an error only for a configuration that is wrong — a base path that
// is not one, a password too short to be worth having. Having no password, or
// no address to listen on, is not wrong: it is how a project that generated the
// page decides not to serve it in this environment, and [Page.Handler] is then
// nil and [Page.Addr] empty. [Page.Unarmed] says which, in one line a main can
// log.
//
//	page, err := tracing.Page(api.Monitoring())
//	if err != nil {
//	    return nil, err
//	}
//	if why := page.Unarmed(); why != "" {
//	    app.Logger.Info("monitoring page not listening", "reason", why)
//	}
//
// Then hand both halves to whatever runs the servers:
//
//	serve.Config{Monitor: page.Handler(), MonitorAddr: page.Addr()}
func (p *Provider) Page(cfg PageConfig) (*Page, error) {
	cfg.Addr = cmp.Or(os.Getenv(AddrEnv), cfg.Addr)
	cfg.BasePath = cmp.Or(cfg.BasePath, DefaultMonitorPath)
	cfg.MaxTraces = cmp.Or(cfg.MaxTraces, DefaultMaxTraces)
	cfg.MaxLogs = cmp.Or(cfg.MaxLogs, DefaultMaxLogs)
	cfg.PasswordEnv = cmp.Or(cfg.PasswordEnv, PasswordEnv)

	if !strings.HasPrefix(cfg.BasePath, "/") || strings.HasSuffix(cfg.BasePath, "/") {
		return nil, fmt.Errorf("observe: monitoring base path %q must start with / and not end with one", cfg.BasePath)
	}

	allow, err := parseAllow(cfg.Allow)
	if err != nil {
		return nil, err
	}

	pg := &Page{cfg: cfg, logs: cfg.Logs, password: cmp.Or(cfg.Password, os.Getenv(cfg.PasswordEnv)), allow: allow}
	if p != nil {
		pg.file = p.cfg.File
	}

	// Two rotating writers on one path interleave their lines and rotate each
	// other's data away, and the symptom is a file that reads as neither. It is
	// a configuration mistake with a silent failure, so it is refused here
	// rather than discovered as a page that shows nonsense.
	if pg.file != "" && pg.file == cfg.Logs.File() {
		return nil, fmt.Errorf("observe: the span file and the log file are both %q; they have to be different files", pg.file)
	}

	// A password too short is refused, and whether there is anywhere to listen
	// does not come into it: somebody who set a five-character password made a
	// mistake either way, and finding out about it only after also setting an
	// address is finding out twice.
	if pg.password != "" && len(pg.password) < MinPasswordLength {
		return nil, fmt.Errorf("observe: the monitoring password is %d characters, and %d is the minimum",
			len(pg.password), MinPasswordLength)
	}

	// Missing altogether is not a mistake. It is an environment saying it does
	// not want the page — a laptop, CI, a one-off container — and refusing to
	// start there would be rig deciding that a server without a monitoring page
	// is not worth running.
	//
	// The address is reported first because it is the coarser answer. With
	// nothing to listen on, whether there is a password to compare has not come
	// up yet, and naming the password would send somebody to set a variable
	// that changes nothing.
	switch {
	case cfg.Addr == "":
		pg.unarmed = "no address: set monitoring.addr in rig.yaml, or $" + AddrEnv
	case pg.password == "":
		pg.unarmed = "no password: set $" + cfg.PasswordEnv
	}
	return pg, nil
}

// Addr is where this page listens, or empty when it is unarmed.
//
// Empty and nil from [Page.Handler] travel together: a caller that passes both
// on gets no listener, which is the whole of what an unarmed page does.
func (pg *Page) Addr() string {
	if pg == nil || pg.unarmed != "" {
		return ""
	}
	return pg.cfg.Addr
}

// BasePath is where the page is mounted on its own listener, resolved.
//
// It is here for the caller that has to build a link to the page — the page is
// on an origin of its own now, so a relative href no longer reaches it — and it
// answers even when the page is unarmed, because it is a fact about the
// configuration rather than about whether anything is serving. [Page.Addr] is
// the half that goes empty.
func (pg *Page) BasePath() string {
	if pg == nil {
		return ""
	}
	return pg.cfg.BasePath
}

// Handler is the page, ready to be served, and nil when it is unarmed.
//
// Nil rather than a handler that refuses, for the reason [Page.Mount] registers
// nothing: a port answering 401 tells anybody scanning that there is a page
// here, and a port that is not open tells them nothing. Passing it to
// [github.com/simonjanss/rig/runtime/serve.Config] Monitor is what makes the
// difference — nil there means no second listener is opened at all.
//
// It is a mux of its own rather than the API's, which is what keeps the page
// off the network the API is on. See [PageConfig.Addr].
func (pg *Page) Handler() http.Handler {
	if pg == nil || pg.unarmed != "" {
		return nil
	}
	mux := http.NewServeMux()
	pg.Mount(mux)
	return mux
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

// Mount registers the page's routes on a mux, and registers nothing when it is
// unarmed.
//
// Nothing rather than a handler that refuses: a route that answers 401 tells
// anybody scanning that there is a page here, and a route that does not exist
// tells them nothing. It is the same argument that leaves the registration
// endpoint unmounted rather than answering 403.
//
// [Page.Handler] is the ordinary way in, and it is this call onto a mux of the
// page's own. This one is exported for the caller who has a mux already and a
// reason — a test, or a reverse proxy arrangement where the page shares a
// listener with something that is not this application's API. Sharing it with
// that API is what [PageConfig.Addr] exists to stop.
//
// The path without its trailing slash redirects to the one with it, rather than
// serving the same page twice. That is what lets the HTML name its stylesheet
// and its script by a relative path: from /_rig/monitor those would resolve
// against /_rig/, and the page's assets are the one thing here that cannot be
// written without knowing what monitoring.base_path was set to. Behind the
// guard, so an address that may not see the page does not learn it exists from
// a redirect either.
//
// The page is not traced and not logged, and that is not arranged here: rig
// opens its spans and writes its request lines inside each generated handler,
// so anything that is not one is already invisible to both. Looking at the page
// does not appear on the page — which was true when it shared the API's mux and
// is now true twice over, since it does not.
func (pg *Page) Mount(mux *http.ServeMux) {
	if pg == nil || pg.unarmed != "" {
		return
	}

	mux.Handle("GET "+pg.cfg.BasePath, pg.guard(pg.redirectToSlash))
	mux.Handle("GET "+pg.cfg.BasePath+"/{$}", pg.guard(pg.serveHTML))
	mux.Handle("GET "+pg.cfg.BasePath+"/page.css", pg.guard(pg.asset("text/css; charset=utf-8", pageCSS)))
	mux.Handle("GET "+pg.cfg.BasePath+"/page.js", pg.guard(pg.asset("text/javascript; charset=utf-8", pageJS)))
	mux.Handle("GET "+pg.cfg.BasePath+"/traces.json", pg.guard(pg.serveTraces))
	mux.Handle("GET "+pg.cfg.BasePath+"/logs.json", pg.guard(pg.serveLogs))
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

// redirectToSlash sends the base path to the base path with a slash. See
// [Page.Mount] for why the page has one entry point rather than two.
func (pg *Page) redirectToSlash(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, pg.cfg.BasePath+"/", http.StatusMovedPermanently)
}

// serveHTML answers the page itself, which is an embedded file and no
// templating: everything it shows it fetches from traces.json and logs.json.
func (pg *Page) serveHTML(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(pageHTML)
}

// asset answers one of the embedded files beside the page.
//
// Behind [Page.guard] like everything else, so the style and the behaviour of a
// page an address may not see are not readable by that address either. They are
// sent with no-store, which the guard sets: they change only when the binary
// does, but a caching header would be a second thing to reason about for two
// files of a few kilobytes.
func (pg *Page) asset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(body)
	}
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

// logList is what logs.json answers.
//
// Levels is a count per level over the whole window, before the filters below
// are applied, so the page's level chips can carry a number that does not move
// while somebody types in the search box.
type logList struct {
	Service string         `json:"service,omitempty"`
	File    string         `json:"file,omitempty"`
	Reason  string         `json:"reason,omitempty"`
	Levels  map[string]int `json:"levels,omitempty"`
	Logs    []LogRecord    `json:"logs"`
}

// serveLogs answers the log lines, filtered by the query.
//
// `trace` is what makes a request and the lines it wrote one view: the page asks
// for one trace's lines when a request is opened, rather than carrying every
// line in the list response.
func (pg *Page) serveLogs(w http.ResponseWriter, r *http.Request) {
	out := logList{Service: pg.cfg.ServiceName, File: pg.logs.File(), Logs: []LogRecord{}}

	// Two ways to have no logs, and they have different remedies: a project
	// that wired no sink at all, and a sink running where nothing said where to
	// write. Saying which is the only way anybody finds out.
	switch {
	case pg.logs == nil:
		out.Reason = "No log sink. Open one with observe.OpenLogs, tee logs.Handler() into your logger, and pass it as PageConfig.Logs."
		writeJSON(w, http.StatusOK, out)
		return
	case pg.logs.Unarmed() != "":
		out.Reason = "No log file. Set $" + LogFileEnv + ", or observe.LogConfig.File, and restart."
		writeJSON(w, http.StatusOK, out)
		return
	}

	limit := pg.cfg.MaxLogs
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = min(n, pg.cfg.MaxLogs)
	}

	recs, err := pg.logs.Read(limit)
	if err != nil {
		out.Reason = "Cannot read " + pg.logs.File() + ": " + err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}

	out.Levels = make(map[string]int, len(logLevels))
	for _, rec := range recs {
		out.Levels[strings.ToUpper(rec.Level)]++
	}

	q := r.URL.Query()
	for _, rec := range recs {
		if id := q.Get("trace"); id != "" && !strings.EqualFold(rec.TraceID, id) {
			continue
		}
		if level := q.Get("level"); level != "" && !atLeast(rec.Level, level) {
			continue
		}
		if term := q.Get("q"); term != "" && !logMatches(rec, term) {
			continue
		}
		out.Logs = append(out.Logs, rec)
	}

	if len(out.Logs) == 0 {
		out.Reason = "No line here matches that filter."
		if len(recs) == 0 {
			out.Reason = "Nothing yet. This server has written no line since the log file was created."
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// logMatches is the search box, over a log line: the message, the level, the
// trace it belongs to, or any of its attributes.
//
// The trace id is in it so that one search term filters both halves of the page
// at once — paste the requestId from somebody's screenshot and the request and
// the lines it wrote both narrow to it.
func logMatches(rec LogRecord, term string) bool {
	term = strings.ToLower(term)
	return strings.Contains(strings.ToLower(rec.Msg), term) ||
		strings.Contains(strings.ToLower(rec.Level), term) ||
		strings.Contains(strings.ToLower(rec.TraceID), term) ||
		strings.Contains(strings.ToLower(rec.SpanID), term) ||
		attrsMatch(rec.Attrs, term)
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
		if attrsMatch(span.Attributes, term) {
			return true
		}
	}
	return false
}

// attrsMatch is a case-insensitive substring over every key and every value of
// an attribute map, a group's contents included.
//
// term arrives already lowered, because both callers loop and lowering it per
// attribute would be the one allocation here that scales with the file.
func attrsMatch(attrs map[string]any, term string) bool {
	for k, v := range attrs {
		if strings.Contains(strings.ToLower(k), term) {
			return true
		}
		if group, ok := v.(map[string]any); ok {
			if attrsMatch(group, term) {
				return true
			}
			continue
		}
		if strings.Contains(strings.ToLower(fmt.Sprint(v)), term) {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

package observe_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/observe"
	"go.opentelemetry.io/otel/trace"
)

const password = "correct horse battery"

// mount is the page on a mux of its own, and the base path it answers under.
func mount(t *testing.T, p *observe.Provider, cfg observe.PageConfig) (*http.ServeMux, string) {
	t.Helper()

	page, err := p.Page(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if why := page.Unarmed(); why != "" {
		t.Fatalf("the page will serve nothing: %s", why)
	}

	mux := http.NewServeMux()
	page.Mount(mux)

	base := cfg.BasePath
	if base == "" {
		base = observe.DefaultMonitorPath
	}
	return mux, base
}

// get is one request at the page, with the password unless told otherwise.
func get(t *testing.T, mux *http.ServeMux, path string, creds bool) *httptest.ResponseRecorder {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, path, nil)
	if creds {
		r.SetBasicAuth("rig", password)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// The page shows paths, request ids, user agents and the causes of every 500 —
// a list of what every caller did. It does not answer without the password.
func TestThePageRefusesWithoutThePassword(t *testing.T) {
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password})

	res := get(t, mux, base, false)
	if res.Code != http.StatusUnauthorized {
		t.Errorf("no credentials = %d, want 401", res.Code)
	}
	if got := res.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("WWW-Authenticate = %q, so a browser is never asked for the password", got)
	}

	r := httptest.NewRequest(http.MethodGet, base, nil)
	r.SetBasicAuth("rig", "wrong")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("the wrong password = %d, want 401", w.Code)
	}

	if res := get(t, mux, base+"/", true); res.Code != http.StatusOK {
		t.Errorf("the right password = %d, want 200", res.Code)
	}
}

// Both spellings of the base path reach the page: the one without the trailing
// slash by way of a redirect to the one with it, which is what lets the HTML
// name its stylesheet and its script relatively. A trailing slash is the
// difference between two URLs to net/http and no difference at all to somebody
// typing one.
func TestThePageAnswersWithAndWithoutATrailingSlash(t *testing.T) {
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password})

	res := get(t, mux, base+"/", true)
	if res.Code != http.StatusOK {
		t.Errorf("GET %s/ = %d, want 200", base, res.Code)
	}
	if got := res.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("GET %s/ answered %q", base, got)
	}

	res = get(t, mux, base, true)
	if res.Code != http.StatusMovedPermanently {
		t.Errorf("GET %s = %d, want 301", base, res.Code)
	}
	if got := res.Header().Get("Location"); got != base+"/" {
		t.Errorf("GET %s redirected to %q, want %q", base, got, base+"/")
	}
}

// The stylesheet and the script are behind the same guard as everything else,
// because the page's own shape is not something an address that may not see it
// should be able to read either.
func TestTheAssetsAreBehindThePassword(t *testing.T) {
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password})

	for path, want := range map[string]string{
		base + "/page.css": "text/css",
		base + "/page.js":  "text/javascript",
	} {
		if res := get(t, mux, path, false); res.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no credentials = %d, want 401", path, res.Code)
		}

		res := get(t, mux, path, true)
		if res.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200", path, res.Code)
		}
		if got := res.Header().Get("Content-Type"); !strings.HasPrefix(got, want) {
			t.Errorf("GET %s answered %q, want %s", path, got, want)
		}
		if res.Body.Len() == 0 {
			t.Errorf("GET %s answered nothing", path)
		}
	}
}

// No password is not a misconfiguration: it is how a project that generated the
// page decides not to serve it here. Nothing is mounted, so nothing scanning
// the server learns that there is a page at all.
func TestAPageWithNoPasswordMountsNothing(t *testing.T) {
	t.Setenv(observe.PasswordEnv, "")

	p := setup(t, observe.Config{ServiceName: "todo"})
	page, err := p.Page(observe.PageConfig{ServiceName: "todo"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Unarmed() == "" {
		t.Fatal("a page with no password says it will serve")
	}
	if !strings.Contains(page.Unarmed(), observe.PasswordEnv) {
		t.Errorf("the reason does not name the variable to set: %q", page.Unarmed())
	}

	mux := http.NewServeMux()
	page.Mount(mux)
	if res := get(t, mux, observe.DefaultMonitorPath, true); res.Code != http.StatusNotFound {
		t.Errorf("an unarmed page answered %d, want 404 — nothing should be registered", res.Code)
	}
}

// The environment is where a password normally comes from, and rig.yaml's
// literal wins when a project wrote one.
func TestThePasswordComesFromTheEnvironment(t *testing.T) {
	t.Setenv(observe.PasswordEnv, password)

	p := setup(t, observe.Config{ServiceName: "todo"})
	page, err := p.Page(observe.PageConfig{ServiceName: "todo"})
	if err != nil {
		t.Fatal(err)
	}
	if why := page.Unarmed(); why != "" {
		t.Fatalf("the page found no password: %s", why)
	}

	mux := http.NewServeMux()
	page.Mount(mux)
	if res := get(t, mux, observe.DefaultMonitorPath+"/", true); res.Code != http.StatusOK {
		t.Errorf("the password from $%s = %d, want 200", observe.PasswordEnv, res.Code)
	}
}

// Wrong is refused loudly, and absent is not wrong. There is no lockout behind
// this password, so its length is the only thing standing between the page and
// a guess.
func TestAConfigurationThatIsWrongIsAnError(t *testing.T) {
	p := setup(t, observe.Config{ServiceName: "todo"})

	if _, err := p.Page(observe.PageConfig{Password: "short"}); err == nil {
		t.Error("a five-character password was accepted")
	}
	for _, base := range []string{"monitor", "/monitor/"} {
		if _, err := p.Page(observe.PageConfig{BasePath: base, Password: password}); err == nil {
			t.Errorf("base path %q was accepted", base)
		}
	}
}

// A project that turned tracing on and never said where the spans go has a page
// with nothing on it forever. Saying so is the only way anybody finds out.
func TestThePageSaysWhyItIsEmpty(t *testing.T) {
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password})

	var body struct {
		Reason string `json:"reason"`
		Traces []any  `json:"traces"`
	}
	res := get(t, mux, base+"/traces.json", true)
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body.Reason, observe.FileEnv) {
		t.Errorf("the empty page does not name the variable to set: %q", body.Reason)
	}
	if body.Traces == nil {
		t.Error("traces is null rather than an empty list, so the page renders nothing at all")
	}
}

// The whole point: the last requests, newest first, with their spans under
// them, and a filter for the ones that failed.
func TestThePageListsTheLastRequests(t *testing.T) {
	path := spanFile(t)

	failed := at("bbb", "3", "1", "repository.Todo.Create.Validator", noon.Add(time.Minute), time.Millisecond)
	failed.Status, failed.Error = "error", "Invalid: a todo needs a title"
	write(t, path,
		at("aaa", "1", "", "GET /api/v1/todos", noon, 12*time.Millisecond),
		at("bbb", "2", "", "POST /api/v1/todos", noon.Add(time.Minute), 40*time.Millisecond),
		failed)

	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo", File: path}),
		observe.PageConfig{ServiceName: "todo", Password: password})

	read := func(query string) []observe.TraceRecord {
		t.Helper()
		var body struct {
			Service string                `json:"service"`
			Traces  []observe.TraceRecord `json:"traces"`
		}
		res := get(t, mux, base+"/traces.json"+query, true)
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Service != "todo" {
			t.Errorf("service = %q, want todo", body.Service)
		}
		return body.Traces
	}

	all := read("")
	if len(all) != 2 {
		t.Fatalf("want two requests, got %d", len(all))
	}
	if all[0].ID != "bbb" {
		t.Errorf("want the newest request first, got %q", all[0].ID)
	}

	errs := read("?status=error")
	if len(errs) != 1 || errs[0].ID != "bbb" {
		t.Fatalf("errors only = %v, want just the failed request", errs)
	}

	found := read("?q=needs+a+title")
	if len(found) != 1 || found[0].ID != "bbb" {
		t.Fatalf("searching the error text found %v", found)
	}
	if none := read("?q=nothing-in-this-file"); len(none) != 0 {
		t.Errorf("a search that matches nothing found %d", len(none))
	}
}

// Looking at the monitoring page does not appear on the monitoring page. It is
// not arranged here and it is worth a test anyway: rig opens its spans inside
// each generated handler, so a route that is not one is invisible to them — and
// a future change that moved span-opening into a wrapper around the mux would
// silently make the page watch itself.
func TestThePageDoesNotAppearOnItself(t *testing.T) {
	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path})
	mux, base := mount(t, p, observe.PageConfig{ServiceName: "todo", Password: password})

	for _, path := range []string{base, base + "/", base + "/page.css", base + "/page.js",
		base + "/traces.json", base + "/logs.json"} {
		get(t, mux, path, true)
	}
	flush(t, p)

	for _, rec := range mustRead(t, path) {
		t.Errorf("looking at the page wrote a span: %q", rec.Name)
	}
}

// The log half. It is a second file and a second reader, and the page has to
// say which of the two ways to have no logs it is looking at, because they have
// different remedies.
func TestThePageSaysWhyItHasNoLogs(t *testing.T) {
	// No sink at all: a project that never wired one.
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password})
	if reason := logReason(t, mux, base, ""); !strings.Contains(reason, "PageConfig.Logs") {
		t.Errorf("with no sink the page says %q, and it should say what to wire", reason)
	}

	// A sink with nowhere to write: the ordinary case on a laptop.
	t.Setenv(observe.LogFileEnv, "")
	logs, err := observe.OpenLogs(observe.LogConfig{})
	if err != nil {
		t.Fatal(err)
	}
	mux, base = mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password, Logs: logs})
	if reason := logReason(t, mux, base, ""); !strings.Contains(reason, observe.LogFileEnv) {
		t.Errorf("with no log file the page says %q, and it should name the variable", reason)
	}
}

// logBody is what logs.json answers, as a test reads it.
type logBody struct {
	Reason string              `json:"reason"`
	Levels map[string]int      `json:"levels"`
	Logs   []observe.LogRecord `json:"logs"`
}

// readLogs is one request at logs.json, decoded.
func readLogs(t *testing.T, mux *http.ServeMux, base, query string) logBody {
	t.Helper()

	var body logBody
	res := get(t, mux, base+"/logs.json"+query, true)
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Logs == nil {
		t.Error("logs is null rather than an empty list, so the page renders nothing at all")
	}
	return body
}

func logReason(t *testing.T, mux *http.ServeMux, base, query string) string {
	t.Helper()
	return readLogs(t, mux, base, query).Reason
}

// seedLogs is a log file with one line per level, two of them belonging to one
// trace.
func seedLogs(t *testing.T) *observe.Logs {
	t.Helper()

	logs, err := observe.OpenLogs(observe.LogConfig{File: filepath.Join(t.TempDir(), "log.jsonl")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logs.Close() })

	logger := slog.New(logs.Handler())
	traced := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: [16]byte{1, 2, 3}, SpanID: [8]byte{4, 5}, TraceFlags: trace.FlagsSampled,
	}))

	logger.DebugContext(traced, "request served", slog.Int("status", 200))
	logger.ErrorContext(traced, "request failed", slog.String("error", "connection refused"))
	logger.InfoContext(context.Background(), "listening", slog.String("addr", ":8080"))
	return logs
}

// The three filters, and the counts the level chips are drawn from.
func TestThePageFiltersTheLogs(t *testing.T) {
	logs := seedLogs(t)
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password, Logs: logs})

	all := readLogs(t, mux, base, "")
	if len(all.Logs) != 3 {
		t.Fatalf("want three lines, got %d", len(all.Logs))
	}
	if all.Logs[0].Msg != "listening" {
		t.Errorf("want the newest line first, got %q", all.Logs[0].Msg)
	}
	if all.Levels["DEBUG"] != 1 || all.Levels["ERROR"] != 1 || all.Levels["INFO"] != 1 {
		t.Errorf("the level counts are %v", all.Levels)
	}

	// A level is a threshold and not an equality test, because that is what
	// somebody filtering a log means by it.
	warned := readLogs(t, mux, base, "?level=WARN")
	if len(warned.Logs) != 1 || warned.Logs[0].Msg != "request failed" {
		t.Errorf("level=WARN gave %d lines", len(warned.Logs))
	}
	if n := len(readLogs(t, mux, base, "?level=DEBUG").Logs); n != 3 {
		t.Errorf("level=DEBUG gave %d lines, want everything", n)
	}

	// The search reaches attribute values, so the cause of a 500 is findable by
	// what it says.
	found := readLogs(t, mux, base, "?q=connection+refused")
	if len(found.Logs) != 1 || found.Logs[0].Msg != "request failed" {
		t.Errorf("searching an attribute value gave %d lines", len(found.Logs))
	}

	// And one trace's lines, which is what the request detail asks for.
	id := all.Logs[1].TraceID
	if id == "" {
		t.Fatal("the traced lines carry no trace id")
	}
	one := readLogs(t, mux, base, "?trace="+id)
	if len(one.Logs) != 2 {
		t.Errorf("trace=%s gave %d lines, want the two written inside it", id, len(one.Logs))
	}

	// A filter that matched nothing is not a server that has logged nothing,
	// and saying the second when the first is true contradicts the list
	// somebody was looking at a keystroke earlier.
	none := readLogs(t, mux, base, "?q=nothing-in-this-file")
	if len(none.Logs) != 0 || !strings.Contains(none.Reason, "matches that filter") {
		t.Errorf("a search that matches nothing said %q", none.Reason)
	}
}

// Same guard as everything else. The log file holds the cause of every 500 this
// server answered, which is more than the spans do.
func TestTheAllowListCoversTheLogs(t *testing.T) {
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{
			ServiceName: "todo", Password: password, Logs: seedLogs(t),
			Allow: []string{"10.0.0.0/8"},
		})

	if got := from(t, mux, base+"/logs.json", "192.0.2.7:41234"); got != http.StatusNotFound {
		t.Errorf("logs.json from an outside address = %d, want 404", got)
	}
	if got := from(t, mux, base+"/logs.json", "10.0.0.1:41234"); got != http.StatusOK {
		t.Errorf("logs.json from an allowed address = %d, want 200", got)
	}

	// And the password, on a page with no list, where the address is not what
	// answers first.
	unrestricted, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password, Logs: seedLogs(t)})
	if res := get(t, unrestricted, base+"/logs.json", false); res.Code != http.StatusUnauthorized {
		t.Errorf("logs.json with no password = %d, want 401", res.Code)
	}
}

// Two rotating writers on one path interleave their lines and rotate each
// other's data away. It is a configuration mistake whose symptom is a file that
// reads as neither, so it is refused where it can still be named.
func TestOneFileForBothIsRefused(t *testing.T) {
	path := spanFile(t)
	logs, err := observe.OpenLogs(observe.LogConfig{File: path})
	if err != nil {
		t.Fatal(err)
	}
	defer logs.Close()

	_, err = setup(t, observe.Config{ServiceName: "todo", File: path}).
		Page(observe.PageConfig{ServiceName: "todo", Password: password, Logs: logs})
	if err == nil {
		t.Fatal("the span file and the log file being one file was accepted")
	}
	if !strings.Contains(err.Error(), "different files") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// mustRead is every record in a span file that may not exist yet.
func mustRead(t *testing.T, path string) []observe.SpanRecord {
	t.Helper()

	recs, err := observe.ReadSpans(path, 100)
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// from is one request at the page, with the password, from a particular
// address — which is the thing the allowlist reads.
func from(t *testing.T, mux *http.ServeMux, path, remote string) int {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.RemoteAddr = remote
	r.SetBasicAuth("rig", password)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w.Code
}

// The list narrows and the password stays. An address that is not on it is
// answered 404 — the same as a page that was never mounted — so a scan learns
// nothing about this server's shape and the password is never compared.
func TestTheAllowListRefusesAnAddressBeforeThePassword(t *testing.T) {
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{
			ServiceName: "todo", Password: password,
			Allow: []string{"10.0.0.0/8", "127.0.0.1"},
		})

	for _, tc := range []struct {
		remote string
		want   int
	}{
		{"10.4.1.9:41234", http.StatusOK},
		{"127.0.0.1:41234", http.StatusOK},
		// A dual-stack listener reports an IPv4 client this way, and an
		// allowlist that refused it would refuse the machine it runs on.
		{"[::ffff:127.0.0.1]:41234", http.StatusOK},
		{"192.0.2.7:41234", http.StatusNotFound},
		{"[2001:db8::1]:41234", http.StatusNotFound},
		// Not an address at all: refused, because a narrowing that opens up
		// when it cannot tell who is calling is not a narrowing.
		{"pipe", http.StatusNotFound},
	} {
		if got := from(t, mux, base+"/", tc.remote); got != tc.want {
			t.Errorf("from %s = %d, want %d", tc.remote, got, tc.want)
		}
	}

	// Refused by address, and the password would have been right. Nothing about
	// the credential is said either way.
	r := httptest.NewRequest(http.MethodGet, base, nil)
	r.RemoteAddr = "192.0.2.7:41234"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if got := w.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("an address off the list was told there is a password here: %q", got)
	}
}

// Empty is every address. The list is a narrowing somebody opts into, not a
// default-deny that would leave the page unreachable for anybody who did not
// know about it.
func TestAnEmptyAllowListAllowsEverything(t *testing.T) {
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password})

	if got := from(t, mux, base+"/", "192.0.2.7:41234"); got != http.StatusOK {
		t.Errorf("with no list, an outside address = %d, want 200", got)
	}
}

// Both routes are behind it, not only the page. traces.json is where the data
// actually is.
func TestTheAllowListCoversTheJSON(t *testing.T) {
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo"}),
		observe.PageConfig{ServiceName: "todo", Password: password, Allow: []string{"10.0.0.0/8"}})

	if got := from(t, mux, base+"/traces.json", "192.0.2.7:41234"); got != http.StatusNotFound {
		t.Errorf("traces.json from an outside address = %d, want 404", got)
	}
	if got := from(t, mux, base+"/traces.json", "10.0.0.1:41234"); got != http.StatusOK {
		t.Errorf("traces.json from an allowed address = %d, want 200", got)
	}
}

// An entry that is not an address is a configuration that is wrong, and wrong
// is loud. On this list a typo means a page nobody can reach.
func TestABadAllowEntryIsAnError(t *testing.T) {
	p := setup(t, observe.Config{ServiceName: "todo"})

	for _, entry := range []string{"not-an-address", "10.0.0.0/64", "10.0.0.0/", ""} {
		if _, err := p.Page(observe.PageConfig{Password: password, Allow: []string{entry}}); err == nil {
			t.Errorf("allow entry %q was accepted", entry)
		}
	}
}

// A filter that matched nothing and a server that served nothing are different
// answers. Saying the second when the first is true contradicts the list
// somebody was looking at a keystroke earlier.
func TestASearchThatMatchesNothingSaysWhichEmptyItIs(t *testing.T) {
	reason := func(mux *http.ServeMux, base, query string) string {
		t.Helper()
		var body struct {
			Reason string `json:"reason"`
		}
		res := get(t, mux, base+"/traces.json"+query, true)
		if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Reason
	}

	served := spanFile(t)
	write(t, served, at("aaa", "1", "", "GET /api/v1/todos", noon, 12*time.Millisecond))
	mux, base := mount(t, setup(t, observe.Config{ServiceName: "todo", File: served}),
		observe.PageConfig{ServiceName: "todo", Password: password})

	if got := reason(mux, base, ""); got != "" {
		t.Errorf("a page with a request on it says %q", got)
	}
	for _, query := range []string{"?q=nothing-in-this-file", "?status=error"} {
		if got := reason(mux, base, query); !strings.Contains(got, "filter") {
			t.Errorf("%s found nothing and said %q, on a server that has served a request", query, got)
		}
	}

	// And the other empty: a file with nothing in it yet, which is what the
	// first minute of every deployment looks like.
	quiet := spanFile(t)
	write(t, quiet)
	mux, base = mount(t, setup(t, observe.Config{ServiceName: "todo", File: quiet}),
		observe.PageConfig{ServiceName: "todo", Password: password})

	if got := reason(mux, base, ""); !strings.Contains(got, "Nothing yet") {
		t.Errorf("a server that has served nothing says %q", got)
	}
}

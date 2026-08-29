package electric_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/electric"
)

// newProxy is [electric.New] with every value it refuses to invent filled in,
// leaving anything the case set alone.
//
// electric has no defaults: a Config that says nothing about how long a first
// read waits, or how large a snapshot may be, is refused rather than given a
// number nobody chose. In a test that lands as five fields per case that are not
// what the case is about, so they are written once, here, from the constants the
// package documents as the answer to write.
func newProxy(cfg electric.Config) (*electric.Proxy, error) {
	if cfg.InitialTimeout == 0 {
		cfg.InitialTimeout = electric.DefaultInitialTimeout
	}
	if cfg.MaxSnapshotRows == 0 {
		cfg.MaxSnapshotRows = electric.DefaultMaxSnapshotRows
	}
	if cfg.SnapshotTimeout == 0 {
		cfg.SnapshotTimeout = electric.DefaultSnapshotTimeout
	}
	if cfg.BreakerThreshold == 0 {
		cfg.BreakerThreshold = electric.DefaultBreakerThreshold
	}
	if cfg.BreakerCooldown == 0 {
		cfg.BreakerCooldown = electric.DefaultBreakerCooldown
	}
	return electric.New(cfg)
}

func TestWhereBindsEveryValue(t *testing.T) {
	t.Parallel()

	var w electric.Where
	w.Eq("tenant_id", "11111111-1111-1111-1111-111111111111").
		IsNull("deleted_at").
		Eq("version_type", "Original").
		Gte("starts_at", "2026-03-01T00:00:00Z")

	const want = `"tenant_id" = $1 AND "deleted_at" IS NULL AND "version_type" = $2 AND "starts_at" >= $3`
	if got := w.SQL(); got != want {
		t.Errorf("sql = %q\nwant %q", got, want)
	}

	params := w.Params()
	if len(params) != 3 {
		t.Fatalf("params = %v", params)
	}
	// Nothing a caller supplied appears in the SQL itself. That is the whole
	// point: the filter is assembled from a tenant identifier and whatever an
	// application adds, and interpolating either makes this an injection point
	// with a streaming response attached.
	for _, p := range params {
		if strings.Contains(w.SQL(), p) {
			t.Errorf("the value %q was interpolated rather than bound", p)
		}
	}
}

// A value that tries to end the string finds itself bound instead.
func TestWhereIsNotFooledByAValue(t *testing.T) {
	t.Parallel()

	var w electric.Where
	w.Eq("tenant_id", "' OR 1=1 --")

	if got := w.SQL(); got != `"tenant_id" = $1` {
		t.Errorf("sql = %q", got)
	}
	if w.Params()[0] != "' OR 1=1 --" {
		t.Error("the value should have reached the parameter list unchanged")
	}
}

func TestWhereIn(t *testing.T) {
	t.Parallel()

	var w electric.Where
	w.In("status", "Planned", "InProgress")

	if got := w.SQL(); got != `"status" IN ($1, $2)` {
		t.Errorf("sql = %q", got)
	}

	// "In nothing" matches nothing. Omitting the condition would widen the
	// shape to everything, which is the opposite of what was asked.
	var empty electric.Where
	empty.In("status")
	if got := empty.SQL(); got != "(false)" {
		t.Errorf("an empty set gave %q", got)
	}
}

func TestWhereQuotesIdentifiers(t *testing.T) {
	t.Parallel()

	var w electric.Where
	w.IsNull("order")

	if got := w.SQL(); got != `"order" IS NULL` {
		t.Errorf("sql = %q; a column named after a keyword needs quoting", got)
	}
}

// upstream stands in for the sync service.
type upstream struct {
	srv    *httptest.Server
	query  url.Values
	header http.Header
	body   string
	status int
	// block holds the response until it is closed, standing in for a long poll.
	block chan struct{}
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()

	u := &upstream{body: `[{"key":"1","value":{"id":"1"}}]`, status: http.StatusOK}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.query = r.URL.Query()
		u.header = r.Header.Clone()

		if u.block != nil {
			select {
			case <-u.block:
			case <-r.Context().Done():
				return
			}
		}

		w.Header().Set("electric-handle", "the-handle")
		w.Header().Set("electric-offset", "0_0")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(u.status)
		_, _ = w.Write([]byte(u.body))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func serve(t *testing.T, p *electric.Proxy, s electric.Shape, rawQuery string) *http.Response {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/shape", func(w http.ResponseWriter, r *http.Request) {
		p.Serve(w, r, s)
	})
	front := httptest.NewServer(mux)
	t.Cleanup(front.Close)

	res, err := http.Get(front.URL + "/shape?" + rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestProxyDecidesTheFilter(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	p, err := newProxy(electric.Config{URL: up.srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	var w electric.Where
	w.Eq("tenant_id", "the-tenant").IsNull("deleted_at")

	res := serve(t, p, electric.Shape{
		Table: "lesson", Where: w.SQL(), Params: w.Params(),
		Columns: []string{"id", "title"},
	}, "")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}

	if got := up.query.Get("table"); got != "lesson" {
		t.Errorf("table = %q", got)
	}
	if got := up.query.Get("where"); got != `"tenant_id" = $1 AND "deleted_at" IS NULL` {
		t.Errorf("where = %q", got)
	}
	if got := up.query.Get("params[1]"); got != "the-tenant" {
		t.Errorf("params[1] = %q", got)
	}
	if got := up.query.Get("columns"); got != "id,title" {
		t.Errorf("columns = %q", got)
	}

	body, _ := io.ReadAll(res.Body)
	if string(body) != up.body {
		t.Errorf("body = %q", body)
	}
	// The cursor headers are how a subscription continues; dropping them ends
	// it after one response.
	if res.Header.Get("electric-handle") != "the-handle" {
		t.Error("the handle should have been passed back")
	}
	if res.Header.Get("electric-offset") != "0_0" {
		t.Error("the offset should have been passed back")
	}
}

// A request that could set `table` could read any table there is.
func TestProxyIgnoresWhatTheClientAsksFor(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	p, _ := newProxy(electric.Config{URL: up.srv.URL})

	var w electric.Where
	w.Eq("tenant_id", "the-tenant")

	serve(t, p, electric.Shape{Table: "lesson", Where: w.SQL(), Params: w.Params()},
		"table=identity_credential&where=true&columns=password_hash&params[1]=other-tenant")

	if got := up.query.Get("table"); got != "lesson" {
		t.Errorf("table = %q; a client chose the table", got)
	}
	if got := up.query.Get("where"); got != `"tenant_id" = $1` {
		t.Errorf("where = %q; a client widened the filter", got)
	}
	if got := up.query.Get("params[1]"); got != "the-tenant" {
		t.Errorf("params[1] = %q; a client rebound the tenant", got)
	}
	if up.query.Has("columns") {
		t.Errorf("columns = %q; a client chose the projection", up.query.Get("columns"))
	}
}

// The cursor genuinely is the client's business: it says where in the stream to
// resume, and none of it can widen what the stream contains.
func TestProxyForwardsTheProtocolParameters(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	p, _ := newProxy(electric.Config{URL: up.srv.URL})

	serve(t, p, electric.Shape{Table: "lesson"},
		"offset=1234_5&handle=abc&live=true&cursor=99&replica=full&nonsense=x")

	for name, want := range map[string]string{
		"offset": "1234_5", "handle": "abc", "live": "true", "cursor": "99", "replica": "full",
	} {
		if got := up.query.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// Anything else is dropped rather than passed along.
	if up.query.Has("nonsense") {
		t.Error("an unrecognized parameter reached the sync service")
	}
}

func TestProxyDoesNotForwardTheCallersCredential(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	p, _ := newProxy(electric.Config{
		URL:     up.srv.URL,
		Headers: http.Header{"Authorization": []string{"Bearer the-sync-credential"}},
		Extra:   url.Values{"source_id": []string{"the-source"}},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/shape", func(w http.ResponseWriter, r *http.Request) {
		p.Serve(w, r, electric.Shape{Table: "lesson"})
	})
	front := httptest.NewServer(mux)
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/shape", nil)
	req.Header.Set("Authorization", "Bearer the-callers-session")
	req.Header.Set("If-None-Match", `"etag"`)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()

	// This server has already decided who the caller is. The sync service's
	// credentials are this server's, not theirs.
	if got := up.header.Get("Authorization"); got != "Bearer the-sync-credential" {
		t.Errorf("authorization = %q", got)
	}
	if got := up.query.Get("source_id"); got != "the-source" {
		t.Errorf("source_id = %q", got)
	}
	// Conditional headers do pass, because 304 is most of what keeps a
	// subscription cheap.
	if got := up.header.Get("If-None-Match"); got != `"etag"` {
		t.Errorf("if-none-match = %q", got)
	}
}

// A client that hangs up mid-poll should take its upstream request with it,
// rather than leaving the sync service holding a connection nobody reads.
func TestClientDisconnectCancelsTheUpstreamPoll(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	up.block = make(chan struct{})

	p, _ := newProxy(electric.Config{URL: up.srv.URL})

	mux := http.NewServeMux()
	served := make(chan struct{})
	mux.HandleFunc("/shape", func(w http.ResponseWriter, r *http.Request) {
		p.Serve(w, r, electric.Shape{Table: "lesson"})
		close(served)
	})
	front := httptest.NewServer(mux)
	defer front.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/shape?live=true", nil)

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("the request should have been cancelled")
	}

	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler is still waiting on an upstream poll nobody is reading")
	}
	close(up.block)
}

func TestUpstreamFailureIsABadGateway(t *testing.T) {
	t.Parallel()

	p, _ := newProxy(electric.Config{URL: "http://127.0.0.1:1"})
	res := serve(t, p, electric.Shape{Table: "lesson"}, "")

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
}

func TestAShapeNeedsATable(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	p, _ := newProxy(electric.Config{URL: up.srv.URL})

	if res := serve(t, p, electric.Shape{}, ""); res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d", res.StatusCode)
	}
}

func TestAURLIsRequired(t *testing.T) {
	t.Parallel()

	if _, err := newProxy(electric.Config{}); err == nil {
		t.Error("a proxy with nowhere to forward to should refuse to exist")
	}
}

// Per-connection headers are the sync service's business with this proxy, not
// this proxy's business with the subscriber, so they stop here. The cursor
// headers do not: dropping electric-handle or electric-offset would end the
// subscription after one response.
//
// TE is the case worth having. Go canonicalizes it to "Te" in the header map, so
// a set keyed by the spelling in the RFC would let it through and nothing else
// would notice.
func TestHopHeadersAreNotForwardedAndTheCursorIs(t *testing.T) {
	t.Parallel()

	hop := []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "TE",
		"Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, h := range hop {
			w.Header().Set(h, "per-connection")
		}
		w.Header().Set("electric-handle", "the-handle")
		w.Header().Set("electric-offset", "0_0")
		w.Write([]byte(`[]`))
	}))
	t.Cleanup(up.Close)

	p, err := newProxy(electric.Config{URL: up.URL})
	if err != nil {
		t.Fatal(err)
	}
	res := serve(t, p, electric.Shape{Table: "lesson"}, "")

	for _, h := range hop {
		if got := res.Header.Get(h); got != "" {
			t.Errorf("%s = %q, want it dropped: it is per-connection", h, got)
		}
	}
	if got := res.Header.Get("electric-handle"); got != "the-handle" {
		t.Errorf("electric-handle = %q, want it forwarded", got)
	}
	if got := res.Header.Get("electric-offset"); got != "0_0" {
		t.Errorf("electric-offset = %q, want it forwarded", got)
	}
}

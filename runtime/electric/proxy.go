// Package electric proxies live-sync shape requests.
//
// A shape is a filtered view of one table that a client subscribes to and keeps
// up to date. The sync service serves it; this package stands in front, because
// the filter is not the client's to choose.
//
// That is the whole design. Everything about which rows exist — the tenant, the
// soft-delete predicate, the snapshot predicate, and whatever the application
// adds — is decided here and sent as a parameterized filter. Everything about
// where the client is in the stream — its offset, its handle, its cursor — is
// forwarded untouched, because that genuinely is the client's business. A
// parameter that is neither is dropped rather than passed along, since a
// request that could set `table` could read any table there is.
package electric

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// protocolParams are the sync protocol's own, forwarded as the client sent
// them. They say where in the stream a request resumes; none of them can widen
// what the stream contains.
var protocolParams = []string{"offset", "handle", "live", "cursor", "replica"}

// Shape is what to subscribe to.
type Shape struct {
	// Table is the table to stream, optionally schema-qualified.
	Table string
	// Where is the filter, with $1-style placeholders bound by Params.
	Where string
	// Params are the placeholder values.
	Params []string
	// Columns narrows the projection. Empty streams every column.
	//
	// It is worth setting: a shape carries every column it names to every
	// subscriber, forever, and a password hash in a live-sync stream is a
	// password hash on somebody's laptop.
	Columns []string
}

// Config builds a proxy.
type Config struct {
	// URL is the sync service, for example http://electric:3000.
	URL string

	// Client makes the upstream request.
	//
	// It must not have a Timeout: a live request is a long poll that
	// deliberately hangs until something changes, and a client timeout would
	// cut every subscription at the same interval. Cancellation comes from the
	// request's context instead, so a client that goes away takes its upstream
	// request with it.
	Client *http.Client

	// Extra are query parameters added to every upstream request, for the
	// credentials a hosted sync service needs.
	Extra url.Values

	// Headers are added to every upstream request, for the same reason.
	Headers http.Header
}

// Proxy forwards shape requests.
type Proxy struct {
	base    *url.URL
	client  *http.Client
	extra   url.Values
	headers http.Header
}

// New builds a proxy.
func New(cfg Config) (*Proxy, error) {
	if cfg.URL == "" {
		return nil, errors.New("electric: a URL is required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("electric: %w", err)
	}

	client := cfg.Client
	if client == nil {
		// No Timeout, on purpose. See Config.Client.
		client = &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: 64,
				// A live poll spends most of its life idle and open.
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 0,
			},
		}
	}
	return &Proxy{base: base, client: client, extra: cfg.Extra, headers: cfg.Headers}, nil
}

// hopHeaders are per-connection and must not be forwarded in either direction.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Transfer-Encoding",
	"TE", "Trailer", "Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
}

// Serve forwards one shape request and streams the answer back.
//
// It writes the response, including on failure. A caller that wants its own
// error rendering should check the shape before calling.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, s Shape) {
	upstream, err := p.request(r, s)
	if err != nil {
		http.Error(w, "cannot reach the sync service", http.StatusBadGateway)
		return
	}

	res, err := p.client.Do(upstream)
	if err != nil {
		// A cancelled request is the client hanging up mid-poll, which is the
		// ordinary end of a subscription rather than a failure to report.
		if r.Context().Err() != nil {
			return
		}
		http.Error(w, "the sync service is unavailable", http.StatusBadGateway)
		return
	}
	defer res.Body.Close()

	// The electric-* headers carry the cursor the client needs to resume, so
	// dropping them would end the subscription after one response.
	out := w.Header()
	for name, values := range res.Header {
		if isHopHeader(name) {
			continue
		}
		for _, v := range values {
			out.Add(name, v)
		}
	}
	w.WriteHeader(res.StatusCode)

	// Flush after the copy rather than during it: a long poll answers all at
	// once, and the flush is what stops a buffering intermediary from sitting
	// on the response until the next one arrives.
	//
	// Through a ResponseController rather than a type assertion for w: a
	// middleware that wraps the writer — the request log's does — is not an
	// http.Flusher itself, and an assertion would quietly find nothing and skip
	// the flush. The controller follows Unwrap to the writer that can.
	_, _ = io.Copy(w, res.Body)
	_ = http.NewResponseController(w).Flush()
}

// request builds the upstream call.
func (p *Proxy) request(r *http.Request, s Shape) (*http.Request, error) {
	if s.Table == "" {
		return nil, errors.New("electric: the shape names no table")
	}

	target := *p.base
	target.Path = strings.TrimRight(target.Path, "/") + "/v1/shape"

	q := url.Values{}
	for name, values := range p.extra {
		for _, v := range values {
			q.Add(name, v)
		}
	}

	// Ours, always. A request that could set these could read any table there
	// is, under any filter it liked.
	q.Set("table", s.Table)
	if s.Where != "" {
		q.Set("where", s.Where)
		for i, v := range s.Params {
			q.Set("params["+strconv.Itoa(i+1)+"]", v)
		}
	}
	if len(s.Columns) > 0 {
		q.Set("columns", strings.Join(s.Columns, ","))
	}

	// Theirs, forwarded. Anything else the client sent is dropped.
	from := r.URL.Query()
	for _, name := range protocolParams {
		if v := from.Get(name); v != "" {
			q.Set(name, v)
		}
	}
	target.RawQuery = q.Encode()

	// The request context, so a client that hangs up cancels the poll it left
	// running upstream.
	upstream, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, err
	}

	// Conditional-request headers pass through so the sync service can answer
	// 304, which is most of what keeps a subscription cheap.
	for _, name := range []string{"If-None-Match", "If-Modified-Since", "Accept-Encoding"} {
		if v := r.Header.Get(name); v != "" {
			upstream.Header.Set(name, v)
		}
	}
	// Deliberately not the client's Authorization header: this server has
	// already decided who the caller is, and the sync service's credentials are
	// this server's, not theirs.
	for name, values := range p.headers {
		for _, v := range values {
			upstream.Header.Add(name, v)
		}
	}
	return upstream, nil
}

func isHopHeader(name string) bool {
	for _, h := range hopHeaders {
		if strings.EqualFold(name, h) {
			return true
		}
	}
	return false
}

// Package rigclient is the half of a rig Go SDK that is not generated.
//
// A generated client is types and one method per endpoint. Everything under
// those methods — building the request, carrying the credential, reading a
// failure back into an error worth switching on, walking a paginated read to its
// end — is the same in every project, because the server it talks to was
// generated too. So it lives here, hand-written, and the generated code is thin
// enough to read.
//
// What the generator supplies is what only the document knows: the base path,
// the shape of each call, and — for a project with authentication — the profile
// in [AuthProfile], whose lifetimes are the ones the server enforces rather than
// numbers a client author guessed.
//
// The three things a caller sets up are in [Config]: where the server is, what
// credential to present, and which [http.Client] to use. Everything else has a
// default that is right until it is not.
package rigclient

import (
	"context"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config is what a caller supplies. Every field but BaseURL has a default.
type Config struct {
	// BaseURL is the origin the API is served from, for example
	// "https://api.example.com". A path on it is kept and the API's own base
	// path is appended, so a server behind "/gateway" is a base URL and not a
	// special case.
	BaseURL string

	// HTTPClient is the transport. The default has a thirty-second timeout,
	// which is a limit on the whole exchange rather than on a stalled read —
	// use a context for anything finer.
	//
	// Thirty seconds is right for a JSON call and wrong for a file, and a
	// context cannot raise it. A client that uploads or downloads at size wants
	// [WithTimeout] on those calls rather than a longer ceiling on every one.
	HTTPClient *http.Client

	// Credential is what authorizes each request: [StaticToken], [APIKey], or a
	// [Session] that keeps itself fresh. Nil sends no Authorization header,
	// which is what a public API or a header-authenticated demo wants.
	Credential Credential

	// Header is sent on every request. It is where a tenant header, a trace
	// header, or anything else a deployment adds belongs.
	Header http.Header

	// UserAgent identifies the caller in the server's logs. An SDK that does not
	// say who it is turns "which integration is hammering us" into a guess.
	UserAgent string

	// RequestID supplies the value of the request-ID header, so a client-side
	// log line and a server-side one can be joined. Nil sends none, and the
	// server generates its own.
	RequestID func() string

	// RequestIDHeader defaults to X-Request-Id, which is what the generated
	// server reads.
	RequestIDHeader string

	// Revision overrides the API revision this client says it was built against.
	//
	// Almost nobody should set it. The generated client carries the revision
	// from the document it was generated from, which is the honest answer and
	// the one the server's logs are for; this is here for a client somebody
	// assembled by hand.
	Revision string

	// RevisionHeader overrides where the revision is sent. Empty takes the
	// generated client's, which is what the server it was generated with reads.
	RevisionHeader string

	// Now is the clock, for a test that has to cross a token expiry without
	// waiting ten minutes for one. Nil is [time.Now].
	Now func() time.Time

	// Trace runs one piece of work inside a span, and is how a caller that
	// traces gets this client's calls into the same trace as everything else it
	// does:
	//
	//	Trace: observe.Call,
	//
	// A function rather than a tracer, because this module is imported by every
	// generated client and most of them do not want an OpenTelemetry dependency
	// — the same argument that keeps otel out of rig/runtime. Anything with
	// this shape works; rig/observe supplies one.
	//
	// Two spans per call, and the nesting is the point. The outer one is the
	// operation, named by [Op.Name]; the inner ones are the attempts, and one
	// call can be several — the QUERY a proxy refused, the POST to the alias,
	// the send after a credential refreshed, and every [Retry]. Tracing the
	// attempts alone would show one call as a handful of unrelated requests.
	//
	// Nil traces nothing, which is what a client that was never told about
	// tracing does.
	Trace func(ctx context.Context, name string, f func(context.Context) error) error

	// Retry is how a call the server could not answer is sent again. The zero
	// value sends a call up to four times with a jittered backoff — a read and a
	// delete because they are repeatable by nature, a write because the SDK names
	// it with an Idempotency-Key first. A form body is the exception and is never
	// repeated. See [Retry].
	Retry Retry
}

// API is what the generated client knows about the server and this package does
// not. Generated code fills it in; a caller never sees it.
type API struct {
	// BasePath is the prefix every route sits under, for example "/api/v1".
	BasePath string
	// Auth is the authentication profile, or nil for a project with none.
	Auth *AuthProfile
	// Revision is the date the API surface this client was generated from last
	// changed. It is sent on every request, which is how a server's logs can
	// answer how old the oldest caller still calling is.
	Revision string
	// RevisionHeader is where Revision is sent — the same header the server it
	// was generated with reads.
	RevisionHeader string
}

// Runtime carries one client's configuration and the state it accumulates.
//
// One per client. The remembered QUERY decision below is the reason it is state
// rather than configuration: a client that learned an intermediary refuses QUERY
// should not have to learn it again on the next search.
type Runtime struct {
	base            *url.URL
	http            *http.Client
	api             API
	header          http.Header
	userAgent       string
	requestID       func() string
	requestIDHeader string
	revision        string
	revisionHeader  string
	now             func() time.Time
	trace           func(ctx context.Context, name string, f func(context.Context) error) error
	retry           Retry
	// jitter is the randomness in a backoff, held here so a test can make one
	// deterministic. It answers in [0, n).
	jitter func(int64) int64

	mu sync.Mutex
	// cred is under the mutex because signing in replaces it while requests may
	// be in flight.
	cred Credential
	// searchByPost records that QUERY was refused once and is not worth trying
	// again. See [Op.Fallback].
	searchByPost bool

	// refreshing is held for the length of a token exchange, so that a hundred
	// goroutines discovering the same expiry produce one refresh. It is separate
	// from mu because a refresh is a whole HTTP request and everything else here
	// is a field read.
	refreshing sync.Mutex
}

// profile is the authentication profile, or the zero value for a project with
// none. [Session] reads it for the lifetimes.
func (rt *Runtime) profile() AuthProfile {
	if rt.api.Auth == nil {
		return AuthProfile{}
	}
	return *rt.api.Auth
}

// DefaultTimeout bounds a whole request when the caller supplied no client of
// their own.
const DefaultTimeout = 30 * time.Second

// DefaultUserAgent is what a client that did not name itself sends.
const DefaultUserAgent = "rig-go-client"

// DefaultRequestIDHeader is where the generated server looks for a caller's own
// request identifier.
const DefaultRequestIDHeader = "X-Request-Id"

// DefaultRevisionHeader carries the API revision, and is what rig.yaml's
// api.revision_header defaults to. A generated client passes its project's own,
// so this is the fallback for a client somebody built by hand.
const DefaultRevisionHeader = "API-Revision"

// New builds a runtime. Generated code calls it; a caller reaches it through the
// generated New, which supplies the api argument from the document.
func New(cfg Config, api API) (*Runtime, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("rigclient: a BaseURL is required")
	}
	base, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, err
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, errors.New("rigclient: BaseURL needs a scheme and a host, for example " +
			"https://api.example.com")
	}

	rt := &Runtime{
		base:      base,
		http:      cfg.HTTPClient,
		api:       api,
		header:    cfg.Header.Clone(),
		userAgent: cfg.UserAgent,
		requestID: cfg.RequestID,
		revision:  cfg.Revision,
		now:       cfg.Now,
		trace:     cfg.Trace,
		cred:      cfg.Credential,
		retry:     cfg.Retry,
		// math/rand/v2 rather than math/rand: its source is per-P and lock-free
		// where the older one is a global mutex, and a client called from a
		// hundred goroutines that are all backing off at once is exactly the
		// moment that contention would show up.
		jitter: rand.Int64N,
	}
	if rt.http == nil {
		rt.http = &http.Client{Timeout: DefaultTimeout}
	}
	if rt.header == nil {
		rt.header = http.Header{}
	}
	if rt.userAgent == "" {
		rt.userAgent = DefaultUserAgent
	}
	if rt.now == nil {
		rt.now = time.Now
	}
	if cfg.RequestIDHeader != "" {
		rt.requestIDHeader = cfg.RequestIDHeader
	} else {
		rt.requestIDHeader = DefaultRequestIDHeader
	}

	// The document's answer unless the caller insisted on their own. A generated
	// client always has one; a hand-built one may have neither, and then it
	// simply does not say what it was built against, which is a normal thing for
	// a caller to be.
	if rt.revision == "" {
		rt.revision = api.Revision
	}
	switch {
	case cfg.RevisionHeader != "":
		rt.revisionHeader = cfg.RevisionHeader
	case api.RevisionHeader != "":
		rt.revisionHeader = api.RevisionHeader
	default:
		rt.revisionHeader = DefaultRevisionHeader
	}

	if s, ok := rt.cred.(*Session); ok {
		s.bind(rt)
	}
	return rt, nil
}

// Use installs a credential, replacing whatever was there.
//
// It is what signing in does, and what a caller does by hand when they already
// hold a token from somewhere else.
func (rt *Runtime) Use(c Credential) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	rt.cred = c
	if s, ok := c.(*Session); ok {
		s.bind(rt)
	}
}

// Credential returns the credential in force, or nil.
func (rt *Runtime) Credential() Credential {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	return rt.cred
}

// Session returns the installed session, and whether the credential is one.
func (rt *Runtime) Session() (*Session, bool) {
	s, ok := rt.Credential().(*Session)
	return s, ok
}

// Auth returns the authentication endpoints, or nil for a project that mounts
// none. Generated code exposes it as Client.Auth.
func (rt *Runtime) Auth() *Auth {
	if rt.api.Auth == nil {
		return nil
	}
	return &Auth{rt: rt, profile: *rt.api.Auth}
}

// Now is the clock this client reads. A test can move it with [Config.Now].
func (rt *Runtime) Now() time.Time { return rt.now() }

// BaseURL is the origin requests go to.
func (rt *Runtime) BaseURL() string { return rt.base.String() }

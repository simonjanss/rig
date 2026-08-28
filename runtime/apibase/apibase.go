// Package apibase is the half of a generated API layer that is the same in
// every project.
//
// A generated `api` package used to carry all of this: the request envelope,
// the request context, the front half of every handler, the error funnel, the
// log lines and the decoders. None of it is derived from anybody's schema —
// what varies is one field on a struct and three lines behind a configuration
// block — so it was the same eight hundred lines copied into every repository
// that ever ran `rig generate`.
//
// It is here so that it can be imported rather than copied, and so that rig's
// own modules can mount routes on the same terms: a shipped handler in
// rig/notify needs Prepare, Fail and RequestContext exactly as much as a
// generated one does, and there was nowhere for it to get them.
//
// Nothing here knows about tracing, files, notifications or authentication.
// Each of those reaches this package as a field on [Server] whose type is
// declared here and satisfied elsewhere — the trick [Authenticator] has always
// used — which is what keeps a project that asked for none of them from naming
// any of them in its go.mod.
package apibase

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/runtime/apirev"
	"github.com/simonjanss/rig/runtime/clientip"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/httpx"
	"github.com/simonjanss/rig/runtime/idempotency"
	"github.com/simonjanss/rig/runtime/readopt"
	"github.com/simonjanss/rig/runtime/reqlog"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/serve"
	"github.com/simonjanss/rig/runtime/tenancy"
	"github.com/simonjanss/rig/runtime/throttle"
)

// DefaultRevisionHeader carries the API revision in both directions: what the
// caller was built against on the way in, what this server was generated from
// on the way out. It is what `api.revision_header` defaults to.
const DefaultRevisionHeader = "API-Revision"

// Tracer is where this API's spans come from.
//
// It is declared here rather than imported, and that is the whole trick:
// [github.com/simonjanss/rig/observe.APITracer] satisfies it without either
// side knowing the other exists. A field typed as the observe package's own
// would put OpenTelemetry in the go.mod of every project that never set
// `tracing:`.
//
// Every method takes and returns nothing but standard-library types, and that
// is not incidental: two interfaces are interchangeable only when their method
// signatures are identical, so a named type from either side would make these
// two different interfaces that merely look alike.
type Tracer interface {
	// Server opens the span for one request and returns the request carrying it,
	// along with the function that ends it.
	//
	// The route is the matched pattern rather than the path, so a trace has one
	// span name per endpoint instead of one per identifier anybody ever fetched.
	// status is how the span learns what was answered.
	Server(r *http.Request, route string, status func() int) (*http.Request, func())

	// TraceID names the trace this request belongs to, or empty when there is
	// none. It is what a request that sent no identifier of its own is labelled
	// with, so that the one in an error body and the trace in a collector are one
	// string.
	TraceID(r *http.Request) string

	// Fail records why a request is being refused, on whatever span the context
	// is in. It ends nothing: the span belongs to the handler.
	Fail(ctx context.Context, status int, err error)
}

// Request is everything a handler received.
//
// Each part is a separate type parameter, and an operation that has no path
// parameters uses struct{} for that slot. That is deliberate: the signature
// says what an endpoint takes, so reaching for something it does not have is a
// compile error rather than a nil check.
type Request[Path, Query, Body any] struct {
	// Claims describe the caller. They are always present: a handler does
	// not run without them.
	Claims tenancy.Claims

	Path  Path
	Query Query
	Body  Body

	ctx RequestContext
}

// Context returns what is known about the request itself, as opposed to what
// it carries.
func (r Request[Path, Query, Body]) Context() RequestContext { return r.ctx }

// BuiltBefore reports whether the client that sent this request was built
// before rev.
//
// It is [RequestContext.BuiltBefore] with the request already in hand, which
// is where a compatibility shim usually is:
//
//	var notesAdded = apirev.MustParse("2026-04-30")
//
//	if r.BuiltBefore(notesAdded) {
//		r.Body.Title = "Unknown"
//	}
//
// A shim belongs here rather than in a hook. This is the one place that has
// the request as it arrived, and on a create the generated validation runs
// after the service method and before every hook — so a hook that filled in
// a missing NOT NULL column would only ever see requests that already passed
// the check it was written for.
func (r Request[Path, Query, Body]) BuiltBefore(rev apirev.Revision) bool {
	return r.ctx.BuiltBefore(rev)
}

// RequestContext is the request's own metadata.
type RequestContext struct {
	// RequestID correlates this request with the server's logs.
	RequestID string
	Method    string
	Path      string
	// Route is the pattern that matched, so a metric can be labelled by
	// endpoint rather than by every distinct identifier.
	Route      string
	RemoteAddr string
	UserAgent  string
	// ClientRevision is the raw header, kept for a log line that wants to
	// count what callers are sending. Empty when the caller said nothing, and
	// unparseable prose when it said something that is not a revision — a
	// hand-rolled client and a curl will not say, and that is a normal thing
	// for a caller to be. Compare with [RequestContext.BuiltBefore] rather
	// than with this.
	ClientRevision string

	// ServerRevision is what this server was generated from, so that
	// [RequestContext.Stale] has both sides of the comparison in hand.
	//
	// A field rather than a package-level value, because this package serves
	// every project rather than one. [RequestContextOf] fills it from
	// [Server.Revision] on every request; a context built by hand in a test has
	// it unset, and an unset one is unknown — so Stale answers false rather than
	// inventing a distance.
	ServerRevision apirev.Revision
}

// LogValue is how a request appears in a log line.
//
// A group, so one attribute carries the lot:
//
//	logger.ErrorContext(ctx, "that failed", slog.Any("request", rc))
//
// Only the fields that have something in them. An empty attribute on every
// line is the same width in a terminal and a key in a structured backend, and
// it says a field was collected when it was not — a project that set no
// RequestID gets lines with no request_id rather than lines with an empty one.
func (rc RequestContext) LogValue() slog.Value {
	attrs := make([]slog.Attr, 0, 6)
	if rc.RequestID != "" {
		attrs = append(attrs, slog.String("request_id", rc.RequestID))
	}
	if rc.Method != "" {
		attrs = append(attrs, slog.String("method", rc.Method))
	}
	if rc.Route != "" {
		attrs = append(attrs, slog.String("route", rc.Route))
	}
	if rc.Path != "" {
		attrs = append(attrs, slog.String("path", rc.Path))
	}
	if rc.RemoteAddr != "" {
		attrs = append(attrs, slog.String("remote_addr", rc.RemoteAddr))
	}
	if rc.UserAgent != "" {
		attrs = append(attrs, slog.String("user_agent", rc.UserAgent))
	}
	return slog.GroupValue(attrs...)
}

// Client is what the caller was built against.
//
// Unknown — the zero [apirev.Revision] — when the caller said nothing and
// equally when what it said is not a revision. The two are the same answer on
// purpose: both mean this caller cannot be placed on the timeline.
func (rc RequestContext) Client() apirev.Revision {
	rev, _ := apirev.Parse(rc.ClientRevision)
	return rev
}

// BuiltBefore reports whether the caller was built before rev, which is the
// question a compatibility shim is written in terms of.
//
// False for a caller that sent no revision. That is a decision rather than a
// fallback: revisions describe what rig's own generated clients were built
// against, so a caller rig cannot place is served the current behavior. An
// application that would rather treat an unknown caller as ancient has
// [RequestContext.ClientRevision] and can say so itself.
func (rc RequestContext) BuiltBefore(rev apirev.Revision) bool { return rc.Client().Before(rev) }

// Stale reports how far behind this server's revision the caller is.
//
// ok is false when the caller did not say, said something unparseable, or is
// not behind at all — including the case of a caller newer than this server,
// which is somebody halfway through a deploy rather than somebody to warn
// about.
func (rc RequestContext) Stale() (time.Duration, bool) {
	client := rc.Client()
	if !client.Before(rc.ServerRevision) {
		return 0, false
	}
	return rc.ServerRevision.Sub(client), true
}

// NewRequest builds a request. The server calls it; it is exported so a test
// can call a service method directly without going through HTTP.
func NewRequest[Path, Query, Body any](claims tenancy.Claims, path Path, query Query, body Body, rc RequestContext) Request[Path, Query, Body] {
	return Request[Path, Query, Body]{Claims: claims, Path: path, Query: query, Body: body, ctx: rc}
}

type requestContextKey struct{}

// NewContext returns a context carrying the request's metadata.
//
// The server calls it on every request, before the [Server.Context] hook runs,
// so anything that hook adds can already see it. It is exported for the same
// reason [NewRequest] is: a test that calls a service method directly still
// has to put one there, or the hooks underneath will find nothing.
func NewContext(ctx context.Context, rc RequestContext) context.Context {
	return context.WithValue(ctx, requestContextKey{}, rc)
}

// RequestContextFrom returns the request metadata on a context.
//
// This is how a validator or a hook reaches what only the service method is
// handed — the revision the caller was built against, the request
// identifier, the route that matched.
//
// ok is false rather than an error, and the zero value is usable: work that
// did not come from a request at all — a migration, a background job — has
// no metadata to find, and that is not a failure.
func RequestContextFrom(ctx context.Context) (RequestContext, bool) {
	rc, ok := ctx.Value(requestContextKey{}).(RequestContext)
	return rc, ok
}

// Caller is the claims a read hook is handed, or nil when the request carried
// none.
//
// Only a public endpoint can reach a hook with none: everything else is
// refused before a handler runs, and every write is refused again by the
// repository. That is why the write hooks take a value and these take a
// pointer.
func Caller(claims tenancy.Claims) *tenancy.Claims {
	if !claims.Valid() {
		return nil
	}
	return &claims
}

// ReadScope turns a requested scope into the read options that produce it.
//
// Only "all" does anything. Every other value — including a zero value from
// a caller that never set the field — leaves the narrow default in place, so
// the failure mode of forgetting this is too few rows rather than too many.
func ReadScope(s tenancy.Scope) []readopt.Option {
	if s == tenancy.ScopeAll {
		return []readopt.Option{readopt.WithoutOwnerScope()}
	}
	return nil
}

// RequestIDHeader is where a caller may name its own request, so that its logs
// and this API's can be lined up afterwards.
//
// It is read on every route, including the authentication ones. What is done
// with it is [Server.RequestID]'s documentation.
const RequestIDHeader = "X-Request-Id"

// maxRequestIDBytes bounds what a caller may name its own request.
//
// The value reaches an error body and every log line the request writes, so it
// is client-controlled text in two places that are read by machines. A bound
// and a character class are what keep a header from being a way to write
// whatever it likes into a log file. The number is generous: every identifier
// anybody actually uses — a UUID, a trace id — is well under it.
const maxRequestIDBytes = 128

// CallerRequestID is the identifier the caller asked this request to be known
// by, or empty when it did not ask or asked for something not worth repeating.
//
// Refusing rather than truncating or escaping, because a header this API does
// not understand is not one it should half-quote into a log line. What a
// Caller gets for sending nonsense is the identifier it would have got for
// sending nothing.
func CallerRequestID(r *http.Request) string {
	id := r.Header.Get(RequestIDHeader)
	if id == "" || len(id) > maxRequestIDBytes {
		return ""
	}
	// Printable ASCII, which is what every identifier format in use is and what a
	// log line can carry without an escape.
	for i := 0; i < len(id); i++ {
		if id[i] < 0x20 || id[i] > 0x7e {
			return ""
		}
	}
	return id
}

// Authenticator identifies a caller and serves its own routes.
//
// It is declared here rather than imported, and that is the whole trick:
// [github.com/simonjanss/rig/auth.Auth] satisfies it without either side
// knowing the other exists. A field typed as the auth package's own would make
// every project depend on it — argon2, OAuth and all — including the ones
// with no authentication at all.
//
// Anything else with these two methods works just as well: a wrapper that adds
// a header check, a stub in a test, an entirely different implementation.
type Authenticator interface {
	// Claims identifies the caller behind a request.
	Claims(*http.Request) (tenancy.Claims, error)
	// Mount registers the authentication routes — signing in, refreshing, keys
	// — on the same mux the resource routes are on.
	Mount(*http.ServeMux)
}

// Server is the behavior every handler shares.
type Server struct {
	// Auth wires the whole authentication foundation in one field.
	//
	// Setting it does two things that otherwise have to be remembered separately:
	// GetClaims comes from it, and its routes are mounted on the mux Register
	// returns. Wiring the claims from one thing and mounting another is a mistake
	// this makes unavailable.
	//
	// Nil for a project with no authentication, which then has to supply GetClaims
	// itself.
	Auth Authenticator

	// GetClaims establishes who is calling. A handler does not run without it, so
	// a route cannot accidentally be left unauthenticated.
	//
	// Auth supplies it. Set this instead for a project that authenticates its own
	// way, or has no auth routes to mount.
	GetClaims func(*http.Request) (tenancy.Claims, error)

	// RequestID labels a request for the logs and for the error body it may end
	// in.
	//
	// Nil is the ordinary case and does not mean nothing: it means
	// [CallerRequestID] — the caller's own X-Request-Id, if it sent one worth
	// trusting, and this request's trace otherwise. Set this only to answer the
	// question differently; the default is already the answer every route in this
	// package gives, including the authentication ones.
	RequestID func(*http.Request) string

	// Logger is where this server says what it did and why a request failed. Nil
	// uses [log/slog.Default].
	//
	// Nil means the default logger and not silence, which is the whole point: the
	// one line worth having is the cause of a 500, and a server that drops it
	// because nobody configured logging is a server whose error bodies say
	// "something went wrong" and mean it literally. Set this to route the lines
	// somewhere, not to turn them on.
	//
	// What comes out: an error line per failed request carrying the whole error,
	// and, at [log/slog.LevelDebug], a line per request with its status and size.
	//
	// There is no logger on the context and no way to reach this one from a
	// service. A service is constructed by the application, so it is handed a
	// logger there — the same one this field gets — and puts the request on
	// its own lines with [RequestContextFrom].
	Logger *slog.Logger

	// MinRevision refuses a caller built against an API revision older than this
	// one. The zero value, the default, refuses nothing.
	//
	//	MinRevision: apirev.MustParse("2026-04-30"),
	//
	// This is the end of the story [Revision] starts: you removed a field, you
	// waited, the logs said nobody old was left, and now you close the door. Until
	// somebody decides to, the revision is telemetry and nothing else.
	//
	// Only a caller that sends a revision older than this is refused. One that
	// sends none is served — an unknown client is not the same as an old one,
	// and turning every curl and every hand-written integration into a 426 is not
	// a door closing, it is an outage.
	MinRevision apirev.Revision

	// OnError turns a service error into a response. Nil uses DefaultErrorMapper,
	// which is the right behavior for almost everyone.
	OnError func(w http.ResponseWriter, r *http.Request, rc RequestContext, err error)

	// PreHooks run before anything else, in order. A hook that writes a response
	// stops the request.
	PreHooks []func(w http.ResponseWriter, r *http.Request) bool

	// Context lets a hook attach values a service will read.
	Context func(ctx context.Context, r *http.Request) context.Context

	// DB is what a write carrying an Idempotency-Key is recorded in, so that a
	// client which had to send the same write twice gets one row and the same
	// answer both times. Pass the pool: `DB: app.Pool`.
	//
	// Required, and Register panics without it. A nil one would mean the header
	// was quietly ignored, and a client that thinks its retry is safe when it is
	// not is worse off than one that knows it is not — this is the one failure
	// mode worth a startup panic rather than a runtime surprise.
	//
	// It costs nothing until a caller sends the header: a write without one takes
	// the path it always took, with no transaction and no extra round trip. See
	// [github.com/simonjanss/rig/runtime/idempotency].
	DB dbx.Beginner

	// Revision is what this server's API surface was generated from, announced
	// on the way out of every response and compared against MinRevision on the
	// way in. The generated Register fills it in; the zero value is a project
	// that has never generated with a revision recorded, and it announces
	// nothing rather than announcing a wrong answer.
	Revision apirev.Revision

	// RevisionHeader is the header the revision travels in, both ways. Empty
	// means [DefaultRevisionHeader], which is what `api.revision_header` leaves
	// it at.
	RevisionHeader string

	// Tracer is where this API's spans come from. Nil is a project that does not
	// trace, and then none of its methods is called.
	Tracer Tracer

	// Throttle is the rate limiter, or nil for a project with no `throttle:`
	// block. A nil Gate allows, so this costs one nil check rather than a
	// limiter nobody configured.
	Throttle *throttle.Gate

	// TrustedProxies are the networks whose X-Forwarded-For this server
	// believes, for the one case Throttle has to key on an address: an
	// unidentified caller. Empty trusts nothing, which is the safe default — an
	// address read from a header the client controls is an address the client
	// chooses, and a limit keyed on one of those is decorative.
	TrustedProxies []netip.Prefix
}

// IdempotencyPruner deletes the records of writes past their retention, and so
// decides how long after a write its Idempotency-Key still replays.
//
// Zero takes idempotency.DefaultRetention, a day: long enough to cover any
// retry, short enough that a key reused a week later is a new request rather
// than a write that silently does nothing.
//
// A task rather than a goroutine, for the reason FileSweeper is one — a cron
// job is one thing running, and a goroutine in every replica is as many as
// there are replicas, all racing to delete the same rows. Register it in
// serve.Config.Tasks and run `<binary> prune-idempotency`:
//
//	Tasks: map[string]serve.Task{"prune-idempotency": api.IdempotencyPruner(0)},
//
// Nothing schedules it for you. Without it rig_idempotency keeps every record
// ever written, and the retention above is a sentence rather than a behaviour.
func IdempotencyPruner(retention time.Duration) serve.Task {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := idempotency.Prune(ctx, pool, retention)
		return err
	}
}

// MaxBodyBytes bounds a request body. Without a limit, one client can exhaust
// the server's memory by streaming forever.
const MaxBodyBytes = 1 << 20

// Prepare runs the shared front half of every handler: the hooks, the claims,
// and the request metadata.
func Prepare(s Server, w http.ResponseWriter, r *http.Request) (context.Context, tenancy.Claims, RequestContext, bool) {
	return Resolve(s, w, r, true)
}

// RequestContextOf is what one request looks like to an error body and to a log
// line.
//
// A function rather than a literal per call site, because a route that builds
// a blank one still answers correctly and still logs a failure — it just
// logs one that names no method, no path and no request identifier, which is
// the one thing the line exists for.
func RequestContextOf(s Server, r *http.Request) RequestContext {
	rc := RequestContext{
		Method:         r.Method,
		Path:           r.URL.Path,
		Route:          r.Pattern,
		RemoteAddr:     r.RemoteAddr,
		UserAgent:      r.UserAgent(),
		ClientRevision: r.Header.Get(s.revisionHeader()),
		ServerRevision: s.Revision,
	}
	if s.RequestID != nil {
		rc.RequestID = s.RequestID(r)
	} else {
		// The caller's own first, so a client correlating its side with this one is
		// believed. Failing that, the trace, when this project traces: the request
		// already has an identifier then, and inventing a second one would be
		// inventing a second answer to the same question. The requestId in the error
		// body, the request_id on every log line and the trace in a collector are one
		// string, and nobody had to wire it up.
		//
		// Empty when neither is there, which is a request nothing can correlate —
		// turning `tracing:` on in rig.yaml is what gives every request one whether
		// or not the caller thought to.
		rc.RequestID = CallerRequestID(r)
		if rc.RequestID == "" && s.Tracer != nil {
			rc.RequestID = s.Tracer.TraceID(r)
		}
	}
	return rc
}

// Resolve is the body of both, differing only in whether a caller who cannot
// be identified is refused.
func Resolve(s Server, w http.ResponseWriter, r *http.Request, required bool) (context.Context, tenancy.Claims, RequestContext, bool) {
	rc := RequestContextOf(s, r)

	// Announced on the way out of every response, including the failed ones: a
	// client that is behind should not have to make a successful request to find
	// out.
	if v := s.Revision.String(); v != "" {
		w.Header().Set(s.revisionHeader(), v)
	}

	for _, hook := range s.PreHooks {
		if !hook(w, r) {
			return nil, tenancy.Claims{}, rc, false
		}
	}

	// Before the claims, because being too old to be served is not a question
	// about who you are.
	if !serveRevision(s, w, r, rc) {
		return nil, tenancy.Claims{}, rc, false
	}

	claims, err := s.GetClaims(r)
	if err != nil {
		if required {
			Fail(s, w, r, rc, err)
			return nil, tenancy.Claims{}, rc, false
		}
		// Public, so the caller is nobody rather than nobody yet. The claims go on the
		// context all the same: what needs a tenant refuses there, where the reason is
		// about the thing being asked for.
		claims = tenancy.Claims{}
	}

	// After the claims, because a limit is per caller and the caller is who the
	// credential says. A nil Gate allows, so a project with no `throttle:` block
	// pays one nil check rather than carrying a limiter it never configured.
	if err := s.Throttle.Check(r.Context(), throttleCaller(r, claims, s.TrustedProxies), r.Pattern, w.Header()); err != nil {
		Fail(s, w, r, rc, err)
		return nil, tenancy.Claims{}, rc, false
	}

	ctx := tenancy.NewContext(r.Context(), claims)
	// So that what only the service method is handed reaches everything under it:
	// a validator and a hook are given a context and nothing else, and the
	// revision the caller was built against is exactly the kind of thing they have
	// to ask about. Before the Context hook, so that hook can see it too.
	ctx = NewContext(ctx, rc)
	if s.Context != nil {
		ctx = s.Context(ctx, r)
	}
	return ctx, claims, rc, true
}

// serveRevision reports whether this caller is new enough to be served, and
// writes the refusal when it is not.
//
// Both ways of not refusing are the one comparison: an unset MinRevision and a
// Caller that sent no revision each leave one side unknown, and nothing is
// before an unknown revision. A caller that cannot be shown to be old is
// served.
func serveRevision(s Server, w http.ResponseWriter, r *http.Request, rc RequestContext) bool {
	if !rc.BuiltBefore(s.MinRevision) {
		return true
	}

	Fail(s, w, r, rc, rigerr.UpgradeRequired(
		"this client was built against API revision %s; this server serves %s and newer",
		rc.ClientRevision, s.MinRevision))
	return false
}

// Fail writes an error response.
//
// OnError is the answer when it is set, and a generated Register sets it to
// that project's own error mapper — which is where `api.json_case` is honoured,
// and the reason this package cannot write that body itself. What is left here
// is the answer rig's own routes give: the same envelope, camelCase, carrying
// the request identifier so a failure from a shipped route lands in the log
// beside every other route's.
func Fail(s Server, w http.ResponseWriter, r *http.Request, rc RequestContext, err error) {
	// Before the mapper and not inside it. A project that set OnError replaced how
	// a failure is *answered*; it did not ask to stop being told what the failure
	// was.
	LogFailure(s, r, rc, err)

	if s.OnError != nil {
		s.OnError(w, r, rc, err)
		return
	}
	httpx.WriteError(w, rc.RequestID, err)
}

// LogFailure records why this request is about to fail.
//
// It is the only place that ever sees the whole error. DefaultErrorMapper
// rewrites an internal message to "something went wrong" before it reaches the
// client — deliberately, because the original is exactly the kind of thing
// that leaks a table name or a connection string — so without this line the
// cause of a 500 exists nowhere. The request identifier goes out in the body
// and comes out here, and that pair is the whole mechanism for answering "what
// happened to my request".
//
// Two levels, because they are two different events. An internal failure is
// the server's fault and is an error. A 404, a 422, a refused permission —
// the server worked, and logging those at anything but debug is how a log
// becomes a thing nobody reads.
func LogFailure(s Server, r *http.Request, rc RequestContext, err error) {
	code := rigerr.CodeOf(err)
	// On the span the handler opened, which this does not end: the span belongs to
	// the handler and is closed by its defer. Only an internal failure makes the
	// span itself red — the same distinction the two log levels below draw, and
	// for the same reason. Nil when this project does not trace, and then there is
	// no span to redden.
	if s.Tracer != nil {
		s.Tracer.Fail(r.Context(), code.HTTPStatus(), err)
	}

	attrs := []any{
		slog.Any("request", rc),
		slog.Int("status", code.HTTPStatus()),
		slog.Any("code", code),
		slog.Any("error", err),
	}

	if code == rigerr.CodeInternal {
		s.logger().ErrorContext(r.Context(), "request failed", attrs...)
		return
	}
	s.logger().DebugContext(r.Context(), "request refused", attrs...)
}

// LogRequest writes the request line, once every handler has finished.
//
// Deferred from the handler rather than wrapped around the mux, because the
// route is only known inside: net/http sets the matched pattern on the request
// the mux dispatches, and a middleware in front of it has an earlier request
// that has matched nothing. A line labelled by path instead would be one line
// per identifier that ever appeared in a URL.
func LogRequest(s Server, r *http.Request, rec *reqlog.Writer, rc RequestContext) {
	l := s.logger()
	// Asked before the attributes are built. This runs on every request, including
	// the ones nobody is watching.
	if !l.Enabled(r.Context(), slog.LevelDebug) {
		return
	}

	l.DebugContext(r.Context(), "request served",
		slog.Any("request", rc),
		slog.Int("status", rec.Status()),
		slog.Int64("bytes", rec.Bytes()),
	)
}

// logger is the server's, or the default one.
//
// Nil is a server nobody configured logging for, not a server that asked for
// silence, and the difference matters on the one line that says why a 500
// happened.
func (s Server) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// WriteJSON writes one JSON response: the content type, the status, and the
// body when there is one.
//
// A nil body writes the status and stops, which is what a 204 is. The encoder
// runs after WriteHeader, so an encoding failure cannot change the status
// already sent — it truncates the body instead, and the request line that
// follows is where that shows up.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

// WriteResult answers with what a write produced, or with what it produced the
// first time somebody sent it.
//
// The bytes are written rather than re-encoded, because a replay that
// round-tripped through a decoder would come back with its keys in whatever
// order that decoder chose — and a client that hashes or signs what it
// received would see two different answers to one request.
func WriteResult(w http.ResponseWriter, res idempotency.Result) {
	if res.Replayed {
		// Nothing a client must act on, and worth saying: it is the difference between
		// a write that happened just now and one that happened the first time this key
		// was seen.
		w.Header().Set("Idempotency-Replayed", "true")
	}
	if len(res.Body) == 0 {
		w.WriteHeader(res.Status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(res.Status)
	_, _ = w.Write(res.Body)
}

// DecodeBody reads a JSON request body.
//
// Unknown fields are rejected. A client that misspells a field name is asking
// for something it will not get, and telling it so beats silently ignoring
// half the request.
func DecodeBody(r *http.Request, into any) error {
	return DecodeReader(r.Body, into)
}

// DecodeReader is the decode itself, separated from the request so that a body
// arriving as one part of a form goes through exactly the same one.
//
// That sharing is the point rather than a tidiness: a multipart create and a
// JSON create have to refuse the same keys and produce the same field errors,
// and two decoders would eventually differ about one of them.
func DecodeReader(r io.Reader, into any) error {
	dec := json.NewDecoder(io.LimitReader(r, MaxBodyBytes))
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		if err == io.EOF {
			return rigerr.BadRequest("the request body is empty")
		}
		return rigerr.BadRequest("cannot read the request body: %v", err)
	}
	return nil
}

// PreparePublic is [Prepare] for an endpoint the configuration marked public.
//
// The claims lookup still runs, and a caller who presents a credential is still
// identified by it — an application that resolves a tenant from the host rather
// than from a token gets one either way. What changes is that a caller who
// presents nothing is served instead of refused.
func PreparePublic(s Server, w http.ResponseWriter, r *http.Request) (context.Context, tenancy.Claims, RequestContext, bool) {
	return Resolve(s, w, r, false)
}

// revisionHeader is the header this server names its revision in.
func (s Server) revisionHeader() string {
	if s.RevisionHeader != "" {
		return s.RevisionHeader
	}
	return DefaultRevisionHeader
}

// throttleCaller is who a request is from, as the limits key on them.
//
// An identity when there is one, and the address only when there is not. An
// address is a poor name for a person — an office behind one NAT is one
// address, and a phone is a different one every few minutes — so once a request
// says who it is, that is the better key.
func throttleCaller(r *http.Request, claims tenancy.Claims, trusted []netip.Prefix) throttle.Caller {
	c := throttle.Caller{}
	if claims.APIKeyID != nil {
		c.APIKey = claims.APIKeyID.String()
	}
	if claims.AccountID != uuid.Nil {
		c.Account = claims.AccountID.String()
	}
	if claims.TenantID != uuid.Nil {
		c.Tenant = claims.TenantID.String()
	}
	if !c.Identified() {
		// Only for an anonymous caller, and only through the trusted-proxy rule: an
		// address read from a header the client controls is an address the client
		// chooses, and a limit keyed on one of those is decorative.
		c.IP = clientip.String(r, trusted)
	}
	return c
}

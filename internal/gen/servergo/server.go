package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// serverFile emits the registration struct, the hooks, and the shared plumbing
// every handler uses.
func (e *emitter) serverFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.requestIDWiring(b)
	e.serverType(b)
	e.handlersStruct(b)
	e.registerFunc(b)
	e.throttleWiring(b)
	e.idempotencyPrunerFunc(b)
	e.linkFunc(b)
	e.helpers(b)

	return artifact("server.gen.go", b)
}

func (e *emitter) serverType(b *gobuf.Buf) {
	var (
		httpPkg = b.Import("net/http")
		ctxPkg  = b.Import("context")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
	)

	b.Comment("Authenticator identifies a caller and serves its own routes.\n\n" +
		"It is declared here rather than imported, and that is the whole trick: " +
		"[github.com/simonjanss/rig/auth.Auth] satisfies it without either side " +
		"knowing the other exists. A field typed as the auth package's own would " +
		"make every project depend on it — argon2, OAuth and all — including the " +
		"ones with no authentication at all.\n\n" +
		"Anything else with these two methods works just as well: a wrapper that " +
		"adds a header check, a stub in a test, an entirely different " +
		"implementation.")
	b.L("type Authenticator interface {")
	b.Comment("Claims identifies the caller behind a request.")
	b.L("Claims(*%s.Request) (%s.Claims, error)", httpPkg, tenPkg)
	b.Comment("Mount registers the authentication routes — signing in, refreshing, " +
		"keys — on the same mux the resource routes are on.")
	b.L("Mount(*%s.ServeMux)", httpPkg)
	b.L("}")
	b.NL()

	b.Comment("Server is the behavior every handler shares.")
	b.L("type Server struct {")
	b.Comment("Auth wires the whole authentication foundation in one field.\n\n" +
		"Setting it does two things that otherwise have to be remembered " +
		"separately: GetClaims comes from it, and its routes are mounted on the " +
		"mux Register returns. Wiring the claims from one thing and mounting " +
		"another is a mistake this makes unavailable.\n\n" +
		"Nil for a project with no authentication, which then has to supply " +
		"GetClaims itself.")
	b.L("Auth Authenticator")
	b.NL()
	b.Comment("GetClaims establishes who is calling. A handler does not run " +
		"without it, so a route cannot accidentally be left unauthenticated.\n\n" +
		"Auth supplies it. Set this instead for a project that authenticates its " +
		"own way, or has no auth routes to mount.")
	b.L("GetClaims func(*%s.Request) (%s.Claims, error)", httpPkg, tenPkg)
	b.NL()
	b.Comment("RequestID labels a request for the logs and for the error body it " +
		"may end in.\n\n" +
		"Nil is the ordinary case and does not mean nothing: it means " +
		"[callerRequestID] — the caller's own " + e.cfg.RequestIDHeader + ", if " +
		"it sent one worth trusting" +
		func() string {
			if e.tracing() {
				return ", and this request's trace otherwise"
			}
			return ""
		}() +
		". Set this only to answer the question differently; the default is " +
		"already the answer every route in this package gives, including the " +
		"authentication ones.")
	b.L("RequestID func(*%s.Request) string", httpPkg)
	b.NL()

	b.Comment("Logger is where this server says what it did and why a request " +
		"failed. Nil uses [log/slog.Default].\n\n" +
		"Nil means the default logger and not silence, which is the whole point: " +
		"the one line worth having is the cause of a 500, and a server that drops " +
		"it because nobody configured logging is a server whose error bodies say " +
		"\"something went wrong\" and mean it literally. Set this to route the " +
		"lines somewhere, not to turn them on.\n\n" +
		"What comes out: an error line per failed request carrying the whole " +
		"error, and, at [log/slog.LevelDebug], a line per request with its status " +
		"and size.\n\n" +
		"There is no logger on the context and no way to reach this one from a " +
		"service. A service is constructed by the application, so it is handed a " +
		"logger there — the same one this field gets — and puts the request on " +
		"its own lines with [RequestContextFrom].")
	b.L("Logger *%s.Logger", b.Import("log/slog"))
	b.NL()

	b.Comment("MinRevision refuses a caller built against an API revision older " +
		"than this one. The zero value, the default, refuses nothing.\n\n" +
		"\tMinRevision: apirev.MustParse(\"2026-04-30\"),\n\n" +
		"This is the end of the story [Revision] starts: you removed a field, you " +
		"waited, the logs said nobody old was left, and now you close the door. " +
		"Until somebody decides to, the revision is telemetry and nothing else.\n\n" +
		"Only a caller that sends a revision older than this is refused. One that " +
		"sends none is served — an unknown client is not the same as an old one, " +
		"and turning every curl and every hand-written integration into a 426 is " +
		"not a door closing, it is an outage.")
	b.L("MinRevision %s.Revision", b.Import(runtimeModule+"/apirev"))
	b.NL()
	b.Comment("OnError turns a service error into a response. Nil uses " +
		"DefaultErrorMapper, which is the right behavior for almost everyone.")
	b.L("OnError func(w %s.ResponseWriter, r *%s.Request, rc RequestContext, err error)", httpPkg, httpPkg)
	b.NL()
	b.Comment("PreHooks run before anything else, in order. A hook that writes a " +
		"response stops the request.")
	b.L("PreHooks []func(w %s.ResponseWriter, r *%s.Request) bool", httpPkg, httpPkg)
	b.NL()
	b.Comment("Context lets a hook attach values a service will read.")
	b.L("Context func(ctx %s.Context, r *%s.Request) %s.Context", ctxPkg, httpPkg, ctxPkg)
	b.NL()
	e.throttleField(b)
	b.NL()
	b.Comment("DB is what a write carrying an Idempotency-Key is recorded in, so " +
		"that a client which had to send the same write twice gets one row and " +
		"the same answer both times. Pass the pool: `DB: app.Pool`.\n\n" +
		"Required, and Register panics without it. A nil one would mean the " +
		"header was quietly ignored, and a client that thinks its retry is safe " +
		"when it is not is worse off than one that knows it is not — this is the " +
		"one failure mode worth a startup panic rather than a runtime surprise.\n\n" +
		"It costs nothing until a caller sends the header: a write without one " +
		"takes the path it always took, with no transaction and no extra round " +
		"trip. See [github.com/simonjanss/rig/runtime/idempotency].")
	b.L("DB %s.Beginner", b.Import(runtimeModule+"/dbx"))
	b.NL()
	b.L("}")
	b.NL()
}

// handlersStruct is the registration surface.
func (e *emitter) handlersStruct(b *gobuf.Buf) {
	b.Comment("Handlers is every resource's service, plus the shared behavior.\n\n" +
		"One field per resource is deliberate: adding a table and forgetting to " +
		"wire it up will not compile, rather than producing a route that answers " +
		"404 until somebody notices.")
	b.L("type Handlers struct {")
	b.L("Server Server")
	b.NL()
	if e.hasNotifications() {
		b.Comment("Notifications is this project's inbox. Setting it mounts the " +
			"routes under /notifications and lets a delete of a notifiable row " +
			"take its notifications with it — a nil one leaves both undone, which " +
			"is why it is a field here rather than something reached for.")
		b.L("Notifications *%s.Service", b.Import(notifyModule))
		b.NL()
	}
	if e.hasPresence() {
		b.Comment("Presence is who is here. Setting it mounts the routes under " +
			"/presence; a nil one leaves them unmounted, so a project that has " +
			"not built a front end for presence yet serves nothing rather than " +
			"routes that write rows nobody reads.\n\n" +
			"Reading presence is not here, and that is the design: the live shape " +
			"is the read path, and it is mounted by the electric generator beside " +
			"the rest of the API.")
		b.L("Presence *%s.Service", b.Import(presenceModule))
		b.NL()
	}
	for _, res := range e.resources() {
		b.L("%s %sService", res.Name, res.Name)
	}
	b.L("}")
	b.NL()
}

// resources are the ones the API layer knows about.
//
// A resource marked unexposed has a model and a repository and no endpoints, so
// there is nothing here to route and no field to put in Handlers.
func (e *emitter) resources() []*ir.Resource {
	out := make([]*ir.Resource, 0, len(e.doc.API.Resources))
	for i := range e.doc.API.Resources {
		if e.doc.API.Resources[i].Unexposed {
			continue
		}
		out = append(out, &e.doc.API.Resources[i])
	}
	return out
}

func (e *emitter) registerFunc(b *gobuf.Buf) {
	httpPkg := b.Import("net/http")

	b.Comment("Register mounts every route and returns the mux.\n\n" +
		"The patterns come from the compiled document, so the router, the OpenAPI " +
		"document, and the generated client all describe the same paths.\n\n" +
		"It makes the mux rather than taking one, because a caller has nothing " +
		"to decide about it: the patterns are absolute and already carry the " +
		"base path. What comes back is a *http.ServeMux and not an opaque " +
		"handler, so anything else this server answers — static files, a second " +
		"API, another generator's routes — is still a Handle call on it.")
	b.L("func Register(h Handlers) *%s.ServeMux {", httpPkg)
	b.L("if h.Server.Auth != nil {")
	b.L("if h.Server.GetClaims != nil {")
	b.Comment("Two answers to \"who is calling\" is not a preference to resolve " +
		"quietly. Whichever one lost would go on looking wired, and the " +
		"difference only shows up as a caller who is somebody else.")
	b.L("panic(\"api.Register: set Server.Auth or Server.GetClaims, not both\")")
	b.L("}")
	b.L("h.Server.GetClaims = h.Server.Auth.Claims")
	b.L("}")
	b.NL()
	b.L("if h.Server.GetClaims == nil {")
	b.Comment("Refusing here rather than defaulting to anonymous is the point: a " +
		"server with no way to identify a caller cannot enforce tenancy, and " +
		"every generated query depends on it.")
	b.L("panic(\"api.Register: set Server.Auth, or Server.GetClaims if this " +
		"project authenticates its own way\")")
	b.L("}")
	b.NL()
	b.L("if h.Server.DB == nil {")
	b.Comment("Here rather than at the first write that carries the header. A " +
		"nil pool would make every Idempotency-Key a header nobody read, and a " +
		"client retrying a create in the belief that it is safe to would be " +
		"writing a second row every time — a failure nobody sees until they go " +
		"looking for duplicates.")
	b.L("panic(\"api.Register: set Server.DB to the database pool, for example " +
		"DB: app.Pool\")")
	b.L("}")
	b.NL()
	if e.throttleEnabled() {
		dbxPkg := b.Import(runtimeModule + "/dbx")
		b.Comment("The limiter, unless one was handed in. It needs the pool and " +
			"the logger, both of which are on the server already, so building it " +
			"here is one less thing to remember — and a project that wants its own " +
			"is not prevented from setting the field.\n\n" +
			"DB is declared as the narrower interface, because everything else " +
			"here only ever begins a transaction. The counters run statements " +
			"outside one, so this asks for the wider half — which a pool has, and " +
			"which anything standing in for a pool in a test may not.")
		b.L("if h.Server.Throttle == nil {")
		b.L("conn, ok := h.Server.DB.(%s.Conn)", dbxPkg)
		b.L("if !ok {")
		b.L("panic(\"api.Register: throttle is configured but Server.DB cannot run " +
			"queries, so there is nowhere to count; set Server.Throttle yourself, or " +
			"set DB to the pool\")")
		b.L("}")
		b.L("h.Server.Throttle = NewThrottle(conn, h.Server.Logger)")
		b.L("}")
		b.NL()
	}

	b.L("Link(h)")
	b.NL()

	b.L("mux := %s.NewServeMux()", httpPkg)
	b.NL()

	for _, res := range e.resources() {
		b.L("if h.%s != nil {", res.Name)
		b.L("register%s(mux, h.Server, h.%s)", res.Name, res.Name)
		b.L("}")
	}

	if e.hasNotifications() {
		b.NL()
		b.Comment("The inbox, on the same mux. Hand-written rather than " +
			"generated, because the tables are rig's own and are the same in every " +
			"project — there is nothing here for a generator to vary.")
		b.L("if h.Notifications != nil {")
		b.L("%s.New(h.Notifications, %s.Options{", b.Import(notifyhttpModule), b.Import(notifyhttpModule))
		b.Comment("The server's own answer to \"who is calling\", so an inbox " +
			"route identifies its caller exactly the way every other route does.")
		b.L("Claims: h.Server.GetClaims,")
		b.L("Fail: func(w %s.ResponseWriter, r *%s.Request, err error) {", httpPkg, httpPkg)
		b.Comment("The project's own error shape, so an inbox route's 404 looks " +
			"like every other route's — and its own metadata, so the line an inbox " +
			"500 writes says which request it was rather than nothing at all. These " +
			"routes do not go through resolve, so the context is built here.")
		b.L("fail(h.Server, w, r, requestContext(h.Server, r), err)")
		b.L("},")
		b.L("}).Mount(mux)")
		b.L("}")
	}

	if e.hasPresence() {
		b.NL()
		b.Comment("Presence, on the same mux, and hand-written for the reason the " +
			"inbox is: the table is rig's own and identical in every project.\n\n" +
			"No permission is checked on these routes, deliberately — everybody " +
			"who may look at a screen may say they are looking at it — and there " +
			"is nowhere in the request to name an account, so \"you may only " +
			"write your own presence\" is a sentence a client cannot phrase " +
			"rather than a rule a handler enforces.")
		b.L("if h.Presence != nil {")
		b.L("%s.New(h.Presence, %s.Options{", b.Import(presencehttpModule), b.Import(presencehttpModule))
		b.L("Claims: h.Server.GetClaims,")
		b.L("Fail: func(w %s.ResponseWriter, r *%s.Request, err error) {", httpPkg, httpPkg)
		b.L("fail(h.Server, w, r, requestContext(h.Server, r), err)")
		b.L("},")
		b.L("}).Mount(mux)")
		b.L("}")
	}

	b.NL()
	b.Comment("After the resources, so a pattern collision between the two is a " +
		"panic naming the auth route rather than the resource one — and the " +
		"resource routes are the ones this project owns.")
	b.L("if h.Server.Auth != nil {")
	b.L("h.Server.Auth.Mount(mux)")
	b.L("}")

	b.NL()
	b.L("return mux")
	b.L("}")
	b.NL()
}

// helpers emit the shared request and response plumbing.
func (e *emitter) helpers(b *gobuf.Buf) {
	var (
		httpPkg = b.Import("net/http")
		jsonPkg = b.Import("encoding/json")
		ctxPkg  = b.Import("context")
		ioPkg   = b.Import("io")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		slogPkg = b.Import("log/slog")
	)

	b.Comment("maxBodyBytes bounds a request body. Without a limit, one client can " +
		"exhaust the server's memory by streaming forever.")
	b.L("const maxBodyBytes = 1 << 20")
	b.NL()

	b.Comment("prepare runs the shared front half of every handler: the hooks, " +
		"the claims, and the request metadata.")
	b.L("func prepare(s Server, w %s.ResponseWriter, r *%s.Request) (%s.Context, %s.Claims, RequestContext, bool) {",
		httpPkg, httpPkg, ctxPkg, tenPkg)
	b.L("return resolve(s, w, r, true)")
	b.L("}")
	b.NL()

	if e.hasPublicEndpoint() {
		b.Comment("preparePublic is prepare for an endpoint the configuration " +
			"marked public.\n\n" +
			"The claims lookup still runs, and a caller who presents a credential " +
			"is still identified by it — an application that resolves a tenant " +
			"from the host rather than from a token gets one either way. What " +
			"changes is that a caller who presents nothing is served instead of " +
			"refused.")
		b.L("func preparePublic(s Server, w %s.ResponseWriter, r *%s.Request) (%s.Context, %s.Claims, RequestContext, bool) {",
			httpPkg, httpPkg, ctxPkg, tenPkg)
		b.L("return resolve(s, w, r, false)")
		b.L("}")
		b.NL()
	}

	b.Comment("requestContext is what one request looks like to an error body and " +
		"to a log line.\n\n" +
		"A function rather than a literal per call site, because a route that " +
		"builds a blank one still answers correctly and still logs a failure — it " +
		"just logs one that names no method, no path and no request identifier, " +
		"which is the one thing the line exists for.")
	b.L("func requestContext(s Server, r *%s.Request) RequestContext {", httpPkg)
	b.L("rc := RequestContext{")
	b.L("Method:     r.Method,")
	b.L("Path:       r.URL.Path,")
	b.L("Route:      r.Pattern,")
	b.L("RemoteAddr: r.RemoteAddr,")
	b.L("UserAgent:  r.UserAgent(),")
	b.L("ClientRevision: r.Header.Get(RevisionHeader),")
	b.L("}")
	b.L("if s.RequestID != nil {")
	b.L("rc.RequestID = s.RequestID(r)")
	b.L("} else {")
	if e.tracing() {
		b.Comment("The caller's own first, so a client correlating its side with " +
			"this one is believed. Failing that, the trace: this project traces, " +
			"so the request already has an identifier, and inventing a second one " +
			"would be inventing a second answer to the same question. The " +
			"requestId in the error body, the request_id on every log line and the " +
			"trace in a collector are one string, and nobody had to wire it up.")
		b.L("rc.RequestID = %s.Or(callerRequestID(r), %s.TraceID(r))",
			b.Import("cmp"), b.Import(observeModule))
	} else {
		b.Comment("This project does not trace, so the caller's own is the only " +
			"identifier there is. Empty when it sent none, which is a request " +
			"nothing can correlate — turning `tracing:` on in rig.yaml is what " +
			"gives every request one whether or not the caller thought to.")
		b.L("rc.RequestID = callerRequestID(r)")
	}
	b.L("}")
	b.L("return rc")
	b.L("}")
	b.NL()

	b.Comment("resolve is the body of both, differing only in whether a caller " +
		"who cannot be identified is refused.")
	b.L("func resolve(s Server, w %s.ResponseWriter, r *%s.Request, required bool) (%s.Context, %s.Claims, RequestContext, bool) {",
		httpPkg, httpPkg, ctxPkg, tenPkg)
	b.L("rc := requestContext(s, r)")
	b.NL()
	b.Comment("Announced on the way out of every response, including the failed " +
		"ones: a client that is behind should not have to make a successful " +
		"request to find out.")
	b.L("if Revision != \"\" { w.Header().Set(RevisionHeader, Revision) }")
	b.NL()
	b.L("for _, hook := range s.PreHooks {")
	b.L("if !hook(w, r) { return nil, %s.Claims{}, rc, false }", tenPkg)
	b.L("}")
	b.NL()
	b.Comment("Before the claims, because being too old to be served is not a " +
		"question about who you are.")
	b.L("if !serveRevision(s, w, r, rc) { return nil, %s.Claims{}, rc, false }", tenPkg)
	b.NL()
	b.L("claims, err := s.GetClaims(r)")
	b.L("if err != nil {")
	b.L("if required {")
	b.L("fail(s, w, r, rc, err)")
	b.L("return nil, %s.Claims{}, rc, false", tenPkg)
	b.L("}")
	b.Comment("Public, so the caller is nobody rather than nobody yet. The " +
		"claims go on the context all the same: what needs a tenant refuses " +
		"there, where the reason is about the thing being asked for.")
	b.L("claims = %s.Claims{}", tenPkg)
	b.L("}")
	b.NL()
	e.throttleCheck(b, tenPkg)

	b.L("ctx := %s.NewContext(r.Context(), claims)", tenPkg)
	b.Comment("So that what only the service method is handed reaches everything " +
		"under it: a validator and a hook are given a context and nothing else, " +
		"and the revision the caller was built against is exactly the kind of " +
		"thing they have to ask about. Before the Context hook, so that hook can " +
		"see it too.")
	b.L("ctx = NewContext(ctx, rc)")
	b.L("if s.Context != nil { ctx = s.Context(ctx, r) }")
	b.L("return ctx, claims, rc, true")
	b.L("}")
	b.NL()

	b.Comment("serveRevision reports whether this caller is new enough to be " +
		"served, and writes the refusal when it is not.\n\n" +
		"Both ways of not refusing are the one comparison: an unset MinRevision " +
		"and a caller that sent no revision each leave one side unknown, and " +
		"nothing is before an unknown revision. A caller that cannot be shown to " +
		"be old is served.")
	b.L("func serveRevision(s Server, w %s.ResponseWriter, r *%s.Request, rc RequestContext) bool {",
		httpPkg, httpPkg)
	b.L("if !rc.BuiltBefore(s.MinRevision) { return true }")
	b.NL()
	b.L("fail(s, w, r, rc, %s.UpgradeRequired(", errPkg)
	b.L("%s,", gobuf.Quote("this client was built against API revision %s; "+
		"this server serves %s and newer"))
	b.L("rc.ClientRevision, s.MinRevision))")
	b.L("return false")
	b.L("}")
	b.NL()

	b.Comment("fail writes an error response.")
	b.L("func fail(s Server, w %s.ResponseWriter, r *%s.Request, rc RequestContext, err error) {", httpPkg, httpPkg)
	b.Comment("Before the mapper and not inside it. A project that set OnError " +
		"replaced how a failure is *answered*; it did not ask to stop being told " +
		"what the failure was.")
	b.L("logFailure(s, r, rc, err)")
	b.NL()
	b.L("if s.OnError != nil {")
	b.L("s.OnError(w, r, rc, err)")
	b.L("return")
	b.L("}")
	b.L("DefaultErrorMapper(w, r, rc, err)")
	b.L("}")
	b.NL()

	b.Comment("logFailure records why this request is about to fail.\n\n" +
		"It is the only place that ever sees the whole error. DefaultErrorMapper " +
		"rewrites an internal message to \"something went wrong\" before it " +
		"reaches the client — deliberately, because the original is exactly the " +
		"kind of thing that leaks a table name or a connection string — so " +
		"without this line the cause of a 500 exists nowhere. The request " +
		"identifier goes out in the body and comes out here, and that pair is the " +
		"whole mechanism for answering \"what happened to my request\".\n\n" +
		"Two levels, because they are two different events. An internal failure " +
		"is the server's fault and is an error. A 404, a 422, a refused " +
		"permission — the server worked, and logging those at anything but debug " +
		"is how a log becomes a thing nobody reads.")
	b.L("func logFailure(s Server, r *%s.Request, rc RequestContext, err error) {", httpPkg)
	b.L("code := %s.CodeOf(err)", errPkg)
	if e.tracing() {
		b.Comment("On the span the handler opened, which this does not end: the " +
			"span belongs to the handler and is closed by its defer. Only an " +
			"internal failure makes the span itself red — the same distinction the " +
			"two log levels below draw, and for the same reason.")
		b.L("%s.Fail(r.Context(), code.HTTPStatus(), err)", b.Import(observeModule))
		b.NL()
	}
	b.L("attrs := []any{")
	b.L("%s.Any(\"request\", rc),", slogPkg)
	b.L("%s.Int(\"status\", code.HTTPStatus()),", slogPkg)
	b.L("%s.Any(\"code\", code),", slogPkg)
	b.L("%s.Any(\"error\", err),", slogPkg)
	b.L("}")
	b.NL()
	b.L("if code == %s.CodeInternal {", errPkg)
	b.L("s.logger().ErrorContext(r.Context(), \"request failed\", attrs...)")
	b.L("return")
	b.L("}")
	b.L("s.logger().DebugContext(r.Context(), \"request refused\", attrs...)")
	b.L("}")
	b.NL()

	b.Comment("logRequest writes the request line, once every handler has finished.\n\n" +
		"Deferred from the handler rather than wrapped around the mux, because " +
		"the route is only known inside: net/http sets the matched pattern on the " +
		"request the mux dispatches, and a middleware in front of it has an " +
		"earlier request that has matched nothing. A line labelled by path " +
		"instead would be one line per identifier that ever appeared in a URL.")
	b.L("func logRequest(s Server, r *%s.Request, rec *%s.Writer, rc RequestContext) {",
		httpPkg, b.Import(runtimeModule+"/reqlog"))
	b.L("l := s.logger()")
	b.Comment("Asked before the attributes are built. This runs on every request, " +
		"including the ones nobody is watching.")
	b.L("if !l.Enabled(r.Context(), %s.LevelDebug) { return }", slogPkg)
	b.NL()
	b.L("l.DebugContext(r.Context(), \"request served\",")
	b.L("%s.Any(\"request\", rc),", slogPkg)
	b.L("%s.Int(\"status\", rec.Status()),", slogPkg)
	b.L("%s.Int64(\"bytes\", rec.Bytes()),", slogPkg)
	b.L(")")
	b.L("}")
	b.NL()

	b.Comment("logger is the server's, or the default one.\n\n" +
		"Nil is a server nobody configured logging for, not a server that asked " +
		"for silence, and the difference matters on the one line that says why a " +
		"500 happened.")
	b.L("func (s Server) logger() *%s.Logger {", b.Import("log/slog"))
	b.L("if s.Logger != nil { return s.Logger }")
	b.L("return %s.Default()", b.Import("log/slog"))
	b.L("}")
	b.NL()

	b.Comment("DefaultErrorMapper turns an error into a response.\n\n" +
		"The decision is [" + runtimeModule + "/httpx.AnswerFor]'s, shared with the " +
		"routes rig mounts itself, " +
		"so a failure from /auth/login and a failure from a generated route are " +
		"classified once: the same code, the same status, an internal failure's " +
		"detail redacted — it is exactly the kind of thing that leaks a table name " +
		"or a connection string, and the request identifier goes out instead so " +
		"the detail can be found in the logs — and a 429 leaving with its " +
		"Retry-After.\n\n" +
		"What is not shared is the envelope. This one's field names go through the " +
		"project's `api.json_case` and rig's own routes always answer camelCase, " +
		"so one struct cannot be both. The classification is one implementation, " +
		"the encoding is two.")
	b.L("func DefaultErrorMapper(w %s.ResponseWriter, _ *%s.Request, rc RequestContext, err error) {", httpPkg, httpPkg)
	b.L("answer := %s.AnswerFor(w, err)", b.Import(runtimeModule+"/httpx"))
	b.L("writeJSON(w, answer.Status, Error{")
	b.L("Code:      answer.Code,")
	b.L("Message:   answer.Message,")
	b.L("RequestID: rc.RequestID,")
	b.L("Fields:    answer.Fields,")
	b.L("})")
	b.L("}")
	b.NL()

	b.L("func writeJSON(w %s.ResponseWriter, status int, body any) {", httpPkg)
	b.L("w.Header().Set(\"Content-Type\", \"application/json; charset=utf-8\")")
	b.L("w.WriteHeader(status)")
	b.L("if body == nil { return }")
	b.L("_ = %s.NewEncoder(w).Encode(body)", jsonPkg)
	b.L("}")
	b.NL()

	e.writeResultFunc(b)

	b.Comment("decodeBody reads a JSON request body.\n\n" +
		"Unknown fields are rejected. A client that misspells a field name is " +
		"asking for something it will not get, and telling it so beats silently " +
		"ignoring half the request.")
	b.L("func decodeBody(r *%s.Request, into any) error {", httpPkg)
	b.L("return decodeReader(r.Body, into)")
	b.L("}")
	b.NL()

	b.Comment("decodeReader is the decode itself, separated from the request so " +
		"that a body arriving as one part of a form goes through exactly the " +
		"same one.\n\n" +
		"That sharing is the point rather than a tidiness: a multipart create " +
		"and a JSON create have to refuse the same keys and produce the same " +
		"field errors, and two decoders would eventually differ about one of " +
		"them.")
	b.L("func decodeReader(r %s.Reader, into any) error {", ioPkg)
	b.L("dec := %s.NewDecoder(%s.LimitReader(r, maxBodyBytes))", jsonPkg, ioPkg)
	b.L("dec.DisallowUnknownFields()")
	b.NL()
	b.L("if err := dec.Decode(into); err != nil {")
	b.L("if err == %s.EOF { return %s.BadRequest(\"the request body is empty\") }", ioPkg, errPkg)
	b.L("return %s.BadRequest(\"cannot read the request body: %%v\", err)", errPkg)
	b.L("}")
	b.L("return nil")
	b.L("}")
	b.NL()

	if e.anyRequiredFilePart() {
		e.hasPartHelper(b)
	}

}

// maxRequestIDBytes bounds what a caller may name its own request.
//
// The value reaches an error body and every log line this request writes, so it
// is client-controlled text in two places that are read by machines. A bound and
// a character class are what keep a header from being a way to write whatever it
// likes into a log file — and the number is generous: every identifier anybody
// actually uses, a UUID or a trace id, is well under it.
const maxRequestIDBytes = 128

// requestIDWiring emits the header this API reads a caller's own identifier
// from, and the one function that decides whether to believe it.
//
// A function rather than a Header.Get at each of the three call sites, because
// the three had drifted: the resource routes took the trace, the auth routes
// took the header, and a project that wanted both wrote the same closure into
// its main. One answer, in one place, is the whole of this.
func (e *emitter) requestIDWiring(b *gobuf.Buf) {
	httpPkg := b.Import("net/http")

	b.Comment("RequestIDHeader is where a caller may name its own request, so that " +
		"its logs and this API's can be lined up afterwards.\n\n" +
		"It is read on every route, including the authentication ones. What is " +
		"done with it is [Server.RequestID]'s documentation.")
	b.L("const RequestIDHeader = %s", gobuf.Quote(e.cfg.RequestIDHeader))
	b.NL()

	b.Comment("maxRequestIDBytes bounds what a caller may name its own request.\n\n" +
		"The value reaches an error body and every log line the request writes, " +
		"so it is client-controlled text in two places that are read by machines. " +
		"A bound and a character class are what keep a header from being a way to " +
		"write whatever it likes into a log file. The number is generous: every " +
		"identifier anybody actually uses — a UUID, a trace id — is well under it.")
	b.L("const maxRequestIDBytes = %d", maxRequestIDBytes)
	b.NL()

	b.Comment("callerRequestID is the identifier the caller asked this request to " +
		"be known by, or empty when it did not ask or asked for something not " +
		"worth repeating.\n\n" +
		"Refusing rather than truncating or escaping, because a header this API " +
		"does not understand is not one it should half-quote into a log line. " +
		"What a caller gets for sending nonsense is the identifier it would have " +
		"got for sending nothing.")
	b.L("func callerRequestID(r *%s.Request) string {", httpPkg)
	b.L("id := r.Header.Get(RequestIDHeader)")
	b.L("if id == \"\" || len(id) > maxRequestIDBytes {")
	b.L("return \"\"")
	b.L("}")
	b.Comment("Printable ASCII, which is what every identifier format in use is " +
		"and what a log line can carry without an escape.")
	b.L("for i := 0; i < len(id); i++ {")
	b.L("if id[i] < 0x20 || id[i] > 0x7e {")
	b.L("return \"\"")
	b.L("}")
	b.L("}")
	b.L("return id")
	b.L("}")
	b.NL()
}

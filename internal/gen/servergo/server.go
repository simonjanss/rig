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

	e.serverAliases(b)
	e.handlersStruct(b)
	e.registerFunc(b)
	e.throttleWiring(b)
	e.linkFunc(b)
	e.errorMapper(b)

	if e.anyRequiredFilePart() {
		e.hasPartHelper(b)
	}

	return artifact("server.gen.go", b)
}

// serverAliases point this package's shared names at
// [github.com/simonjanss/rig/runtime/apibase], which is where the plumbing
// lives now.
//
// What is here is a naming decision and nothing else. `api.Server` is what
// every main function builds and what docs/services.md quotes, and an alias
// keeps that true while the declaration lives somewhere it can be imported
// rather than copied. The unexported ones are why the per-resource route files
// did not have to change: `prepare`, `fail` and the rest still resolve.
func (e *emitter) serverAliases(b *gobuf.Buf) {
	basePkg := b.Import(runtimeModule + "/apibase")

	b.Comment("RequestIDHeader is where a caller may name its own request, so that " +
		"its logs and this API's can be lined up afterwards.\n\n" +
		"It is read on every route, including the authentication ones. What is " +
		"done with it is [Server.RequestID]'s documentation.")
	b.L("const RequestIDHeader = %s", gobuf.Quote(e.cfg.RequestIDHeader))
	b.NL()

	b.Comment("Authenticator identifies a caller and serves its own routes.\n\n" +
		"[github.com/simonjanss/rig/auth.Auth] satisfies it without either side " +
		"knowing the other exists, which is what keeps a project with no " +
		"authentication from depending on argon2 and OAuth. Anything else with " +
		"these two methods works just as well: a wrapper that adds a header " +
		"check, a stub in a test, an entirely different implementation.")
	b.L("type Authenticator = %s.Authenticator", basePkg)
	b.NL()

	b.Comment("Server is the behavior every handler shares: who the caller is, " +
		"where the log lines go, what a failure is answered with.\n\n" +
		"[Register] fills in what this project already decided — its revision, " +
		"the headers it names, its rate limiter and, when `tracing:` is on, where " +
		"its spans go — so what is left to set is the handful of fields only an " +
		"application can answer. See the package documentation of " +
		"[github.com/simonjanss/rig/runtime/apibase] for every field.")
	b.L("type Server = %s.Server", basePkg)
	b.NL()

	b.Comment("IdempotencyPruner deletes the records of writes past their " +
		"retention, and so decides how long after a write its Idempotency-Key " +
		"still replays.\n\n" +
		"Zero takes idempotency.DefaultRetention, a day: long enough to cover any " +
		"retry, short enough that a key reused a week later is a new request " +
		"rather than a write that silently does nothing.\n\n" +
		"A task rather than a goroutine, for the reason FileSweeper is one — a " +
		"cron job is one thing running, and a goroutine in every replica is as " +
		"many as there are replicas, all racing to delete the same rows. Register " +
		"it in serve.Config.Tasks and run `<binary> prune-idempotency`:\n\n" +
		"\tTasks: map[string]serve.Task{\"prune-idempotency\": api.IdempotencyPruner(0)},\n\n" +
		"Nothing schedules it for you. Without it rig_idempotency keeps every " +
		"record ever written, and the retention above is a sentence rather than a " +
		"behaviour.")
	b.L("var IdempotencyPruner = %s.IdempotencyPruner", basePkg)
	b.NL()

	for _, a := range []struct{ name, target string }{
		{"prepare", "Prepare"},
		{"requestContext", "RequestContextOf"},
		{"fail", "Fail"},
		{"logRequest", "LogRequest"},
		{"writeJSON", "WriteJSON"},
		{"writeResult", "WriteResult"},
		{"decodeBody", "DecodeBody"},
		{"decodeReader", "DecodeReader"},
	} {
		b.L("var %s = %s.%s", a.name, basePkg, a.target)
	}
	if e.hasPublicEndpoint() {
		b.L("var preparePublic = %s.PreparePublic", basePkg)
	}
	b.NL()
}

// serverDefaults fills in what this project already decided, on the way through
// Register.
//
// Every one of these is a fact the document holds and an application has no
// business restating: which revision this build serves, which headers it names,
// where its spans go. They are fields rather than constants because
// [github.com/simonjanss/rig/runtime/apibase] serves every project rather than
// one — and Register is the one place that runs before any handler does.
//
// OnError is the exception and is set only when it is empty, because that one is
// a choice: a project that replaced how a failure is answered has said so, and
// this must not undo it.
func (e *emitter) serverDefaults(b *gobuf.Buf) {
	b.Comment("What this project already decided. An application sets who is " +
		"calling and where the log lines go; the rest is the document's, and " +
		"restating it in a main function would be somewhere for the two to " +
		"disagree.")
	b.L("h.Server.Revision = serverRevision")
	b.L("h.Server.RevisionHeader = RevisionHeader")
	b.L("h.Server.RequestIDHeader = RequestIDHeader")
	if e.tracing() {
		b.Comment("Where this project's spans go, for the two questions the shared " +
			"plumbing asks: what to label a request nobody labelled, and which span " +
			"to redden when one fails. The per-route span is opened by the handler " +
			"itself.")
		b.L("h.Server.Tracer = %s.APITracer{}", b.Import(observeModule))
	}
	if e.throttleEnabled() {
		b.L("h.Server.TrustedProxies = throttleTrustedProxies")
	}
	b.NL()

	b.Comment("Only when it is empty: a project that replaced how a failure is " +
		"answered has said so, and this is not the place to undo it.")
	b.L("if h.Server.OnError == nil {")
	b.L("h.Server.OnError = DefaultErrorMapper")
	b.L("}")
	b.NL()
}

// errorMapper emits the one piece of the plumbing that could not move.
//
// The classification is shared — [httpx.AnswerFor], the same call the routes rig
// mounts itself go through — but the envelope is not: this one's field names go
// through the project's `api.json_case` and rig's own routes always answer
// camelCase, so one struct cannot be both. Go struct tags are not
// parameterisable, so the encoding stays here and Register hands it to the
// server as OnError.
func (e *emitter) errorMapper(b *gobuf.Buf) {
	httpPkg := b.Import("net/http")

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
// A resource marked unexposed has no endpoints, so there is nothing here to
// route and no field to put in Handlers. What else it still gets is
// [ir.Resource.Unreachable]'s question rather than this one's: a hidden table of
// the project's own keeps its model and its repository, and one of rig's own
// keeps neither.
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

	e.serverDefaults(b)

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

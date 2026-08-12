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

	e.serverType(b)
	e.handlersStruct(b)
	e.registerFunc(b)
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
	b.Comment("RequestID labels a request for the logs. Nil generates nothing, " +
		"and the field is simply absent from error bodies.")
	b.L("RequestID func(*%s.Request) string", httpPkg)
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
	b.L("mux := %s.NewServeMux()", httpPkg)
	b.NL()

	for _, res := range e.resources() {
		b.L("if h.%s != nil {", res.Name)
		b.L("register%s(mux, h.Server, h.%s)", res.Name, res.Name)
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
		strPkg  = b.Import("strconv")
		timePkg = b.Import("time")
		uuidPkg = b.Import("github.com/google/uuid")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
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

	b.Comment("resolve is the body of both, differing only in whether a caller " +
		"who cannot be identified is refused.")
	b.L("func resolve(s Server, w %s.ResponseWriter, r *%s.Request, required bool) (%s.Context, %s.Claims, RequestContext, bool) {",
		httpPkg, httpPkg, ctxPkg, tenPkg)
	b.L("rc := RequestContext{")
	b.L("Method:     r.Method,")
	b.L("Path:       r.URL.Path,")
	b.L("Route:      r.Pattern,")
	b.L("RemoteAddr: r.RemoteAddr,")
	b.L("UserAgent:  r.UserAgent(),")
	b.L("}")
	b.L("if s.RequestID != nil { rc.RequestID = s.RequestID(r) }")
	b.NL()
	b.L("for _, hook := range s.PreHooks {")
	b.L("if !hook(w, r) { return nil, %s.Claims{}, rc, false }", tenPkg)
	b.L("}")
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
	b.L("ctx := %s.NewContext(r.Context(), claims)", tenPkg)
	b.L("if s.Context != nil { ctx = s.Context(ctx, r) }")
	b.L("return ctx, claims, rc, true")
	b.L("}")
	b.NL()

	b.Comment("fail writes an error response.")
	b.L("func fail(s Server, w %s.ResponseWriter, r *%s.Request, rc RequestContext, err error) {", httpPkg, httpPkg)
	b.L("if s.OnError != nil {")
	b.L("s.OnError(w, r, rc, err)")
	b.L("return")
	b.L("}")
	b.L("DefaultErrorMapper(w, r, rc, err)")
	b.L("}")
	b.NL()

	b.Comment("DefaultErrorMapper turns an error into a response.\n\n" +
		"An internal failure's detail never reaches the client: it is exactly the " +
		"kind of thing that leaks a table name or a connection string. The request " +
		"identifier goes out instead, so the detail can be found in the logs.")
	b.L("func DefaultErrorMapper(w %s.ResponseWriter, _ *%s.Request, rc RequestContext, err error) {", httpPkg, httpPkg)
	b.L("code := %s.CodeOf(err)", errPkg)
	b.NL()
	b.L("message := err.Error()")
	b.L("var typed *%s.Error", errPkg)
	b.L("if %s.As(err, &typed) { message = typed.Message }", b.Import("errors"))
	b.L("if code == %s.CodeInternal { message = \"something went wrong\" }", errPkg)
	b.NL()
	b.Comment("A 429 with no Retry-After leaves a client with nothing to do but " +
		"guess, and clients that guess retry immediately.")
	b.L("if refusal, ok := %s.RefusalOf(err); ok { refusal.Decision().SetHeaders(w.Header()) }",
		b.Import(runtimeModule+"/throttle"))
	b.NL()
	b.Comment("A validation failure carries its own shape — one member per field " +
		"of the input — and answering with that instead of only a sentence is " +
		"the difference between a client highlighting the field and a client " +
		"parsing prose to find out which one.")
	b.L("fields, _ := %s.FieldsOf(err)", errPkg)
	b.NL()
	b.L("writeJSON(w, code.HTTPStatus(), Error{")
	b.L("Code:      code,")
	b.L("Message:   message,")
	b.L("RequestID: rc.RequestID,")
	b.L("Fields:    fields,")
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

	b.Comment("decodeBody reads a JSON request body.\n\n" +
		"Unknown fields are rejected. A client that misspells a field name is " +
		"asking for something it will not get, and telling it so beats silently " +
		"ignoring half the request.")
	b.L("func decodeBody(r *%s.Request, into any) error {", httpPkg)
	b.L("body := %s.LimitReader(r.Body, maxBodyBytes)", ioPkg)
	b.L("dec := %s.NewDecoder(body)", jsonPkg)
	b.L("dec.DisallowUnknownFields()")
	b.NL()
	b.L("if err := dec.Decode(into); err != nil {")
	b.L("if err == %s.EOF { return %s.BadRequest(\"the request body is empty\") }", ioPkg, errPkg)
	b.L("return %s.BadRequest(\"cannot read the request body: %%v\", err)", errPkg)
	b.L("}")
	b.L("return nil")
	b.L("}")
	b.NL()

	e.paramHelpers(b, httpPkg, errPkg, uuidPkg, strPkg, timePkg)
}

// paramHelpers parse path and query parameters.
func (e *emitter) paramHelpers(b *gobuf.Buf, httpPkg, errPkg, uuidPkg, strPkg, timePkg string) {
	b.L("func pathUUID(r *%s.Request, name string) (%s.UUID, error) {", httpPkg, uuidPkg)
	b.L("raw := r.PathValue(name)")
	b.L("id, err := %s.Parse(raw)", uuidPkg)
	b.L("if err != nil {")
	b.L("return %s.Nil, %s.BadRequest(\"%%s is not a valid identifier\", name)", uuidPkg, errPkg)
	b.L("}")
	b.L("return id, nil")
	b.L("}")
	b.NL()

	b.L("func pathInt(r *%s.Request, name string) (int, error) {", httpPkg)
	b.L("v, err := %s.Atoi(r.PathValue(name))", strPkg)
	b.L("if err != nil { return 0, %s.BadRequest(\"%%s must be a whole number\", name) }", errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func pathString(r *%s.Request, name string) (string, error) {", httpPkg)
	b.L("raw := r.PathValue(name)")
	b.L("if raw == \"\" { return \"\", %s.BadRequest(\"%%s is required\", name) }", errPkg)
	b.L("return raw, nil")
	b.L("}")
	b.NL()

	b.Comment("queryInt reads an integer query parameter, falling back when absent.")
	b.L("func queryInt(r *%s.Request, name string, fallback int) (int, error) {", httpPkg)
	b.L("raw := r.URL.Query().Get(name)")
	b.L("if raw == \"\" { return fallback, nil }")
	b.L("v, err := %s.Atoi(raw)", strPkg)
	b.L("if err != nil { return 0, %s.BadRequest(\"%%s must be a whole number\", name) }", errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func queryString(r *%s.Request, name, fallback string) string {", httpPkg)
	b.L("if raw := r.URL.Query().Get(name); raw != \"\" { return raw }")
	b.L("return fallback")
	b.L("}")
	b.NL()

	b.L("func queryBool(r *%s.Request, name string, fallback bool) (bool, error) {", httpPkg)
	b.L("raw := r.URL.Query().Get(name)")
	b.L("if raw == \"\" { return fallback, nil }")
	b.L("v, err := %s.ParseBool(raw)", strPkg)
	b.L("if err != nil { return false, %s.BadRequest(\"%%s must be true or false\", name) }", errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func queryUUID(r *%s.Request, name string) (%s.UUID, error) {", httpPkg, uuidPkg)
	b.L("raw := r.URL.Query().Get(name)")
	b.L("if raw == \"\" { return %s.Nil, nil }", uuidPkg)
	b.L("v, err := %s.Parse(raw)", uuidPkg)
	b.L("if err != nil { return %s.Nil, %s.BadRequest(\"%%s is not a valid identifier\", name) }", uuidPkg, errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func queryTime(r *%s.Request, name string) (%s.Time, error) {", httpPkg, timePkg)
	b.L("raw := r.URL.Query().Get(name)")
	b.L("if raw == \"\" { return %s.Time{}, nil }", timePkg)
	b.L("v, err := %s.Parse(%s.RFC3339, raw)", timePkg, timePkg)
	b.L("if err != nil {")
	b.L("return %s.Time{}, %s.BadRequest(\"%%s must be an RFC 3339 timestamp\", name)", timePkg, errPkg)
	b.L("}")
	b.L("return v, nil")
	b.L("}")
	b.NL()

}

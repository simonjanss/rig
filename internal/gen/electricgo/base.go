package electricgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// baseFile emits the server, the registration struct, and the shared plumbing.
func (e *emitter) baseFile(shapes []*ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)
	b.Doc("Package " + e.cfg.Package + " is the generated live-sync layer: one shape " +
		"endpoint per table that asked for one. The tenant and lifecycle filters are " +
		"built here and cannot be influenced by a client; an application narrows a " +
		"shape further through the Scope function on each handler.")

	e.serverType(b)
	e.handlersStruct(b, shapes)
	e.registerFunc(b, shapes)
	e.helpers(b)

	return artifact("electric.gen.go", b, gen.Overwrite)
}

func (e *emitter) serverType(b *gobuf.Buf) {
	var (
		httpPkg = b.Import("net/http")
		elecPkg = b.Import(runtimeModule + "/electric")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
	)

	b.Comment("Server is what every shape endpoint shares.")
	b.L("type Server struct {")
	b.Comment("Proxy forwards to the sync service.")
	b.L("Proxy *%s.Proxy", elecPkg)
	b.NL()
	b.Comment("GetClaims establishes who is subscribing. A shape does not stream " +
		"without it: the tenant filter is built from the claims, and there is no " +
		"filter to build without them.")
	b.L("GetClaims func(*%s.Request) (%s.Claims, error)", httpPkg, tenPkg)
	b.NL()
	b.Comment("IsAdmin reports whether a caller may subscribe to a shape marked " +
		"admin-only. Nil refuses every one of them, which is the safe way to " +
		"leave it unconfigured.")
	b.L("IsAdmin func(%s.Claims) bool", tenPkg)
	b.NL()
	b.Comment("OnError renders a refusal. Nil writes the status and a short line.")
	b.L("OnError func(w %s.ResponseWriter, r *%s.Request, err error)", httpPkg, httpPkg)
	b.L("}")
	b.NL()

	if e.cfg.ElectricURL != "" {
		b.Comment("DefaultElectricURL is the sync service configured when this was " +
			"generated. A deployment can point somewhere else by building its own proxy.")
		b.L("const DefaultElectricURL = %s", gobuf.Quote(e.cfg.ElectricURL))
		b.NL()
	}
}

func (e *emitter) handlersStruct(b *gobuf.Buf, shapes []*ir.Resource) {
	b.Comment("Handlers is one scoping function per shape, plus the shared behavior.\n\n" +
		"A nil scope is not an error: it means the shape is filtered by tenant and " +
		"lifecycle and nothing else, which is exactly right for most tables.\n\n" +
		"A soft-deletable table has a trash shape here as well as a live one, and a " +
		"table that keeps its previous versions has a history shape. Neither is " +
		"configured: the columns are what decide, the same way they decide whether " +
		"the API has a GET /_deleted.\n\n" +
		"Those two inherit the live shape's scope while their own field is nil. They " +
		"carry the same table's rows, so a narrowing that mattered for the live " +
		"shape almost always matters for the trash and the history too — and a " +
		"narrowing the application has to remember to repeat on a route rig added " +
		"is one that eventually does not get repeated. Set the field to scope them " +
		"differently; setting it replaces the inherited scope rather than adding to " +
		"it.")
	b.L("type Handlers struct {")
	b.L("Server Server")
	b.NL()
	for _, res := range shapes {
		for _, sh := range e.shapesFor(res) {
			b.L("%s %sScope", sh.name, sh.name)
		}
	}
	if e.cfg.fallbacks() {
		b.NL()
		b.Comment("What answers a shape while the sync service cannot be reached. " +
			"Nil is a 502 and a subscriber with no rows, which is what every shape " +
			"did before these fields existed.\n\n" +
			"These do not inherit the way the scopes above do, and the reason is that " +
			"there would be nothing to inherit: the three shapes carry three " +
			"different generations of a row, so they correspond to three different " +
			"reads — a live list, its trash, one row's history. A trash route quietly " +
			"answering with live rows is the worst of the available outcomes, so nil " +
			"stays nil.\n\n" +
			"Whatever the scope above narrows, the fallback has to narrow too. A scope " +
			"is a filter the proxy sends and can therefore promise; a fallback is a " +
			"read it cannot see inside.")
		for _, res := range shapes {
			for _, sh := range e.shapesFor(res) {
				b.L("%sFallback %sFallback", sh.name, sh.name)
			}
		}
	}
	b.L("}")
	b.NL()
}

func (e *emitter) registerFunc(b *gobuf.Buf, shapes []*ir.Resource) {
	httpPkg := b.Import("net/http")

	b.Comment("Register mounts every shape endpoint.\n\n" +
		"It takes a mux rather than making one, because these routes belong on " +
		"the same server as the API: api.Register makes it, this adds to it.")
	b.L("func Register(mux *%s.ServeMux, h Handlers) {", httpPkg)
	b.L("if h.Server.Proxy == nil {")
	b.L("panic(\"electric: Server.Proxy is required\")")
	b.L("}")
	b.L("if h.Server.GetClaims == nil {")
	b.Comment("The tenant filter is built from the claims. A server that cannot " +
		"identify a subscriber has no filter to build, and a shape with no " +
		"tenant filter streams the whole table to whoever asked.")
	b.L("panic(\"electric: Server.GetClaims is required\")")
	b.L("}")
	b.NL()

	e.inherit(b, shapes)

	for _, res := range shapes {
		for _, sh := range e.shapesFor(res) {
			if e.cfg.fallbacks() {
				b.L("mux.HandleFunc(%s, handle%sShape(h.Server, h.%s, h.%sFallback))",
					gobuf.Quote("GET "+sh.path), sh.name, sh.name, sh.name)
			} else {
				b.L("mux.HandleFunc(%s, handle%sShape(h.Server, h.%s))",
					gobuf.Quote("GET "+sh.path), sh.name, sh.name)
			}
		}
	}
	b.L("}")
	b.NL()
}

// inherit points a derived shape with no scope of its own at the live one.
//
// The trash and the history are the same table's rows, so an application that
// narrowed the live shape meant something by it, and the narrowing is what
// decides who may see those rows. Defaulting these to nil would mean a project
// that regenerates gains two routes that show more than the one it already had,
// without a line of its own code changing and without failing to compile. This
// is the same argument the handler makes for building the owner predicate
// itself: a narrowing somebody has to add by hand is one somebody eventually
// does not.
//
// Inheriting can only ever show less, which is the direction to be wrong in. An
// application that wants these scoped differently says so by setting the field.
func (e *emitter) inherit(b *gobuf.Buf, shapes []*ir.Resource) {
	var wrote bool
	for _, res := range shapes {
		for _, sh := range e.shapesFor(res) {
			if sh.kind == shapeLive {
				continue
			}
			if !wrote {
				b.Comment("A derived shape falls back to the live shape's scope. Nil " +
					"stays nil: a table nobody scoped is scoped by tenant and lifecycle " +
					"on all three of its routes.")
				wrote = true
			}
			b.L("if h.%s == nil {", sh.name)
			if sh.kind == shapeVersions {
				b.L("h.%s = versionsFromLive%s(h.%s)", sh.name, res.Name, res.Name)
			} else {
				b.L("h.%s = %sScope(h.%s)", sh.name, sh.name, res.Name)
			}
			b.L("}")
		}
	}
	if wrote {
		b.NL()
	}
}

// helpers emit the shared request plumbing.
func (e *emitter) helpers(b *gobuf.Buf) {
	var (
		httpPkg = b.Import("net/http")
		errPkg  = b.Import(runtimeModule + "/rigerr")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		errsPkg = b.Import("errors")
		strPkg  = b.Import("strconv")
		timePkg = b.Import("time")
		uuidPkg = b.Import("github.com/google/uuid")
	)

	b.Comment("prepare authenticates a subscription and starts its filter.\n\n" +
		"The tenant condition goes on before anything else has a chance to run, " +
		"so there is no path through this package that reaches the sync service " +
		"without it.")
	b.L("func prepare(s Server, w %s.ResponseWriter, r *%s.Request, admin bool) (%s.Claims, *%s.Where, bool) {",
		httpPkg, httpPkg, tenPkg, b.Import(runtimeModule+"/electric"))
	b.L("claims, err := s.GetClaims(r)")
	b.L("if err != nil {")
	b.L("fail(s, w, r, err)")
	b.L("return claims, nil, false")
	b.L("}")
	b.L("if !claims.Valid() {")
	b.L("fail(s, w, r, %s.Unauthorized(\"this request is not authenticated\"))", errPkg)
	b.L("return claims, nil, false")
	b.L("}")
	b.NL()
	b.L("if admin && (s.IsAdmin == nil || !s.IsAdmin(claims)) {")
	b.L("fail(s, w, r, %s.Forbidden(\"this shape is not available to you\"))", errPkg)
	b.L("return claims, nil, false")
	b.L("}")
	b.NL()
	b.L("return claims, &%s.Where{}, true", b.Import(runtimeModule+"/electric"))
	b.L("}")
	b.NL()

	b.L("func fail(s Server, w %s.ResponseWriter, r *%s.Request, err error) {", httpPkg, httpPkg)
	b.L("if s.OnError != nil {")
	b.L("s.OnError(w, r, err)")
	b.L("return")
	b.L("}")
	b.NL()
	b.L("code := %s.CodeOf(err)", errPkg)
	b.L("message := err.Error()")
	b.L("var typed *%s.Error", errPkg)
	b.L("if %s.As(err, &typed) { message = typed.Message }", errsPkg)
	b.L("if code == %s.CodeInternal { message = \"something went wrong\" }", errPkg)
	b.L("%s.Error(w, message, code.HTTPStatus())", httpPkg)
	b.L("}")
	b.NL()

	e.paramHelpers(b, httpPkg, errPkg, strPkg, timePkg, uuidPkg)
}

// paramHelpers read the declared parameters off the query string.
func (e *emitter) paramHelpers(b *gobuf.Buf, httpPkg, errPkg, strPkg, timePkg, uuidPkg string) {
	b.Comment("required reads a parameter that must be present. A shape whose " +
		"scoping depends on a value is a shape that must not stream without it.")
	b.L("func required(r *%s.Request, name string) (string, error) {", httpPkg)
	b.L("v := r.URL.Query().Get(name)")
	b.L("if v == \"\" { return \"\", %s.BadRequest(\"%%s is required\", name) }", errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func optional(r *%s.Request, name string) (string, bool) {", httpPkg)
	b.L("v := r.URL.Query().Get(name)")
	b.L("return v, v != \"\"")
	b.L("}")
	b.NL()

	b.L("func parseInt(name, raw string) (int, error) {")
	b.L("v, err := %s.Atoi(raw)", strPkg)
	b.L("if err != nil { return 0, %s.BadRequest(\"%%s must be a whole number\", name) }", errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func parseInt64(name, raw string) (int64, error) {")
	b.L("v, err := %s.ParseInt(raw, 10, 64)", strPkg)
	b.L("if err != nil { return 0, %s.BadRequest(\"%%s must be a whole number\", name) }", errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func parseFloat(name, raw string) (float64, error) {")
	b.L("v, err := %s.ParseFloat(raw, 64)", strPkg)
	b.L("if err != nil { return 0, %s.BadRequest(\"%%s must be a number\", name) }", errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func parseBool(name, raw string) (bool, error) {")
	b.L("v, err := %s.ParseBool(raw)", strPkg)
	b.L("if err != nil { return false, %s.BadRequest(\"%%s must be true or false\", name) }", errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func parseUUID(name, raw string) (%s.UUID, error) {", uuidPkg)
	b.L("v, err := %s.Parse(raw)", uuidPkg)
	b.L("if err != nil { return %s.Nil, %s.BadRequest(\"%%s is not a valid identifier\", name) }",
		uuidPkg, errPkg)
	b.L("return v, nil")
	b.L("}")
	b.NL()

	b.L("func parseTime(name, raw string) (%s.Time, error) {", timePkg)
	b.L("v, err := %s.Parse(%s.RFC3339, raw)", timePkg, timePkg)
	b.L("if err != nil {")
	b.L("return %s.Time{}, %s.BadRequest(\"%%s must be an RFC 3339 timestamp\", name)", timePkg, errPkg)
	b.L("}")
	b.L("return v, nil")
	b.L("}")
	b.NL()
}

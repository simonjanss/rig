package servicego

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// baseFile emits what every resource shares: the request envelope, the error
// shape, and pagination.
func (e *emitter) baseFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)
	b.Doc("Package " + e.cfg.Package + " is the generated API layer: the wire types, " +
		"the interfaces your service layer implements, and a working default " +
		"implementation of each. Business logic belongs in the service layer, " +
		"not here: every .gen.go file in this package is rewritten whenever the " +
		"schema changes.")

	e.revision(b)
	e.requestEnvelope(b)
	e.requestContextPlumbing(b)
	e.callerHelper(b)
	if e.anyOwnerScoped() {
		e.scopeHelper(b)
	}
	e.errorShape(b)
	e.paginationShape(b)

	return artifact("api.gen.go", b, gen.Overwrite)
}

// revision emits what this API was generated from, and the header it travels in.
func (e *emitter) revision(b *gobuf.Buf) {
	revPkg := b.Import(runtimeModule + "/apirev")

	b.Comment("Revision is the date this API surface last changed, as YYYY-MM-DD.\n\n" +
		"It is not a build stamp: it moves when the surface moves and not when " +
		"somebody regenerates, so two clients built a month apart against an " +
		"unchanged API report the same thing — because they are the same thing. " +
		"`rig generate` records it in .rig/revision.json, which is committed.\n\n" +
		"Empty when the project has never generated with a revision recorded.")
	b.L("const Revision = %s", gobuf.Quote(e.doc.API.Revision))
	b.NL()

	b.Comment("RevisionHeader carries the revision in both directions: what the " +
		"caller was built against on the way in, what this server was generated " +
		"from on the way out.")
	b.L("const RevisionHeader = %s", gobuf.Quote(e.revisionHeader()))
	b.NL()

	b.Comment("serverRevision is [Revision] as a value, parsed once. Unknown for a " +
		"project that has never generated with a revision recorded, which reads " +
		"as \"nobody is behind this server\" rather than as an error.")
	b.L("var serverRevision, _ = %s.Parse(Revision)", revPkg)
	b.NL()
}

// revisionHeader is the configured header name, or the default for a document
// compiled before the setting existed.
func (e *emitter) revisionHeader() string {
	if h := e.doc.API.RevisionHeader; h != "" {
		return h
	}
	return defaultRevisionHeader
}

func (e *emitter) requestEnvelope(b *gobuf.Buf) {
	tenPkg := b.Import(runtimeModule + "/tenancy")

	b.Comment("Request is everything a handler received.\n\n" +
		"Each part is a separate type parameter, and an operation that has no " +
		"path parameters uses struct{} for that slot. That is deliberate: the " +
		"signature says what an endpoint takes, so reaching for something it " +
		"does not have is a compile error rather than a nil check.")
	b.L("type Request[Path, Query, Body any] struct {")
	b.L("// Claims describe the caller. They are always present: a handler does")
	b.L("// not run without them.")
	b.L("Claims %s.Claims", tenPkg)
	b.NL()
	b.L("Path  Path")
	b.L("Query Query")
	b.L("Body  Body")
	b.NL()
	b.L("ctx RequestContext")
	b.L("}")
	b.NL()

	b.Comment("Context returns what is known about the request itself, as opposed to " +
		"what it carries.")
	b.L("func (r Request[Path, Query, Body]) Context() RequestContext { return r.ctx }")
	b.NL()

	b.Comment("BuiltBefore reports whether the client that sent this request was " +
		"built before rev.\n\n" +
		"It is [RequestContext.BuiltBefore] with the request already in hand, which " +
		"is where a compatibility shim usually is:\n\n" +
		"\tvar notesAdded = apirev.MustParse(\"2026-04-30\")\n\n" +
		"\tif r.BuiltBefore(notesAdded) {\n" +
		"\t\tr.Body.Title = \"Unknown\"\n" +
		"\t}\n\n" +
		"A shim belongs here rather than in a hook. This is the one place that has " +
		"the request as it arrived, and on a create the generated validation runs " +
		"after the service method and before every hook — so a hook that filled in " +
		"a missing NOT NULL column would only ever see requests that already " +
		"passed the check it was written for.")
	b.L("func (r Request[Path, Query, Body]) BuiltBefore(rev %s.Revision) bool { return r.ctx.BuiltBefore(rev) }",
		b.Import(runtimeModule+"/apirev"))
	b.NL()

	b.Comment("RequestContext is the request's own metadata.")
	b.L("type RequestContext struct {")
	b.L("// RequestID correlates this request with the server's logs.")
	b.L("RequestID string")
	b.L("Method    string")
	b.L("Path      string")
	b.L("// Route is the pattern that matched, so a metric can be labelled by")
	b.L("// endpoint rather than by every distinct identifier.")
	b.L("Route     string")
	b.L("RemoteAddr string")
	b.L("UserAgent  string")
	b.L("// ClientRevision is the raw header, kept for a log line that wants to")
	b.L("// count what callers are sending. Empty when the caller said nothing, and")
	b.L("// unparseable prose when it said something that is not a revision — a")
	b.L("// hand-rolled client and a curl will not say, and that is a normal thing")
	b.L("// for a caller to be. Compare with [RequestContext.BuiltBefore] rather")
	b.L("// than with this.")
	b.L("ClientRevision string")
	b.L("}")
	b.NL()

	e.revisionMethods(b)

	b.Comment("NewRequest builds a request. The server calls it; it is exported so a " +
		"test can call a service method directly without going through HTTP.")
	b.L("func NewRequest[Path, Query, Body any](claims %s.Claims, path Path, query Query, body Body, rc RequestContext) Request[Path, Query, Body] {", tenPkg)
	b.L("return Request[Path, Query, Body]{Claims: claims, Path: path, Query: query, Body: body, ctx: rc}")
	b.L("}")
	b.NL()
}

// revisionMethods emit the one place a revision string becomes a value.
//
// Two failures share an answer here, deliberately: a caller that sent nothing
// and a caller that sent nonsense are both simply not something to compare
// against, and giving them separate fields would invite a logging hook to treat
// "unknown" as an event when it is the ordinary state of a curl.
func (e *emitter) revisionMethods(b *gobuf.Buf) {
	var (
		timePkg = b.Import("time")
		revPkg  = b.Import(runtimeModule + "/apirev")
	)

	b.Comment("Client is what the caller was built against.\n\n" +
		"Unknown — the zero [" + revPkg + ".Revision] — when the caller said nothing " +
		"and equally when what it said is not a revision. The two are the same " +
		"answer on purpose: both mean this caller cannot be placed on the " +
		"timeline.")
	b.L("func (rc RequestContext) Client() %s.Revision {", revPkg)
	b.L("rev, _ := %s.Parse(rc.ClientRevision)", revPkg)
	b.L("return rev")
	b.L("}")
	b.NL()

	b.Comment("BuiltBefore reports whether the caller was built before rev, which " +
		"is the question a compatibility shim is written in terms of.\n\n" +
		"False for a caller that sent no revision. That is a decision rather than " +
		"a fallback: revisions describe what rig's own generated clients were " +
		"built against, so a caller rig cannot place is served the current " +
		"behavior. An application that would rather treat an unknown caller as " +
		"ancient has [RequestContext.ClientRevision] and can say so itself.")
	b.L("func (rc RequestContext) BuiltBefore(rev %s.Revision) bool { return rc.Client().Before(rev) }", revPkg)
	b.NL()

	b.Comment("Stale reports how far behind this server's revision the caller " +
		"is.\n\n" +
		"ok is false when the caller did not say, said something unparseable, or " +
		"is not behind at all — including the case of a caller newer than this " +
		"server, which is somebody halfway through a deploy rather than somebody " +
		"to warn about.")
	b.L("func (rc RequestContext) Stale() (%s.Duration, bool) {", timePkg)
	b.L("client := rc.Client()")
	b.L("if !client.Before(serverRevision) { return 0, false }")
	b.L("return serverRevision.Sub(client), true")
	b.L("}")
	b.NL()
}

// requestContextPlumbing emits the context carriage.
//
// The service method is handed a RequestContext; everything below it — a
// validator, a dbhook, a repository — is handed only a context.Context, and
// until this existed the revision simply stopped at the service layer. It is
// the same value either way, reached two ways, rather than a second copy that
// can drift from the first.
func (e *emitter) requestContextPlumbing(b *gobuf.Buf) {
	ctxPkg := b.Import("context")

	b.L("type requestContextKey struct{}")
	b.NL()

	b.Comment("NewContext returns a context carrying the request's metadata.\n\n" +
		"The server calls it on every request, before the [Server.Context] hook " +
		"runs, so anything that hook adds can already see it. It is exported for " +
		"the same reason [NewRequest] is: a test that calls a service method " +
		"directly still has to put one there, or the hooks underneath will find " +
		"nothing.")
	b.L("func NewContext(ctx %s.Context, rc RequestContext) %s.Context {", ctxPkg, ctxPkg)
	b.L("return %s.WithValue(ctx, requestContextKey{}, rc)", ctxPkg)
	b.L("}")
	b.NL()

	b.Comment("RequestContextFrom returns the request metadata on a context.\n\n" +
		"This is how a validator or a hook reaches what only the service method is " +
		"handed — the revision the caller was built against, the request " +
		"identifier, the route that matched.\n\n" +
		"ok is false rather than an error, and the zero value is usable: work that " +
		"did not come from a request at all — a migration, a background job — has " +
		"no metadata to find, and that is not a failure.")
	b.L("func RequestContextFrom(ctx %s.Context) (RequestContext, bool) {", ctxPkg)
	b.L("rc, ok := ctx.Value(requestContextKey{}).(RequestContext)")
	b.L("return rc, ok")
	b.L("}")
	b.NL()
}

// callerHelper emits the one place "was there a caller" is decided.
//
// A read hook takes the claims as a pointer because a read the table
// configuration marked public is reached by somebody who presented nothing.
// Nil is that somebody. The zero value would be a tenant of all zeroes that
// reads like a real one, which is the mistake worth making impossible.
func (e *emitter) callerHelper(b *gobuf.Buf) {
	tenPkg := b.Import(runtimeModule + "/tenancy")

	b.Comment("caller is the claims a read hook is handed, or nil when the " +
		"request carried none.\n\n" +
		"Only a public endpoint can reach a hook with none: everything else is " +
		"refused before a handler runs, and every write is refused again by the " +
		"repository. That is why the write hooks take a value and these take a " +
		"pointer.")
	b.L("func caller(claims %s.Claims) *%s.Claims {", tenPkg, tenPkg)
	b.L("if !claims.Valid() { return nil }")
	b.L("return &claims")
	b.L("}")
	b.NL()
}

func (e *emitter) errorShape(b *gobuf.Buf) {
	errPkg := b.Import(runtimeModule + "/rigerr")

	b.Comment("ErrorCode is the machine-readable reason a request failed. Clients " +
		"switch on this rather than on the status, because three unrelated " +
		"failures share a status and none of them share a code.")
	b.L("type ErrorCode = %s.Code", errPkg)
	b.NL()

	// From the document's own enumeration rather than a list written here. The
	// set is closed, and a second copy of a closed set is a copy that is one
	// commit away from being wrong about what the closed set is.
	if codes := e.doc.Enum(enumErrorCode); codes != nil {
		b.L("// The codes a client may see.")
		b.L("const (")
		for _, c := range codes.Values {
			b.L("ErrorCode%s ErrorCode = %s.Code%s", c.Name, errPkg, c.Name)
		}
		b.L(")")
		b.NL()
	}

	// From the document, field by field, rather than from sentences written
	// here. The same shape is about to be an OpenAPI schema and a TypeScript
	// interface, and three hand-written descriptions of one field are three
	// things to keep in step.
	obj := e.object(objectError)
	if obj == nil {
		return
	}

	b.Comment(obj.Description)
	b.L("type Error struct {")
	for _, f := range obj.Fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		tag := e.namer.JSON(f.Name)
		if f.IsNullable() {
			tag += ",omitempty"
		}
		b.L("%s %s `json:%s`", f.Name, f.GoType, gobuf.Quote(tag))
	}
	b.L("}")
	b.NL()
}

func (e *emitter) paginationShape(b *gobuf.Buf) {
	obj := e.object(objectPagination)
	if obj == nil {
		return
	}

	b.Comment(obj.Description)
	b.L("type Pagination struct {")
	for _, f := range obj.Fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name, e.goType(b, f), gobuf.Quote(f.Wire))
	}
	b.L("}")
	b.NL()
}

// jsonTag renders a field's struct tag.
//
// A nullable field is omitted when empty so that a response does not carry a
// wall of nulls, while a required one is always present so a client can rely on
// the key existing.
func jsonTag(f ir.Field) string {
	tag := f.Wire
	if f.IsNullable() || f.IsArray() {
		tag += ",omitempty"
	}
	return gobuf.Quote(tag)
}

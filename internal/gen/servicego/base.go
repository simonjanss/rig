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

	e.requestEnvelope(b)
	e.callerHelper(b)
	if e.anyOwnerScoped() {
		e.scopeHelper(b)
	}
	e.errorShape(b)
	e.paginationShape(b)

	return artifact("api.gen.go", b, gen.Overwrite)
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
	b.L("}")
	b.NL()

	b.Comment("NewRequest builds a request. The server calls it; it is exported so a " +
		"test can call a service method directly without going through HTTP.")
	b.L("func NewRequest[Path, Query, Body any](claims %s.Claims, path Path, query Query, body Body, rc RequestContext) Request[Path, Query, Body] {", tenPkg)
	b.L("return Request[Path, Query, Body]{Claims: claims, Path: path, Query: query, Body: body, ctx: rc}")
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

	b.L("// The codes a client may see.")
	b.L("const (")
	for _, c := range errorCodes() {
		b.L("ErrorCode%s ErrorCode = %s.Code%s", c.name, errPkg, c.name)
	}
	b.L(")")
	b.NL()

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

// errorCodes mirrors the closed set the compiler defines.
func errorCodes() []struct{ name string } {
	return []struct{ name string }{
		{"BadRequest"}, {"Unauthorized"}, {"Forbidden"}, {"NotFound"},
		{"Conflict"}, {"UnprocessableEntity"}, {"RateLimited"}, {"Internal"},
	}
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

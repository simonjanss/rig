package servicego

import (
	"github.com/simonjanss/rig/internal/gen/genutil"
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
	e.baseAliases(b)
	e.errorShape(b)
	e.paginationShape(b)
	e.fileShapeType(b)
	if e.anyFileColumn() {
		e.fileResponseHelper(b)
	}

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
	b.L("const RevisionHeader = %s", gobuf.Quote(genutil.RevisionHeader(e.doc)))
	b.NL()

	b.Comment("serverRevision is [Revision] as a value, parsed once. Unknown for a " +
		"project that has never generated with a revision recorded, which reads " +
		"as \"nobody is behind this server\" rather than as an error.")
	b.L("var serverRevision, _ = %s.Parse(Revision)", revPkg)
	b.NL()
}

// baseAliases point this package's shared names at
// [github.com/simonjanss/rig/runtime/apibase], which is where they live now.
//
// Aliases rather than a changed spelling everywhere, and that is the whole
// reason they are here: `api.Request`, `api.RequestContext` and
// `api.NewRequest` are what a service signature says and what the
// documentation quotes, and none of that should have to know which module the
// declaration came from. An alias is not a wrapper either — it is the same
// type — so a handler in one of rig's own modules and a service method in this
// one are talking about one struct.
func (e *emitter) baseAliases(b *gobuf.Buf) {
	var (
		basePkg = b.Import(runtimeModule + "/apibase")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
	)

	b.Comment("Request is everything a handler received.\n\n" +
		"Each part is a separate type parameter, and an operation that has no " +
		"path parameters uses struct{} for that slot. That is deliberate: the " +
		"signature says what an endpoint takes, so reaching for something it does " +
		"not have is a compile error rather than a nil check.")
	b.L("type Request[Path, Query, Body any] = %s.Request[Path, Query, Body]", basePkg)
	b.NL()

	b.Comment("RequestContext is the request's own metadata: how it was " +
		"labelled, what matched it, and what the caller was built against.")
	b.L("type RequestContext = %s.RequestContext", basePkg)
	b.NL()

	b.Comment("NewRequest builds a request. The server calls it; it is exported " +
		"so a test can call a service method directly without going through HTTP.")
	b.L("func NewRequest[Path, Query, Body any](claims %s.Claims, path Path, query Query, body Body, rc RequestContext) Request[Path, Query, Body] {",
		tenPkg)
	b.L("return %s.NewRequest(claims, path, query, body, rc)", basePkg)
	b.L("}")
	b.NL()

	b.Comment("NewContext carries a request's metadata down to everything the " +
		"service method calls, so a validator or a hook — which are handed a " +
		"context and nothing else — can ask what the caller was built against.")
	b.L("var NewContext = %s.NewContext", basePkg)
	b.NL()

	b.Comment("RequestContextFrom reads back what [NewContext] carried, and " +
		"reports false for a context that never went through a handler.")
	b.L("var RequestContextFrom = %s.RequestContextFrom", basePkg)
	b.NL()

	b.Comment("caller is the claims a read hook is handed, or nil when the " +
		"request carried none.")
	b.L("var caller = %s.Caller", basePkg)
	b.NL()

	if e.anyOwnerScoped() {
		b.Comment("readScope turns a requested scope into the read options that " +
			"produce it.")
		b.L("var readScope = %s.ReadScope", basePkg)
		b.NL()
	}
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
func jsonTag(f ir.Field) string { return genutil.JSONTag(f) }

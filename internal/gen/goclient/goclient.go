// Package goclient generates a Go SDK for the API: the types a caller holds and
// one method per endpoint.
//
// It exists for the same reason the server generator does. A hand-written client
// is a second description of the API, kept in step by whoever remembers — and
// the failure is quiet, because a client that stopped sending a field compiles
// perfectly. This one is written from the document the router was built from, so
// the two cannot disagree about a path, a status, or the name of a key.
//
// What it does not generate is everything underneath the methods: the request,
// the credential, the failure, the pagination. That is the same in every project
// and lives, hand-written, in github.com/simonjanss/rig/rigclient. So a
// generated method is one call and a doc comment, which is what makes the output
// worth reading rather than merely worth compiling.
//
// Three things are deliberately not restated here. Update inputs use
// rig/runtime/patch, because a client sending patch.Nullable where the server
// decodes patch.Optional is a null that gets refused, and one definition of
// which is which is the only way they agree. Error codes are rig/runtime/rigerr,
// so the constant a client switches on is the constant a handler returned. And
// the scope parameter is rig/runtime/tenancy, which is the package whose two
// values it is.
package goclient

import (
	"context"
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

func init() { gen.Register(New()) }

// DefaultClientModule is the hand-written half of the SDK, which the generated
// half calls into.
const DefaultClientModule = "github.com/simonjanss/rig/rigclient"

// Options configure the generator.
type Options struct {
	// Package is the Go package the generated files declare.
	Package string `json:"package"`

	// ClientImport is the import path of the SDK runtime. It is here for a fork
	// or a vendored copy; a project has no reason to set it.
	ClientImport string `json:"client_import"`

	// DefaultBaseURL is emitted as a constant when it is set, so a demo or a
	// development tool can build a client without naming the server. Leaving it
	// out is right for anything that runs in more than one place.
	DefaultBaseURL string `json:"default_base_url"`
}

// Generator emits the Go client.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "go-client" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "a typed Go client: the wire types and one method per endpoint"
}

// Version implements [gen.Generator].
func (*Generator) Version() string { return "1" }

// Generate implements [gen.Generator].
func (g *Generator) Generate(
	_ context.Context, doc *ir.Document, opts gen.Options,
) ([]gen.Artifact, error) {
	cfg, err := gen.Decode[Options](opts)
	if err != nil {
		return nil, err
	}
	if cfg.Package == "" {
		cfg.Package = "client"
	}
	if cfg.ClientImport == "" {
		cfg.ClientImport = DefaultClientModule
	}

	e := &emitter{doc: doc, cfg: cfg}
	e.reachable = e.reach()

	base, err := e.clientFile()
	if err != nil {
		return nil, err
	}
	artifacts := []gen.Artifact{base}

	for _, enum := range doc.API.Enums {
		// ErrorCode is rigerr's, and is aliased in the base file rather than
		// declared here. Everything else is a column's type — and only the ones
		// a caller can actually receive.
		if enum.PgType == "" || !e.reachable[enum.Name] {
			continue
		}
		art, err := e.enumFile(enum)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, art)
	}

	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Unexposed || len(res.Endpoints) == 0 {
			continue
		}

		types, err := e.typesFile(res)
		if err != nil {
			return nil, err
		}
		inputs, err := e.inputFile(res)
		if err != nil {
			return nil, err
		}
		methods, err := e.methodFile(res)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, types, inputs, methods)

		if filters := e.filterObjects(res); len(filters) > 0 {
			art, err := e.filterFile(res, filters)
			if err != nil {
				return nil, err
			}
			artifacts = append(artifacts, art)
		}
	}

	// Anything the document declares that no resource claimed — a shape from a
	// table's YAML used by a custom endpoint, say — still has to exist, or a
	// method that returns one will not compile.
	if rest := e.unclaimedObjects(); len(rest) > 0 {
		art, err := e.objectFile(rest)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, art)
	}

	if doc.API.Auth != nil {
		art, err := e.authFile(doc.API.Auth)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, art)
	}

	return artifacts, nil
}

// emitter carries what every file needs.
type emitter struct {
	doc *ir.Document
	cfg Options
	// reachable names every type a caller can actually send or receive. See
	// [emitter.reach].
	reachable map[string]bool
}

// reach walks out from the endpoints and returns every type they can carry.
//
// It exists because a document describes more than an API exposes. The
// authentication foundation's own tables have models, repositories and filter
// shapes — and no endpoints, deliberately — so emitting every object in the
// document would put a filter for the session table in the SDK of an
// application that cannot search sessions. What a client should declare is what
// its own methods mention, and what those mention in turn.
func (e *emitter) reach() map[string]bool {
	seen := make(map[string]bool)

	var follow func(name string)
	visit := func(fields []ir.Field) {
		for _, f := range fields {
			switch f.TypeKind {
			case ir.TypeKindEnum, ir.TypeKindObject, ir.TypeKindResource:
				follow(f.Type)
			}
		}
	}
	follow = func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true

		if obj := e.object(name); obj != nil {
			visit(obj.Fields)
		}
	}

	// Error and Pagination are in every response, and the base file declares
	// them whether or not a field happens to mention one.
	follow("Error")
	follow("Pagination")

	for _, res := range e.exposed() {
		follow(res.Name)
		follow(res.Name + "ListResponse")

		for i := range res.Endpoints {
			ep := &res.Endpoints[i]

			follow(ep.Request.BodyObject)
			visit(ep.Request.PathParams)
			visit(ep.Request.QueryParams)
			visit(ep.Request.BodyParams)
			visit(ep.Request.Headers)

			for _, r := range ep.Responses {
				follow(r.BodyObject)
				visit(r.BodyFields)
				visit(r.Headers)
			}
		}
	}
	return seen
}

// client imports the SDK runtime and returns the name to qualify with.
func (e *emitter) client(b *gobuf.Buf) string { return b.Import(e.cfg.ClientImport) }

// goType renders a field's type. Every named type the client uses — its enums,
// its filters, its entities — is declared in this same package, so there is
// nothing to qualify.
func (e *emitter) goType(b *gobuf.Buf, f ir.Field) string { return genutil.GoType(b, f, nil) }

func (e *emitter) artifact(path string, b *gobuf.Buf) (gen.Artifact, error) {
	return genutil.Artifact(path, b, gen.Overwrite)
}

// object returns a named object, or nil.
func (e *emitter) object(name string) *ir.Object {
	for i := range e.doc.API.Objects {
		if e.doc.API.Objects[i].Name == name {
			return &e.doc.API.Objects[i]
		}
	}
	return nil
}

// listResponse is the paged shape a resource's reads answer with, or nil.
func (e *emitter) listResponse(res *ir.Resource) *ir.Object {
	return e.object(res.Name + "ListResponse")
}

// filterObjects are the search shapes belonging to a resource.
//
// A filter is reached from the search body rather than from the entity, so a
// resource with no Search endpoint has filter shapes in the document and no use
// for them here.
func (e *emitter) filterObjects(res *ir.Resource) []*ir.Object {
	var out []*ir.Object
	for i := range e.doc.API.Objects {
		obj := &e.doc.API.Objects[i]
		if obj.Origin != ir.OriginFilter || !e.reachable[obj.Name] {
			continue
		}
		if strings.HasPrefix(obj.Name, res.Name+"Filter") {
			out = append(out, obj)
		}
	}
	return out
}

// unclaimedObjects are the objects no other file emits.
//
// Error and Pagination are the base file's; a resource's own shapes are its
// files'. What is left is whatever a project declared for itself, and it has to
// land somewhere.
func (e *emitter) unclaimedObjects() []*ir.Object {
	claimed := map[string]bool{"Error": true, "Pagination": true}
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		if res.Unexposed || len(res.Endpoints) == 0 {
			continue
		}
		claimed[res.Name] = true
		claimed[res.Name+"ListResponse"] = true
		for _, obj := range e.filterObjects(res) {
			claimed[obj.Name] = true
		}
	}

	var out []*ir.Object
	for i := range e.doc.API.Objects {
		obj := &e.doc.API.Objects[i]
		if !claimed[obj.Name] && e.reachable[obj.Name] {
			out = append(out, obj)
		}
	}
	return out
}

// fileName is the file a resource's own artifacts go in.
func fileName(res *ir.Resource, suffix string) string {
	return naming.Snake(res.Name) + suffix + ".gen.go"
}

// jsonTag renders a field's tag.
//
// A nullable or empty field is omitted rather than sent as a wall of nulls, and
// a required one is always present so the server can rely on the key. It is the
// same rule the model layer follows, for the same reason.
func jsonTag(f ir.Field) string {
	tag := f.Wire
	if f.IsNullable() || f.IsArray() {
		tag += ",omitempty"
	}
	return gobuf.Quote(tag)
}

// structFields writes one member per field, descriptions and all.
func (e *emitter) structFields(b *gobuf.Buf, fields []ir.Field) {
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name, e.goType(b, f), jsonTag(f))
	}
}

// objectStruct emits one of the document's objects as a struct.
func (e *emitter) objectStruct(b *gobuf.Buf, obj *ir.Object) {
	b.Comment(genutil.Describe(obj.Description, obj.Name+" as the API sends it."))
	b.L("type %s struct {", obj.Name)
	e.structFields(b, obj.Fields)
	b.L("}")
	b.NL()
}

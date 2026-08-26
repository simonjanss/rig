// Package openapigen generates an OpenAPI 3.1 document from the compiled
// document, so a specification cannot describe an API that does not exist.
//
// It exists for the reason the server and client generators do, and the IR was
// shaped around it before it was written: ir.Endpoint.Pattern is computed once
// at freeze "so the router, the OpenAPI document, and the TypeScript client
// cannot disagree about path shape", ir.Endpoint.Errors stores bare status
// codes because every one of them shares the Error body, and
// ir.Field.Description is the single copy of the prose precisely so one
// sentence reaches a Go doc comment and an OpenAPI description alike. A
// hand-written specification is a second description of the API, and it goes
// wrong quietly — nothing fails to compile when a document keeps describing a
// field that was renamed a year ago.
//
// The document is assembled as a model and rendered, rather than written as
// text. A specification built by concatenating strings is one that eventually
// stops parsing, and the failure lands on whoever downloaded it.
//
// What it does not describe is what the compiled document does not carry. The
// routes the authentication module and the notification inbox mount are
// hand-written and reach no IR endpoint, so they are absent here and said to be
// absent in info.description — an omission a reader can see beats one they
// discover.
package openapigen

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pb33f/libopenapi/datamodel/high/base"
	"github.com/pb33f/libopenapi/orderedmap"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

func init() { gen.Register(New()) }

// SpecVersion is the OpenAPI version emitted, and the only one accepted.
//
// 3.1 has no way to express an operation on the QUERY method, so a search is
// documented as the POST alias the compiler already emits beside it. 3.2 can
// express it and almost nothing can read 3.2 yet; when that changes, this
// becomes a branch rather than a constant, which is why the option exists and
// is validated instead of ignored.
const SpecVersion = "3.1"

// specVersionFull is what goes in the document. Tooling keys off the patch
// version being present.
const specVersionFull = "3.1.0"

// Options configure the generator.
type Options struct {
	// Version selects the specification version. Only "3.1" is accepted.
	Version string `json:"version"`

	// Formats are the renderings to write: json, yaml, or both.
	Formats []string `json:"formats"`

	// Servers are the origins the API answers on. The default is a single
	// relative server, which is true of every deployment and is what keeps a
	// specification usable in a documentation viewer without naming a host the
	// project may not have.
	Servers []Server `json:"servers"`

	// Electric says whether the live-sync shape routes are described. They are
	// routes of this API and are documented by default; a project that would
	// rather not publish a proxied protocol turns them off here.
	Electric *bool `json:"electric"`
}

// Server is one origin the API answers on.
type Server struct {
	URL         string `json:"url"`
	Description string `json:"description"`
}

// Generator emits the OpenAPI document.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "openapi" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "an OpenAPI 3.1 document: every endpoint, schema and status the API answers with"
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
	if cfg.Version != "" && cfg.Version != SpecVersion {
		return nil, fmt.Errorf(
			"openapi: version %q is not supported; rig emits %s, and documents a QUERY search "+
				"as its POST alias because 3.1 cannot express one", cfg.Version, SpecVersion)
	}
	if len(cfg.Formats) == 0 {
		cfg.Formats = []string{"json", "yaml"}
	}
	for _, f := range cfg.Formats {
		if f != "json" && f != "yaml" {
			return nil, fmt.Errorf("openapi: format %q is not one of json, yaml", f)
		}
	}
	if len(cfg.Servers) == 0 {
		cfg.Servers = []Server{{
			URL:         "/",
			Description: "This server. Set openapi.servers to name the deployment instead.",
		}}
	}
	if cfg.Electric == nil {
		cfg.Electric = boolPtr(true)
	}

	e := &emitter{doc: doc, cfg: cfg, extra: map[string]*base.Schema{}}
	e.reachable = e.reach()

	model := e.document()
	return e.render(model)
}

// emitter carries what every part of the document needs.
type emitter struct {
	doc *ir.Document
	cfg Options

	// reachable names every shape a caller can send or receive. See
	// [emitter.reach].
	reachable map[string]bool

	// extra holds the shapes this generator had to name because the compiled
	// document did not. See [emitter.bodySchema].
	extra map[string]*base.Schema

	// usedStatuses are the error responses some operation referred to, so
	// components carries those and no others. An unused component is a lint
	// finding, and a document listing a 426 no endpoint can return is describing
	// a failure that cannot happen.
	usedStatuses map[int]bool

	// usedIdempotency records that some operation pointed at the
	// Idempotency-Key parameter or the Idempotency-Replayed header. Recorded
	// rather than predicted, for the reason usedStatuses is: an API of nothing
	// but reads has no idempotent write to carry either, and a component
	// nothing references is a lint finding.
	usedIdempotencyKey      bool
	usedIdempotencyReplayed bool
}

// exposed are the resources with a surface worth describing.
//
// Streaming is asked before either of the other two questions, and not after
// them. A shape is its own read surface: rig_notification_recipient is
// unexposed and has no endpoints, and the mux still serves its shape route, so
// gating on either of those would leave a route the server answers on out of
// the document that is supposed to describe every one of them.
//
// An unexposed resource brings no schema in with it. The compiler emits no wire
// object for one — see expandResource, which returns before naming it — and a
// shape route's body is the sync protocol's own rather than the row, so nothing
// here refers to a component that does not exist.
func (e *emitter) exposed() []*ir.Resource {
	var out []*ir.Resource
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		if !e.syncs(res) && (res.Unexposed || len(res.Endpoints) == 0) {
			continue
		}
		out = append(out, res)
	}
	return out
}

// syncs reports whether a resource's live-sync routes belong in the document.
//
// A resource can be unexposed and still stream — the notification recipient
// table is exactly that — so this is asked separately from whether the resource
// has endpoints.
func (e *emitter) syncs(res *ir.Resource) bool {
	return res.Electric != nil && *e.cfg.Electric
}

// enum returns a named enum from the document, or nil.
func (e *emitter) enum(name string) *ir.Enum { return e.doc.Enum(name) }

// reach names every shape an operation can carry.
//
// A compiled document describes more than an API exposes. The authentication
// foundation's own tables have models, repositories and filter shapes and — on
// purpose — no endpoints, so writing out every object would put a filter for
// the session table in the specification of an application that cannot search
// sessions. AuthLogEntry is the sharpest case: it is in the document for a
// consumer that does not exist yet, and nothing references it, so it must not
// appear here.
//
// The walk itself is [genutil.Walk], shared with the two client generators.
// What is not shared is the seeding, and this generator's differs twice over: a
// specification declares the shapes its operations carry rather than the ones a
// method mentions, so ErrorCode is a seed here and an endpoint with no route in
// 3.1 is skipped.
func (e *emitter) reach() map[string]bool {
	w := genutil.NewWalk(e.doc)

	// Error is in every operation's failures and ErrorCode is inside it;
	// Pagination is in every list response. They are reachable whether or not a
	// field happens to mention one.
	w.Follow(objectError)
	w.Follow(objectPagination)
	w.Follow(enumErrorCode)

	for _, res := range e.exposed() {
		w.Follow(res.Name)
		w.Follow(res.Name + "ListResponse")

		for i := range res.Endpoints {
			ep := &res.Endpoints[i]
			// An endpoint with no route in 3.1 carries nothing. Following it
			// would pull a resource's whole filter grammar into a document that
			// never mentions it.
			if len(routesOf(ep)) == 0 {
				continue
			}
			w.Endpoint(ep)
		}
	}
	return w.Seen()
}

// schemas builds components/schemas: every reachable object and enum, plus the
// shapes this generator had to name itself.
//
// Sorted by name, once, before anything is inserted. Every collection in the
// rendered model is an ordered map keyed by insertion, so emission order is
// whatever order things go in — and a generator has to be a pure function of
// its input, which means never ranging a Go map into one of them.
func (e *emitter) schemas() *orderedmap.Map[string, *base.SchemaProxy] {
	type entry struct {
		name   string
		schema *base.Schema
	}
	var all []entry

	for i := range e.doc.API.Enums {
		en := &e.doc.API.Enums[i]
		if e.reachable[en.Name] {
			all = append(all, entry{en.Name, e.enumSchema(en)})
		}
	}
	for i := range e.doc.API.Objects {
		obj := &e.doc.API.Objects[i]
		if e.reachable[obj.Name] {
			all = append(all, entry{obj.Name, e.objectSchema(obj)})
		}
	}
	for name, s := range e.extra {
		all = append(all, entry{name, s})
	}

	slices.SortFunc(all, func(a, b entry) int { return strings.Compare(a.name, b.name) })

	out := orderedmap.New[string, *base.SchemaProxy]()
	for _, en := range all {
		out.Set(en.name, base.CreateSchemaProxy(en.schema))
	}
	return out
}

// objectSchema renders one of the document's objects.
//
// Properties keep the document's field order rather than being sorted: an
// entity that starts with id reads like the row it describes, and alphabetising
// it would be a worse document for no gain. The order is deterministic because
// the compiled document's is.
func (e *emitter) objectSchema(obj *ir.Object) *base.Schema {
	props := orderedmap.New[string, *base.SchemaProxy]()
	for _, f := range obj.Fields {
		props.Set(f.Wire, e.fieldSchema(f))
	}
	return &base.Schema{
		Type:        []string{"object"},
		Description: genutil.Describe(obj.Description, obj.Name+" as the API sends it."),
		Properties:  props,
	}
}

// enumSchema renders a closed set.
//
// The values on the wire are the enum's Wire spellings. x-enum-varnames carries
// the identifiers beside them, in the same order, because the split is real
// information — a Postgres label is frequently in_progress where a generated
// identifier is InProgress — and JSON Schema's enum has nowhere to keep it. A
// client generator reading this document can produce the pair; one reading only
// `enum` has to invent the names, and will invent different ones.
func (e *emitter) enumSchema(en *ir.Enum) *base.Schema {
	s := &base.Schema{
		Type:        []string{"string"},
		Description: genutil.Describe(en.Description, en.Name+"."),
	}
	var names []string
	for _, v := range en.Values {
		s.Enum = append(s.Enum, scalarNode("!!str", v.Wire))
		names = append(names, v.Name)
	}
	setExtension(&s.Extensions, "x-enum-varnames", stringSeq(names))
	return s
}

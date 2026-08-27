// Package electricgo generates live-sync shape endpoints.
//
// The filter is the whole point. A client subscribes to a shape and receives
// every matching row, then every change to one, indefinitely — so a shape whose
// filter the client can influence is a subscription to somebody else's data.
// Everything about which rows exist is decided here, from the compiled
// document: the tenant, the soft-delete predicate, the snapshot predicate, and
// a hook the application can only narrow with.
//
// A table gets up to three of them. The live shape is the rows an ordinary read
// returns. A soft-deletable table also gets a trash shape, and a table that
// keeps its previous versions also gets a history shape scoped to one row —
// the two the API already exposes as GET /_deleted and GET /{id}/_versions.
// Which of them exist is not configured, because the columns already say: the
// widening a subscriber can ask for is a route rig generated from the schema,
// never a parameter it sent.
//
// It generates into its own package rather than alongside the API layer, so a
// project can have live sync without the HTTP generator, or the other way
// round.
package electricgo

import (
	"context"
	"fmt"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

func init() { gen.Register(New()) }

const runtimeModule = "github.com/simonjanss/rig/runtime"

// Options configure the generator.
type Options struct {
	// Package is the Go package the generated files declare.
	Package string `json:"package"`

	// ElectricURL is the sync service the endpoints proxy to. It becomes the
	// default a wiring can override, so a deployment can point at a different
	// one without regenerating.
	ElectricURL string `json:"electric_url"`

	// StubDir is where the hand-owned scoping files go, relative to the project
	// root. {table} and {Table} are substituted. Empty writes no stubs.
	StubDir string `json:"stub_dir"`
	// StubPackage names the package a stub declares. Empty uses the table name.
	StubPackage string `json:"stub_package"`
	// ShapeImport is the import path of this generated package, so a stub in
	// another directory can refer back to it.
	ShapeImport string `json:"shape_import"`
}

// Generator emits the shape endpoints.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "electric" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "live-sync shape endpoints, with the tenant and lifecycle filters built in"
}

// Version implements [gen.Generator].
func (*Generator) Version() string { return "1" }

// Generate implements [gen.Generator].
func (g *Generator) Generate(_ context.Context, doc *ir.Document, opts gen.Options) ([]gen.Artifact, error) {
	cfg, err := gen.Decode[Options](opts)
	if err != nil {
		return nil, err
	}
	if cfg.Package == "" {
		cfg.Package = "electric"
	}

	e := &emitter{doc: doc, cfg: cfg, root: opts.Root, namer: naming.New(naming.Config{})}

	shapes := e.shapes()
	if len(shapes) == 0 {
		// Nothing has asked for live sync. Emitting an empty package would
		// leave a directory nobody imports and a manifest entry nobody wants.
		return nil, nil
	}

	base, err := e.baseFile(shapes)
	if err != nil {
		return nil, err
	}
	artifacts := []gen.Artifact{base}

	for _, res := range shapes {
		shape, err := e.shapeFile(res)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, shape)

		// No scope stub for a table rig created, for the reason the service
		// generator gives: it would be a file about somebody else's table, and
		// the three in this repository's own examples were byte-identical
		// `return nil`s. A project that does want to narrow one — presence is the
		// case that earns it, where every heartbeat reaches every subscriber —
		// writes the file, and CreateOnce then leaves it alone.
		if cfg.StubDir != "" && !res.Foundation {
			for _, sh := range e.shapesFor(res) {
				stub, err := e.stubFile(res, sh)
				if err != nil {
					return nil, err
				}
				artifacts = append(artifacts, stub)
			}
		}
	}
	return artifacts, nil
}

type emitter struct {
	doc   *ir.Document
	cfg   Options
	root  string
	namer *naming.Namer
}

// shapes are the resources that asked for a live-sync endpoint.
func (e *emitter) shapes() []*ir.Resource {
	var out []*ir.Resource
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		if res.Electric != nil && res.Storage != nil {
			out = append(out, res)
		}
	}
	return out
}

func artifact(path string, b *gobuf.Buf, mode gen.WriteMode) (gen.Artifact, error) {
	content, err := b.Bytes()
	if err != nil {
		return gen.Artifact{}, fmt.Errorf("%s: %w", path, err)
	}
	return gen.Artifact{Path: path, Content: content, Mode: mode}, nil
}

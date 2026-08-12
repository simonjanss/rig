// Package servicego generates the API layer's types and the interface your
// service layer implements.
//
// The generated default implementation is the point. A table with no business
// logic needs nothing but a constructor: embedding DefaultLessonService already
// satisfies LessonService. Adding a rule means overriding one method and
// delegating to the embedded default for the rest — rather than reimplementing
// create, read, update, delete and search to change one of them.
package servicego

import (
	"context"
	"fmt"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

func init() { gen.Register(New()) }

// runtimeModule is the import path generated code depends on.
const runtimeModule = "github.com/simonjanss/rig/runtime"

// Options configure the generator.
type Options struct {
	// Package is the Go package the generated files declare.
	Package string `json:"package"`
	// StoreImport is the import path of the generated persistence layer.
	StoreImport string `json:"store_import"`
	// ModelImport is the import path of the generated model package, whose
	// types this layer returns rather than copying into its own.
	ModelImport string `json:"model_import"`
	// StubDir is where the hand-owned service files go, relative to the project
	// root. {table} and {Table} are substituted. Empty writes no stubs.
	StubDir string `json:"stub_dir"`
	// StubPackage names the package a stub declares. Empty uses the table name.
	StubPackage string `json:"stub_package"`
	// APIImport is the import path of this generated package, so a stub in a
	// different directory can refer back to it.
	APIImport string `json:"api_import"`
}

// Generator emits the API layer.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "service-go" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "API types, service interfaces, and a working default implementation"
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
		cfg.Package = "api"
	}
	if cfg.StoreImport == "" {
		return nil, fmt.Errorf("store_import is required: the default service calls the generated repositories")
	}
	if cfg.ModelImport == "" {
		return nil, fmt.Errorf("model_import is required: the API returns the model's types rather than copies of them")
	}

	e := &emitter{doc: doc, cfg: cfg, root: opts.Root, namer: naming.New(naming.Config{})}

	var artifacts []gen.Artifact

	base, err := e.baseFile()
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, base)

	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		// A table can be worth generating persistence for and not worth
		// exposing. rig_account_token is the example: a REST interface for the
		// session table is not a feature.
		if res.Unexposed {
			continue
		}

		types, err := e.typesFile(res)
		if err != nil {
			return nil, err
		}
		service, err := e.serviceFile(res)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, types, service)

		if cfg.StubDir != "" {
			stub, err := e.stubFile(res)
			if err != nil {
				return nil, err
			}
			artifacts = append(artifacts, stub)
		}
	}

	return artifacts, nil
}

const (
	objectError      = "Error"
	objectPagination = "Pagination"
)

type emitter struct {
	doc   *ir.Document
	cfg   Options
	root  string
	namer *naming.Namer
}

// store imports the persistence package and returns its qualifier.
func (e *emitter) store(b *gobuf.Buf) string { return b.Import(e.cfg.StoreImport) }

// model imports the model package and returns its qualifier.
func (e *emitter) model(b *gobuf.Buf) string { return b.Import(e.cfg.ModelImport) }

// entity is the model's name for a resource, qualified for use here.
func (e *emitter) entity(b *gobuf.Buf, res *ir.Resource) string {
	return e.model(b) + "." + res.Name
}

// object returns a named object from the document.
func (e *emitter) object(name string) *ir.Object { return e.doc.Object(name) }

// objectRef is how this package names an object in Go.
//
// The filter shapes live in the model, because the repository takes one and the
// service is what hands it over. Everything else is declared here. Qualifying
// from the object's origin rather than from a list of names means a shape that
// moves packages moves in one place.
func (e *emitter) objectRef(b *gobuf.Buf, name string) string {
	if obj := e.object(name); obj != nil && obj.Origin == ir.OriginFilter {
		return e.model(b) + "." + name
	}
	return name
}

// goType renders a field's Go type, qualifying whatever the model declares.
//
// The model's types are reused rather than mirrored. A second identical enum
// with conversions in both directions would be more layers and no more safety.
func (e *emitter) goType(b *gobuf.Buf, f ir.Field) string {
	return genutil.GoType(b, f, func() string { return e.model(b) })
}

func artifact(path string, b *gobuf.Buf, mode gen.WriteMode) (gen.Artifact, error) {
	return genutil.Artifact(path, b, mode)
}

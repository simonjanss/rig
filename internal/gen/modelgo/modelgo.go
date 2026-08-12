// Package modelgo generates the model layer: the entity, its enums, its query
// types, and the inputs that change it.
//
// It exists because the persistence layer and the API layer were each emitting
// their own copy of the same row, with a field-by-field conversion between
// them. Two definitions of one thing is two places to add a column and one
// place to forget, and the forgetting is silent — the field simply stops being
// returned.
//
// So there is one definition, in a package both layers import, and no
// conversion at all. What each layer keeps is what only it can know: how a row
// is read and written, and how a request becomes one.
package modelgo

import (
	"context"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

func init() { gen.Register(New()) }

// Options configure the generator.
type Options struct {
	// Package is the Go package the generated files declare.
	Package string `json:"package"`
}

// Generator emits the model layer.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "model-go" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "the shared entity, its enums, its query types, and its inputs"
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
		cfg.Package = "model"
	}

	e := &emitter{doc: doc, pkg: cfg.Package}

	base, err := e.baseFile()
	if err != nil {
		return nil, err
	}
	artifacts := []gen.Artifact{base}

	for _, enum := range doc.API.Enums {
		// ErrorCode belongs to the API layer. It is not a column's type and
		// never reaches the model.
		if enum.PgType == "" {
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
		if res.Storage == nil {
			continue
		}

		entity, err := e.entityFile(res)
		if err != nil {
			return nil, err
		}
		inputs, err := e.inputFile(res)
		if err != nil {
			return nil, err
		}
		queries, err := e.queryFile(res)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, entity, inputs, queries)
	}

	return artifacts, nil
}

// someInput names an input, for the package documentation to point at.
func (e *emitter) someInput() string {
	for i := range e.doc.API.Resources {
		if e.doc.API.Resources[i].Storage != nil {
			return e.doc.API.Resources[i].Name + "CreateInputError"
		}
	}
	return "<Resource>CreateInputError"
}

// emitter carries what every file needs.
type emitter struct {
	doc *ir.Document
	pkg string
}

// table returns the table behind a resource.
func (e *emitter) table(res *ir.Resource) *ir.Table { return e.doc.Table(res.Storage.Table) }

// goType renders a field's type. Inside the model package every name is local,
// so there is no qualifier to add.
func (e *emitter) goType(b *gobuf.Buf, f ir.Field) string { return genutil.GoType(b, f, nil) }

func (e *emitter) artifact(path string, b *gobuf.Buf) (gen.Artifact, error) {
	return genutil.Artifact(path, b, gen.Overwrite)
}

// baseFile carries the package documentation and the shared validation helpers.
func (e *emitter) baseFile() (gen.Artifact, error) {
	b := gobuf.New(e.pkg)
	b.Doc("Package " + e.pkg + " is the generated model layer: one definition of each " +
		"entity, shared by the persistence layer that reads it and the API layer that " +
		"returns it. Nothing here knows about SQL or HTTP, which is what lets both " +
		"import it.\n\n" +
		"A validation failure is one type per input — " + e.someInput() + ", say " +
		"— whose members are the input's members. What goes in them is " +
		"rigerr.FieldError, which is the same everywhere and so is not generated " +
		"here.")

	return e.artifact("model.gen.go", b)
}

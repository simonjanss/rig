// Package persistgo generates the persistence layer: a repository interface and
// its pgx implementation.
//
// The entity, its enums, its query types, and its inputs come from the model
// package, which the API layer imports too. What is left here is everything
// that is only true of a database: how a row is read, how a query becomes a
// WHERE clause, and how a write is made safe.
//
// The generated repository is the floor the service layer stands on. Everything
// it does that the service layer does not have to think about — scoping by
// tenant, excluding soft-deleted rows, snapshotting before an update, stamping
// the actor, writing the audit entry — is done here precisely so that forgetting
// it is not possible.
package persistgo

import (
	"context"
	"fmt"

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
	// ModelImport is the import path of the generated model package. It is
	// required: a repository with no entity to scan into is not a repository.
	ModelImport string `json:"model_import"`
}

// Generator emits the persistence layer.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "persist-go" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "the repository interface and its pgx implementation"
}

// Version implements [gen.Generator].
func (*Generator) Version() string { return "1" }

// runtimeModule is the import path generated code depends on.
const runtimeModule = genutil.RuntimeModule

// Generate implements [gen.Generator].
func (g *Generator) Generate(_ context.Context, doc *ir.Document, opts gen.Options) ([]gen.Artifact, error) {
	cfg, err := gen.Decode[Options](opts)
	if err != nil {
		return nil, err
	}
	if cfg.Package == "" {
		cfg.Package = "store"
	}
	if cfg.ModelImport == "" {
		return nil, fmt.Errorf("model_import is required: the repository scans into the model's types")
	}

	e := &emitter{doc: doc, pkg: cfg.Package, modelImport: cfg.ModelImport}

	var artifacts []gen.Artifact

	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Storage == nil {
			continue
		}

		repo, err := e.repositoryFile(res)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, repo)
	}

	store, err := e.storeFile()
	if err != nil {
		return nil, err
	}
	artifacts = append(artifacts, store)

	return artifacts, nil
}

// emitter carries what every file needs.
type emitter struct {
	doc         *ir.Document
	pkg         string
	modelImport string
}

// model imports the model package and returns its qualifier.
func (e *emitter) model(b *gobuf.Buf) string { return b.Import(e.modelImport) }

// entity is the model's name for a resource, qualified for use here.
func (e *emitter) entity(b *gobuf.Buf, res *ir.Resource) string {
	return e.model(b) + "." + res.Name
}

// table returns the table behind a resource.
func (e *emitter) table(res *ir.Resource) *ir.Table {
	return e.doc.Table(res.Storage.Table)
}

// storedFields are the resource's fields that map to a column, in column order.
func storedFields(res *ir.Resource) []ir.ResourceField { return genutil.StoredFields(res) }

// writableFields are the fields a client supplies for one operation.
func writableFields(res *ir.Resource, op string) []ir.ResourceField {
	return genutil.WritableFields(res, op)
}

// goType renders a field's Go type, qualifying anything the model declares.
func (e *emitter) goType(b *gobuf.Buf, f ir.Field) string {
	return genutil.GoType(b, f, func() string { return e.model(b) })
}

// artifact wraps a finished buffer.
func artifact(path string, b *gobuf.Buf) (gen.Artifact, error) {
	return genutil.Artifact(path, b, gen.Overwrite)
}

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
	"strings"

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

	// Before anything is emitted, because the answer is about the whole document
	// and half a package is worse than none.
	for _, res := range e.cachedResources() {
		if bad := uncopyableFields(res); len(bad) > 0 {
			names := make([]string, 0, len(bad))
			for _, f := range bad {
				names = append(names, f.Name+" ("+f.GoType+")")
			}
			return nil, fmt.Errorf(
				"%s sets `cache: true` but rig cannot give each caller its own copy of %s: "+
					"a cached read has to be indistinguishable from a fresh one, and a copy of "+
					"the row would share whatever is inside that type. Remove `cache: true`, or "+
					"change the column: a jsonb one read as raw JSON rather than through a "+
					"`go_type` is copyable, and so is an array of any scalar but numeric, bytea "+
					"and jsonb",
				res.Storage.Table, strings.Join(names, ", "))
		}
	}

	var artifacts []gen.Artifact

	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Storage == nil || res.Unreachable() {
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

// artifact wraps a finished buffer.
func artifact(path string, b *gobuf.Buf) (gen.Artifact, error) {
	return genutil.Artifact(path, b, gen.Overwrite)
}

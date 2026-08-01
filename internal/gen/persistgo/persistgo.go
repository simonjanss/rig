// Package persistgo generates the persistence layer: models, typed queries, a
// repository interface, and its pgx implementation.
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

// Generator emits the persistence layer.
type Generator struct{}

// New builds the generator.
func New() *Generator { return &Generator{} }

// Name implements [gen.Generator].
func (*Generator) Name() string { return "persist-go" }

// Description implements [gen.Generator].
func (*Generator) Description() string {
	return "Go models, typed queries, repository interface and pgx implementation"
}

// Version implements [gen.Generator].
func (*Generator) Version() string { return "1" }

// runtimeModule is the import path generated code depends on.
const runtimeModule = "github.com/simonjanss/rig/runtime"

// Generate implements [gen.Generator].
func (g *Generator) Generate(_ context.Context, doc *ir.Document, opts gen.Options) ([]gen.Artifact, error) {
	cfg, err := gen.Decode[Options](opts)
	if err != nil {
		return nil, err
	}
	if cfg.Package == "" {
		cfg.Package = "store"
	}

	e := &emitter{doc: doc, pkg: cfg.Package}

	var artifacts []gen.Artifact

	for _, enum := range doc.API.Enums {
		// ErrorCode belongs to the API layer; the persistence layer never
		// stores one.
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

		model, err := e.modelFile(res)
		if err != nil {
			return nil, err
		}
		queries, err := e.queryFile(res)
		if err != nil {
			return nil, err
		}
		repo, err := e.repositoryFile(res)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, model, queries, repo)
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
	doc *ir.Document
	pkg string
}

// table returns the table behind a resource.
func (e *emitter) table(res *ir.Resource) *ir.Table {
	return e.doc.Table(res.Storage.Table)
}

// storedFields are the resource's fields that map to a column, in column order.
func storedFields(res *ir.Resource) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range res.Fields {
		if f.Column != nil {
			out = append(out, f)
		}
	}
	return out
}

// writableFields are the fields a client supplies for one operation.
func writableFields(res *ir.Resource, op string) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range storedFields(res) {
		if f.ReadOnly || !f.In(op) {
			continue
		}
		if op == ir.FieldOpUpdate && f.Immutable {
			continue
		}
		out = append(out, f)
	}
	return out
}

// goType renders a field's Go type, importing whatever it needs.
//
// The IR carries the type as source text — "*time.Time", "[]string" — so the
// package qualifier has to be recognized and registered rather than parsed out
// of a structured form.
func (e *emitter) goType(b *gobuf.Buf, f ir.Field) string {
	t := f.GoType
	if t == "" {
		t = "any"
	}

	prefix := ""
	for strings.HasPrefix(t, "*") || strings.HasPrefix(t, "[]") {
		if strings.HasPrefix(t, "*") {
			prefix += "*"
			t = t[1:]
			continue
		}
		prefix += "[]"
		t = t[2:]
	}

	pkg, name, qualified := strings.Cut(t, ".")
	if !qualified {
		return prefix + t
	}

	importPath, known := runtimeImports[pkg]
	if !known {
		// An unrecognized qualifier is a named type from the application's own
		// package, which needs no import.
		return prefix + t
	}
	return prefix + b.Import(importPath) + "." + name
}

// runtimeImports maps the package qualifiers that appear in IR Go types to
// their import paths.
var runtimeImports = map[string]string{
	"time":    "time",
	"uuid":    "github.com/google/uuid",
	"json":    "encoding/json",
	"netip":   "net/netip",
	"pgtype":  "github.com/jackc/pgx/v5/pgtype",
	"patch":   runtimeModule + "/patch",
	"query":   runtimeModule + "/query",
	"tenancy": runtimeModule + "/tenancy",
}

// elemType strips one pointer from a type, for the element inside a slice.
func elemType(t string) string { return strings.TrimPrefix(t, "*") }

// artifact wraps a finished buffer.
func artifact(path string, b *gobuf.Buf) (gen.Artifact, error) {
	content, err := b.Bytes()
	if err != nil {
		return gen.Artifact{}, fmt.Errorf("%s: %w", path, err)
	}
	return gen.Artifact{Path: path, Content: content, Mode: gen.Overwrite}, nil
}

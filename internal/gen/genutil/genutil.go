// Package genutil holds what more than one Go generator needs.
//
// It exists because rendering a field's type was written three times — once in
// the persistence generator, once in the API generator, once more when the
// model layer arrived — and the three had already begun to disagree about
// enums. A shared answer is not shared code for its own sake; it is the only
// way three generators can emit the same type for the same field.
package genutil

import (
	"fmt"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// RuntimeModule is the import path generated code depends on.
const RuntimeModule = "github.com/simonjanss/rig/runtime"

// knownImports maps the package qualifiers that appear in IR Go types to their
// import paths.
var knownImports = map[string]string{
	"time":    "time",
	"uuid":    "github.com/google/uuid",
	"json":    "encoding/json",
	"netip":   "net/netip",
	"pgtype":  "github.com/jackc/pgx/v5/pgtype",
	"patch":   RuntimeModule + "/patch",
	"query":   RuntimeModule + "/query",
	"tenancy": RuntimeModule + "/tenancy",
}

// GoType renders a field's Go type, importing whatever it needs.
//
// The IR carries the type as source text — "*time.Time", "[]string" — so a
// package qualifier has to be recognized and registered rather than parsed out
// of a structured form.
//
// modelPkg qualifies a type the model package declares, which is every enum. It
// is called only when there is something to qualify — importing on every field
// would leave an unused import in any file whose types are all builtin.
//
// Pass nil inside the model package itself, where those names are local.
func GoType(b *gobuf.Buf, f ir.Field, modelPkg func() string) string {
	t := f.GoType
	if t == "" {
		t = "any"
	}

	prefix := ""
	for strings.HasPrefix(t, "*") || strings.HasPrefix(t, "[]") {
		if strings.HasPrefix(t, "*") {
			prefix, t = prefix+"*", t[1:]
			continue
		}
		prefix, t = prefix+"[]", t[2:]
	}

	if pkg, name, qualified := strings.Cut(t, "."); qualified {
		if importPath, known := knownImports[pkg]; known {
			return prefix + b.Import(importPath) + "." + name
		}
		// An unrecognized qualifier is a named type from the application's own
		// package, which needs no import.
		return prefix + t
	}

	// A bare name that the document declares as an enum lives in the model
	// package, wherever this is being emitted.
	if f.TypeKind == ir.TypeKindEnum && modelPkg != nil {
		return prefix + modelPkg() + "." + t
	}
	return prefix + t
}

// ElemType strips one pointer, for the value inside a wrapper.
func ElemType(t string) string { return strings.TrimPrefix(t, "*") }

// Describe falls back to a generated sentence when there is no comment, so
// every exported type carries documentation even before anyone writes any.
func Describe(comment, fallback string) string {
	if strings.TrimSpace(comment) != "" {
		return comment
	}
	return fallback
}

// StoredFields are the resource's fields that map to a column, in column order.
func StoredFields(res *ir.Resource) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range res.Fields {
		if f.Column != nil {
			out = append(out, f)
		}
	}
	return out
}

// WritableFields are the fields a client supplies for one operation.
func WritableFields(res *ir.Resource, op string) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range StoredFields(res) {
		if f.ReadOnly || !f.In(op) {
			continue
		}
		// An immutable field may be set once and never changed, so it is not
		// absent from the update input by rejection — it is simply not there.
		if op == ir.FieldOpUpdate && f.Immutable {
			continue
		}
		out = append(out, f)
	}
	return out
}

// ModelInputName is which of the model's input types an endpoint's body is,
// or empty when the body is a shape of its own.
//
// Create and Update decode straight into the model's inputs: what a client
// sends and what the repository takes are the same fields, so a wire struct in
// between would be a third copy of the entity and a third place to forget a
// column.
//
// It lives here because the API generator and the server generator both have to
// answer this, and they have to answer it the same way: one decodes into the
// type the other declared.
func ModelInputName(ep *ir.Endpoint) string {
	if ep.Impl.Kind != ir.EndpointGenerated {
		return ""
	}
	switch ep.Name {
	case ir.OpCreate:
		return ir.OpCreate
	case ir.OpUpdate:
		return ir.OpUpdate
	}
	return ""
}

// UsesModelInput reports whether an endpoint's body is one of the model's
// input types.
func UsesModelInput(ep *ir.Endpoint) bool { return ModelInputName(ep) != "" }

// Artifact wraps a finished buffer.
func Artifact(path string, b *gobuf.Buf, mode gen.WriteMode) (gen.Artifact, error) {
	content, err := b.Bytes()
	if err != nil {
		return gen.Artifact{}, fmt.Errorf("%s: %w", path, err)
	}
	return gen.Artifact{Path: path, Content: content, Mode: mode}, nil
}

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
	"time"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
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

// PatchType picks the update wrapper a column's nullability calls for, around an
// element type the caller has already rendered.
//
// Which wrapper a column gets is not a matter of taste: a client sending
// patch.Nullable where the server decodes patch.Optional is a null the server
// refuses, and the two ends have to agree. So the model generator and the client
// generator ask the same function rather than each deciding.
func PatchType(patchPkg string, f ir.Field, elem string) string {
	kind := "Optional"
	if f.IsNullable() {
		kind = "Nullable"
	}
	return patchPkg + "." + kind + "[" + ElemType(elem) + "]"
}

// GoDuration renders a duration as Go source a person can check against the
// configuration file.
//
// 30 * 24 * time.Hour rather than 2592000000000000: somebody reading the
// generated wiring has to be able to see that it says thirty days, because the
// whole point of configuring this in a file is that the number is reviewable.
func GoDuration(b *gobuf.Buf, d ir.Duration) string {
	v := d.Duration()
	if v == 0 {
		return "0"
	}

	timePkg := b.Import("time")

	var terms []string
	for _, unit := range []struct {
		expr string
		size time.Duration
	}{
		{"24*" + timePkg + ".Hour", 24 * time.Hour},
		{timePkg + ".Hour", time.Hour},
		{timePkg + ".Minute", time.Minute},
		{timePkg + ".Second", time.Second},
		{timePkg + ".Millisecond", time.Millisecond},
		{timePkg + ".Microsecond", time.Microsecond},
		{timePkg + ".Nanosecond", time.Nanosecond},
	} {
		n := v / unit.size
		if n == 0 {
			continue
		}
		v -= n * unit.size

		if n == 1 && !strings.HasPrefix(unit.expr, "24*") {
			terms = append(terms, unit.expr)
			continue
		}
		terms = append(terms, fmt.Sprintf("%d*%s", n, unit.expr))
	}

	return strings.Join(terms, " + ")
}

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

// JSONTag renders a field's struct tag.
//
// A nullable or empty field is omitted rather than sent as a wall of nulls, and
// a required one is always present so the receiver can rely on the key.
//
// It is here because the model layer, the service layer and the Go client all
// tag the same field, and a field the client omits and the server requires is a
// request refused for a reason neither end can see. It is deliberately not the
// rule [BodyRequired] applies, which ignores Default: the two answer different
// questions — may I leave this out of the JSON I send, versus must the server
// have received it.
func JSONTag(f ir.Field) string {
	tag := f.Wire
	if f.IsNullable() || f.IsArray() {
		tag += ",omitempty"
	}
	return gobuf.Quote(tag)
}

// PointerTo makes a rendered Go type optional, unless it already is: a slice and
// a pointer both have a nil to mean absent, and a pointer to either would be a
// second one.
//
// Note that internal/compile keeps a copy of this rule. That is not an oversight
// waiting to be tidied: the compiler must not import a generator package, and
// its copy also maps the empty type to the empty string, which is a contract for
// a field the IR has not typed yet rather than one this ever sees.
func PointerTo(t string) string {
	if strings.HasPrefix(t, "*") || strings.HasPrefix(t, "[]") {
		return t
	}
	return "*" + t
}

// ExpandLayout fills the placeholders in a stub directory template — {table},
// {Table}, {tables} — from the resource a stub is being written for.
//
// service-go's service stub and server-go's shape scopes both go into a
// project's own tree, from the same `layout` settings and — since the two now
// share a `stub_dir` — usually into the same directory, so the path one of them
// computes has to be the path the other would.
func ExpandLayout(namer *naming.Namer, res *ir.Resource, tmpl string) string {
	table := res.Storage.Table
	return strings.NewReplacer(
		"{table}", table,
		"{Table}", namer.Go(table),
		"{tables}", naming.Snake(namer.Plural(table)),
	).Replace(tmpl)
}

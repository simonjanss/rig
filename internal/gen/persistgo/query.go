package persistgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// queryFile emits the typed query for a resource.
//
// The shape mirrors the API's filter objects, so the service layer copies
// fields across rather than translating between two different vocabularies.
// Splitting operators into separate structs is what keeps it typed: a range
// struct only carries orderable columns, so "created_at contains 3" is not
// something anyone can write.
func (e *emitter) queryFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)
	queryPkg := b.Import(runtimeModule + "/query")

	fields := storedFields(res)

	b.Comment(res.Name + "Query selects rows.\n\n" +
		"Fill in the fields you care about and leave the rest alone; an empty " +
		"query matches every row the caller is allowed to see. Nest queries to " +
		"mix AND and OR.")
	b.L("type %sQuery struct {", res.Name)
	b.L("Params %sQueryParams", res.Name)
	b.NL()
	b.L("// Nested are sub-queries combined with this one.")
	b.L("Nested []%sQuery", res.Name)
	b.L("// OrCondition combines this query's conditions with OR instead of AND.")
	b.L("OrCondition bool")
	b.NL()
	b.L("// OrderBy replaces the table's default ordering.")
	b.L("OrderBy []%s.Order", queryPkg)
	b.L("Limit   int")
	b.L("Offset  int")
	b.L("}")
	b.NL()

	b.Comment(res.Name + "QueryParams are the conditions, one struct per operator.")
	b.L("type %sQueryParams struct {", res.Name)
	b.L("Equals    *%sFields", res.Name)
	b.L("NotEquals *%sFields", res.Name)
	b.NL()
	b.L("GreaterThan    *%sComparable", res.Name)
	b.L("SmallerThan    *%sComparable", res.Name)
	b.L("GreaterOrEqual *%sComparable", res.Name)
	b.L("SmallerOrEqual *%sComparable", res.Name)
	b.NL()
	b.L("In    *%sIn", res.Name)
	b.L("NotIn *%sIn", res.Name)
	b.NL()
	b.L("Like    *%sLike", res.Name)
	b.L("NotLike *%sLike", res.Name)
	b.NL()
	b.L("Null    *%sNull", res.Name)
	b.L("NotNull *%sNull", res.Name)
	b.L("}")
	b.NL()

	e.fieldStruct(b, res, res.Name+"Fields", "Exact-match values.", fields, valueOf)
	e.fieldStruct(b, res, res.Name+"Comparable", "Values for ordering comparisons.",
		filterFields(fields, comparableField), valueOf)
	e.fieldStruct(b, res, res.Name+"In", "Sets of values.",
		filterFields(fields, func(f ir.ResourceField) bool { return !f.IsArray() }), sliceOf)
	e.fieldStruct(b, res, res.Name+"Like", "Patterns, matched case-insensitively.",
		filterFields(fields, stringField), stringPointer)
	e.fieldStruct(b, res, res.Name+"Null", "Presence flags: true means the column must be null.",
		filterFields(fields, func(f ir.ResourceField) bool { return f.IsNullable() }), boolPointer)

	e.constructors(b, res)
	e.conditionBuilder(b, res, fields)

	return artifact(naming.Snake(res.Name)+"_query.gen.go", b)
}

// fieldKind renders the Go type one operator's struct uses for a field.
type fieldKind func(e *emitter, b *gobuf.Buf, f ir.ResourceField) string

func valueOf(e *emitter, b *gobuf.Buf, f ir.ResourceField) string {
	t := e.goType(b, f.Field)
	if t[0] == '*' || t[0] == '[' {
		return t
	}
	return "*" + t
}

func sliceOf(e *emitter, b *gobuf.Buf, f ir.ResourceField) string {
	return "[]" + elemType(e.goType(b, f.Field))
}

func stringPointer(*emitter, *gobuf.Buf, ir.ResourceField) string { return "*string" }

func boolPointer(*emitter, *gobuf.Buf, ir.ResourceField) string { return "*bool" }

func (e *emitter) fieldStruct(b *gobuf.Buf, res *ir.Resource, name, doc string, fields []ir.ResourceField, kind fieldKind) {
	b.Comment(name + " carries " + doc)
	b.L("type %s struct {", name)
	for _, f := range fields {
		b.L("%s %s", f.Name, kind(e, b, f))
	}
	b.L("}")
	b.NL()
}

func filterFields(fields []ir.ResourceField, keep func(ir.ResourceField) bool) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range fields {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out
}

func comparableField(f ir.ResourceField) bool {
	if f.IsArray() {
		return false
	}
	switch f.Type {
	case ir.TypeInt, ir.TypeInt64, ir.TypeFloat64, ir.TypeDecimal,
		ir.TypeDate, ir.TypeTime, ir.TypeTimestamp:
		return true
	default:
		return false
	}
}

func stringField(f ir.ResourceField) bool { return f.Type == ir.TypeString && !f.IsArray() }

// constructors give each operator struct a maker, so filling one in is two
// lines rather than a nested composite literal.
func (e *emitter) constructors(b *gobuf.Buf, res *ir.Resource) {
	for _, suffix := range []string{"Fields", "Comparable", "In", "Like", "Null"} {
		name := res.Name + suffix
		b.Comment("New" + name + " builds an empty " + name + " to fill in.")
		b.L("func New%s() *%s { return &%s{} }", name, name, name)
		b.NL()
	}

	b.Comment("New" + res.Name + "Query builds an empty query.")
	b.L("func New%sQuery() %sQuery { return %sQuery{} }", res.Name, res.Name, res.Name)
	b.NL()
}

// conditionBuilder emits the translation from the typed query to the runtime's
// condition tree.
//
// It is generated rather than done by reflection so that the caller's side
// stays fully typed and the translation is readable when something goes wrong.
func (e *emitter) conditionBuilder(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	queryPkg := b.Import(runtimeModule + "/query")

	b.Comment("group turns the query into the condition tree the runtime renders.")
	b.L("func (q %sQuery) group() %s.Group {", res.Name, queryPkg)
	b.L("g := %s.Group{Or: q.OrCondition}", queryPkg)
	b.NL()

	type operator struct {
		param string
		fn    string
		// text marks the pattern operators, whose parameter is always a
		// *string regardless of the column's own type.
		text bool
		keep func(ir.ResourceField) bool
	}
	all := func(ir.ResourceField) bool { return true }

	for _, op := range []operator{
		{"Equals", "Eq", false, all},
		{"NotEquals", "Ne", false, all},
		{"GreaterThan", "Gt", false, comparableField},
		{"SmallerThan", "Lt", false, comparableField},
		{"GreaterOrEqual", "Gte", false, comparableField},
		{"SmallerOrEqual", "Lte", false, comparableField},
		{"In", "In", false, func(f ir.ResourceField) bool { return !f.IsArray() }},
		{"NotIn", "NotIn", false, func(f ir.ResourceField) bool { return !f.IsArray() }},
		{"Like", "Like", true, stringField},
		{"NotLike", "NotLike", true, stringField},
	} {
		selected := filterFields(fields, op.keep)
		if len(selected) == 0 {
			continue
		}

		b.L("if p := q.Params.%s; p != nil {", op.param)
		for _, f := range selected {
			column := gobuf.Quote(f.Column.Name)
			switch op.param {
			case "In", "NotIn":
				b.L("if len(p.%s) > 0 { g.Add(%s.%s(%s, p.%s)) }", f.Name, queryPkg, op.fn, column, f.Name)
			default:
				// A pointer field distinguishes "no condition" from "equal to
				// the zero value", which is why every one of these is a
				// pointer even when the column is not nullable.
				b.L("if p.%s != nil { g.Add(%s.%s(%s, %s)) }",
					f.Name, queryPkg, op.fn, column, deref(f, op.text))
			}
		}
		b.L("}")
	}

	nullable := filterFields(fields, func(f ir.ResourceField) bool { return f.IsNullable() })
	if len(nullable) > 0 {
		for _, spec := range []struct{ param, whenTrue, whenFalse string }{
			{"Null", "IsNull", "NotNull"},
			{"NotNull", "NotNull", "IsNull"},
		} {
			b.L("if p := q.Params.%s; p != nil {", spec.param)
			for _, f := range nullable {
				column := gobuf.Quote(f.Column.Name)
				b.L("if p.%s != nil {", f.Name)
				b.L("if *p.%s { g.Add(%s.%s(%s)) } else { g.Add(%s.%s(%s)) }",
					f.Name, queryPkg, spec.whenTrue, column, queryPkg, spec.whenFalse, column)
				b.L("}")
			}
			b.L("}")
		}
	}

	b.NL()
	b.L("for _, n := range q.Nested {")
	b.L("g.Nest(n.group())")
	b.L("}")
	b.L("return g")
	b.L("}")
	b.NL()
}

// deref renders the expression that reads a pointer field's value.
func deref(f ir.ResourceField, forceString bool) string {
	if forceString {
		return "*p." + f.Name
	}
	// A field that is already nilable — a slice, or a nullable column's pointer
	// — is passed as-is; anything else is dereferenced.
	t := f.GoType
	if len(t) > 0 && (t[0] == '*' || t[0] == '[') {
		return "p." + f.Name
	}
	return "*p." + f.Name
}

package modelgo

import (
	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The filter shapes, in the order they are emitted. The names are the IR's, so
// what a client sends and what a repository takes are one type under one name.
// FilterWithout is emitted only for a resource that has relations, and
// filterStruct skips a shape the document does not carry.
var filterSuffixes = []string{
	"FilterEquals", "FilterRange", "FilterContains", "FilterLike", "FilterNull",
	"FilterWithout", "Filter",
}

// queryFile emits the typed filter for a resource, and the page that says how
// much of the result to return.
//
// The filter lives in the model rather than in the persistence layer because
// the service builds one and the repository executes it — and the service is
// not supposed to import the package that knows about SQL. What stays behind is
// the rendering: turning this into a WHERE clause is the repository's business,
// and nothing here mentions it.
//
// It is also the wire shape. Search's request body is this type, not a copy of
// it with a conversion in between: the copy was a field-by-field transcription
// between two structs with identical fields, and the only thing it could ever
// do differently was miss one.
//
// Splitting operators into separate structs is what keeps the filter typed: a
// range struct only carries orderable columns, so "created_at contains 3" is
// not something anyone can write.
func (e *emitter) queryFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)

	for _, suffix := range filterSuffixes {
		e.filterStruct(b, res, res.Name+suffix)
	}

	e.pageType(b, res)
	e.orderType(b, res)
	e.constructors(b, res)

	return e.artifact(naming.Snake(res.Name)+"_query.gen.go", b)
}

// filterStruct emits one filter shape from the object the compiler froze.
//
// Reading the shape out of the document rather than deriving it again here is
// what keeps the Go type and the documented wire shape the same thing. Deriving
// it twice is how a field ends up in the OpenAPI document with nowhere to put
// it, or in the struct with nothing to describe it.
func (e *emitter) filterStruct(b *gobuf.Buf, res *ir.Resource, name string) {
	obj := e.doc.Object(name)
	if obj == nil {
		return
	}

	doc := obj.Description
	if doc == "" {
		doc = name + " is part of a " + res.Name + " filter."
	}
	b.Comment(doc)

	b.L("type %s struct {", name)
	for _, f := range obj.Fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name, e.filterFieldType(b, f), gobuf.Quote(f.Wire+",omitempty"))
	}
	b.L("}")
	b.NL()
}

// filterFieldType renders a filter field. A reference to another filter shape
// stays a pointer to that shape — nil is how a condition says it was not asked
// for — and everything else follows the ordinary rules.
func (e *emitter) filterFieldType(b *gobuf.Buf, f ir.Field) string {
	if kind, ok := e.doc.TypeKindOf(f.Type); ok && kind == ir.TypeKindObject {
		if f.IsArray() {
			return "[]" + f.Type
		}
		return "*" + f.Type
	}
	return e.goType(b, f)
}

// pageType emits what a read returns rather than what it selects.
//
// Separate from the filter, because the filter is the wire shape a client
// sends in a body and these three arrive as query parameters. Keeping them in
// one struct meant a client could ask for a page inside a filter and an
// ordering it was never offered, and a repository could not tell which of the
// two a caller had actually set.
func (e *emitter) pageType(b *gobuf.Buf, res *ir.Resource) {
	b.Comment(res.Name + "Page says which slice of a result to return, and in what order.\n\n" +
		"The zero value is the first page at the repository's default size, in the " +
		"table's configured order, which is what a caller who has not thought about " +
		"it wants.")
	b.L("type %sPage struct {", res.Name)
	b.L("Limit  int")
	b.L("Offset int")
	b.NL()
	b.L("// OrderBy replaces the table's default ordering.")
	b.L("OrderBy []%sOrder", res.Name)
	b.L("}")
	b.NL()
}

// orderType emits the ordering terms, one per sortable column.
//
// Named constants rather than a free-text column and direction: an ordering
// that can be written wrong is an ordering that reaches the database wrong.
func (e *emitter) orderType(b *gobuf.Buf, res *ir.Resource) {
	fields := FilterFields(genutil.StoredFields(res), func(f ir.ResourceField) bool {
		return !f.IsArray()
	})

	b.Comment(res.Name + "Order is one clause of an ordering.\n\n" +
		"Relation names the relation the column belongs to, and is empty for this " +
		"table's own columns. The repository turns it into a left join, so a row " +
		"whose related row is missing still appears — at one end of the order " +
		"rather than not at all.")
	b.L("type %sOrder struct {", res.Name)
	b.L("Relation string")
	b.L("Column   string")
	b.L("Desc     bool")
	b.L("}")
	b.NL()

	b.L("// The orderings a %s supports.", res.Name)
	b.L("var (")
	for _, f := range fields {
		column := gobuf.Quote(f.Column.Name)
		b.L("%sOrder%sAsc  = %sOrder{Column: %s}", res.Name, f.Name, res.Name, column)
		b.L("%sOrder%sDesc = %sOrder{Column: %s, Desc: true}", res.Name, f.Name, res.Name, column)
	}
	b.L(")")
	b.NL()

	e.orderLifts(b, res)
}

// orderLifts emit one function per relation, turning an ordering of the far side
// into an ordering of this one.
//
// A function rather than a constant per related column, for two reasons. The
// names would collide — a fixture has an away_team_id column and an AwayTeam
// relation whose target has an id, and both want to be called
// FixtureOrderAwayTeamID. And the far side already has a constant for each of
// its own columns, so lifting one keeps a single definition of what Team can be
// ordered by rather than a copy of it on every table that points at Team.
func (e *emitter) orderLifts(b *gobuf.Buf, res *ir.Resource) {
	for _, r := range e.orderRelations(res) {
		name := res.Name + "Order" + r.name

		b.Comment(name + " orders a " + res.Name + " by a column of its " + r.name + ".\n\n" +
			"Take the term from " + r.target + "'s own orderings:\n\n\t" +
			name + "(" + r.target + "Order" + r.columns[0].Name + "Desc)\n\n" +
			"The repository reaches it with a left join, so a " + res.Name +
			" with no " + r.name + " sorts to one end rather than vanishing. Only " +
			r.target + "'s own columns can be used; an ordering that itself reaches " +
			"across a relation is refused, because one join is where this stops.")
		b.L("func %s(o %sOrder) %sOrder {", name, r.target, res.Name)
		b.L("return %sOrder{Relation: %s, Column: o.Column, Desc: o.Desc}",
			res.Name, gobuf.Quote(r.name))
		b.L("}")
		b.NL()
	}
}

// orderRelation is a relation whose columns this resource can be ordered by.
type orderRelation struct {
	name    string
	target  string
	columns []ir.ResourceField
}

// orderRelations are the relations a list can be ordered through.
//
// Belongs-to only, and for the same reason a filter across a has-many is a
// subquery: ordering by a column of a table that has many rows per row of this
// one is not a question with an answer until you say which of them — a minimum,
// a maximum, a count — and that is an aggregate rather than an ordering.
//
// The columns come from the target's own sortable set, so a relation cannot be
// a way to order by something the target itself does not offer.
func (e *emitter) orderRelations(res *ir.Resource) []orderRelation {
	if res.Storage == nil {
		return nil
	}

	var out []orderRelation
	for _, rel := range res.Storage.Relations {
		if rel.Kind != ir.RelationBelongsTo || rel.Name == "" || rel.LinkTable != nil {
			continue
		}
		// The same exclusions the filter makes: a resource the API does not
		// expose, and the self-reference a snapshot table carries.
		target := e.doc.Resource(rel.Target)
		if target == nil || target.Unexposed || target.Storage == nil {
			continue
		}
		if snap := res.Storage.Snapshot; snap != nil && snap.FromID != nil &&
			rel.LocalColumn == snap.FromID.Name {
			continue
		}

		columns := FilterFields(genutil.StoredFields(target), func(f ir.ResourceField) bool {
			return !f.IsArray()
		})
		if len(columns) == 0 {
			continue
		}
		out = append(out, orderRelation{name: rel.Name, target: target.Name, columns: columns})
	}
	return out
}

// constructors give each shape a maker, so filling one in is two lines rather
// than a nested composite literal.
func (e *emitter) constructors(b *gobuf.Buf, res *ir.Resource) {
	for _, suffix := range filterSuffixes {
		name := res.Name + suffix
		if e.doc.Object(name) == nil {
			continue
		}
		if suffix == "Filter" {
			b.Comment("New" + name + " builds an empty filter, which matches every row the " +
				"caller is allowed to see.")
			b.L("func New%s() %s { return %s{} }", name, name, name)
			b.NL()
			continue
		}
		b.Comment("New" + name + " builds an empty " + name + " to fill in.")
		b.L("func New%s() *%s { return &%s{} }", name, name, name)
		b.NL()
	}
}

// FilterFields keeps the fields a predicate accepts.
//
// It is exported because the persistence generator has to select exactly the
// same fields when it emits the code that renders a query — two answers to
// "which columns can be compared" would produce a struct field with nowhere to
// go.
func FilterFields(fields []ir.ResourceField, keep func(ir.ResourceField) bool) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range fields {
		if keep(f) {
			out = append(out, f)
		}
	}
	return out
}

// ComparableField reports whether a column can be ordered.
func ComparableField(f ir.ResourceField) bool {
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

// StringField reports whether a column can be matched against a pattern.
func StringField(f ir.ResourceField) bool { return f.IsTextual() }

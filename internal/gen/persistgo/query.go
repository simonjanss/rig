package persistgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/ir"
)

// filterSlot is one operator field of a filter, paired with the object it points
// at.
type filterSlot struct {
	// field is the filter's own field, and object the shape it points at.
	field, object, fn string
	// text marks the pattern operators, whose parameter is always a *string
	// regardless of the column's own type.
	text bool
}

// comparisonSlots are the operators whose conditions compare a column to a
// value. The presence operators are separate because theirs is a flag rather
// than an operand, so the same field means two different conditions.
var comparisonSlots = []filterSlot{
	{field: "Equals", object: "FilterEquals", fn: "Eq"},
	{field: "NotEquals", object: "FilterEquals", fn: "Ne"},
	{field: "GreaterThan", object: "FilterRange", fn: "Gt"},
	{field: "SmallerThan", object: "FilterRange", fn: "Lt"},
	{field: "GreaterOrEqual", object: "FilterRange", fn: "Gte"},
	{field: "SmallerOrEqual", object: "FilterRange", fn: "Lte"},
	{field: "Contains", object: "FilterContains", fn: "In"},
	{field: "NotContains", object: "FilterContains", fn: "NotIn"},
	{field: "Like", object: "FilterLike", fn: "Like", text: true},
	{field: "NotLike", object: "FilterLike", fn: "NotLike", text: true},
}

// presenceSlots are the two null operators.
var presenceSlots = []filterSlot{
	{field: "Null", object: "FilterNull"},
	{field: "NotNull", object: "FilterNull"},
}

// conditionBuilder emits the translation from the model's typed query to the
// runtime's condition tree.
//
// This is the half of a query that belongs to the database. The types live in
// the model, because the service builds one; turning them into a WHERE clause
// lives here, because nothing above this layer should know there is one.
//
// It is generated rather than done by reflection so that the caller's side
// stays fully typed and the translation is readable when something goes wrong.
func (e *emitter) conditionBuilder(b *gobuf.Buf, res *ir.Resource) {
	var (
		queryPkg = b.Import(runtimeModule + "/query")
		model    = e.model(b)
		name     = queryFuncName(res)
	)

	e.relationSubFilters(b, res)

	b.Comment(name + " turns a filter into the condition tree the runtime " +
		"renders.\n\n" +
		"The scope carries what a condition needs besides the filter itself: who " +
		"is asking, which alias this level's columns belong to, and how deep the " +
		"nesting has gone. It is a value rather than three parameters because " +
		"every relation passes a changed copy of it down.")
	b.L("func %s(f %s.%sFilter, sc filterScope) (%s.Group, error) {",
		name, model, res.Name, queryPkg)
	b.L("if err := sc.ok(); err != nil { return %s.Group{}, err }", queryPkg)
	b.NL()
	b.L("g := %s.Group{Or: f.OrCondition}", queryPkg)
	b.NL()

	for _, op := range comparisonSlots {
		// The fields are read off the object the compiler froze, which is the
		// same object the model's struct was emitted from. Deriving "which
		// columns can be compared" a second time here is how a condition ends
		// up reading a struct field that does not exist.
		selected := e.filterOperands(res, op.object)
		if len(selected) == 0 {
			continue
		}

		b.L("if p := f.%s; p != nil {", op.field)
		for _, f := range selected {
			column := gobuf.Quote(f.Column.Name)
			switch op.object {
			case "FilterContains":
				b.L("if len(p.%s) > 0 { g.Add(sc.at(%s.%s(%s, p.%s))) }",
					f.Name, queryPkg, op.fn, column, f.Name)
			default:
				// A pointer field distinguishes "no condition" from "equal to
				// the zero value", which is why every one of these is a
				// pointer even when the column is not nullable.
				b.L("if p.%s != nil { g.Add(sc.at(%s.%s(%s, %s))) }",
					f.Name, queryPkg, op.fn, column, deref(f, op.text))
			}
		}
		b.L("}")
	}

	if nullable := e.filterOperands(res, "FilterNull"); len(nullable) > 0 {
		for _, spec := range []struct{ field, whenTrue, whenFalse string }{
			{"Null", "IsNull", "NotNull"},
			{"NotNull", "NotNull", "IsNull"},
		} {
			b.L("if p := f.%s; p != nil {", spec.field)
			for _, f := range nullable {
				column := gobuf.Quote(f.Column.Name)
				b.L("if p.%s != nil {", f.Name)
				b.L("if *p.%s { g.Add(sc.at(%s.%s(%s))) } else { g.Add(sc.at(%s.%s(%s))) }",
					f.Name, queryPkg, spec.whenTrue, column, queryPkg, spec.whenFalse, column)
				b.L("}")
			}
			b.L("}")
		}
	}

	e.relationConditions(b, res, queryPkg)

	b.NL()
	b.L("for _, n := range f.NestedFilters {")
	b.L("nested, err := %s(n, sc)", name)
	b.L("if err != nil { return %s.Group{}, err }", queryPkg)
	b.L("g.Nest(nested)")
	b.L("}")
	b.L("return g, nil")
	b.L("}")
	b.NL()

	e.orderBuilder(b, res)
}

// filterOperands are the resource fields one filter shape carries.
//
// The names come from the frozen object — the same one the model's struct was
// emitted from — and the column from the resource field behind each name. That
// is the only pairing that cannot drift: a field the object does not have is
// not read, and a field it has always has somewhere to read from.
func (e *emitter) filterOperands(res *ir.Resource, suffix string) []ir.ResourceField {
	obj := e.doc.Object(res.Name + suffix)
	if obj == nil {
		return nil
	}

	byName := make(map[string]ir.ResourceField, len(res.Fields))
	for _, f := range storedFields(res) {
		byName[f.Name] = f
	}

	out := make([]ir.ResourceField, 0, len(obj.Fields))
	for _, f := range obj.Fields {
		if rf, ok := byName[f.Name]; ok {
			out = append(out, rf)
		}
	}
	return out
}

// orderBuilder converts the model's ordering terms into the runtime's, and says
// which tables have to be joined to satisfy them.
//
// A term naming a relation becomes a LEFT JOIN, and left is the whole point: the
// join is there to reach a value rather than to decide anything, so a row whose
// foreign key is null sorts to one end instead of disappearing from a list it
// belongs in. An inner join here would turn "order these by their team's name"
// into "and hide the ones with no team", which is not what anybody asked for and
// is invisible in the result.
func (e *emitter) orderBuilder(b *gobuf.Buf, res *ir.Resource) {
	var (
		queryPkg = b.Import(runtimeModule + "/query")
		errPkg   = b.Import(runtimeModule + "/rigerr")
		model    = e.model(b)
		name     = orderFuncName(res)
	)

	e.sortableSet(b, res)

	relations := e.orderRelations(res)
	if len(relations) > 0 {
		e.orderJoinBuilder(b, res, relations, queryPkg, errPkg)
	}

	b.Comment(name + " converts the model's ordering terms into the runtime's.")
	b.L("func %s(terms []%s.%sOrder, sc filterScope) ([]%s.Order, []%s.Join, error) {",
		name, model, res.Name, queryPkg, queryPkg)
	b.L("if len(terms) == 0 { return nil, nil, nil }")
	b.NL()
	b.L("out := make([]%s.Order, 0, len(terms))", queryPkg)
	if len(relations) > 0 {
		b.L("var joins []%s.Join", queryPkg)
		b.Comment("One join per relation however many of its columns are named.")
		b.L("aliases := make(map[string]string)")
	}
	b.NL()
	b.L("for _, t := range terms {")

	if len(relations) > 0 {
		b.L("if t.Relation != \"\" {")
		b.L("alias, ok := aliases[t.Relation]")
		b.L("if !ok {")
		b.L("alias = %s.Itoa(len(joins) + 1)", b.Import("strconv"))
		b.L("alias = \"o\" + alias")
		b.L("join, err := %s(t.Relation, t.Column, alias, sc)", orderJoinFuncName(res))
		b.L("if err != nil { return nil, nil, err }")
		b.L("joins = append(joins, join)")
		b.L("aliases[t.Relation] = alias")
		b.L("} else if err := %s(t.Relation, t.Column); err != nil { return nil, nil, err }",
			orderCheckFuncName(res))
		b.L("out = append(out, %s.Order{Table: alias, Column: t.Column, Desc: t.Desc})", queryPkg)
		b.L("continue")
		b.L("}")
		b.NL()
	}

	b.Comment("A column this table cannot be ordered by is the caller's mistake, " +
		"not a column name to paste into a statement.")
	b.L("if !%s(t.Column) {", sortableFuncName(res))
	b.L("return nil, nil, %s.Invalid(\"a %s cannot be ordered by %%q\", t.Column)", errPkg, res.Name)
	b.L("}")
	b.L("out = append(out, %s.Order{Table: sc.as, Column: t.Column, Desc: t.Desc})", queryPkg)
	b.L("}")
	b.NL()
	if len(relations) > 0 {
		b.L("return out, joins, nil")
	} else {
		b.L("return out, nil, nil")
	}
	b.L("}")
	b.NL()
}

// sortableSet emits the set of columns this table may be ordered by.
//
// The same set the model emits a constant for, asked the other way round: the
// constants are what a caller writes, and this is what the repository will
// accept. A term is a struct with a string in it, so something has to hold the
// two together, and it had better be generated from the same list.
func (e *emitter) sortableSet(b *gobuf.Buf, res *ir.Resource) {
	b.Comment(sortableFuncName(res) + " reports whether a column can be ordered by.")
	b.L("func %s(column string) bool {", sortableFuncName(res))
	b.L("switch column {")
	b.P("case ")
	for i, f := range sortableFields(res) {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s", gobuf.Quote(f.Column.Name))
	}
	b.L(":")
	b.L("return true")
	b.L("}")
	b.L("return false")
	b.L("}")
	b.NL()
}

// orderJoinBuilder emits the lookup from a relation's name to the join that
// reaches it.
func (e *emitter) orderJoinBuilder(b *gobuf.Buf, res *ir.Resource, relations []relationFilter, queryPkg, errPkg string) {
	check, join := orderCheckFuncName(res), orderJoinFuncName(res)

	b.Comment(check + " reports whether an ordering names a column of a relation " +
		"that can be ordered by.")
	b.L("func %s(relation, column string) error {", check)
	b.L("switch relation {")
	for _, r := range relations {
		b.L("case %s:", gobuf.Quote(r.field))
		b.L("if !%s(column) {", sortableFuncName(r.target))
		b.L("return %s.Invalid(\"a %s has no %s that can be ordered by %%q\", column)",
			errPkg, res.Name, r.field)
		b.L("}")
		b.L("return nil")
	}
	b.L("}")
	b.L("return %s.Invalid(\"a %s has no relation named %%q to order by\", relation)", errPkg, res.Name)
	b.L("}")
	b.NL()

	b.Comment(join + " builds the left join an ordering across a relation needs.\n\n" +
		"The scope predicates go into the join's own condition rather than the " +
		"statement's WHERE. In WHERE they would be false for a row that matched " +
		"nothing and would discard it, which is an inner join wearing a left " +
		"join's clothes.")
	b.L("func %s(relation, column, alias string, sc filterScope) (%s.Join, error) {",
		join, queryPkg)
	b.L("if err := %s(relation, column); err != nil { return %s.Join{}, err }", check, queryPkg)
	b.NL()
	b.L("switch relation {")
	for _, r := range relations {
		t := r.target.Storage
		b.L("case %s:", gobuf.Quote(r.field))
		// A far side with nothing to scope by does not need the scope, and an
		// unused one would not compile.
		receiver := "far"
		if t.Tenant == nil && !t.IsSoftDeletable() && !t.IsSnapshotable() {
			receiver = "_"
		}
		b.L("%s, j := sc.orderJoin(%s, alias, %s, %s)", receiver,
			gobuf.Quote(t.Table), gobuf.Quote(r.rel.ForeignColumn), gobuf.Quote(r.rel.LocalColumn))
		if t.Tenant != nil {
			b.L("j.Where.Add(far.tenant(%s))", gobuf.Quote(t.Tenant.Name))
		}
		if t.IsSoftDeletable() {
			b.L("j.Where.Add(far.live(%s))", gobuf.Quote(t.SoftDelete.Column.Name))
		}
		if t.IsSnapshotable() {
			b.L("j.Where.Add(far.original(%s, %s))",
				gobuf.Quote(t.Snapshot.VersionType.Name), e.versionOriginal(b, r.target))
		}
		b.L("return j, nil")
	}
	b.L("}")
	b.L("return %s.Join{}, %s.Invalid(\"a %s has no relation named %%q to order by\", relation)",
		queryPkg, errPkg, res.Name)
	b.L("}")
	b.NL()
}

// orderRelations are the relations a list can be ordered through: belongs-to
// only, and to a resource the API exposes.
//
// A has-many is left out for the same reason a filter across one is a subquery.
// Ordering by a column of a table with many rows per row of this one is not a
// question with an answer until you say which of them — the least, the greatest,
// how many — and that is an aggregate, not an ordering.
func (e *emitter) orderRelations(res *ir.Resource) []relationFilter {
	var out []relationFilter
	for _, r := range e.relationFilters(res) {
		if r.rel.Kind != ir.RelationBelongsTo || r.rel.LinkTable != nil {
			continue
		}
		if len(sortableFields(r.target)) == 0 {
			continue
		}
		out = append(out, r)
	}
	return out
}

// sortableFields are the columns of a resource that an ordering may name.
func sortableFields(res *ir.Resource) []ir.ResourceField {
	var out []ir.ResourceField
	for _, f := range storedFields(res) {
		if f.IsArray() {
			continue
		}
		out = append(out, f)
	}
	return out
}

func sortableFuncName(res *ir.Resource) string   { return lowerName(res) + "Sortable" }
func orderCheckFuncName(res *ir.Resource) string { return lowerName(res) + "OrderColumn" }
func orderJoinFuncName(res *ir.Resource) string  { return lowerName(res) + "OrderJoin" }

// The two helpers are named from the resource so that a package holding a
// dozen repositories does not end up with a dozen functions called group.
func queryFuncName(res *ir.Resource) string { return lowerName(res) + "Group" }
func orderFuncName(res *ir.Resource) string { return lowerName(res) + "Order" }

func lowerName(res *ir.Resource) string {
	return naming.New(naming.Config{}).GoUnexported(res.Name)
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

// relationSubFilters emits, per relation, the function that collects everything
// the caller asked about it.
//
// A relation is a condition beside the ones around it — equals.homeTeam sits
// with equals.kickoffAt — so the conditions on one relation arrive spread across
// as many operator objects as the caller used. Gathering them into the far
// side's own filter is what makes them one question about one related row rather
// than one subquery per operator, and it means the far side is rendered by its
// own builder, the same one a search on that resource goes through.
func (e *emitter) relationSubFilters(b *gobuf.Buf, res *ir.Resource) {
	model := e.model(b)

	for _, r := range e.relationFilters(res) {
		name := relationFilterFuncName(res, r.field)

		b.Comment(name + " collects the conditions written on a " + res.Name +
			"'s " + r.field + ".\n\n" +
			"The bool is whether there were any: a relation nobody mentioned is " +
			"not a condition that everything satisfies, it is no condition at all.")
		b.L("func %s(f %s.%sFilter) (%s.%sFilter, bool) {",
			name, model, res.Name, model, r.target.Name)
		b.L("var (")
		b.L("sub %s.%sFilter", model, r.target.Name)
		b.L("asked bool")
		b.L(")")
		b.NL()

		for _, slot := range append(append([]filterSlot{}, comparisonSlots...), presenceSlots...) {
			b.L("if p := f.%s; p != nil && p.%s != nil { sub.%s, asked = p.%s, true }",
				slot.field, r.field, slot.field, r.field)
		}

		b.NL()
		b.L("if !asked { return sub, false }")
		b.NL()
		b.Comment("The connective comes down with them, and it has to: under OR the " +
			"caller asked for a related row satisfying either condition, and one " +
			"subquery whose inside is a disjunction is exactly that. Under AND it " +
			"is the same row satisfying both, which is the part a subquery per " +
			"operator could not say.")
		b.L("sub.OrCondition = f.OrCondition")
		b.L("return sub, true")
		b.L("}")
		b.NL()
	}
}

// relationConditions emit one correlated subquery per relation the filter can
// reach across.
//
// EXISTS rather than a join, and the reason is arithmetic: joining to the far
// side of a has-many multiplies the row, so the count would be wrong, the page
// would be wrong, and a DISTINCT over the top would break the ordering. EXISTS
// asks the only question a filter asks — is there such a row — and leaves the
// shape of the result alone.
//
// The subquery carries the scope predicates itself, and that is the part worth
// reading twice. Without the tenant inside it, a condition on a related table
// would be answered from every tenant's rows: the caller still could not read
// the far row, but could learn it exists from which of their own rows came back.
// The lifecycle predicates are there for the same reason at lower stakes — a row
// somebody deleted should not be a way to find its neighbours.
func (e *emitter) relationConditions(b *gobuf.Buf, res *ir.Resource, queryPkg string) {
	for _, r := range e.relationFilters(res) {
		rel, target, t := r.rel, r.target, r.target.Storage

		b.NL()
		b.L("if sub, ok := %s(f); ok {", relationFilterFuncName(res, r.field))

		e.relationCorrelation(b, res, rel, t)
		b.L("where, err := %s(sub, inner)", queryFuncName(target))
		b.L("if err != nil { return %s.Group{}, err }", queryPkg)
		b.NL()
		e.relationScopePredicates(b, target, t)

		b.L("g.Add(%s.Related(%s.Exists{From: from, On: on, Where: where}))", queryPkg, queryPkg)
		b.L("}")
	}

	e.relationAbsenceConditions(b, res, queryPkg)
}

// relationAbsenceConditions emit the negated subquery for each relation.
//
// NOT EXISTS is the anti-join, and it is the only way to ask two questions the
// operators cannot: whether a row has no related row at all, and whether it has
// none matching — which a row with none at all satisfies. An operator condition
// is always about some related row, so a row without one never matches it, not
// even a negated one.
//
// The scope predicates go inside here for a sharper reason than in the positive
// case. Without the tenant inside, a row pointing at another tenant's would be
// judged against a row the caller cannot see and dropped from the answer, so its
// absence from a page would report that the other tenant has a matching row.
func (e *emitter) relationAbsenceConditions(b *gobuf.Buf, res *ir.Resource, queryPkg string) {
	for _, r := range e.relationFilters(res) {
		rel, target, t := r.rel, r.target, r.target.Storage

		b.NL()
		b.L("if p := f.Without; p != nil && p.%s != nil {", r.field)
		e.relationCorrelation(b, res, rel, t)
		b.L("where, err := %s(*p.%s, inner)", queryFuncName(target), r.field)
		b.L("if err != nil { return %s.Group{}, err }", queryPkg)
		b.NL()
		e.relationScopePredicates(b, target, t)
		b.L("g.Add(%s.Related(%s.Exists{From: from, On: on, Where: where, Not: true}))",
			queryPkg, queryPkg)
		b.L("}")
	}
}

// relationFilter pairs a filter field with what it reaches.
type relationFilter struct {
	field  string
	rel    *ir.Relation
	target *ir.Resource
}

// relationFilters is the list of relations this table's filter can reach
// across, in the filter's own field order.
//
// The filter object decides which those are — the compiler has already dropped
// the relations that lead nowhere useful, such as one to a resource the API
// does not expose or the self-reference a snapshot table carries. Reading it
// back from the object rather than re-deriving it means the generated code
// cannot offer a field it has no condition for, or a condition for a field
// that is not there.
func (e *emitter) relationFilters(res *ir.Resource) []relationFilter {
	obj := e.doc.Object(res.Name + "FilterEquals")
	if obj == nil || res.Storage == nil {
		return nil
	}

	var out []relationFilter
	for _, field := range obj.Fields {
		rel := relationNamed(res, field.Name)
		if rel == nil {
			continue
		}
		target := e.doc.Resource(rel.Target)
		if target == nil || target.Storage == nil {
			continue
		}
		out = append(out, relationFilter{field: field.Name, rel: rel, target: target})
	}
	return out
}

// relationFilterFuncName is the collector for one relation. It carries the
// resource as well as the relation, because two resources can both have a
// HomeTeam and the store package holds every repository.
func relationFilterFuncName(res *ir.Resource, field string) string {
	return lowerName(res) + field + "Filter"
}

// relationCorrelation emits the line that opens the subquery: which table it
// reads and how it is tied to the outer row.
func (e *emitter) relationCorrelation(b *gobuf.Buf, res *ir.Resource, rel *ir.Relation, t *ir.ResourceStorage) {
	switch {
	case rel.LinkTable != nil:
		lt := rel.LinkTable
		near, far := lt.LeftColumn, lt.RightColumn
		if lt.LeftTable != res.Storage.Table {
			near, far = lt.RightColumn, lt.LeftColumn
		}
		b.L("inner, from, on := sc.throughLink(%s, %s, %s, %s, %s)",
			gobuf.Quote(lt.Table), gobuf.Quote(near), gobuf.Quote(t.Table),
			gobuf.Quote(far), gobuf.Quote(primaryKeyOf(res)))

	case rel.Kind == ir.RelationBelongsTo:
		b.L("inner, from, on := sc.belongsTo(%s, %s, %s)",
			gobuf.Quote(t.Table), gobuf.Quote(rel.ForeignColumn), gobuf.Quote(rel.LocalColumn))

	default:
		b.L("inner, from, on := sc.hasMany(%s, %s, %s)",
			gobuf.Quote(t.Table), gobuf.Quote(rel.ForeignColumn), gobuf.Quote(primaryKeyOf(res)))
	}
}

// relationScopePredicates emit what the far side is scoped by, into the
// subquery's own WHERE.
func (e *emitter) relationScopePredicates(b *gobuf.Buf, target *ir.Resource, t *ir.ResourceStorage) {
	b.Comment("The far side is scoped whatever the read asked for. A read " +
		"option widens what this query returns, not what it may look through " +
		"to decide.")
	if t.Tenant != nil {
		b.L("where.Add(inner.tenant(%s))", gobuf.Quote(t.Tenant.Name))
	}
	if t.IsSoftDeletable() {
		b.L("where.Add(inner.live(%s))", gobuf.Quote(t.SoftDelete.Column.Name))
	}
	if t.IsSnapshotable() {
		b.L("where.Add(inner.original(%s, %s))",
			gobuf.Quote(t.Snapshot.VersionType.Name), e.versionOriginal(b, target))
	}
	b.NL()
}

// relationNamed finds the relation a filter field stands for.
func relationNamed(res *ir.Resource, name string) *ir.Relation {
	if res.Storage == nil {
		return nil
	}
	for i := range res.Storage.Relations {
		if res.Storage.Relations[i].Name == name {
			return &res.Storage.Relations[i]
		}
	}
	return nil
}

// primaryKeyOf is the column a relation correlates against.
func primaryKeyOf(res *ir.Resource) string {
	if pk, ok := (&ir.Table{PrimaryKey: res.Storage.PrimaryKey}).SinglePrimaryKey(); ok {
		return pk
	}
	return "id"
}

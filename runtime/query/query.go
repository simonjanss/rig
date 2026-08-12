// Package query turns a tree of conditions into parameterized SQL.
//
// Callers never build these types by hand. They fill in a generated query
// struct — q.Params.Equals.Status, q.Params.In.FixtureID — and the generated
// repository translates it into the tree this package renders. Keeping the
// translation in generated code rather than doing it by reflection is what lets
// the caller's side stay fully typed: asking for "created_at contains 3" is not
// expressible, rather than merely rejected at runtime.
//
// Column names are never taken from user input. They come from generated
// constants, so this package concatenates them into SQL directly while every
// value goes through a placeholder.
package query

import (
	"fmt"
	"strconv"
	"strings"
)

// Op is a comparison.
type Op string

const (
	OpEq      Op = "="
	OpNe      Op = "<>"
	OpGt      Op = ">"
	OpGte     Op = ">="
	OpLt      Op = "<"
	OpLte     Op = "<="
	OpIn      Op = "IN"
	OpNotIn   Op = "NOT IN"
	OpLike    Op = "ILIKE"
	OpNotLike Op = "NOT ILIKE"
	OpIsNull  Op = "IS NULL"
	OpNotNull Op = "IS NOT NULL"
)

// Cond is one comparison against one column, or one correlated subquery.
type Cond struct {
	// Table qualifies the column. It is empty at the top level, where only one
	// table is in scope, and set inside a subquery, where two are: an
	// unqualified name there resolves to whichever scope happens to have it,
	// which is a silent correlation to the wrong row rather than an error.
	Table  string
	Column string
	Op     Op
	// Value is the operand. It is nil for the null checks, and a slice for the
	// set-membership ones.
	Value any

	// Exists is a condition on a related table. When it is set, the column and
	// the operator are not used.
	Exists *Exists
}

// Exists is a condition on rows of another table, correlated to this one.
//
//	EXISTS (SELECT 1 FROM <From> WHERE <On> AND <Where>)
//
// A subquery rather than a join, because a join to the far side of a has-many
// multiplies rows: the count would be wrong, the page would be wrong, and
// putting DISTINCT over the top would make the ordering wrong too. EXISTS asks
// the only question a filter is asking — is there such a row — and leaves the
// shape of the result alone.
type Exists struct {
	// From is the table and alias, or a join chain for a many-to-many. It is
	// generated, never built from anything a client sent.
	From string
	// On correlates the subquery to the outer row, and is generated the same way.
	On string
	// Where is the caller's condition on the related table, its columns
	// qualified with the alias From declares.
	Where Group
	// Not inverts it: no such row rather than at least one.
	//
	// Which is not the same as negating a condition inside Where, and the
	// difference is the one callers get wrong. A negated comparison still needs
	// the row to be there — "there is a related row whose name differs" — so a
	// row with no related row fails it. This asks whether a matching row exists
	// at all, so a row with none satisfies it.
	Not bool
}

// SQL renders the subquery.
func (e Exists) SQL(args *Args) string {
	clause := e.On
	if where := e.Where.SQL(args); where != "" {
		clause += " AND (" + where + ")"
	}

	keyword := "EXISTS"
	if e.Not {
		keyword = "NOT EXISTS"
	}
	return keyword + " (SELECT 1 FROM " + e.From + " WHERE " + clause + ")"
}

// Related builds a condition on rows of another table.
func Related(e Exists) Cond { return Cond{Exists: &e} }

// qualified is the column with its table, when it has one.
func (c Cond) qualified() string {
	if c.Table == "" {
		return c.Column
	}
	return c.Table + "." + c.Column
}

// Eq builds an equality condition.
func Eq(column string, v any) Cond { return Cond{Column: column, Op: OpEq, Value: v} }

// Ne builds an inequality condition.
func Ne(column string, v any) Cond { return Cond{Column: column, Op: OpNe, Value: v} }

// Gt builds a greater-than condition.
func Gt(column string, v any) Cond { return Cond{Column: column, Op: OpGt, Value: v} }

// Gte builds a greater-or-equal condition.
func Gte(column string, v any) Cond { return Cond{Column: column, Op: OpGte, Value: v} }

// Lt builds a less-than condition.
func Lt(column string, v any) Cond { return Cond{Column: column, Op: OpLt, Value: v} }

// Lte builds a less-or-equal condition.
func Lte(column string, v any) Cond { return Cond{Column: column, Op: OpLte, Value: v} }

// In builds a set-membership condition.
func In(column string, v any) Cond { return Cond{Column: column, Op: OpIn, Value: v} }

// NotIn builds a set-exclusion condition.
func NotIn(column string, v any) Cond { return Cond{Column: column, Op: OpNotIn, Value: v} }

// Like builds a case-insensitive pattern condition.
func Like(column string, v any) Cond { return Cond{Column: column, Op: OpLike, Value: v} }

// NotLike builds a negated pattern condition.
func NotLike(column string, v any) Cond { return Cond{Column: column, Op: OpNotLike, Value: v} }

// IsNull builds a presence condition.
func IsNull(column string) Cond { return Cond{Column: column, Op: OpIsNull} }

// NotNull builds an absence condition.
func NotNull(column string) Cond { return Cond{Column: column, Op: OpNotNull} }

// Group is a set of conditions combined with AND or OR, plus nested groups.
// Nesting is what lets the two be mixed to any depth.
type Group struct {
	Or     bool
	Conds  []Cond
	Groups []Group
}

// Add appends conditions, skipping any that are zero.
//
// Skipping is what lets a caller add a condition it may or may not have —
// the tenant predicate of a table with no tenant column, for instance — without
// asking first. A subquery has no column of its own, so it is judged by whether
// it is there.
func (g *Group) Add(conds ...Cond) {
	for _, c := range conds {
		if c.Column == "" && c.Exists == nil {
			continue
		}
		g.Conds = append(g.Conds, c)
	}
}

// Nest appends a sub-group, ignoring an empty one.
func (g *Group) Nest(sub Group) {
	if !sub.Empty() {
		g.Groups = append(g.Groups, sub)
	}
}

// Empty reports whether the group constrains anything.
func (g Group) Empty() bool {
	if len(g.Conds) > 0 {
		return false
	}
	for _, sub := range g.Groups {
		if !sub.Empty() {
			return false
		}
	}
	return true
}

// And builds a conjunction.
func And(conds ...Cond) Group { return Group{Conds: conds} }

// Or builds a disjunction.
func Or(conds ...Cond) Group { return Group{Or: true, Conds: conds} }

// Args accumulates placeholder values while SQL is rendered.
//
// Postgres numbers its placeholders, so they have to be allocated in the order
// the fragments are written. Threading one of these through the rendering is
// simpler than counting after the fact and getting it wrong once.
type Args struct {
	values []any
}

// NewArgs starts an empty argument list.
func NewArgs() *Args { return &Args{} }

// Next records a value and returns its placeholder.
func (a *Args) Next(v any) string {
	a.values = append(a.values, v)
	return "$" + strconv.Itoa(len(a.values))
}

// Values returns the collected arguments.
func (a *Args) Values() []any { return a.values }

// Len is the number of arguments so far.
func (a *Args) Len() int { return len(a.values) }

// SQL renders a group. The result carries no surrounding parentheses; a caller
// combining it with other predicates adds its own.
func (g Group) SQL(args *Args) string {
	parts := make([]string, 0, len(g.Conds)+len(g.Groups))

	for _, c := range g.Conds {
		if s := c.SQL(args); s != "" {
			parts = append(parts, s)
		}
	}
	for _, sub := range g.Groups {
		if sub.Empty() {
			continue
		}
		if s := sub.SQL(args); s != "" {
			parts = append(parts, "("+s+")")
		}
	}

	if len(parts) == 0 {
		return ""
	}

	joiner := " AND "
	if g.Or {
		joiner = " OR "
	}
	return strings.Join(parts, joiner)
}

// SQL renders one condition.
func (c Cond) SQL(args *Args) string {
	if c.Exists != nil {
		return c.Exists.SQL(args)
	}

	switch c.Op {
	case OpIsNull, OpNotNull:
		return c.qualified() + " " + string(c.Op)

	case OpIn, OpNotIn:
		// = ANY and <> ALL take an array as a single parameter, which keeps the
		// statement shape constant however many values are passed. Expanding
		// into a placeholder list would produce a different query text for
		// every list length and defeat statement caching.
		op := "= ANY"
		if c.Op == OpNotIn {
			op = "<> ALL"
		}
		return fmt.Sprintf("%s %s(%s)", c.qualified(), op, args.Next(c.Value))

	default:
		return fmt.Sprintf("%s %s %s", c.qualified(), c.Op, args.Next(c.Value))
	}
}

// Order is one term of an ORDER BY.
type Order struct {
	// Table qualifies the column, and has to whenever the statement joins: two
	// tables with a created_at make an unqualified one ambiguous, and Postgres
	// is right to refuse it.
	Table  string
	Column string
	Desc   bool
}

// qualified is the column with its table, when it has one.
func (o Order) qualified() string {
	if o.Table == "" {
		return o.Column
	}
	return o.Table + "." + o.Column
}

// OrderSQL renders an ordering, returning empty when there is none.
//
// Nulls are left where Postgres puts them: last when ascending, first when
// descending. A row ordered by a column of a related row it does not have sorts
// to one end rather than disappearing, which is the whole point of reaching it
// through a left join.
func OrderSQL(terms []Order) string {
	if len(terms) == 0 {
		return ""
	}
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		if t.Desc {
			parts = append(parts, t.qualified()+" DESC")
			continue
		}
		parts = append(parts, t.qualified()+" ASC")
	}
	return " ORDER BY " + strings.Join(parts, ", ")
}

// Join is a related table brought into a statement so that its columns can be
// ordered by.
//
// LEFT and never INNER, because the join is here to reach a value rather than
// to decide anything: an inner join would drop every row whose foreign key is
// null, silently turning "order these by their team's name" into "and hide the
// ones with no team". A filter across a relation is a Cond with an Exists, not
// one of these — see Exists for why that one is a subquery.
type Join struct {
	// Table is the table and its alias, and On correlates it to the outer row.
	// Both are generated from the schema, never built from anything a client
	// sent.
	Table string
	On    string

	// Where is what the far side is scoped by: the tenant, and whatever
	// lifecycle predicates it has.
	//
	// It renders into the ON clause rather than the statement's WHERE, and that
	// is not a stylistic choice. A predicate on the joined table in WHERE is
	// false for a row that matched nothing, so it would discard exactly the rows
	// the left join was chosen to keep — an inner join with extra steps.
	Where Group
}

// JoinSQL renders the joins, returning empty when there are none.
//
// It has to be rendered before the statement's WHERE clause, because Postgres
// numbers placeholders in the order they appear in the text and a join stands
// earlier in the statement than the conditions do.
func JoinSQL(joins []Join, args *Args) string {
	if len(joins) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, j := range joins {
		sb.WriteString(" LEFT JOIN ")
		sb.WriteString(j.Table)
		sb.WriteString(" ON ")
		sb.WriteString(j.On)
		if where := j.Where.SQL(args); where != "" {
			sb.WriteString(" AND (")
			sb.WriteString(where)
			sb.WriteString(")")
		}
	}
	return sb.String()
}

// Page bounds a result set.
type Page struct {
	Limit  int
	Offset int
}

// Clamp applies the defaults and the ceiling.
//
// A limit is always applied. An unbounded list is a production incident waiting
// for the table to grow, and the caller who omitted it did not mean "all of
// them" — they just did not think about it.
func (p Page) Clamp(defaultLimit, maxLimit int) Page {
	out := p
	if out.Limit <= 0 {
		out.Limit = defaultLimit
	}
	if maxLimit > 0 && out.Limit > maxLimit {
		out.Limit = maxLimit
	}
	if out.Offset < 0 {
		out.Offset = 0
	}
	return out
}

// SQL renders the LIMIT and OFFSET.
func (p Page) SQL(args *Args) string {
	s := " LIMIT " + args.Next(p.Limit)
	if p.Offset > 0 {
		s += " OFFSET " + args.Next(p.Offset)
	}
	return s
}

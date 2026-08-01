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

// Cond is one comparison against one column.
type Cond struct {
	Column string
	Op     Op
	// Value is the operand. It is nil for the null checks, and a slice for the
	// set-membership ones.
	Value any
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
func (g *Group) Add(conds ...Cond) {
	for _, c := range conds {
		if c.Column != "" {
			g.Conds = append(g.Conds, c)
		}
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
	switch c.Op {
	case OpIsNull, OpNotNull:
		return c.Column + " " + string(c.Op)

	case OpIn, OpNotIn:
		// = ANY and <> ALL take an array as a single parameter, which keeps the
		// statement shape constant however many values are passed. Expanding
		// into a placeholder list would produce a different query text for
		// every list length and defeat statement caching.
		op := "= ANY"
		if c.Op == OpNotIn {
			op = "<> ALL"
		}
		return fmt.Sprintf("%s %s(%s)", c.Column, op, args.Next(c.Value))

	default:
		return fmt.Sprintf("%s %s %s", c.Column, c.Op, args.Next(c.Value))
	}
}

// Order is one term of an ORDER BY.
type Order struct {
	Column string
	Desc   bool
}

// OrderSQL renders an ordering, returning empty when there is none.
func OrderSQL(terms []Order) string {
	if len(terms) == 0 {
		return ""
	}
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		if t.Desc {
			parts = append(parts, t.Column+" DESC")
			continue
		}
		parts = append(parts, t.Column+" ASC")
	}
	return " ORDER BY " + strings.Join(parts, ", ")
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

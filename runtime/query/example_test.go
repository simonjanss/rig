package query_test

import (
	"fmt"

	"github.com/simonjanss/rig/runtime/query"
)

// A generated repository builds one of these from the caller's typed query
// struct and renders it into the WHERE clause. Every value becomes a numbered
// placeholder; only the column names, which come from generated constants, are
// written into the SQL.
//
// A set membership renders as `= ANY` against a single array parameter rather
// than an expanded placeholder list, so the statement text does not change with
// the length of the list.
func ExampleGroup() {
	g := query.And(
		query.Eq("status", "active"),
		query.Gte("score", 10),
		query.In("fixture_id", []int{1, 2, 3}),
	)

	args := query.NewArgs()
	fmt.Println(g.SQL(args))
	fmt.Println(args.Values())

	// Output:
	// status = $1 AND score >= $2 AND fixture_id = ANY($3)
	// [active 10 [1 2 3]]
}

// Mixing AND with OR is what nesting is for: the flag belongs to a group, so
// the two are combined by putting one inside the other rather than by a
// precedence rule nobody can see at the call site.
func ExampleGroup_Nest() {
	g := query.And(query.Eq("tenant_id", "t-1"))
	g.Nest(query.Or(
		query.Eq("status", "draft"),
		query.IsNull("published_at"),
	))

	args := query.NewArgs()
	fmt.Println(g.SQL(args))
	fmt.Println(args.Values())

	// Output:
	// tenant_id = $1 AND (status = $2 OR published_at IS NULL)
	// [t-1 draft]
}

// Add skips a zero condition, which is what lets a repository add a predicate it
// may or may not have — the tenant filter of a table with no tenant column, say
// — without asking first.
func ExampleGroup_Add() {
	var g query.Group
	g.Add(query.Eq("tenant_id", "t-1"))
	g.Add(query.Cond{}) // a filter this table does not have
	g.Add(query.NotNull("published_at"))

	args := query.NewArgs()
	fmt.Println(g.SQL(args))

	// Output:
	// tenant_id = $1 AND published_at IS NOT NULL
}

// Clamp is why a generated list cannot return an unbounded result set: the
// caller who omitted a limit did not mean "all of them", and a table that grows
// would turn that omission into an incident.
func ExamplePage_Clamp() {
	fmt.Println(query.Page{}.Clamp(50, 200))
	fmt.Println(query.Page{Limit: 1000, Offset: 20}.Clamp(50, 200))

	// Output:
	// {50 0}
	// {200 20}
}

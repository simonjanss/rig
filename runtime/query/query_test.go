package query_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/runtime/query"
)

func render(g query.Group) (string, []any) {
	args := query.NewArgs()
	return g.SQL(args), args.Values()
}

func TestConditions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cond query.Cond
		want string
		args int
	}{
		{"equality", query.Eq("title", "x"), "title = $1", 1},
		{"inequality", query.Ne("title", "x"), "title <> $1", 1},
		{"greater", query.Gt("n", 1), "n > $1", 1},
		{"greater or equal", query.Gte("n", 1), "n >= $1", 1},
		{"less", query.Lt("n", 1), "n < $1", 1},
		{"less or equal", query.Lte("n", 1), "n <= $1", 1},
		{"pattern", query.Like("title", "%x%"), "title ILIKE $1", 1},
		{"negated pattern", query.NotLike("title", "%x%"), "title NOT ILIKE $1", 1},
		{"is null", query.IsNull("notes"), "notes IS NULL", 0},
		{"is not null", query.NotNull("notes"), "notes IS NOT NULL", 0},
	} {
		got, args := render(query.And(tc.cond))
		if got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
		if len(args) != tc.args {
			t.Errorf("%s: %d args, want %d", tc.name, len(args), tc.args)
		}
	}
}

// Set membership uses an array parameter rather than a placeholder list, so the
// statement text is the same however many values are passed and the database
// can still cache the plan.
func TestSetMembershipUsesOneParameter(t *testing.T) {
	t.Parallel()

	got, args := render(query.And(query.In("id", []int{1, 2, 3})))
	if got != "id = ANY($1)" {
		t.Errorf("IN = %q", got)
	}
	if len(args) != 1 {
		t.Errorf("%d args, want the whole slice as one", len(args))
	}

	got, _ = render(query.And(query.NotIn("id", []int{1})))
	if got != "id <> ALL($1)" {
		t.Errorf("NOT IN = %q", got)
	}
}

func TestAndOr(t *testing.T) {
	t.Parallel()

	got, _ := render(query.And(query.Eq("a", 1), query.Eq("b", 2)))
	if got != "a = $1 AND b = $2" {
		t.Errorf("AND = %q", got)
	}

	got, _ = render(query.Or(query.Eq("a", 1), query.Eq("b", 2)))
	if got != "a = $1 OR b = $2" {
		t.Errorf("OR = %q", got)
	}
}

// Nesting is what lets AND and OR be mixed to any depth, which is the whole
// point of a filter tree rather than a flat list.
func TestNesting(t *testing.T) {
	t.Parallel()

	g := query.And(query.Eq("tenant_id", "t"))
	g.Nest(query.Or(query.Eq("status", "a"), query.Eq("status", "b")))

	got, args := render(g)
	want := "tenant_id = $1 AND (status = $2 OR status = $3)"
	if got != want {
		t.Errorf("nested = %q, want %q", got, want)
	}
	if len(args) != 3 {
		t.Errorf("%d args, want 3", len(args))
	}
}

func TestPlaceholdersAreNumberedInRenderOrder(t *testing.T) {
	t.Parallel()

	// Postgres numbers its placeholders, so they have to be allocated as the
	// fragments are written rather than counted afterwards.
	g := query.And(query.Eq("a", "first"))
	g.Nest(query.And(query.Eq("b", "second")))

	got, args := render(g)
	if got != "a = $1 AND (b = $2)" {
		t.Errorf("sql = %q", got)
	}
	if args[0] != "first" || args[1] != "second" {
		t.Errorf("args out of order: %v", args)
	}
}

func TestEmptyGroups(t *testing.T) {
	t.Parallel()

	var g query.Group
	if !g.Empty() {
		t.Error("a group with nothing in it is empty")
	}
	if sql, _ := render(g); sql != "" {
		t.Errorf("an empty group renders nothing, got %q", sql)
	}

	// An empty sub-group must not leave stray parentheses behind.
	outer := query.And(query.Eq("a", 1))
	outer.Nest(query.Group{})
	if sql, _ := render(outer); sql != "a = $1" {
		t.Errorf("an empty nested group should vanish, got %q", sql)
	}
}

func TestAddSkipsZeroConditions(t *testing.T) {
	t.Parallel()

	var g query.Group
	g.Add(query.Cond{}, query.Eq("a", 1))

	if sql, _ := render(g); sql != "a = $1" {
		t.Errorf("a zero condition should be skipped, got %q", sql)
	}
}

func TestOrderSQL(t *testing.T) {
	t.Parallel()

	if got := query.OrderSQL(nil); got != "" {
		t.Errorf("no terms should render nothing, got %q", got)
	}

	got := query.OrderSQL([]query.Order{{Column: "created_at", Desc: true}, {Column: "id"}})
	if got != " ORDER BY created_at DESC, id ASC" {
		t.Errorf("OrderSQL = %q", got)
	}
}

// A read without a limit is a production incident waiting for the table to
// grow, so one is always applied.
func TestPageClamp(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in         query.Page
		wantLimit  int
		wantOffset int
	}{
		{query.Page{}, 50, 0},
		{query.Page{Limit: 10}, 10, 0},
		{query.Page{Limit: 10_000}, 500, 0},
		{query.Page{Limit: -1}, 50, 0},
		{query.Page{Offset: -5}, 50, 0},
		{query.Page{Limit: 20, Offset: 40}, 20, 40},
	} {
		got := tc.in.Clamp(50, 500)
		if got.Limit != tc.wantLimit || got.Offset != tc.wantOffset {
			t.Errorf("Clamp(%+v) = %+v, want limit %d offset %d",
				tc.in, got, tc.wantLimit, tc.wantOffset)
		}
	}
}

func TestPageSQL(t *testing.T) {
	t.Parallel()

	args := query.NewArgs()
	if got := (query.Page{Limit: 10}).SQL(args); got != " LIMIT $1" {
		t.Errorf("SQL = %q", got)
	}

	args = query.NewArgs()
	if got := (query.Page{Limit: 10, Offset: 20}).SQL(args); got != " LIMIT $1 OFFSET $2" {
		t.Errorf("SQL = %q", got)
	}
	if len(args.Values()) != 2 {
		t.Errorf("%d args, want 2", len(args.Values()))
	}
}

// Values always travel as parameters. Column names come from generated
// constants, never from a request.
func TestValuesAreAlwaysParameterized(t *testing.T) {
	t.Parallel()

	malicious := "'; DROP TABLE lesson; --"
	sql, args := render(query.And(query.Eq("title", malicious)))

	if sql != "title = $1" {
		t.Errorf("sql = %q", sql)
	}
	if len(args) != 1 || args[0] != malicious {
		t.Errorf("the value should be a parameter, got %v", args)
	}
}

func TestQualifiedColumns(t *testing.T) {
	t.Parallel()

	// Inside a subquery an unqualified name silently resolves to whichever
	// scope has it, so a column meant to be the far side's can end up
	// correlating to the outer row and quietly answering the wrong question.
	got, _ := render(query.And(query.Cond{Table: "r1", Column: "name", Op: query.OpEq, Value: "x"}))
	if want := "r1.name = $1"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestRelatedRendersACorrelatedSubquery(t *testing.T) {
	t.Parallel()

	// EXISTS rather than a join, because a join to a has-many multiplies the
	// outer row and then the total a paginated list reports counts matches
	// instead of rows.
	got, args := render(query.And(query.Related(query.Exists{
		From:  "team r1",
		On:    "r1.id = fixture.home_team_id",
		Where: query.And(query.Cond{Table: "r1", Column: "name", Op: query.OpEq, Value: "Rovers"}),
	})))

	want := "EXISTS (SELECT 1 FROM team r1 WHERE r1.id = fixture.home_team_id AND (r1.name = $1))"
	if got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	if len(args) != 1 || args[0] != "Rovers" {
		t.Errorf("the inner condition's value should be parameterized, got %v", args)
	}
}

func TestRelatedWithoutAConditionIsJustExistence(t *testing.T) {
	t.Parallel()

	// An empty filter on the far side means "has one at all", which is a
	// question worth asking, so the AND is dropped rather than rendered
	// against an empty string.
	got, _ := render(query.And(query.Related(query.Exists{
		From: "player r1",
		On:   "r1.team_id = team.id",
	})))
	if want := "EXISTS (SELECT 1 FROM player r1 WHERE r1.team_id = team.id)"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestRelatedCanBeNegated(t *testing.T) {
	t.Parallel()

	got, _ := render(query.And(query.Related(query.Exists{
		From: "player r1",
		On:   "r1.team_id = team.id",
		Not:  true,
	})))
	if want := "NOT EXISTS (SELECT 1 FROM player r1 WHERE r1.team_id = team.id)"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestRelatedNestsAndKeepsPlaceholderOrder(t *testing.T) {
	t.Parallel()

	// Numbering follows render order, and a subquery renders in the middle of
	// its parent group. If the inner condition took its number from anywhere
	// but the shared Args, everything after it would read the wrong value.
	inner := query.And(query.Related(query.Exists{
		From:  "team r2",
		On:    "r2.id = r1.team_id",
		Where: query.And(query.Cond{Table: "r2", Column: "name", Op: query.OpEq, Value: "Rovers"}),
	}))
	got, args := render(query.And(
		query.Eq("kickoff_at", "2026-08-03"),
		query.Related(query.Exists{From: "player r1", On: "r1.fixture_id = fixture.id", Where: inner}),
		query.Eq("id", 7),
	))

	want := "kickoff_at = $1 AND EXISTS (SELECT 1 FROM player r1 WHERE r1.fixture_id = fixture.id " +
		"AND (EXISTS (SELECT 1 FROM team r2 WHERE r2.id = r1.team_id AND (r2.name = $2)))) AND id = $3"
	if got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	if len(args) != 3 || args[1] != "Rovers" {
		t.Errorf("args should be numbered in render order, got %v", args)
	}
}

func TestAddKeepsASubquery(t *testing.T) {
	t.Parallel()

	// A subquery has no column, and the zero-condition skip is written in terms
	// of the column. Dropping it here is the worst kind of failure: the query
	// still runs and still returns rows, just not the ones that were asked for.
	var g query.Group
	g.Add(query.Related(query.Exists{From: "player r1", On: "r1.team_id = team.id"}))

	if g.Empty() {
		t.Fatal("a relation condition was dropped")
	}
	if got, _ := render(g); got != "EXISTS (SELECT 1 FROM player r1 WHERE r1.team_id = team.id)" {
		t.Errorf("unexpected SQL: %q", got)
	}
}

func TestOrderQualifiesItsColumn(t *testing.T) {
	t.Parallel()

	// A statement that joins has two of some column names, and Postgres is
	// right to refuse an unqualified one.
	got := query.OrderSQL([]query.Order{
		{Table: "fixture", Column: "kickoff_at", Desc: true},
		{Table: "o1", Column: "name"},
	})
	if want := " ORDER BY fixture.kickoff_at DESC, o1.name ASC"; got != want {
		t.Errorf("%q, want %q", got, want)
	}
}

func TestJoinScopesInsideTheOnClause(t *testing.T) {
	t.Parallel()

	// The predicate has to be part of the join condition. In the statement's
	// WHERE it would be false for a row that matched nothing, discarding exactly
	// the rows a left join was chosen to keep.
	args := query.NewArgs()
	got := query.JoinSQL([]query.Join{{
		Table: "team o1",
		On:    "o1.id = fixture.home_team_id",
		Where: query.And(query.Cond{Table: "o1", Column: "tenant_id", Op: query.OpEq, Value: "t"}),
	}}, args)

	want := " LEFT JOIN team o1 ON o1.id = fixture.home_team_id AND (o1.tenant_id = $1)"
	if got != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
	if len(args.Values()) != 1 {
		t.Errorf("the scope value should be parameterized, got %v", args.Values())
	}
}

func TestJoinsAreNumberedBeforeTheConditions(t *testing.T) {
	t.Parallel()

	// Placeholders are numbered in render order and a join stands earlier in the
	// statement than the conditions do, so it has to be rendered first. Getting
	// this backwards does not fail — it silently swaps two values.
	args := query.NewArgs()
	join := query.JoinSQL([]query.Join{{
		Table: "team o1", On: "o1.id = fixture.home_team_id",
		Where: query.And(query.Eq("tenant_id", "tenant")),
	}}, args)
	where := query.And(query.Eq("kickoff_at", "today")).SQL(args)

	if !strings.Contains(join, "$1") || !strings.Contains(where, "$2") {
		t.Errorf("join %q should hold $1 and where %q should hold $2", join, where)
	}
	if v := args.Values(); v[0] != "tenant" || v[1] != "today" {
		t.Errorf("values are in the wrong order: %v", v)
	}
}

func TestNoJoinsRendersNothing(t *testing.T) {
	t.Parallel()

	if got := query.JoinSQL(nil, query.NewArgs()); got != "" {
		t.Errorf("%q, want empty", got)
	}
}

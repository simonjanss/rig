package persistgo_test

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const pkg = "store"

func opts() gen.Options {
	return gen.Options{OutDir: ".", Raw: map[string]any{
		"package":      pkg,
		"model_import": "rigtest/model",
	}}
}

func TestGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())

	gentest.Golden(t, filepath.Join("testdata", "lifecycle"), artifacts, *update)
}

func TestDeterministic(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	gentest.Deterministic(t, persistgo.New(), doc, opts())
}

// TestGeneratedCodeCompiles is the check golden files cannot make. A generator
// can emit a file that formats cleanly, matches its golden exactly, and refers
// to a method that does not exist.
func TestGeneratedCodeCompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	gentest.MustCompileAll(t,
		gentest.Package{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		gentest.Package{Dir: pkg, Artifacts: gentest.Run(t, persistgo.New(), doc, opts())},
	)
}

// TestRelationFilterCompiles type-checks the condition builders for a document
// that actually has foreign keys.
//
// The lifecycle document is one table, so it exercises none of the relation
// code: every subquery helper, every alias, and every mutually recursive call
// between two resources' builders is unreachable from it. This fixture has a
// belongs-to, a has-many, a join table, and a self-reference, so the emitted
// EXISTS clauses are held to the same standard as the rest — that they compile.
func TestRelationFilterCompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "relations.ir.json"))
	gentest.MustCompileAll(t,
		gentest.Package{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		gentest.Package{Dir: pkg, Artifacts: gentest.Run(t, persistgo.New(), doc, opts())},
	)
}

// TestRelationFilterCompilesWithoutATenant covers the tables a project has
// that are not tenant-scoped at all — the shared lookups: permissions, plans,
// countries.
//
// Two separate cases, because the claims are bound by the read that needs them:
// a table with no tenant column of its own still needs them the moment its
// filter reaches into a table that has one, and a filter with a tenant nowhere
// in it must not refer to claims that were never bound. Both were compile
// errors in generated code that this package's own fixtures could not see.
func TestRelationFilterCompilesWithoutATenant(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		untenants []string
	}{
		{"the far side is still scoped", []string{"Player"}},
		{"nothing in reach is scoped", []string{"Player", "Team", "Fixture"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			doc := gentest.LoadDocument(t, filepath.Join("testdata", "relations.ir.json"))
			for _, name := range tc.untenants {
				res := doc.Resource(name)
				if res == nil || res.Storage == nil {
					t.Fatalf("no %s in the fixture", name)
				}
				res.Storage.Tenant = nil
			}

			gentest.MustCompileAll(t,
				gentest.Package{
					Dir: "model",
					Artifacts: gentest.Run(t, modelgo.New(), doc,
						gen.Options{Raw: map[string]any{"package": "model"}}),
				},
				gentest.Package{Dir: pkg, Artifacts: gentest.Run(t, persistgo.New(), doc, opts())},
			)
		})
	}
}

// TestRelationFilterIsScopedInsideTheSubquery pins the properties that make
// filtering across a relation safe.
//
// The whole feature is one correlated subquery per relation, and the subquery
// is where the scoping has to happen. A tenant predicate left outside it is the
// interesting failure: the query still returns only this tenant's rows, but it
// decides which ones by looking at every tenant's related rows, so a filter
// becomes an oracle for whether a row exists somewhere else.
func TestRelationFilterIsScopedInsideTheSubquery(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "relations.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())

	for _, tc := range []struct{ file, what, want string }{
		{"fixture_repository.gen.go", "a belongs-to reaches the row it points at",
			`inner, from, on := sc.belongsTo("team", "id", "home_team_id")`},
		{"team_repository.gen.go", "a has-many reaches the rows pointing back",
			`inner, from, on := sc.hasMany("fixture", "home_team_id", "id")`},
		{"team_repository.gen.go", "a many-to-many reaches through the link table",
			`inner, from, on := sc.throughLink("team_player", "team_id", "player", "player_id", "id")`},

		// The tenant goes on the inner group, which is the subquery's WHERE.
		{"fixture_repository.gen.go", "the tenant is scoped inside the subquery",
			`where.Add(inner.tenant("tenant_id"))`},
		{"fixture_repository.gen.go", "the caller's condition is the subquery's WHERE",
			"g.Add(query.Related(query.Exists{From: from, On: on, Where: where}))"},

		// The conditions on one relation arrive spread across the operator
		// objects the caller used, and become one subquery rather than one per
		// operator — so several conditions on a related row are a question
		// about the same row.
		{"fixture_repository.gen.go", "the operators are collected into one filter",
			"if p := f.GreaterThan; p != nil && p.HomeTeam != nil { sub.GreaterThan, asked = p.HomeTeam, true }"},
		{"fixture_repository.gen.go", "the connective comes down with them",
			"sub.OrCondition = f.OrCondition"},
		{"fixture_repository.gen.go", "a relation nobody mentioned is no condition",
			"if !asked { return sub, false }"},

		// Every level re-checks, because the recursion is the client's to drive.
		{"fixture_repository.gen.go", "nesting is bounded", "if err := sc.ok(); err != nil"},
		{"fixture_repository.gen.go", "a rejected depth is not silently dropped",
			"return query.Group{}, err"},
	} {
		src := collapse(find(t, artifacts, tc.file))
		if !strings.Contains(src, collapse(tc.want)) {
			t.Errorf("%s: %s\nexpected: %s", tc.file, tc.what, tc.want)
		}
	}

	// The far side's alias is not this side's, so a self-reference at two
	// depths does not resolve its inner condition against its outer row.
	store := find(t, artifacts, "store.gen.go")
	for _, want := range []string{
		`return filterScope{claims: s.claims, as: "r" + strconv.Itoa(d)`,
		"if s.depth > MaxFilterDepth",
	} {
		if !strings.Contains(collapse(store), collapse(want)) {
			t.Errorf("the filter scope should contain %s", want)
		}
	}
}

// TestAbsenceAndOrderingUseTheRightJoin pins the two shapes that are not the
// positive subquery, and which a join would get wrong in opposite directions.
func TestAbsenceAndOrderingUseTheRightJoin(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "relations.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())

	for _, tc := range []struct{ file, what, want string }{
		// Absence: the anti-join, with the scope still inside. Outside it, a row
		// pointing at another tenant's would be judged against a row the caller
		// cannot see, and its absence from the page would report that row exists.
		{"fixture_repository.gen.go", "without renders a negated subquery",
			"g.Add(query.Related(query.Exists{From: from, On: on, Where: where, Not: true}))"},
		{"fixture_repository.gen.go", "the negated subquery is scoped too",
			`if p := f.Without; p != nil && p.HomeTeam != nil {`},

		// Ordering: the left join, with the scope in the ON clause rather than
		// the WHERE — in WHERE it would discard the rows the left join keeps.
		{"fixture_repository.gen.go", "ordering across a relation joins",
			`far, j := sc.orderJoin("team", alias, "id", "home_team_id")`},
		{"fixture_repository.gen.go", "the join carries its own scope",
			`j.Where.Add(far.tenant("tenant_id"))`},
		{"fixture_repository.gen.go", "the joins render before the conditions",
			"joinSQL := query.JoinSQL(joins, args)"},
		{"fixture_repository.gen.go", "the count is not joined",
			"countArgs := query.NewArgs()"},
		{"fixture_repository.gen.go", "an unknown ordering is refused",
			"has no relation named %q to order by"},
	} {
		src := collapse(find(t, artifacts, tc.file))
		if !strings.Contains(src, collapse(tc.want)) {
			t.Errorf("%s: %s\nexpected: %s", tc.file, tc.what, tc.want)
		}
	}

	// A has-many or a many-to-many cannot be ordered through — that needs an
	// aggregate, not a join — so Team, whose every relation is one of those,
	// gets no join builder at all rather than one that refuses at runtime.
	team := find(t, artifacts, "team_repository.gen.go")
	if strings.Contains(team, "teamOrderJoin") {
		t.Error("a table with no belongs-to relation should have no ordering join")
	}
	if !strings.Contains(collapse(team), collapse("return out, nil, nil")) {
		t.Error("its order builder should still answer with no joins")
	}

	// Every column named in a statement is qualified, because a joined
	// statement has two of some of them.
	if !strings.Contains(collapse(find(t, artifacts, "fixture_repository.gen.go")),
		collapse(`const fixtureRepoSelect = "fixture.id`)) {
		t.Error("the select list should be qualified with its table")
	}
}

// A key must be no less traceable than a person: a table that records who
// changed a row and not which key a change came through refuses a write from a
// key, rather than writing one nobody can attribute.
//
// Constructed here rather than demonstrated in an example, because the
// foundation carries the key columns on every table that records an actor — so
// the interesting case does not occur there, which is the point of it.
func TestAKeyCannotWriteATableThatWouldNotRecordIt(t *testing.T) {
	t.Parallel()

	withKeys := func(t *testing.T) *ir.Document {
		t.Helper()
		doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
		// The guard only appears in a project that has keys at all: without an
		// rig_api_key table no request can arrive with one, and a check for something
		// that cannot happen is noise in everybody's repository.
		doc.Schema.Tables = append(doc.Schema.Tables, ir.Table{Name: "rig_api_key"})
		// The document indexes itself lazily and caches it, so a table added
		// after a lookup would be invisible.
		doc.Reindex()
		return doc
	}

	t.Run("refused when the table cannot record the key", func(t *testing.T) {
		t.Parallel()

		artifacts := gentest.Run(t, persistgo.New(), withKeys(t), opts())
		src := collapse(find(t, artifacts, "lesson_repository.gen.go"))

		if !strings.Contains(src, collapse("if claims.APIKeyID != nil {")) {
			t.Fatalf("no guard emitted:\n%s", src)
		}
		if !strings.Contains(src, "does not record which key made a change") {
			t.Error("the refusal should say why")
		}

		// Every write, not only the create: an update or a delete nobody can
		// attribute is the same hole.
		if n := strings.Count(src, "claims.APIKeyID != nil"); n != 4 {
			t.Errorf("%d guards, want one per write (create, update, delete, restore)", n)
		}
	})

	t.Run("allowed when it can", func(t *testing.T) {
		t.Parallel()

		doc := withKeys(t)
		res := doc.Resource("Lesson")
		for _, pair := range []struct {
			key **ir.ColumnRef
			col string
		}{
			{&res.Storage.Audit.CreatedByAPIKey, "created_by_api_key_id"},
			{&res.Storage.Audit.UpdatedByAPIKey, "updated_by_api_key_id"},
			{&res.Storage.Audit.DeletedByAPIKey, "deleted_by_api_key_id"},
		} {
			*pair.key = &ir.ColumnRef{Table: res.Storage.Table, Name: pair.col, SQLType: "uuid", Nullable: true}
		}

		artifacts := gentest.Run(t, persistgo.New(), doc, opts())
		src := find(t, artifacts, "lesson_repository.gen.go")

		if strings.Contains(src, "claims.APIKeyID != nil") {
			t.Error("a table that records the key should not refuse one")
		}
		// And it stamps it instead, which is the other half of the same rule.
		if !strings.Contains(collapse(src), collapse("claims.ActorKey()")) {
			t.Error("the key should be stamped on a write")
		}
	})
}

func TestEmitsExpectedFiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())

	var names []string
	for _, a := range artifacts {
		names = append(names, a.Path)
		if a.Mode != gen.Overwrite {
			t.Errorf("%s should be rewritten on every run, not written once", a.Path)
		}
		if !strings.Contains(a.Path, ".gen.go") {
			t.Errorf("%s should be named .gen.go so it is gitignored and lint-excluded", a.Path)
		}
	}

	for _, want := range []string{"store.gen.go", "lesson_repository.gen.go"} {
		if !contains(names, want) {
			t.Errorf("%s was not emitted; got %v", want, names)
		}
	}

	// The entity, its enums, and its query types moved to the model package,
	// which both this layer and the API layer import.
	for _, moved := range []string{"lesson.gen.go", "lesson_query.gen.go", "lesson_status.gen.go"} {
		if contains(names, moved) {
			t.Errorf("%s belongs to model-go now; got %v", moved, names)
		}
	}
}

// The generated code is the floor the service layer stands on, so what it does
// without being asked is the part worth pinning down.
func TestRepositoryEnforcesTheInvariants(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())
	src := find(t, artifacts, "lesson_repository.gen.go")

	for _, tc := range []struct{ what, want string }{
		{"reads require claims", "tenancy.FromContext(ctx)"},
		{"reads are scoped by tenant", `query.Eq("tenant_id", claims.TenantID)`},
		{"deleted rows are excluded by default", `query.IsNull("deleted_at")`},
		{"snapshots are excluded by default", `query.Eq("version_type", model.LessonVersionTypeOriginal)`},
		{"an update snapshots first", "r.writeSnapshot(ctx, tx, prev)"},
		{"a snapshot cannot be updated", "is a snapshot and cannot be changed"},
		{"delete retires rather than removes", `columns := []string{"deleted_at"}`},
		{"restore checks the window", "LessonRestoreCutoff()"},
		{"the window is the configured one", "AddDate(0, 0, -30)"},
	} {
		if !strings.Contains(src, tc.want) {
			t.Errorf("%s: expected %q in the repository", tc.what, tc.want)
		}
	}

	// An update and its snapshot have to land together or not at all.
	if !strings.Contains(src, "dbx.InTx(ctx, r.db.pool") {
		t.Error("writes should run in a transaction")
	}
}

func TestPackageOption(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, gen.Options{Raw: map[string]any{
		"package":      "persistence",
		"model_import": "rigtest/model",
	}})

	for _, a := range artifacts {
		if !strings.Contains(string(a.Content), "package persistence") {
			t.Errorf("%s does not use the configured package name", a.Path)
		}
	}
}

func TestUnknownOptionIsRejected(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	_, err := persistgo.New().Generate(t.Context(), doc, gen.Options{Raw: map[string]any{
		"packge":       "store",
		"model_import": "rigtest/model",
	}})

	// A mistyped option that is silently ignored looks configured and behaves
	// as though it is not.
	if err == nil {
		t.Fatal("an unknown option should be rejected")
	}
	if !strings.Contains(err.Error(), "packge") {
		t.Errorf("the error should name the offending key: %v", err)
	}
}

// The five steps of a delete, in order, and the guard around them. The order is
// the whole of what the repository contributes to the propagation — every
// decision above it was the compiler's — so it is asserted against the text
// rather than inferred from the fact that it compiles.
func TestDeleteRunsTheChildrenInTheRightPlace(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "relations.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())
	team := collapse(find(t, artifacts, "team_repository.gen.go"))

	for _, tc := range []struct{ what, want string }{
		// The parent's own veto first: the cheapest and most specific rule
		// should not have to wait for every child's cleanup.
		{"the row's own Before comes first", "if in.Hooks.Before != nil {"},
		{"then every child, in the order the document settled",
			"for _, child := range in.Hooks.Children { if child.Deleting == nil { continue }"},
		{"a refusal names the relation that refused",
			`return fmt.Errorf("%s: %w", child.Child, err)`},
		{"the after-commit half is queued, not run",
			"dbx.AfterCommit(ctx, func() { done(ctx, claims, row, input) })"},
		{"a cycle terminates rather than exhausting the stack",
			`dbx.EnterDelete(ctx, "team", in.Input.ID.String())`},
		{"a row already being deleted in this transaction is a no-op",
			"if !more { return nil }"},
	} {
		if !strings.Contains(team, collapse(tc.want)) {
			t.Errorf("%s\nexpected: %s", tc.what, tc.want)
		}
	}

	before := strings.Index(team, "if in.Hooks.Before != nil {")
	children := strings.Index(team, "for _, child := range in.Hooks.Children {")
	write := strings.Index(team, "DELETE FROM team WHERE id = $1")
	if !(before < children && children < write) {
		t.Errorf("the order should be Before, then the children, then the row; got %d, %d, %d",
			before, children, write)
	}

	// A table nothing points at gets none of it, so a schema without relations
	// pays nothing for this feature.
	player := collapse(find(t, artifacts, "player_repository.gen.go"))
	if strings.Contains(player, "in.Hooks.Children") {
		t.Error("a resource with no children should not walk a child list")
	}
	if strings.Contains(player, "dbx.EnterDelete") {
		t.Error("a resource with no children cannot start a cycle, so it needs no guard")
	}
}

func find(t *testing.T, artifacts []gen.Artifact, name string) string {
	t.Helper()
	for _, a := range artifacts {
		if filepath.Base(a.Path) == name {
			return string(a.Content)
		}
	}
	t.Fatalf("no artifact named %s", name)
	return ""
}

// collapse squeezes runs of spaces, so an assertion about generated code does
// not depend on how gofmt chose to align it.
func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// Every instant a repository hands back is in UTC, and "every" has to mean every:
// pgx decodes a timestamptz into the host's zone, and it decodes the elements of
// a timestamptz[] the same way.
func TestEveryScannedInstantIsNormalized(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	res := doc.Resource("Lesson")

	// An array of instants, which nothing in the corpus has and which is the one
	// shape that would otherwise slip through.
	res.Fields = append(res.Fields, ir.ResourceField{
		Field: ir.Field{
			Name: "RemindedAts", Wire: "remindedAts", Type: ir.TypeTimestamp,
			TypeKind: ir.TypeKindPrimitive, GoType: "[]time.Time",
			Modifiers: []string{ir.ModifierArray},
			Column: &ir.ColumnRef{
				Table: res.Storage.Table, Name: "reminded_ats",
				SQLType: "timestamptz[]", Nullable: true, Scan: ir.ScanArray,
			},
		},
		Operations: []string{ir.FieldOpRead},
	})
	// And a date beside it, which is a calendar day rather than a moment.
	res.Fields = append(res.Fields, ir.ResourceField{
		Field: ir.Field{
			Name: "PlayedOn", Wire: "playedOn", Type: ir.TypeDate,
			TypeKind: ir.TypeKindPrimitive, GoType: "*time.Time",
			Modifiers: []string{ir.ModifierNullable},
			Column: &ir.ColumnRef{
				Table: res.Storage.Table, Name: "played_on",
				SQLType: "date", Nullable: true, Scan: ir.ScanDirect,
			},
		},
		Operations: []string{ir.FieldOpRead},
	})

	table := doc.Table(res.Storage.Table)
	table.Columns = append(table.Columns,
		ir.Column{
			Name: "reminded_ats", SQLType: "timestamptz[]", UDTName: "_timestamptz",
			Nullable: true, Ordinal: len(table.Columns) + 1,
		},
		ir.Column{
			Name: "played_on", SQLType: "date",
			Nullable: true, Ordinal: len(table.Columns) + 2,
		},
	)
	doc.Reindex()

	src := collapse(find(t, gentest.Run(t, persistgo.New(), doc, opts()),
		"lesson_repository.gen.go"))

	for _, want := range []string{
		// Not null: a plain value.
		"m.CreatedAt = dbx.UTC(m.CreatedAt)",
		// Nullable: nil has to stay nil, or a row that was never deleted would
		// claim to have been deleted at the zero time.
		"m.DeletedAt = dbx.UTCPtr(m.DeletedAt)",
		// And the array, element by element.
		"m.RemindedAts = dbx.UTCSlice(m.RemindedAts)",
	} {
		if !strings.Contains(src, collapse(want)) {
			t.Errorf("missing %q", want)
		}
	}

	// A date is not an instant and must not be touched. pgx already returns one
	// in UTC, and a calendar day has no zone to convert — normalizing it in a
	// negative-offset session would be free to move it to the day before.
	if !strings.Contains(src, "m.PlayedOn") {
		t.Fatal("the date column should still be scanned")
	}
	if strings.Contains(src, collapse("m.PlayedOn = dbx.")) {
		t.Error("a date should not be normalized as though it were an instant")
	}
}

// TestAnOwnerScopedReadNarrowsToTheCaller is the security-relevant half of the
// scope parameter: the predicate is in the repository, applied unless something
// explicitly drops it.
func TestAnOwnerScopedReadNarrowsToTheCaller(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())
	src := collapse(find(t, artifacts, "memo_repository.gen.go"))

	// The list path, and the by-identifier path every write starts from.
	for _, want := range []string{
		`if !cfg.SkipOwnerScope { scope.Add(sc.at(query.Eq("created_by_account_id", claims.AccountID))) }`,
		`if !cfg.SkipOwnerScope { args = append(args, claims.AccountID) where += fmt.Sprintf(" AND created_by_account_id = $%d", len(args)) }`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing the owner predicate:\n%s", want)
		}
	}

	// The trash takes options too, so a wide caller can see the whole tenant's
	// deleted rows rather than only its own.
	if !strings.Contains(src, "ListDeleted(ctx context.Context, f model.MemoFilter, page model.MemoPage, opts ...readopt.Option)") {
		t.Error("ListDeleted does not accept read options")
	}
}

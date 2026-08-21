package electricgo_test

import (
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/internal/gen/electricgo"
	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const fixture = "lifecycle.ir.json"

func opts() gen.Options {
	return gen.Options{
		OutDir: ".",
		Raw: map[string]any{
			"package":      "electric",
			"electric_url": "http://electric:3000",
		},
	}
}

func TestGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, electricgo.New(), doc, opts())

	gentest.Golden(t, filepath.Join("testdata", "lifecycle"), artifacts, *update)
}

func TestDeterministic(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	gentest.Deterministic(t, electricgo.New(), doc, opts())
}

func TestGeneratedCodeCompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))

	all := gentest.Run(t, electricgo.New(), doc, gen.Options{
		Raw: map[string]any{
			"package":      "electric",
			"electric_url": "http://electric:3000",
			"shape_import": "rigtest/electric",
			"stub_dir":     "shapes/{table}",
		},
	})

	var shapes, stubs []gen.Artifact
	for _, a := range all {
		if a.Mode == gen.CreateOnce {
			stubs = append(stubs, a)
			continue
		}
		shapes = append(shapes, a)
	}
	if len(stubs) == 0 {
		t.Fatal("no scoping stub was emitted")
	}

	gentest.MustCompileAll(t,
		gentest.Package{Dir: "electric", Artifacts: shapes},
		gentest.Package{Dir: "shapes/lesson", Artifacts: stubs},
	)
}

// The filter is the whole point of the package. A shape whose tenant condition
// can be left off is a subscription to somebody else's data.
func TestTheFilterIsBuiltBeforeAnythingElseRuns(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "lesson_shape.gen.go")

	body, ok := between(src, "func handleLessonShape(", "\n}")
	if !ok {
		t.Fatal("no handler")
	}
	collapsed := collapse(body)

	for _, want := range []string{
		`where.Eq("tenant_id", claims.TenantID.String())`,
		`where.IsNull("deleted_at")`,
		`where.Eq("version_type", "Original")`,
	} {
		if !strings.Contains(collapsed, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	// Order matters: the scope runs last, and every condition is joined with
	// AND, so there is nothing a scope can do to widen the shape.
	tenant := strings.Index(collapsed, "tenant_id")
	scope := strings.Index(collapsed, "scope(r.Context()")
	if tenant < 0 || scope < 0 || tenant > scope {
		t.Error("the tenant condition should be added before the application's scope runs")
	}

	// And the proxy call comes after both.
	if serve := strings.Index(collapsed, "s.Proxy.Serve("); serve < scope {
		t.Error("the shape should not be served before the scope has run")
	}
}

// An owner-scoped table streams the caller's own rows and nothing else.
//
// This was a hole rather than a decision: the shape builder emitted the tenant,
// soft-delete and snapshot predicates and ignored the owner the IR has carried
// since owner scoping shipped, so an owner-scoped table with a shape streamed
// the whole tenant unless the application remembered to narrow it in the stub —
// the one narrowing a stub should never have been responsible for, because the
// repository does not make anybody remember it.
func TestAnOwnerScopedShapeNarrowsToTheCaller(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "memo_shape.gen.go")

	body, ok := between(src, "func handleMemoShape(", "\n}")
	if !ok {
		t.Fatal("no handler")
	}
	collapsed := collapse(body)

	if want := `where.Eq("created_by_account_id", claims.AccountID.String())`; !strings.Contains(collapsed, collapse(want)) {
		t.Errorf("missing %s:\n%s", want, body)
	}

	// Before the stub, so the stub can still only narrow.
	owner := strings.Index(collapsed, "created_by_account_id")
	scope := strings.Index(collapsed, "scope(r.Context()")
	if owner < 0 || scope < 0 || owner > scope {
		t.Error("the owner condition should be added before the application's scope runs")
	}

	// Refused rather than narrowed. An API key and a system credential both
	// have a nil identifier, and Eq against one matches nothing *silently* — a
	// subscriber handed an empty stream cannot tell it from having no rows.
	if !strings.Contains(collapsed, "if claims.AccountID == uuid.Nil {") {
		t.Errorf("a caller with no account should be refused:\n%s", body)
	}
	if refuse := strings.Index(collapsed, "uuid.Nil"); refuse < 0 || refuse > owner {
		t.Error("the refusal should come before the predicate it stands in for")
	}
}

func TestATableWithNoTenantIsStillFiltered(t *testing.T) {
	t.Parallel()

	// A shape on a table with no tenant column has no tenant condition to add,
	// which is correct — but it must not silently lose the lifecycle ones.
	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	res := doc.Resource("Lesson")
	res.Storage.Tenant = nil

	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "lesson_shape.gen.go")
	body, _ := between(src, "func handleLessonShape(", "\n}")

	if strings.Contains(body, "tenant_id") {
		t.Error("there is no tenant column to filter by")
	}
	if !strings.Contains(collapse(body), `where.IsNull("deleted_at")`) {
		t.Errorf("the lifecycle conditions should still be there:\n%s", body)
	}
}

// A shape carries every column it names to every subscriber, forever. A column
// excluded from the API has no business in a live stream either.
func TestOnlyReadableColumnsAreStreamed(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "lesson_shape.gen.go")

	columns, ok := between(src, "var LessonShapeColumns = []string{", "\n}")
	if !ok {
		t.Fatal("no column list")
	}
	if !strings.Contains(columns, `"title"`) {
		t.Errorf("a readable column is missing:\n%s", columns)
	}
	// row_version is excluded in the fixture's configuration.
	if strings.Contains(columns, "row_version") {
		t.Errorf("an excluded column reached the stream:\n%s", columns)
	}
}

func TestDeclaredParametersAreTyped(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "lesson_shape.gen.go")

	params, ok := between(src, "type LessonShapeParams struct {", "\n}")
	if !ok {
		t.Fatal("no params type")
	}
	if !strings.Contains(collapse(params), "Matchday int") {
		t.Errorf("a declared Int parameter should be an int:\n%s", params)
	}
	// An optional parameter needs a way to tell a zero value from an absent
	// one, or "matchday=0" and "no matchday" are the same request.
	if !strings.Contains(collapse(params), "HasMatchday bool") {
		t.Errorf("an optional parameter needs a presence flag:\n%s", params)
	}
	if strings.Contains(collapse(params), "HasStatus bool") {
		t.Error("a required parameter is always present and needs no flag")
	}

	parse, _ := between(src, "func parseLessonShapeParams(", "\n}")
	if !strings.Contains(collapse(parse), `required(r, "status")`) {
		t.Errorf("a required parameter should be required:\n%s", parse)
	}
	if !strings.Contains(collapse(parse), `optional(r, "matchday")`) {
		t.Errorf("an optional one should not be:\n%s", parse)
	}
}

// A server that cannot identify a subscriber has no tenant filter to build.
func TestRegisterRefusesWithoutClaimsOrAProxy(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "electric.gen.go")

	register, ok := between(src, "func Register(", "\n}")
	if !ok {
		t.Fatal("no Register")
	}
	for _, want := range []string{"Server.Proxy is required", "Server.GetClaims is required"} {
		if !strings.Contains(register, want) {
			t.Errorf("Register should refuse without %q:\n%s", want, register)
		}
	}
}

// An admin-only shape with no way to check for an administrator must refuse
// everybody, not admit everybody.
func TestAdminShapesRefuseWhenUnconfigured(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.Resource("Lesson").Electric.Auth = ir.ElectricAuthAdmin

	artifacts := gentest.Run(t, electricgo.New(), doc, opts())

	base := find(t, artifacts, "electric.gen.go")
	if !strings.Contains(collapse(base), "s.IsAdmin == nil || !s.IsAdmin(claims)") {
		t.Error("an unconfigured IsAdmin should refuse rather than admit")
	}

	shape := find(t, artifacts, "lesson_shape.gen.go")
	if !strings.Contains(collapse(shape), "prepare(s, w, r, true)") {
		t.Error("an admin shape should ask for the admin check")
	}
}

// Nothing has asked for live sync, so there is nothing to generate — and an
// empty package is a directory nobody imports plus a manifest entry nobody
// wants.
func TestNothingIsEmittedWithoutAShape(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.Resource("Lesson").Electric = nil

	if artifacts := gentest.Run(t, electricgo.New(), doc, opts()); len(artifacts) != 0 {
		t.Errorf("got %d files, want none", len(artifacts))
	}
}

func TestUnknownOptionIsRejected(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	_, err := electricgo.New().Generate(t.Context(), doc,
		gen.Options{Raw: map[string]any{"electric_ur": "http://electric:3000"}})

	if err == nil {
		t.Fatal("a mistyped option should be rejected")
	}
	if !strings.Contains(err.Error(), "electric_ur") {
		t.Errorf("the error should name the key: %v", err)
	}
}

// The trash shape is the live one inverted. A row deleted while somebody holds
// both subscriptions leaves one stream and arrives in the other, which is the
// whole reason to have it rather than to poll GET /_deleted.
func TestTheTrashShapeWantsWhatTheLiveShapeExcludes(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "lesson_shape.gen.go")

	body, ok := between(src, "func handleLessonDeletedShape(", "\n}")
	if !ok {
		t.Fatal("no trash handler")
	}
	collapsed := collapse(body)

	for _, want := range []string{
		`where.Eq("tenant_id", claims.TenantID.String())`,
		`where.NotNull("deleted_at")`,
		// Still the live generation of the row: the trash is what was deleted,
		// not the history of what was deleted.
		`where.Eq("version_type", "Original")`,
	} {
		if !strings.Contains(collapsed, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	if strings.Contains(collapsed, collapse(`where.IsNull("deleted_at")`)) {
		t.Errorf("the trash shape should not exclude the rows it exists to carry:\n%s", body)
	}

	// Same as everywhere else: the scope runs last and can only narrow.
	deleted := strings.Index(collapsed, "deleted_at")
	scope := strings.Index(collapsed, "scope(r.Context()")
	if deleted < 0 || scope < 0 || deleted > scope {
		t.Error("the lifecycle condition should be added before the application's scope runs")
	}
}

// History is per row, matching GET /{id}/_versions and the ListSnapshots it
// calls. A table-wide stream of every version of everything would be a
// different and much larger thing than the endpoint it is named after.
func TestAHistoryShapeIsScopedToOneRow(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "lesson_shape.gen.go")

	body, ok := between(src, "func handleLessonVersionsShape(", "\n}")
	if !ok {
		t.Fatal("no history handler")
	}
	collapsed := collapse(body)

	for _, want := range []string{
		`id, err := parseUUID("id", r.PathValue("id"))`,
		`where.Eq("tenant_id", claims.TenantID.String())`,
		`where.Eq("version_type", "Snapshot")`,
		`where.Eq("snapshot_from_lesson_id", id.String())`,
	} {
		if !strings.Contains(collapsed, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	// Never the live row. That one is what the live shape is for.
	if strings.Contains(collapsed, collapse(`where.Eq("version_type", "Original")`)) {
		t.Errorf("the history shape should not carry the live row:\n%s", body)
	}

	// The row is bound before the scope runs, so there is nothing a scope can
	// do to point the shape at somebody else's history.
	row := strings.Index(collapsed, "snapshot_from_lesson_id")
	scope := strings.Index(collapsed, "scope(r.Context()")
	if row < 0 || scope < 0 || row > scope {
		t.Error("the row condition should be added before the application's scope runs")
	}
	if !strings.Contains(collapsed, "scope(r.Context(), r, claims, id, params, where)") {
		t.Errorf("the scope should receive the row it is the history of:\n%s", body)
	}
}

// Which shapes a table has is not configured. The columns decide, the same way
// they decide whether the API has a GET /_deleted — asking the schema twice is
// how the two answers get to disagree.
func TestTheExtraShapesComeFromTheColumns(t *testing.T) {
	t.Parallel()

	// Memo retires its rows and keeps no previous versions, so it has a trash
	// shape and no history one.
	doc := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	base := routes(t, find(t, gentest.Run(t, electricgo.New(), doc, opts()), "electric.gen.go"))

	if !strings.Contains(base, `"GET /electric/memo"`) {
		t.Error("the live shape should be mounted")
	}
	if !strings.Contains(base, `"GET /electric/memo/_deleted"`) {
		t.Errorf("a soft-deletable table should have a trash shape:\n%s", base)
	}
	if strings.Contains(base, "_versions") {
		t.Errorf("a table that keeps no versions has no history to stream:\n%s", base)
	}

	// And a table with neither has the live shape and nothing else.
	plain := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	plain.Resource("Memo").Storage.SoftDelete = nil
	only := routes(t, find(t, gentest.Run(t, electricgo.New(), plain, opts()), "electric.gen.go"))

	if !strings.Contains(only, `"GET /electric/memo"`) {
		t.Error("the live shape should still be mounted")
	}
	if strings.Contains(only, "_deleted") || strings.Contains(only, "_versions") {
		t.Errorf("nothing in the schema asks for an extra shape here:\n%s", only)
	}
}

// The owner predicate went missing from shapes once already, and it went
// missing because somebody wrote the tenant, soft-delete and snapshot ones and
// stopped. Adding two more routes is exactly the shape of that mistake, so
// every one of them is checked rather than the first.
func TestEveryShapeCarriesTheTenantAndOwnerConditions(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	src := find(t, gentest.Run(t, electricgo.New(), doc, opts()), "memo_shape.gen.go")

	for _, handler := range []string{"handleMemoShape(", "handleMemoDeletedShape("} {
		body, ok := between(src, "func "+handler, "\n}")
		if !ok {
			t.Fatalf("no %s", handler)
		}
		collapsed := collapse(body)

		for _, want := range []string{
			`where.Eq("tenant_id", claims.TenantID.String())`,
			`where.Eq("created_by_account_id", claims.AccountID.String())`,
			"if claims.AccountID == uuid.Nil {",
		} {
			if !strings.Contains(collapsed, collapse(want)) {
				t.Errorf("%s is missing %s:\n%s", handler, want, body)
			}
		}
	}
}

// A stub is written once and then belongs to the developer, so a shape that
// shared one with another would have no way to be scoped separately — and a
// project that regenerates after these shapes existed would find its own file
// rewritten, which is the one thing CreateOnce promises never happens.
func TestEachShapeGetsItsOwnStub(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, electricgo.New(), doc, gen.Options{
		Raw: map[string]any{
			"package":      "electric",
			"shape_import": "rigtest/electric",
			"stub_dir":     "shapes/{table}",
		},
	})

	stubs := map[string]string{}
	for _, a := range artifacts {
		if a.Mode == gen.CreateOnce {
			stubs[filepath.Base(a.Path)] = string(a.Content)
		}
	}

	for file, want := range map[string]string{
		"lesson_shape.go":          ".LessonScope = Shape",
		"lesson_deleted_shape.go":  ".LessonDeletedScope = DeletedShape",
		"lesson_versions_shape.go": ".LessonVersionsScope = VersionsShape",
	} {
		src, ok := stubs[file]
		if !ok {
			t.Errorf("no stub named %s", file)
			continue
		}
		if !strings.Contains(src, want) {
			t.Errorf("%s should assert %q:\n%s", file, want, src)
		}
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

// http.ServeMux panics on conflicting patterns, and it does it at registration
// — so a shape whose route overlaps another's is not a bad response, it is an
// application that will not start. The generated patterns are mounted here for
// real rather than reasoned about, because "/x/_deleted" and "/x/{id}/_thing"
// are exactly the pair where the reasoning is easy to get wrong.
func TestTheGeneratedRoutesMountOnARealMux(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	body := routes(t, find(t, gentest.Run(t, electricgo.New(), doc, opts()), "electric.gen.go"))

	patterns := mounted(body)
	if len(patterns) != 3 {
		t.Fatalf("got %d routes %v, want the live, trash and history shapes", len(patterns), patterns)
	}

	mux := http.NewServeMux()
	for _, p := range patterns {
		p := p
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, p)
		})
	}

	// And each one answers for itself: registering without a panic proves only
	// that the patterns can coexist, not that they mean different things.
	for path, want := range map[string]string{
		"/electric/lesson":                                    "GET /electric/lesson",
		"/electric/lesson/_deleted":                           "GET /electric/lesson/_deleted",
		"/electric/lesson/" + uuid.NewString() + "/_versions": "GET /electric/lesson/{id}/_versions",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if got := rec.Body.String(); got != want {
			t.Errorf("%s dispatched to %q, want %q", path, got, want)
		}
	}
}

// mounted is the pattern of every mux.HandleFunc in a Register body.
func mounted(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([^"]+)"`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// routes is Register's body: the mounted patterns and nothing else. The doc
// comments around it talk about _deleted in prose, and a test that grepped the
// whole file would find that and call it a route.
func routes(t *testing.T, src string) string {
	t.Helper()
	body, ok := between(src, "func Register(", "\n}")
	if !ok {
		t.Fatal("no Register function")
	}
	return body
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func between(s, start, end string) (string, bool) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

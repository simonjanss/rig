package electricgo_test

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

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

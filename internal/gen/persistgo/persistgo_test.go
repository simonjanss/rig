package persistgo_test

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/pkg/gen"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const pkg = "store"

func opts() gen.Options {
	return gen.Options{OutDir: ".", Raw: map[string]any{"package": pkg}}
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
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())

	gentest.MustCompile(t, artifacts, pkg)
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

	for _, want := range []string{
		"store.gen.go",
		"lesson.gen.go",
		"lesson_query.gen.go",
		"lesson_repository.gen.go",
		"lesson_status.gen.go",
		"lesson_version_type.gen.go",
	} {
		if !contains(names, want) {
			t.Errorf("%s was not emitted; got %v", want, names)
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
		{"snapshots are excluded by default", `query.Eq("version_type", LessonVersionTypeOriginal)`},
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

func TestImmutableFieldIsAbsentFromUpdate(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())
	src := find(t, artifacts, "lesson.gen.go")

	update, ok := between(src, "type LessonUpdate struct {", "\n}")
	if !ok {
		t.Fatal("no LessonUpdate type")
	}

	// starts_at is immutable in the fixture. It is not rejected at runtime; it
	// is simply not a field anyone can set.
	if strings.Contains(update, "StartsAt") {
		t.Errorf("an immutable field must not appear in the update input:\n%s", update)
	}
	if !strings.Contains(update, "Title patch.Patch[string]") {
		t.Errorf("an ordinary field should be a patch:\n%s", update)
	}

	create, _ := between(src, "type LessonCreate struct {", "\n}")
	if !strings.Contains(create, "StartsAt") {
		t.Errorf("an immutable field should still be settable on create:\n%s", create)
	}
	// The framework's own columns are never the caller's to supply.
	for _, managed := range []string{"TenantID", "CreatedAt", "VersionType"} {
		if strings.Contains(create, managed) {
			t.Errorf("%s should not be in the create input:\n%s", managed, create)
		}
	}
}

func TestExcludedColumnIsNotAField(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())
	src := find(t, artifacts, "lesson.gen.go")

	model, _ := between(src, "type Lesson struct {", "\n}")
	if strings.Contains(model, "RowVersion") {
		t.Errorf("an excluded column should not reach the model:\n%s", model)
	}
	// It is still a column, so the constant and the select list keep it: the
	// row has to be scanned in the table's order.
	// gofmt aligns the constant block, so the assertion cannot assume a single
	// space around the equals sign.
	if !strings.Contains(collapse(src), `ColumnLessonRowVersion = "row_version"`) {
		t.Error("an excluded column should still have a name constant")
	}
}

func TestQueryTypesAreNarrow(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())
	src := find(t, artifacts, "lesson_query.gen.go")

	// Splitting operators into separate structs is what keeps the query typed.
	comparable, ok := between(src, "type LessonComparable struct {", "\n}")
	if !ok {
		t.Fatal("no LessonComparable type")
	}
	if strings.Contains(comparable, "Title") {
		t.Errorf("a text column cannot be ordered, so it does not belong here:\n%s", comparable)
	}
	if !strings.Contains(comparable, "StartsAt") {
		t.Errorf("a timestamp should be comparable:\n%s", comparable)
	}

	like, _ := between(src, "type LessonLike struct {", "\n}")
	if strings.Contains(like, "Capacity") {
		t.Errorf("an integer has no pattern to match:\n%s", like)
	}
	if !strings.Contains(like, "Title") {
		t.Errorf("a text column should be matchable:\n%s", like)
	}

	null, _ := between(src, "type LessonNull struct {", "\n}")
	if strings.Contains(null, "Title") {
		t.Errorf("a NOT NULL column can never be null:\n%s", null)
	}
	if !strings.Contains(null, "Notes") {
		t.Errorf("a nullable column should have a presence flag:\n%s", null)
	}
}

func TestEnumIsAStringType(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc, opts())
	src := find(t, artifacts, "lesson_status.gen.go")

	// The value that reaches the database has to be the label, so the Go
	// constant and the column contents are one thing rather than two.
	if !strings.Contains(src, "type LessonStatus string") {
		t.Error("an enum should be a string type")
	}
	if !strings.Contains(src, `LessonStatusInProgress LessonStatus = "in_progress"`) {
		t.Error("the constant should carry the Postgres label as its value")
	}
	if !strings.Contains(src, "func (v LessonStatus) Valid() bool") {
		t.Error("an enum should be able to check itself")
	}
}

func TestPackageOption(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, persistgo.New(), doc,
		gen.Options{Raw: map[string]any{"package": "persistence"}})

	for _, a := range artifacts {
		if !strings.Contains(string(a.Content), "package persistence") {
			t.Errorf("%s does not use the configured package name", a.Path)
		}
	}
}

func TestUnknownOptionIsRejected(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	_, err := persistgo.New().Generate(t.Context(), doc,
		gen.Options{Raw: map[string]any{"packge": "store"}})

	// A mistyped option that is silently ignored looks configured and behaves
	// as though it is not.
	if err == nil {
		t.Fatal("an unknown option should be rejected")
	}
	if !strings.Contains(err.Error(), "packge") {
		t.Errorf("the error should name the offending key: %v", err)
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

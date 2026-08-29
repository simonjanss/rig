//go:build docker

// This suite needs a container engine. It runs one Postgres, applies a
// migration corpus, and asserts what comes back out.
//
//	go test -tags docker ./internal/introspect/
//
// The corpus deliberately includes the awkward cases — a generated column, a
// partial index, a gin index, an array, a numeric with precision, a view, a
// self-referencing foreign key, a cascade — because those are the ones a
// hand-written fixture gets subtly wrong.
package introspect_test

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"
	"github.com/jackc/pgx/v5"

	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/internal/introspect"
	"github.com/simonjanss/rig/pkg/ir"
)

var update = flag.Bool("update", false, "rewrite the golden schema")

const (
	containerName = "rig-introspect-test"
	containerPort = dockerdb.PortIntrospect
)

// schema brings up the corpus once and reads it back. Every test shares it:
// starting a container per test would turn a two-second suite into a minute.
func schema(t *testing.T) ir.Schema {
	t.Helper()
	once.Do(func() { shared, sharedErr = read() })
	if sharedErr != nil {
		t.Fatalf("read the corpus: %v", sharedErr)
	}
	return shared
}

var (
	once   sync.Once
	shared ir.Schema
	// sharedURL is where the corpus ended up, which under isolation is the only
	// way a second connection finds the same container.
	sharedURL string
	sharedErr error
)

func read() (ir.Schema, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	db, err := dockerdb.Start(ctx, dockerdb.Config{
		Image:     "postgres:17-alpine",
		Name:      dockerdb.Qualify(containerName),
		Port:      dockerdb.HostPort(containerPort),
		Database:  "rig",
		User:      "rig",
		Password:  "rig",
		Log:       os.Stderr,
		StartWait: 2 * time.Minute,
	})
	if err != nil {
		return ir.Schema{}, err
	}
	sharedURL = db.URL()

	// A leftover container from a previous run would already have the corpus
	// applied, so the migration is a no-op rather than a failure.
	if _, err := dockerdb.Migrate(ctx, dockerdb.MigrateOptions{
		Dir:   filepath.Join("testdata", "migrations"),
		Table: "rig_test_migrations",
		URL:   db.URL(),
	}); err != nil {
		return ir.Schema{}, err
	}

	conn, err := pgx.Connect(ctx, db.URL())
	if err != nil {
		return ir.Schema{}, err
	}
	defer conn.Close(ctx)

	return introspect.Read(ctx, conn, introspect.Options{IncludeViews: true})
}

// TestGoldenSchema is the regression surface. A Postgres version bump lands
// here as a reviewable diff rather than as a mystery failure downstream.
func TestGoldenSchema(t *testing.T) {
	got := schema(t)

	// Bookkeeping is not part of the corpus.
	got.Tables = slices.DeleteFunc(got.Tables, func(tb ir.Table) bool {
		return tb.Name == "rig_test_migrations"
	})
	sortSchema(&got)

	encoded, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')

	path := filepath.Join("testdata", "schema.golden.json")
	if *update {
		if err := os.WriteFile(path, encoded, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Log("golden schema rewritten")
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v\n\nRun `go test -tags docker ./internal/introspect/ -update` to create it.", err)
	}
	if diff := gocmp.Diff(string(want), string(encoded)); diff != "" {
		t.Errorf("introspection changed (-want +got):\n%s", diff)
	}
}

func TestEnumsKeepDeclarationOrder(t *testing.T) {
	s := schema(t)

	e := findEnum(t, s, "lesson_status")
	got := []string{e.Values[0].Value, e.Values[1].Value, e.Values[2].Value}
	want := []string{"planned", "in_progress", "completed"}

	// Alphabetizing would turn a deliberate lifecycle into nonsense.
	if diff := gocmp.Diff(want, got); diff != "" {
		t.Errorf("enum order (-want +got):\n%s", diff)
	}
}

func TestColumnFacts(t *testing.T) {
	s := schema(t)
	lesson := findTable(t, s, "lesson")

	for _, tc := range []struct {
		column   string
		sqlType  string
		udt      string
		nullable bool
	}{
		{"id", "uuid", "uuid", false},
		{"title", "text", "text", false},
		{"notes", "text", "text", true},
		{"capacity", "integer", "int4", true},
		{"price", "numeric(10,2)", "numeric", true},
		{"tags", "text[]", "_text", true},
		{"payload", "jsonb", "jsonb", true},
		{"created_at", "timestamp with time zone", "timestamptz", false},
		{"starts_date", "date", "date", true},
		{"status", "lesson_status", "lesson_status", false},
	} {
		c := lesson.Column(tc.column)
		if c == nil {
			t.Errorf("no column %q", tc.column)
			continue
		}
		if c.SQLType != tc.sqlType || c.UDTName != tc.udt || c.Nullable != tc.nullable {
			t.Errorf("%s = %s/%s nullable=%v, want %s/%s nullable=%v",
				tc.column, c.SQLType, c.UDTName, c.Nullable, tc.sqlType, tc.udt, tc.nullable)
		}
	}
}

// TestGeneratedColumnHasNoDefault is the distinction that decides whether a
// column is writable. Postgres stores a generated column's expression in the
// same place as a default, and reporting it as one would offer clients a field
// they can never set.
func TestGeneratedColumnHasNoDefault(t *testing.T) {
	s := schema(t)
	lesson := findTable(t, s, "lesson")

	slug := lesson.Column("slug")
	if !slug.Generated {
		t.Error("slug should be marked generated")
	}
	if slug.HasDefault || slug.Default != "" {
		t.Errorf("a generated column has no default, got %q", slug.Default)
	}

	// An ordinary default is still reported.
	published := lesson.Column("is_published")
	if !published.HasDefault || published.Default == "" {
		t.Errorf("is_published should carry its default, got %+v", published)
	}
}

func TestComments(t *testing.T) {
	s := schema(t)
	lesson := findTable(t, s, "lesson")

	if lesson.Comment != "A scheduled teaching occasion." {
		t.Errorf("table comment = %q", lesson.Comment)
	}
	title := lesson.Column("title")
	if title.Comment != "Name shown in the timetable." {
		t.Errorf("column comment = %q", title.Comment)
	}
	if title.CommentSource != ir.CommentSourceDatabase {
		t.Errorf("comment source = %q, want database", title.CommentSource)
	}
	if lesson.Column("notes").Comment != "" {
		t.Error("a column with no COMMENT ON should have none")
	}
}

func TestConstraints(t *testing.T) {
	s := schema(t)
	lesson := findTable(t, s, "lesson")

	if diff := gocmp.Diff([]string{"id"}, lesson.PrimaryKey); diff != "" {
		t.Errorf("primary key (-want +got):\n%s", diff)
	}
	if len(lesson.Uniques) != 1 {
		t.Fatalf("uniques = %v, want one", lesson.Uniques)
	}
	if diff := gocmp.Diff([]string{"tenant_id", "title"}, lesson.Uniques[0]); diff != "" {
		t.Errorf("unique columns (-want +got):\n%s", diff)
	}
	if len(lesson.Checks) != 1 || lesson.Checks[0].Name != "lesson_capacity_positive" {
		t.Errorf("checks = %+v", lesson.Checks)
	}
	if len(lesson.ForeignKeys) != 1 {
		t.Fatalf("foreign keys = %+v, want the self reference", lesson.ForeignKeys)
	}
	fk := lesson.ForeignKeys[0]
	if fk.ForeignTable != "lesson" || fk.Columns[0] != "snapshot_from_lesson_id" {
		t.Errorf("self reference = %+v", fk)
	}

	// Referential actions decide whether rig's cascade rule can fire.
	link := findTable(t, s, "lesson_tag")
	var sawCascade, sawRestrict bool
	for _, fk := range link.ForeignKeys {
		if fk.OnDelete == "CASCADE" {
			sawCascade = true
		}
		if fk.OnUpdate == "RESTRICT" {
			sawRestrict = true
		}
	}
	if !sawCascade || !sawRestrict {
		t.Errorf("referential actions were not read back: %+v", link.ForeignKeys)
	}
}

func TestIndexes(t *testing.T) {
	s := schema(t)
	lesson := findTable(t, s, "lesson")

	byName := map[string]ir.Index{}
	for _, ix := range lesson.Indexes {
		byName[ix.Name] = ix
	}

	// Column order decides whether an index can serve a lookup.
	if diff := gocmp.Diff([]string{"tenant_id", "starts_at"}, byName["lesson_tenant_starts_idx"].Columns); diff != "" {
		t.Errorf("index columns (-want +got):\n%s", diff)
	}
	// A partial index cannot serve a general lookup, so the predicate has to
	// survive introspection or the index rules would accept it wrongly.
	if p := byName["lesson_live_idx"].Partial; p == "" {
		t.Error("the partial predicate was lost")
	}
	if m := byName["lesson_payload_idx"].Method; m != "gin" {
		t.Errorf("index method = %q, want gin", m)
	}
	if !byName["lesson_pkey"].Unique {
		t.Error("the primary key index should be unique")
	}
}

func TestViews(t *testing.T) {
	s := schema(t)

	v := findTable(t, s, "published_lesson")
	if v.Kind != ir.TableKindView {
		t.Errorf("kind = %q, want View", v.Kind)
	}
	if len(v.Columns) != 3 {
		t.Errorf("view columns = %d, want 3", len(v.Columns))
	}
}

func TestViewsExcludedByDefault(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	// Reuse the container the shared read already started.
	_ = schema(t)

	conn, err := pgx.Connect(ctx, sharedURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	s, err := introspect.Read(ctx, conn, introspect.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tb := range s.Tables {
		if tb.Name == "published_lesson" {
			t.Error("views should be excluded unless asked for")
		}
	}
}

func findTable(t *testing.T, s ir.Schema, name string) *ir.Table {
	t.Helper()
	for i := range s.Tables {
		if s.Tables[i].Name == name {
			return &s.Tables[i]
		}
	}
	t.Fatalf("no table %q; got %v", name, tableNames(s))
	return nil
}

func findEnum(t *testing.T, s ir.Schema, name string) ir.PgEnum {
	t.Helper()
	for _, e := range s.Enums {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("no enum %q", name)
	return ir.PgEnum{}
}

func tableNames(s ir.Schema) []string {
	out := make([]string, 0, len(s.Tables))
	for _, tb := range s.Tables {
		out = append(out, tb.Name)
	}
	return out
}

// sortSchema puts everything in a stable order so the golden file does not
// depend on the order the catalog happened to return rows in.
func sortSchema(s *ir.Schema) {
	slices.SortFunc(s.Tables, func(a, b ir.Table) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(s.Enums, func(a, b ir.PgEnum) int { return cmp.Compare(a.Name, b.Name) })
	for i := range s.Tables {
		t := &s.Tables[i]
		slices.SortFunc(t.Columns, func(a, b ir.Column) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
		slices.SortFunc(t.Indexes, func(a, b ir.Index) int { return cmp.Compare(a.Name, b.Name) })
		slices.SortFunc(t.ForeignKeys, func(a, b ir.ForeignKey) int { return cmp.Compare(a.Name, b.Name) })
		slices.SortFunc(t.Checks, func(a, b ir.Check) int { return cmp.Compare(a.Name, b.Name) })
	}
}

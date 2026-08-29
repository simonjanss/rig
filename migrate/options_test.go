package migrate_test

import (
	"database/sql"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/simonjanss/rig/migrate"
)

// What can be checked without a database: the refusals, the defaults, and the
// case that is not a failure. The rest is in migrate_docker_test.go, because
// applying a migration without somewhere to apply it proves nothing.

func TestUpRefusesWithoutADatabase(t *testing.T) {
	t.Parallel()

	_, err := migrate.Up(t.Context(), nil, fstest.MapFS{}, migrate.Options{})
	if err == nil {
		t.Fatal("no database is an error, not an empty result")
	}
	if !strings.Contains(err.Error(), "no database") {
		t.Errorf("err = %v", err)
	}
}

// A project that has not written a migration yet is a new project. Refusing to
// start over it would make `rig init` followed by `go run .` a failure.
func TestAnEmptyDirectoryIsNothingToDo(t *testing.T) {
	t.Parallel()

	db := unopened(t)

	for _, files := range []fstest.MapFS{
		{},                                      // no directory at all
		{"migrations/.keep": &fstest.MapFile{}}, // the one rig scaffolds
		{"elsewhere/00001.sql": &fstest.MapFile{}}, // nothing under migrations
	} {
		applied, err := migrate.Up(t.Context(), db, files, migrate.Options{})
		if err != nil {
			t.Errorf("%v: %v", keys(files), err)
		}
		if len(applied) != 0 {
			t.Errorf("%v: applied %v", keys(files), applied)
		}

		pending, err := migrate.Pending(t.Context(), db, files, migrate.Options{})
		if err != nil || len(pending) != 0 {
			t.Errorf("%v: pending = %v, %v", keys(files), pending, err)
		}
	}
}

// Dir is where rig puts them, which is also what an embed directive in the
// project root produces — so the common case needs no options at all.
func TestTheDirectoryDefaultsToMigrations(t *testing.T) {
	t.Parallel()

	db := unopened(t)
	files := fstest.MapFS{
		"migrations/00001_create.sql": &fstest.MapFile{Data: []byte(
			"-- +goose Up\nCREATE TABLE t (id int);\n")},
	}

	// Found under the default, so there is something pending. Reaching the
	// database is the next step and not this test's business, so the query is
	// allowed to fail — what matters is that it got that far.
	_, err := migrate.Pending(t.Context(), db, files, migrate.Options{})
	if err == nil || !strings.Contains(err.Error(), "migration status") {
		t.Errorf("err = %v, want it to have found the file and tried the database", err)
	}

	// And named explicitly, a different directory is read instead.
	elsewhere := fstest.MapFS{
		"db/00001_create.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")},
	}
	if _, err := migrate.Pending(t.Context(), db, elsewhere, migrate.Options{Dir: "db"}); err == nil {
		t.Error("Dir should have found the file under db/")
	}
}

// The bookkeeping table is what `rig db up` and a binary migrating itself have
// to agree about: two names means the second reader thinks nothing has run.
func TestTheTableDefaultIsTheOneTheCLIUses(t *testing.T) {
	t.Parallel()

	if migrate.DefaultTable != "rig_migrations" {
		t.Errorf("DefaultTable = %q: changing it silently re-applies every migration "+
			"in every existing database", migrate.DefaultTable)
	}
}

// Two sets recording themselves in one table is refused rather than allowed to
// half-work. They are numbered independently, so one set's version 2 would mark
// the other's as applied and the migration that never ran would never run.
func TestTwoSetsCannotShareABookkeepingTable(t *testing.T) {
	t.Parallel()

	db := unopened(t)
	srcs := []migrate.Source{
		{Name: "rig/auth", FS: fstest.MapFS{}, Dir: ".", Table: "rig_auth_migrations"},
		{Name: "rig/notify", FS: fstest.MapFS{}, Dir: ".", Table: "rig_auth_migrations"},
	}

	_, err := migrate.UpAll(t.Context(), db, srcs, migrate.Options{})
	if err == nil {
		t.Fatal("two sets sharing a table is an error, not something to sort out later")
	}
	for _, want := range []string{"rig/auth", "rig/notify", "rig_auth_migrations"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, should name %s", err, want)
		}
	}
}

// The project's own set is the one that may leave Table empty, so it collides
// with a module's set only if that module named the project's table.
func TestAnUnnamedTableIsTheProjectsOwn(t *testing.T) {
	t.Parallel()

	db := unopened(t)
	srcs := []migrate.Source{
		{Name: "rig/auth", FS: fstest.MapFS{}, Dir: ".", Table: "rig_auth_migrations"},
		{Name: "the project", FS: fstest.MapFS{}},
	}
	if _, err := migrate.UpAll(t.Context(), db, srcs, migrate.Options{}); err != nil {
		t.Fatalf("an unnamed table beside a named one is the ordinary case: %v", err)
	}

	clash := []migrate.Source{
		{Name: "rig/auth", FS: fstest.MapFS{}, Dir: ".", Table: migrate.DefaultTable},
		{Name: "the project", FS: fstest.MapFS{}},
	}
	if _, err := migrate.UpAll(t.Context(), db, clash, migrate.Options{}); err == nil {
		t.Error("a module writing into the project's own table is the collision that matters")
	}
}

// No sources at all is a caller mistake, not an empty run: it would report
// success having applied nothing, which is the one answer nobody can act on.
func TestNoSourcesIsRefused(t *testing.T) {
	t.Parallel()

	if _, err := migrate.UpAll(t.Context(), unopened(t), nil, migrate.Options{}); err == nil {
		t.Error("no sources should be an error")
	}
	if _, err := migrate.PendingAll(t.Context(), unopened(t), nil, migrate.Options{}); err == nil {
		t.Error("no sources should be an error")
	}
}

// Each set reads its own directory whatever the shared Options say, because Dir
// is a per-set fact: a module's set sits at the root of its own embedded
// filesystem, the project's under `migrations/`, and one Dir for both would empty
// whichever one lost.
//
// Checked without a database by what happens next. A set with nothing in its
// directory returns early and cannot fail; a set with a file in it builds a
// provider and then cannot connect. So the connection error is the evidence that
// Dir came from the [migrate.Source] — see the Docker suite for the same property
// asserted against a database that answers.
func TestASourceReadsItsOwnDir(t *testing.T) {
	t.Parallel()

	sql := fstest.MapFS{"00001_files.sql": &fstest.MapFile{Data: []byte("-- +goose Up\nSELECT 1;\n")}}
	shared := migrate.Options{Dir: "somewhere-else", Table: "rig_migrations"}

	// Dir "." is where the file is. Options names a directory that does not exist.
	found := []migrate.Source{{Name: "rig/files", FS: sql, Dir: ".", Table: "rig_files_migrations"}}
	if _, err := migrate.PendingAll(t.Context(), unopened(t), found, shared); err == nil {
		t.Error("the set's own Dir was not read: a migration was there to find")
	}

	// And the other way: a set whose own Dir is empty finds nothing, however many
	// files sit elsewhere in the same filesystem.
	empty := []migrate.Source{{Name: "rig/files", FS: sql, Dir: "nowhere", Table: "rig_files_migrations"}}
	pending, err := migrate.PendingAll(t.Context(), unopened(t), empty, shared)
	if err != nil {
		t.Errorf("an empty directory is a new set, not a failure: %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("pending = %v, want nothing", pending)
	}
}

// [migrate.Apply] and [migrate.Require] are the plural forms with the list
// written out, so the empty case has to survive the extra layer: a project that
// has not written a migration yet still starts.
//
// Checked the way [TestASourceReadsItsOwnDir] is — a set with nothing in its
// directory returns before a provider is built, so nothing connects and the pool
// below is never dialled. What a set with a file in it reports is a question for
// the Docker suite, because that is where the message can be read.
func TestTheSingularFormsAreThePluralOnes(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(t.Context(), "postgres://nowhere:5432/nothing")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	empty := fstest.MapFS{"elsewhere/00001.sql": &fstest.MapFile{}}

	if err := migrate.Apply(empty, migrate.Options{})(t.Context(), pool); err != nil {
		t.Errorf("Apply over an empty set: %v", err)
	}
	if err := migrate.Require(empty, migrate.Options{})(t.Context(), pool); err != nil {
		t.Errorf("Require over an empty set: %v", err)
	}
}

// unopened is a handle that has not connected to anything. Everything before
// the first query works on it, which is exactly the part with no database.
func unopened(t *testing.T) *sql.DB {
	t.Helper()

	db, err := sql.Open("pgx", "postgres://nowhere:5432/nothing")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return db
}

func keys(files fstest.MapFS) []string {
	out := make([]string, 0, len(files))
	for name := range files {
		out = append(out, name)
	}
	return out
}

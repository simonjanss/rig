package migrate_test

import (
	"database/sql"
	"strings"
	"testing"
	"testing/fstest"

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

		if version, err := migrate.Version(t.Context(), db, files, migrate.Options{}); err != nil || version != 0 {
			t.Errorf("%v: version = %d, %v", keys(files), version, err)
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

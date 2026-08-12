//go:build docker

package migrate_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/simonjanss/rig/migrate"
)

const dsnFallback = "postgres://rig:rig@localhost:55440/rig?sslmode=disable"

// One migration, in the shape goose reads and rig writes.
func schema(table string) fstest.MapFS {
	return fstest.MapFS{
		"migrations/00001_create.sql": &fstest.MapFile{Data: []byte(fmt.Sprintf(`-- +goose Up
CREATE TABLE %s (id int PRIMARY KEY);

-- +goose Down
DROP TABLE %s;
`, table, table))},
	}
}

func TestUpAppliesOnceAndIsIdempotent(t *testing.T) {
	db, opt := connect(t, "up")
	files := schema("migrate_test_up")

	pending, err := migrate.Pending(t.Context(), db, files, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending = %v, want the one migration", pending)
	}

	applied, err := migrate.Up(t.Context(), db, files, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 1 {
		t.Fatalf("applied = %v, want one", applied)
	}

	// The table is really there.
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM migrate_test_up`).Scan(&n); err != nil {
		t.Fatalf("the migration did not run: %v", err)
	}

	// And running again does nothing rather than failing.
	again, err := migrate.Up(t.Context(), db, files, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a second run applied %v, want nothing", again)
	}

	version, err := migrate.Version(t.Context(), db, files, opt)
	if err != nil {
		t.Fatal(err)
	}
	if version != 1 {
		t.Errorf("version = %d, want 1", version)
	}
}

// Replicas start together. The advisory lock is what keeps that from being a
// race between several processes running the same CREATE TABLE.
func TestConcurrentRunnersDoNotCollide(t *testing.T) {
	files := schema("migrate_test_race")

	const runners = 4
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		total   int
		failure error
	)

	for i := range runners {
		wg.Add(1)
		go func() {
			defer wg.Done()

			db, opt := connect(t, "race")
			applied, err := migrate.Up(context.Background(), db, files, opt)

			mu.Lock()
			defer mu.Unlock()
			total += len(applied)
			if err != nil {
				failure = errors.Join(failure, fmt.Errorf("runner %d: %w", i, err))
			}
		}()
	}
	wg.Wait()

	if failure != nil {
		t.Fatalf("a concurrent run failed: %v", failure)
	}
	// Exactly one of them applied it; the rest waited and found nothing to do.
	if total != 1 {
		t.Errorf("applied %d times across %d runners, want 1", total, runners)
	}
}

func TestAnEmptyDirectoryIsNotAFailure(t *testing.T) {
	db, opt := connect(t, "empty")

	applied, err := migrate.Up(t.Context(), db, fstest.MapFS{"migrations/.keep": &fstest.MapFile{}}, opt)
	if err != nil {
		t.Fatalf("a project with no migrations yet should not be an error: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("applied = %v", applied)
	}
}

// connect opens a handle and gives the run its own bookkeeping table, so one
// test's history is not another's.
func connect(t *testing.T, name string) (*sql.DB, migrate.Options) {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	t.Cleanup(func() { db.Close() })

	table := "migrate_test_" + name + "_version"
	t.Cleanup(func() {
		clean, err := sql.Open("pgx", dsn)
		if err != nil {
			return
		}
		defer clean.Close()
		_, _ = clean.Exec(`DROP TABLE IF EXISTS ` + table)
		_, _ = clean.Exec(`DROP TABLE IF EXISTS migrate_test_` + name)
	})

	return db, migrate.Options{Table: table}
}

// The other way round: a server that will not start against a database that is
// behind it, without being the thing that changes the schema.
func TestRequireRefusesUntilApplied(t *testing.T) {
	db, opt := connect(t, "require")
	pool := openPool(t)
	files := schema("migrate_test_require")

	check := migrate.Require(files, opt)

	err := check(t.Context(), pool)
	if err == nil {
		t.Fatal("a binary ahead of its database should refuse to start")
	}
	if !strings.Contains(err.Error(), "behind this binary") {
		t.Errorf("the refusal should say what is wrong: %v", err)
	}
	if !strings.Contains(err.Error(), "00001_create.sql") {
		t.Errorf("it should name the missing migration: %v", err)
	}

	if _, err := migrate.Up(t.Context(), db, files, opt); err != nil {
		t.Fatal(err)
	}

	if err := check(t.Context(), pool); err != nil {
		t.Errorf("with the schema applied it should start: %v", err)
	}
}

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}

	pool, err := pgxpool.New(t.Context(), dsn)
	if err != nil {
		t.Skipf("no database at %s: %v", dsn, err)
	}
	t.Cleanup(pool.Close)

	return pool
}

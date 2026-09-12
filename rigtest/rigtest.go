// Package rigtest brings up rig's foundation for a test, and reads its tables
// back.
//
// Every project on rig writes the same first two hundred lines before it can
// assert anything: a pool that refuses rather than skips when there is no
// database, the migration sets in the right order, a tenant of this test's own,
// and hand-written SQL against rig_account, rig_auth_log and rig_account_token.
// None of that is about the application. It is a schema rig owns and can change,
// so it is rig's to answer.
//
// What this package deliberately does not do is start a container or build an
// object graph. The first is the runner's — `rig db up`, a compose file, a CI
// service — and a harness that shelled out to Docker would be a second opinion
// about where the database is. The second is the application's: which resources
// exist, what they are called, and every assertion stay yours. A harness that
// owned those would be a framework for writing tests rather than a way to start
// one.
//
//	rt := rigtest.New(t, rigtest.Config{
//	    DatabaseURL: os.Getenv("DATABASE_URL"),
//	    Migrations:  api.MigrationSources(ownMigrations),
//	})
//
//	tenant := rt.Tenant(t)
//	accounts := rt.Accounts(t, tenant.ID)
//
// It imports testing, the way a package of helpers has to. Import it from a test
// and never from a path a deployment takes.
package rigtest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/migrate"
)

// Config is what a suite has to say.
type Config struct {
	// DatabaseURL is the database to run against. There is no default and no
	// fallback: a hardcoded port is wrong the moment two checkouts run at once,
	// and a harness that guessed would send one branch's migrations into
	// another's schema.
	//
	// Leave it empty and set Pool instead when the suite already has one.
	DatabaseURL string

	// Pool is an existing pool to use rather than opening one, for a suite
	// adopting this package a read at a time: the application's own fixtures go
	// on using the pool they already had, and nothing has two pools against one
	// database competing for its connections.
	//
	// It is not closed here, because it was not opened here.
	Pool *pgxpool.Pool

	// Migrations are every set to apply, in order — which is what the generated
	// api.MigrationSources returns, rig's sets first and the project's last.
	//
	// They are the caller's rather than this package's on purpose. rig's
	// foundation DDL travels with the module that owns it, an application has
	// tables of its own, and a harness that carried a copy of either would be a
	// second place for the schema to be.
	Migrations []migrate.Source

	// Isolate skips applying them, for a suite handed a database something else
	// has already migrated. The schema is still checked: a database behind the
	// binary refuses here rather than failing later as a missing column.
	Isolate bool
}

// Rig is a database with rig's foundation on it.
type Rig struct {
	// Pool is the connection pool, closed when the test ends. It is exported
	// because an application's own fixtures need one and making a second would
	// be two pools against one database.
	Pool *pgxpool.Pool
}

// New connects, applies the migrations and answers with the result.
//
// It fails rather than skips when there is no database, and that is the whole
// point of it. A suite that skips silently reports the same green as one that
// ran, so the day the database stops being started is the day the suite stops
// being a test and nothing says so.
func New(tb testing.TB, cfg Config) *Rig {
	tb.Helper()

	if cfg.DatabaseURL == "" && cfg.Pool == nil {
		tb.Fatal("rigtest: no DatabaseURL and no Pool. Point it at a database — " +
			"`rig db url` prints this project's — rather than letting the suite skip " +
			"itself green.")
	}
	if len(cfg.Migrations) == 0 {
		tb.Fatal("rigtest: no Migrations. Pass api.MigrationSources(yours) from the " +
			"generated package, so rig's sets are applied before your own.")
	}

	ctx := context.Background()
	pool := cfg.Pool
	if pool == nil {
		opened, err := pgxpool.New(ctx, cfg.DatabaseURL)
		if err != nil {
			tb.Fatalf("rigtest: %s: %v", cfg.DatabaseURL, err)
		}
		if err := opened.Ping(ctx); err != nil {
			opened.Close()
			tb.Fatalf("rigtest: nothing answering at %s: %v", cfg.DatabaseURL, err)
		}
		// Only what this package opened, which is why a borrowed Pool is left
		// alone: closing somebody else's would end their suite, not this one.
		tb.Cleanup(opened.Close)
		pool = opened
	}

	if err := apply(ctx, pool, cfg); err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
	return &Rig{Pool: pool}
}

// apply runs the migrations, or checks them when the caller says something else
// already has.
func apply(ctx context.Context, pool *pgxpool.Pool, cfg Config) error {
	if cfg.Isolate {
		return migrate.RequireAll(cfg.Migrations, migrate.Options{})(ctx, pool)
	}

	// FromPool rather than a second connection, so the migrations run against
	// the database this harness is about and the advisory lock is held by
	// something that will let it go.
	db := migrate.FromPool(pool)
	defer db.Close()

	if _, err := migrate.UpAll(ctx, db, cfg.Migrations, migrate.Options{}); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// exec runs a statement and fails the test with the statement's own words.
func (r *Rig) exec(tb testing.TB, sql string, args ...any) {
	tb.Helper()
	if _, err := r.Pool.Exec(context.Background(), sql, args...); err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
}

// one reads a single value, or fails.
func one[T any](tb testing.TB, r *Rig, query string, args ...any) T {
	tb.Helper()

	var out T
	err := r.Pool.QueryRow(context.Background(), query, args...).Scan(&out)
	if err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
	return out
}

// maybe reads a single value and answers whether there was a row.
func maybe[T any](tb testing.TB, r *Rig, query string, args ...any) (T, bool) {
	tb.Helper()

	var out T
	switch err := r.Pool.QueryRow(context.Background(), query, args...).Scan(&out); {
	case err == nil:
		return out, true
	case errors.Is(err, pgx.ErrNoRows):
		return out, false
	default:
		tb.Fatalf("rigtest: %v", err)
		return out, false
	}
}

// Package migrate applies a project's migrations from inside its own binary.
//
// It is a module of its own, and that is the whole design. The rig CLI is a
// development tool: it wants Docker, cobra, a compiler. Production wants none
// of those, and a deployment that has to install a development tool to move its
// schema forward will eventually run a different version of it than the one the
// code was written against. The binary that has the code embeds the migrations
// that go with it, so the two cannot disagree.
//
// It is not part of rig/runtime for the opposite reason: goose would then be in
// the go.mod of every generated application, including the ones that migrate
// some other way.
//
// The files are the same goose files the CLI runs, read through the same
// library, so `rig db up` in development and this in production apply the
// schema identically.
//
// # Where to run them
//
// There are three answers and they are not equally good.
//
// In development, `rig db up` starts a throwaway Postgres and migrates it.
// Nothing below applies.
//
// As a step of the deployment is what this package is shaped for: [Apply]
// behind a subcommand, run as a job before the new version rolls out. One
// process migrates, the replicas only serve, and a migration that fails fails
// on its own — leaving the previous version running rather than taking the new
// one down with it.
//
// At startup — [Apply] as the server's pre-listen hook — is one line, and for a
// single instance it is fine. Four things make it worse as soon as there is
// more than one:
//
//   - The application's database user needs rights to change the schema. A
//     separate job can run as a role that may ALTER TABLE while the server runs
//     as one that may not, and giving the process that answers public requests
//     the ability to drop a table is not a trade that gets undone later.
//   - Every replica migrates at once. The advisory lock makes that safe — one
//     applies and the rest wait — but waiting is the problem: a three-minute
//     migration means every other replica sits on the lock for three minutes
//     before it listens, and an orchestrator with a startup budget shorter than
//     that kills them mid-wait. Safe, and still a crash loop.
//   - A bad migration takes down the new version instead of a job. With a
//     rollout that keeps the old pods until the new ones are ready this is a
//     stuck deploy rather than an outage, which is survivable — but it surfaces
//     as replicas crash-looping rather than as a job that failed and stopped.
//   - Every restart runs it. A process cycling at three in the morning takes the
//     migration lock on the recovery path. Almost always a no-op. Not always.
//
// Checking rather than applying — [Require] as the same hook — is the middle,
// and the one to reach for by default. The server refuses to start when the
// database is behind it, so deploying before the migration job finished is a
// process that stops with a message naming what is missing, rather than one
// that serves against a schema it does not expect and fails a query at a time.
// The application keeps a database user that cannot change anything.
//
// What not to do is apply migrations beside a server that is already answering.
// Every option above finishes before the first request; running them
// concurrently means serving against a half-applied schema, and the failures
// that produces are per-query, transient, and unreproducible by the time
// anybody looks.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"
	"github.com/pressly/goose/v3/lock"
)

// DefaultTable is where the applied versions are recorded.
//
// It is the same name `rig db up` uses, which is the point: development and
// production have to agree about which migrations have run, and two
// bookkeeping tables would mean the second reader thinks nothing has.
const DefaultTable = "rig_migrations"

// Options configure a run. The zero value is the sensible one.
type Options struct {
	// Dir is the subdirectory of the filesystem holding the .sql files. Empty
	// means "migrations", which is where rig puts them and what an embed
	// directive in the project root produces.
	Dir string

	// Table records which versions have been applied. Empty means
	// DefaultTable. Naming it per project keeps two projects sharing one
	// database from reading each other's history as their own.
	//
	// A project that changed migrations.table in rig.yaml has to pass the same
	// name here. They are the same bookkeeping.
	Table string

	// Log receives one line per migration applied.
	Log io.Writer

	// Unlocked skips the advisory lock.
	//
	// The lock is what makes it safe for several instances to start at once:
	// one applies, the rest wait and then find nothing to do. Skipping it is
	// for a database whose user cannot take advisory locks, and it means
	// arranging by hand that only one runner exists.
	Unlocked bool
}

// Up applies every pending migration and returns what it applied.
//
// It is safe to call from several processes at once: the first takes an
// advisory lock, the others wait for it and then find nothing to do. That is
// what makes it usable from a deployment that starts its replicas together.
func Up(ctx context.Context, db *sql.DB, fsys fs.FS, opt Options) ([]string, error) {
	provider, err := newProvider(db, fsys, opt)
	if err != nil || provider == nil {
		return nil, err
	}

	results, err := provider.Up(ctx)

	applied := make([]string, 0, len(results))
	for _, r := range results {
		applied = append(applied, r.Source.Path)
		if opt.Log != nil {
			fmt.Fprintf(opt.Log, "applied %s\n", r.Source.Path)
		}
	}

	if err != nil {
		return applied, fmt.Errorf("apply migrations: %w", err)
	}
	return applied, nil
}

// Pending is what Up would apply, without applying it.
//
// A deployment that wants to fail before it starts rather than migrate on the
// way up can ask this first: an unexpected pending migration means the binary
// and the database disagree about which came first.
func Pending(ctx context.Context, db *sql.DB, fsys fs.FS, opt Options) ([]string, error) {
	provider, err := newProvider(db, fsys, opt)
	if err != nil || provider == nil {
		return nil, err
	}

	status, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the migration status: %w", err)
	}

	var out []string
	for _, s := range status {
		if s.State == goose.StatePending {
			out = append(out, s.Source.Path)
		}
	}
	return out, nil
}

// Version is the newest migration the database has applied, or zero for none.
func Version(ctx context.Context, db *sql.DB, fsys fs.FS, opt Options) (int64, error) {
	provider, err := newProvider(db, fsys, opt)
	if err != nil || provider == nil {
		return 0, err
	}

	v, err := provider.GetDBVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("read the applied version: %w", err)
	}
	return v, nil
}

// Apply is Up as a one-argument function, for wiring straight into a server.
//
//	Tasks: map[string]serve.Task{"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout})},
//
// It borrows the pool, applies what is pending, and hands the borrowed handle
// back. That is three lines every project would otherwise write the same way,
// including the deferred close that is easy to leave out.
func Apply(fsys fs.FS, opt Options) func(ctx context.Context, pool *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		db := FromPool(pool)
		defer db.Close()

		applied, err := Up(ctx, db, fsys, opt)
		if err != nil {
			return err
		}

		// Up says what it applied. Silence when there was nothing is
		// indistinguishable from silence because it never ran.
		if opt.Log != nil && len(applied) == 0 {
			fmt.Fprintln(opt.Log, "no migrations to apply")
		}
		return nil
	}
}

// Require is Apply's careful sibling: it refuses to start when the database is
// behind, instead of bringing it forward.
//
//	Migrate: migrate.Require(migrations, migrate.Options{}),
//
// This is the shape for a deployment that migrates as its own step. The server
// still notices — a binary started before its migration job finished stops with
// a message naming what is missing, rather than serving against a schema it
// does not expect and failing one query at a time. And the application's
// database user never needs the rights to change a schema, which is worth more
// than the convenience of applying it here.
func Require(fsys fs.FS, opt Options) func(ctx context.Context, pool *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		db := FromPool(pool)
		defer db.Close()

		pending, err := Pending(ctx, db, fsys, opt)
		if err != nil {
			return err
		}
		if len(pending) > 0 {
			return fmt.Errorf("the database is behind this binary: %d migration(s) not applied, starting with %s",
				len(pending), pending[0])
		}
		return nil
	}
}

// FromPool borrows a pool for migrations.
//
// goose speaks database/sql and the application speaks pgx, so something has to
// bridge them. Doing it from the pool rather than from the connection string
// means the migration uses the credentials and the settings the application is
// actually running with.
//
// The returned handle must be closed; closing it does not close the pool.
func FromPool(pool *pgxpool.Pool) *sql.DB { return stdlib.OpenDBFromPool(pool) }

// newProvider builds the goose provider, or returns nil when there is nothing
// to migrate.
//
// goose is driven through its provider API rather than its package-level
// functions, whose globals would make two projects in one process — and every
// parallel test — share a dialect and a table name.
func newProvider(db *sql.DB, fsys fs.FS, opt Options) (*goose.Provider, error) {
	if db == nil {
		return nil, errors.New("migrate: no database")
	}

	dir := opt.Dir
	if dir == "" {
		dir = "migrations"
	}
	table := opt.Table
	if table == "" {
		table = DefaultTable
	}

	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations from %s: %w", dir, err)
	}

	store, err := database.NewStore(database.DialectPostgres, table)
	if err != nil {
		return nil, fmt.Errorf("migration store: %w", err)
	}

	options := []goose.ProviderOption{
		goose.WithStore(store),
		goose.WithVerbose(false),
	}
	if !opt.Unlocked {
		locker, err := lock.NewPostgresSessionLocker()
		if err != nil {
			return nil, fmt.Errorf("migration lock: %w", err)
		}
		options = append(options, goose.WithSessionLocker(locker))
	}

	provider, err := goose.NewProvider("", db, sub, options...)
	if err != nil {
		if errors.Is(err, goose.ErrNoMigrations) {
			// An empty directory is a new project, not a failure.
			return nil, nil
		}
		return nil, fmt.Errorf("read migrations from %s: %w", dir, err)
	}
	return provider, nil
}

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
// # More than one set
//
// A project may have several. rig owns a dozen tables of its own, and the modules
// that read them can carry their own DDL rather than have it vendored into the
// project's migrations directory — see
// [github.com/simonjanss/rig/runtime/dbschema]. Each set is then numbered from one
// in its own namespace and records itself in its own table, which is what lets
// those modules be released without agreeing a migration number.
//
// [Source] is one such set and [UpAll], [ApplyAll] and [RequireAll] are the
// functions that take a list of them. Order is the caller's, and rig's sets come
// before the project's own: rig's DDL never references a project's table, while a
// project's routinely references rig's.
//
// A project that vendored the foundation has exactly one set and wants [Apply] or
// [Require], unchanged.
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
	"io/fs"
	"log/slog"

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

	// Logger receives one line per migration applied, at INFO, and a line at
	// DEBUG for a run that found nothing to do.
	//
	// Nil is not silence. It is [log/slog.Default], which is the reading every
	// other Logger in rig gives it, and it is the whole point of this field
	// being a logger rather than the writer it used to be: the generated boot
	// passes an Options with nothing in it, so which migrations a deployment
	// applied went nowhere at all. A caller that wants silence says so, and
	// then it is a decision somebody made rather than one nobody noticed:
	//
	//	migrate.Options{Logger: slog.New(slog.DiscardHandler)}
	//
	// INFO rather than the DEBUG most of what rig says about its own lifecycle
	// goes at. Which migrations this database has had applied to it is a fact
	// somebody goes looking for months later, and a level that has to have been
	// turned on in advance cannot answer that.
	Logger *slog.Logger

	// Unlocked skips the advisory lock.
	//
	// The lock is what makes it safe for several instances to start at once:
	// one applies, the rest wait and then find nothing to do. Skipping it is
	// for a database whose user cannot take advisory locks, and it means
	// arranging by hand that only one runner exists.
	Unlocked bool
}

// A Source is one migration set: a filesystem, the directory inside it, and the
// table recording how far that set has been applied.
//
// It exists because a project has more than one. rig's own tables — the
// identities, the file rows, the notifications — belong to the modules whose code
// reads them, and a project can leave those modules to carry their own schema
// instead of vendoring it. Then there are several sets to apply, each numbered
// from one in its own namespace, and each has to record its progress somewhere of
// its own: two sets sharing a table would share a numbering sequence, and two
// separately released modules would eventually collide on a version.
//
// The fields restate [github.com/simonjanss/rig/runtime/dbschema.Set] without its
// manifest, because applying a set needs only these. The conversion is the
// caller's — usually three lines of generated code — and it is deliberate that
// the dependency does not exist: a module that carries schema should not thereby
// carry goose, which is the same reason this package is not part of rig/runtime.
type Source struct {
	// Name is who the set belongs to, for a line that has to say whose migration
	// applied or whose is missing.
	Name string
	// FS holds the files, and Dir names the directory inside it. Empty Dir means
	// "migrations", as in [Options].
	FS  fs.FS
	Dir string
	// Table records the applied versions for this set alone. Empty means
	// [DefaultTable], which is the project's own; a module's set names its own.
	Table string
}

// UpAll applies several sets in the order given and returns everything it
// applied.
//
// The order is the caller's and it matters. rig's sets go first and the project's
// own last, because rig's DDL never references a project's table while a
// project's routinely references rig's — a join table pointing at
// rig_notification, a file column pointing at rig_file. So "all of rig's, then
// all of yours" is a total order that holds under upgrades too, and there is no
// need for anything cleverer.
//
// [Options.Dir] and [Options.Table] are ignored: those are per-set facts and each
// [Source] carries its own. Logger and Unlocked apply to every set.
//
// Applying the sets one after another rather than together is not a weaker
// arrangement. goose's advisory lock is a single fixed identifier, so several
// replicas starting at once still serialise across all of it — the per-set tables
// are bookkeeping, never a concurrency boundary.
func UpAll(ctx context.Context, db *sql.DB, srcs []Source, opt Options) ([]string, error) {
	if err := checkSources(srcs); err != nil {
		return nil, err
	}

	var applied []string
	for _, src := range srcs {
		got, err := Up(ctx, db, src.FS, src.options(opt))
		applied = append(applied, label(src, got)...)
		if err != nil {
			return applied, fmt.Errorf("%s: %w", src.who(), err)
		}
	}
	return applied, nil
}

// PendingAll is what [UpAll] would apply, without applying it.
func PendingAll(ctx context.Context, db *sql.DB, srcs []Source, opt Options) ([]string, error) {
	if err := checkSources(srcs); err != nil {
		return nil, err
	}

	var pending []string
	for _, src := range srcs {
		got, err := Pending(ctx, db, src.FS, src.options(opt))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.who(), err)
		}
		pending = append(pending, label(src, got)...)
	}
	return pending, nil
}

// ApplyAll is [UpAll] as a one-argument function, for wiring straight into a
// server — [Apply]'s shape for a project whose modules carry their own schema.
func ApplyAll(srcs []Source, opt Options) func(ctx context.Context, pool *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		db := FromPool(pool)
		defer db.Close()

		applied, err := UpAll(ctx, db, srcs, opt)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			// Debug, unlike the applied lines: that the schema was already
			// where it should be is the ordinary outcome of every boot after
			// the first.
			opt.log().DebugContext(ctx, "no migrations to apply")
		}
		return nil
	}
}

// RequireAll is [Require] over several sets: it refuses to start when any of them
// is behind, and says which.
//
// Any rather than the project's own, because being behind on rig's schema is the
// same failure. A binary pinning a module one version newer than the database has
// applied is exactly the skew this catches, and it is the skew that gets worse
// once the modules carry their own migrations.
func RequireAll(srcs []Source, opt Options) func(ctx context.Context, pool *pgxpool.Pool) error {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		db := FromPool(pool)
		defer db.Close()

		pending, err := PendingAll(ctx, db, srcs, opt)
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

// log is where this run says what it applied, and [log/slog.Default] when the
// caller named nowhere.
func (o Options) log() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

// options is the per-set view of a run: the shared settings, with this set's own
// directory and bookkeeping table.
func (s Source) options(opt Options) Options {
	opt.Dir = s.Dir
	opt.Table = s.Table
	return opt
}

func (s Source) who() string {
	if s.Name == "" {
		return "migrations"
	}
	return s.Name
}

// label prefixes each path with the set it came from, so a message about a
// pending migration says whose it is. Two sets both numbered from one would
// otherwise report two different `00001_...` files under the same name.
func label(src Source, paths []string) []string {
	if len(paths) == 0 {
		return nil
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, src.who()+" "+p)
	}
	return out
}

// checkSources refuses a list that would record two sets as one.
//
// Sharing a table is not a degraded arrangement, it is a broken one: the two
// sets are numbered independently, so one's version 2 would mark the other's as
// applied and the migration that never ran would never run.
func checkSources(srcs []Source) error {
	if len(srcs) == 0 {
		return errors.New("migrate: no migration sources")
	}
	seen := map[string]string{}
	for _, src := range srcs {
		table := src.Table
		if table == "" {
			table = DefaultTable
		}
		if prev, taken := seen[table]; taken {
			return fmt.Errorf("migrate: %s and %s would both record themselves in %s, "+
				"and they are numbered independently", prev, src.who(), table)
		}
		seen[table] = src.who()
	}
	return nil
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
		opt.log().InfoContext(ctx, "applied", "migration", r.Source.Path)
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

// Apply is Up as a one-argument function, for wiring straight into a server.
//
//	Tasks: map[string]serve.Task{"migrate": migrate.Apply(migrations, migrate.Options{})},
//
// It borrows the pool, applies what is pending, and hands the borrowed handle
// back. That is three lines every project would otherwise write the same way,
// including the deferred close that is easy to leave out.
//
// One set is a list of one, so this is [ApplyAll] with the list written out. The
// set has no name, which is what a failure names it by: a migration that will
// not apply is reported under the word "migrations" rather than under a name
// nobody supplied.
func Apply(fsys fs.FS, opt Options) func(ctx context.Context, pool *pgxpool.Pool) error {
	return ApplyAll([]Source{{FS: fsys, Dir: opt.Dir, Table: opt.Table}}, opt)
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
//
// One set is a list of one, so this is [RequireAll] with the list written out,
// and the message names the set the way that one does — for a set with no name,
// by the word "migrations".
func Require(fsys fs.FS, opt Options) func(ctx context.Context, pool *pgxpool.Pool) error {
	return RequireAll([]Source{{FS: fsys, Dir: opt.Dir, Table: opt.Table}}, opt)
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

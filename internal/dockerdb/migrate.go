package dockerdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/database"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
)

// MigrateOptions describe a migration run.
type MigrateOptions struct {
	// Dir holds the migration files.
	Dir string
	// Table is the bookkeeping table. Naming it per project keeps two rig
	// projects sharing a database from fighting over one another's history.
	Table string
	// URL is the database to migrate.
	URL string
	// Log receives progress, if set.
	Log io.Writer
}

// Migrate applies every pending migration.
//
// goose is driven through its provider API rather than its package-level
// functions. The globals would make two projects in one process — and every
// parallel test — share a dialect and a table name, which is exactly the kind
// of action-at-a-distance that makes a test suite flaky.
func Migrate(ctx context.Context, opt MigrateOptions) (applied int, err error) {
	if err := ensureDir(opt.Dir); err != nil {
		return 0, err
	}

	db, err := sql.Open("pgx", opt.URL)
	if err != nil {
		return 0, fmt.Errorf("connect for migrations: %w", err)
	}
	defer db.Close()

	store, err := database.NewStore(database.DialectPostgres, opt.Table)
	if err != nil {
		return 0, fmt.Errorf("migration store: %w", err)
	}

	provider, err := goose.NewProvider("", db, os.DirFS(opt.Dir),
		goose.WithStore(store),
		goose.WithVerbose(opt.Log != nil),
	)
	if err != nil {
		if errors.Is(err, goose.ErrNoMigrations) {
			// An empty migration directory is a new project, not a failure.
			return 0, nil
		}
		return 0, fmt.Errorf("read migrations from %s: %w", opt.Dir, err)
	}

	results, err := provider.Up(ctx)
	if err != nil {
		return len(results), fmt.Errorf("apply migrations: %w", err)
	}

	if opt.Log != nil && len(results) > 0 {
		for _, r := range results {
			fmt.Fprintf(opt.Log, "applied %s\n", r.Source.Path)
		}
	}
	return len(results), nil
}

func ensureDir(dir string) error {
	st, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("migration directory %s does not exist", dir)
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}

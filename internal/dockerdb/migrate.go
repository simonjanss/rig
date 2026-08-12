package dockerdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/simonjanss/rig/migrate"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver
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

// Migrate applies every pending migration to the development database.
//
// The work is rig/migrate, the same package an application embeds to migrate
// itself in production. One implementation is the point: `rig db up` here and
// the deployment there have to agree about what the schema is, and two readers
// of the same files would eventually disagree about one of them.
func Migrate(ctx context.Context, opt MigrateOptions) (applied int, err error) {
	if err := ensureDir(opt.Dir); err != nil {
		return 0, err
	}

	db, err := sql.Open("pgx", opt.URL)
	if err != nil {
		return 0, fmt.Errorf("connect for migrations: %w", err)
	}
	defer db.Close()

	// The directory is addressed from its parent so that the name on disk is
	// the name inside the filesystem, which is what an embedded one looks like.
	parent, dir := filepath.Split(filepath.Clean(opt.Dir))
	if parent == "" {
		parent = "."
	}

	names, err := migrate.Up(ctx, db, os.DirFS(parent), migrate.Options{
		Dir:   dir,
		Table: opt.Table,
		Log:   opt.Log,
	})
	return len(names), err
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

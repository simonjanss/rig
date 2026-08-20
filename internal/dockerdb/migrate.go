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
	// Dir holds the project's own migration files.
	Dir string
	// Table is the bookkeeping table. Naming it per project keeps two rig
	// projects sharing a database from fighting over one another's history.
	Table string
	// Foundation are rig's own migration sets, applied before the project's.
	//
	// Empty is the ordinary case, and it is what `migrations.foundation: vendored`
	// produces: those migrations are already files under Dir, so applying them
	// from here as well would be applying them twice under two histories.
	//
	// Under `embedded` they are the modules' own sets and this is the only place
	// they come from. They go first because rig's DDL never references a project's
	// table while a project's routinely references rig's — a join table pointing
	// at rig_notification, a file column pointing at rig_file. A schema read
	// without them is a schema rig would then generate the wrong code from, and
	// quietly: the API-key write guard and every notifiable resource are decided
	// by whether those tables were there.
	Foundation []migrate.Source
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

	sources := append([]migrate.Source{}, opt.Foundation...)
	sources = append(sources, migrate.Source{
		Name:  "the project",
		FS:    os.DirFS(parent),
		Dir:   dir,
		Table: opt.Table,
	})

	names, err := migrate.UpAll(ctx, db, sources, migrate.Options{Log: opt.Log})
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

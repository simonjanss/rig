package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/internal/introspect"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/pkg/ir"
)

// containerRuntime overrides the engine choice, for `--container-bin`.
var containerRuntime string

// database brings up whatever database the project points at.
//
// With no URL configured, rig manages a throwaway container: it starts one if
// needed, reuses it if it is already right, and leaves it running afterwards so
// the next command finds it warm. With a URL, that machinery is skipped
// entirely — which is how CI points rig at a service container.
func (e *env) database(ctx context.Context, p *project.Project) (url string, err error) {
	if !p.UsesContainer() {
		return p.DatabaseURL(), nil
	}

	cfg := containerConfig(p)
	cfg.Log = e.errOut
	cfg.StartWait = 90 * time.Second

	db, err := dockerdb.Start(ctx, cfg)
	if err != nil {
		return "", err
	}
	return db.URL(), nil
}

// containerConfig is the throwaway database this project asks for.
//
// The name and the port come from the configuration and then through
// [dockerdb.Qualify] and [dockerdb.HostPort], which are what one checkout of a
// project does not to collide with another. Both are the identity function
// unless somebody said there is more than one checkout, so a project sees the
// name and port it wrote down.
func containerConfig(p *project.Project) dockerdb.Config {
	cfg := p.Config.Database
	return dockerdb.Config{
		Image:    cfg.Image,
		Name:     dockerdb.Qualify(cfg.ContainerName),
		Port:     dockerdb.HostPort(cfg.Port),
		Database: cfg.Name,
		User:     cfg.User,
		Password: cfg.Password,
		Runtime:  containerRuntime,
	}
}

// migrate applies pending migrations: rig's own sets first, then the project's.
func (e *env) migrate(ctx context.Context, p *project.Project, url string) error {
	foundation, err := foundationSources(p)
	if err != nil {
		return err
	}

	applied, err := dockerdb.Migrate(ctx, dockerdb.MigrateOptions{
		Dir:        p.MigrationsDir(),
		Table:      p.Config.Migrations.Table,
		Foundation: foundation,
		URL:        url,
	})
	if err != nil {
		return err
	}
	if applied > 0 {
		fmt.Fprintf(e.errOut, "applied %d migration(s)\n", applied)
	}
	return nil
}

// foundationSources are rig's own migration sets for this project, in apply
// order.
//
// None under `vendored`, because those migrations are already files in the
// project's own directory: applying them from the modules as well would be the
// same DDL under two histories, and the second would fail on the first CREATE
// TABLE.
//
// None under `auth.own` either. That project forked rig's migrations and
// maintains those tables itself, so applying the modules' sets over them stops on
// a table that already exists. RIG3004 refuses the combination, but `rig db up`
// runs no diagnostics, so the rule has to hold here as well as there.
//
// Under `embedded` this is the only place they come from, and it is why the CLI
// has to know about the mode at all. `rig generate` introspects a live database,
// so a set that never got applied is a table missing from the schema — and the
// two failures that produces are silent ones: every user repository loses its
// "cannot be changed with an API key" guard, and every notifiable table quietly
// stops being one.
func foundationSources(p *project.Project) ([]migrate.Source, error) {
	if p.Config.Migrations.Vendored() || p.Config.Auth.Own {
		return nil, nil
	}

	parts, err := foundationParts(p)
	if err != nil {
		return nil, err
	}

	var out []migrate.Source
	for _, s := range scaffold.SetsFor(parts) {
		out = append(out, migrate.Source{Name: s.Module, FS: s.FS, Dir: s.Dir, Table: s.Table})
	}
	return out, nil
}

// readSchema brings the database up to date and reads it back.
//
// Migrating and introspecting are one step on purpose: reading a schema that
// does not reflect the migrations on disk would produce a document that
// compiles cleanly and describes the wrong system.
func (e *env) readSchema(ctx context.Context, p *project.Project) (ir.Schema, error) {
	url, err := e.database(ctx, p)
	if err != nil {
		return ir.Schema{}, err
	}
	if err := e.migrate(ctx, p, url); err != nil {
		return ir.Schema{}, err
	}

	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return ir.Schema{}, fmt.Errorf("connect to the database: %w", err)
	}
	defer conn.Close(ctx)

	schema, err := introspect.Read(ctx, conn, introspect.Options{Schema: p.Config.Database.Schema})
	if err != nil {
		return ir.Schema{}, fmt.Errorf("read the schema: %w", err)
	}
	return schema, nil
}

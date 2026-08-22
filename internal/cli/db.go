package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/internal/project"
)

func newDBCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Manage the local database",
		Long: "rig keeps a throwaway Postgres container running so that migrations and\n" +
			"introspection are fast. Nothing in it is precious: `rig db reset` throws it\n" +
			"away and rebuilds it from the migrations.",
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "up",
			Short: "Start the database and apply migrations",
			Long: "With `database.electric.enabled`, the sync service comes up beside it:\n" +
				"an ElectricSQL container following the database over logical replication,\n" +
				"which is what the generated _stream routes forward to.",
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := e.mustProject()
				if err != nil {
					return err
				}
				electric := p.Config.Database.Electric.Enabled

				if !p.UsesContainer() {
					if electric {
						return fmt.Errorf("database.url is set, so rig does not manage this database — " +
							"and cannot run a sync service against it; run ElectricSQL yourself " +
							"and remove database.electric, or remove database.url")
					}
					url := p.DatabaseURL()
					if err := e.migrate(cmd.Context(), p, url); err != nil {
						return err
					}
					fmt.Fprintf(e.errOut, "database ready at %s\n", url)
					return nil
				}

				db, err := e.startDatabase(cmd.Context(), p)
				if err != nil {
					return err
				}
				if err := e.migrate(cmd.Context(), p, db.URL()); err != nil {
					return err
				}
				fmt.Fprintf(e.errOut, "database ready at %s\n", db.URL())

				if electric {
					// A fresh database is an empty one, so a sync service that
					// was following the old container holds a replication slot
					// into nothing. Removing it first makes the restart a
					// resubscription rather than a hang.
					if db.Fresh() {
						stale, err := dockerdb.AttachElectric(cmd.Context(), attachElectricConfig(p))
						if err != nil {
							return err
						}
						_ = stale.Remove(cmd.Context())
					}

					cfg := electricConfig(p, db)
					cfg.Log = e.errOut
					el, err := dockerdb.StartElectric(cmd.Context(), cfg)
					if err != nil {
						return err
					}
					fmt.Fprintf(e.errOut, "sync service ready at %s\n", el.URL())
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "down",
			Short: "Stop the database without deleting it",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := e.mustProject()
				if err != nil {
					return err
				}
				if !p.UsesContainer() {
					return fmt.Errorf("database.url is set, so rig does not manage this database")
				}
				if p.Config.Database.Electric.Enabled {
					el, err := dockerdb.AttachElectric(cmd.Context(), attachElectricConfig(p))
					if err != nil {
						return err
					}
					if err := el.Stop(cmd.Context()); err == nil {
						fmt.Fprintf(e.errOut, "stopped %s\n", dockerdb.Qualify(p.Config.Database.Electric.ContainerName))
					}
				}
				db, err := containerFor(cmd.Context(), e, p)
				if err != nil {
					return err
				}
				if err := db.Stop(cmd.Context()); err != nil {
					return err
				}
				fmt.Fprintf(e.errOut, "stopped %s\n", dockerdb.Qualify(p.Config.Database.ContainerName))
				return nil
			},
		},
		&cobra.Command{
			Use:   "reset",
			Short: "Delete the database and rebuild it from the migrations",
			Long: "Use this after editing a migration that has already been applied. Rebuilding\n" +
				"from scratch is the only way to be sure the schema matches the files.",
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := e.mustProject()
				if err != nil {
					return err
				}
				if !p.UsesContainer() {
					return fmt.Errorf("database.url is set, so rig does not manage this database")
				}

				// The sync service goes first: its replication slot lives in the
				// database being thrown away, and a service that outlives its
				// slot hangs rather than fails.
				if p.Config.Database.Electric.Enabled {
					el, err := dockerdb.AttachElectric(cmd.Context(), attachElectricConfig(p))
					if err != nil {
						return err
					}
					_ = el.Remove(cmd.Context())
				}

				db, err := containerFor(cmd.Context(), e, p)
				if err != nil {
					return err
				}
				// Removing a container that was never created is not a failure;
				// reset should work from any starting state.
				_ = db.Remove(cmd.Context())

				fresh, err := e.startDatabase(cmd.Context(), p)
				if err != nil {
					return err
				}
				if err := e.migrate(cmd.Context(), p, fresh.URL()); err != nil {
					return err
				}
				fmt.Fprintf(e.errOut, "database rebuilt at %s\n", fresh.URL())

				if p.Config.Database.Electric.Enabled {
					cfg := electricConfig(p, fresh)
					cfg.Log = e.errOut
					el, err := dockerdb.StartElectric(cmd.Context(), cfg)
					if err != nil {
						return err
					}
					fmt.Fprintf(e.errOut, "sync service ready at %s\n", el.URL())
				}
				return nil
			},
		},
		&cobra.Command{
			Use:   "url",
			Short: "Print the connection string",
			Long: "The port is the one in rig.yaml, so this answers without touching Docker.\n" +
				"Under " + dockerdb.IsolateEnv + " there is no port until a container has one, so\n" +
				"this starts the database the same way `rig db up` would.",
			Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := e.mustProject()
				if err != nil {
					return err
				}
				if !dockerdb.Isolated() || !p.UsesContainer() {
					fmt.Fprintln(e.out, p.DatabaseURL())
					return nil
				}

				url, err := e.database(cmd.Context(), p)
				if err != nil {
					return err
				}
				fmt.Fprintln(e.out, url)
				return nil
			},
		},
		&cobra.Command{
			Use:   "psql",
			Short: "Open a psql session against the database",
			Args:  cobra.ArbitraryArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				p, err := e.mustProject()
				if err != nil {
					return err
				}
				url, err := e.database(cmd.Context(), p)
				if err != nil {
					return err
				}

				bin, err := exec.LookPath("psql")
				if err != nil {
					return fmt.Errorf("psql is not installed; the connection string is %s", url)
				}

				// psql wants the terminal, so it inherits the real streams
				// rather than the captured ones.
				c := exec.CommandContext(cmd.Context(), bin, append([]string{url}, args...)...)
				c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
				return c.Run()
			},
		},
	)

	return cmd
}

// containerFor builds a handle to the project's container without starting it.
func containerFor(ctx context.Context, e *env, p *project.Project) (*dockerdb.DB, error) {
	cfg := containerConfig(p)
	cfg.Log = e.errOut
	return dockerdb.Attach(ctx, cfg)
}

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
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := e.mustProject()
				if err != nil {
					return err
				}
				url, err := e.database(cmd.Context(), p)
				if err != nil {
					return err
				}
				if err := e.migrate(cmd.Context(), p, url); err != nil {
					return err
				}
				fmt.Fprintf(e.errOut, "database ready at %s\n", url)
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
				db, err := containerFor(cmd.Context(), e, p)
				if err != nil {
					return err
				}
				if err := db.Stop(cmd.Context()); err != nil {
					return err
				}
				fmt.Fprintf(e.errOut, "stopped %s\n", p.Config.Database.ContainerName)
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

				db, err := containerFor(cmd.Context(), e, p)
				if err != nil {
					return err
				}
				// Removing a container that was never created is not a failure;
				// reset should work from any starting state.
				_ = db.Remove(cmd.Context())

				url, err := e.database(cmd.Context(), p)
				if err != nil {
					return err
				}
				if err := e.migrate(cmd.Context(), p, url); err != nil {
					return err
				}
				fmt.Fprintf(e.errOut, "database rebuilt at %s\n", url)
				return nil
			},
		},
		&cobra.Command{
			Use:   "url",
			Short: "Print the connection string",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				p, err := e.mustProject()
				if err != nil {
					return err
				}
				fmt.Fprintln(e.out, p.DatabaseURL())
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
	cfg := p.Config.Database
	return dockerdb.Attach(ctx, dockerdb.Config{
		Image:    cfg.Image,
		Name:     cfg.ContainerName,
		Port:     cfg.Port,
		Database: cfg.Name,
		User:     cfg.User,
		Password: cfg.Password,
		Runtime:  containerRuntime,
		Log:      e.errOut,
	})
}

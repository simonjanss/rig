package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/tablesync"
)

func newSyncCmd(e *env) *cobra.Command {
	var (
		table  string
		prune  bool
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Bring table configuration in step with the database",
		Long: "Creates a configuration file for every table that lacks one and adds entries\n" +
			"for columns and enum values the files do not mention yet.\n\n" +
			"Existing content is preserved exactly: comments, blank lines, and key order\n" +
			"all survive, because the file is edited rather than rewritten.\n\n" +
			"New entries are written with a TODO comment, which fails validation until\n" +
			"someone says what the column means.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := e.mustProject()
			if err != nil {
				return err
			}

			raw, err := e.readSchema(cmd.Context(), p)
			if err != nil {
				return err
			}

			// Sync plans against the normalized schema, not the raw one. It
			// needs the two things normalization works out: which columns are
			// enums, and which tables are join tables. Without it, sync would
			// miss every enum and write a configuration file for every join
			// table — neither of which the compiler would ever read.
			schema, _ := compile.Normalize(raw, compile.NormalizeOptions{
				IgnoreTables: []string{p.Config.Migrations.Table},
			})

			// Configuration is loaded but its diagnostics are held back: sync
			// exists precisely to fix the things it would complain about.
			set, _ := loadTables(p)

			changes, err := tablesync.Plan(schema, set, p.TableConfigPath, tablesync.Options{
				Namer: p.Namer(),
				Prune: prune,
				Only:  table,
			})
			if err != nil {
				return err
			}

			if len(changes) == 0 {
				fmt.Fprintln(e.errOut, "table configuration is already up to date")
				return nil
			}

			var orphans int
			for _, c := range changes {
				label := p.Rel(c.Path)
				switch c.Kind {
				case tablesync.ChangeCreate:
					fmt.Fprintf(e.errOut, "create  %s (%s)\n", label, strings.Join(c.Notes, "; "))
				case tablesync.ChangeUpdate:
					fmt.Fprintf(e.errOut, "update  %s: %s\n", label, strings.Join(c.Notes, "; "))
				case tablesync.ChangeOrphan:
					orphans++
					// The file is left alone. It may hold endpoint definitions
					// that took real work, and a flag is not enough authority
					// to throw those away.
					fmt.Fprintf(e.errOut, "orphan  %s: %s — delete it if the table is gone for good\n",
						label, strings.Join(c.Notes, "; "))
				}
			}

			if dryRun {
				fmt.Fprintln(e.errOut, "\nnothing written (--dry-run)")
				return nil
			}

			if err := tablesync.Apply(changes); err != nil {
				return err
			}

			written := len(changes) - orphans
			if written > 0 {
				fmt.Fprintf(e.errOut, "\nwrote %d file(s). Replace the TODO comments, then run `rig validate`.\n", written)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&table, "table", "", "sync only this table")
	cmd.Flags().BoolVar(&prune, "prune", false, "remove entries for columns and enum values that no longer exist")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without writing")
	return cmd
}

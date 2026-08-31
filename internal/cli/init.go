package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/scaffold"
)

func newInitCmd(e *env) *cobra.Command {
	var (
		module string
		name   string
		image  string
	)

	cmd := &cobra.Command{
		Use:   "init [directory]",
		Short: "Start a new rig project",
		Long: "Writes rig.yaml, a migrations directory, and an AGENTS.md describing the\n" +
			"layout. Nothing that already exists is overwritten.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := e.dir
			if len(args) == 1 {
				dir = args[0]
				if !filepath.IsAbs(dir) {
					dir = filepath.Join(e.dir, dir)
				}
			}
			if err := mkdirAll(dir); err != nil {
				return err
			}

			if name == "" {
				name = filepath.Base(dir)
			}
			if module == "" {
				// A guess beats an error here: the module path is easy to fix
				// and hard to know, and a project that will not initialize
				// without one is a worse first impression.
				module = "example.com/" + name
			}

			files := scaffold.Project(scaffold.ProjectOptions{
				Name:   name,
				Module: module,
				Image:  image,
			})
			for i := range files {
				files[i].Path = filepath.Join(dir, files[i].Path)
			}

			written, skipped, err := scaffold.Write(files)
			if err != nil {
				return err
			}
			for _, p := range written {
				fmt.Fprintf(e.errOut, "created %s\n", rel(dir, p))
			}
			for _, p := range skipped {
				fmt.Fprintf(e.errOut, "kept    %s (already exists)\n", rel(dir, p))
			}

			// The schemas come next so the editor directive at the top of the
			// generated rig.yaml resolves immediately.
			p, diags := project.LoadFile(filepath.Join(dir, "rig.yaml"))
			if p == nil {
				return e.report(&diags)
			}
			if err := writeSchemas(e, p); err != nil {
				return err
			}

			fmt.Fprintf(e.errOut, "\nNext:\n"+
				"  rig migration new create_%s   write your first migration\n"+
				"  rig sync                     read the database into table configuration\n"+
				"  rig validate                 check it\n", firstTableHint(name))
			return nil
		},
	}

	cmd.Flags().StringVar(&module, "module", "", "Go module path of the application")
	cmd.Flags().StringVar(&name, "name", "", "project name (default: the directory name)")
	cmd.Flags().StringVar(&image, "db-image", "", "Postgres image for the local database")
	return cmd
}

func newMigrationCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migration",
		Short: "Work with migrations",
	}

	var (
		table         string
		softDelete    bool
		snapshot      bool
		publishShapes bool
	)

	newCmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Write a new migration file",
		Long: "Numbers the file after the highest one present. With --table, scaffolds a\n" +
			"CREATE TABLE carrying the columns rig recognizes by name. With\n" +
			"--publish-shapes, writes the publication and replica identity every table\n" +
			"that streams needs — reading the database to add only what is missing.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := e.mustProject()
			if err != nil {
				return err
			}

			// Two different files. --table scaffolds a CREATE TABLE for a table
			// that does not exist yet; --publish-shapes reads the tables that do.
			if publishShapes && table != "" {
				return errors.New("--publish-shapes and --table write different migrations; run the command twice")
			}
			if publishShapes {
				return e.publishShapes(cmd.Context(), p, args[0])
			}

			// Before anything is created. The same rule `rig validate` enforces,
			// asked here because this is the last moment the fix is a different
			// word on a command line rather than a migration, a rename in every
			// client, and a deprecation window.
			//
			// The prefix is refused and a reserved resource name is not, because a
			// `resource:` key still answers the second one. Refusing it would make
			// a table rig's own rules allow — `table: file` with
			// `resource: Document` — one rig cannot scaffold, so the warning says
			// what the configuration then has to carry.
			if table != "" {
				_, foundation, err := foundationTables(p)
				if err != nil {
					return err
				}
				why, escapable := compile.Reserved(p, foundation, table)
				switch {
				case why != "" && !escapable:
					return errors.New(why)
				case why != "":
					fmt.Fprintf(e.errOut, "warning: %s.\n"+
						"Give the table a `resource:` of its own in its configuration, or rename it — "+
						"`rig validate` refuses it otherwise.\n\n", why)
				}
			}

			dir := p.MigrationsDir()
			if err := mkdirAll(dir); err != nil {
				return err
			}

			number, err := scaffold.NextMigrationNumber(dir)
			if err != nil {
				return err
			}

			path := scaffold.MigrationFilename(dir, number, args[0])
			if fileExists(path) {
				return fmt.Errorf("%s already exists", p.Rel(path))
			}

			content := scaffold.Migration(scaffold.MigrationOptions{
				Name:       args[0],
				Table:      table,
				SoftDelete: softDelete,
				Snapshot:   snapshot,
			})
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				return err
			}

			fmt.Fprintf(e.errOut, "created %s\n", p.Rel(path))
			if table != "" {
				fmt.Fprintf(e.errOut, "\nEdit it, then run `rig sync` to pick %s up.\n", table)
			}
			return nil
		},
	}

	newCmd.Flags().StringVar(&table, "table", "", "scaffold a CREATE TABLE with the conventional columns")
	newCmd.Flags().BoolVar(&softDelete, "soft-delete", false, "add deleted_at, making the table soft-deletable")
	newCmd.Flags().BoolVar(&snapshot, "snapshot", false, "add the snapshot columns, keeping prior versions")
	newCmd.Flags().BoolVar(&publishShapes, "publish-shapes", false,
		"publish the tables that stream, and give them REPLICA IDENTITY FULL")

	cmd.AddCommand(newCmd)
	return cmd
}

func rel(base, path string) string {
	r, err := filepath.Rel(base, path)
	if err != nil || strings.HasPrefix(r, "..") {
		return path
	}
	return r
}

// firstTableHint turns a project name into a plausible first table, so the
// suggested command is copyable rather than a placeholder.
func firstTableHint(projectName string) string {
	name := strings.NewReplacer("-", "_", " ", "_").Replace(strings.ToLower(projectName))
	if name == "" {
		return "table"
	}
	return name
}

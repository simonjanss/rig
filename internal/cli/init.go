package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/migcheck"
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
		table      string
		softDelete bool
		snapshot   bool
	)

	newCmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Write a new migration file",
		Long: "Numbers the file after the highest one present. With --table, scaffolds a\n" +
			"CREATE TABLE carrying the columns rig recognizes by name.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := e.mustProject()
			if err != nil {
				return err
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

	cmd.AddCommand(newCmd)
	cmd.AddCommand(newMigrationCheckCmd(e))
	return cmd
}

// newMigrationCheckCmd is the pipeline half of migration validation.
//
// It reads rig.yaml and the migrations directory and stops there: no database,
// no container, no introspection, no generators. That is the point of it being a
// command of its own rather than a flag on `rig validate`, which compiles
// against a live schema and therefore wants a Postgres that a pull-request check
// has no reason to start. Everything this asks is answerable from the names on
// disk in about a millisecond, on a bare checkout.
//
// The two file rules run with or without --base, so the command is also the
// quick local answer to "did I number this right". --base adds the third, which
// is the one nothing else in rig can ask: goose reports an out-of-order
// migration at boot, against a database that already has the higher version
// applied, which is to say after the merge that caused it.
func newMigrationCheckCmd(e *env) *cobra.Command {
	var (
		base   string
		strict bool
	)

	cmd := &cobra.Command{
		Use:   "check",
		Short: "Check migration version numbers",
		Long: "Reports migrations rig or goose will not read the way you meant them: a name\n" +
			"that is not NNNNN_snake_case.sql, and two files claiming one version number.\n" +
			"Reads no database and starts no container, so it runs on a bare checkout.\n\n" +
			"--base also compares what this branch adds against a git ref, and refuses a\n" +
			"migration numbered at or below what that ref already has. Merged as it is,\n" +
			"goose has stepped past the number and reports it as missing rather than\n" +
			"applying it. Give it the branch this one merges into, fetched:\n\n" +
			"  rig migration check --base origin/main\n\n" +
			"The comparison is against that ref's tip, so a migration somebody else merged\n" +
			"while you were working counts. --format github turns any of it into pull\n" +
			"request annotations.",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			p, err := e.mustProject()
			if err != nil {
				return err
			}

			diags := checkMigrationFiles(p)

			if base != "" {
				d, err := checkMigrationOrder(p, base)
				if err != nil {
					return err
				}
				diags.Append(d)
			}

			// The same bargain --strict makes everywhere else in rig: the only
			// rule here that can be a warning is the naming one, and a warning
			// nobody ever fails on is a warning nobody ever fixes.
			if strict && diags.Count(diag.SeverityWarning) > 0 && !diags.HasErrors() {
				fmt.Fprintf(e.errOut, "\n%d warnings, and --strict was given\n",
					diags.Count(diag.SeverityWarning))
				_ = e.report(&diags)
				return ErrDiagnostics
			}

			if err := e.report(&diags); err != nil {
				return err
			}
			if diags.Len() > 0 {
				// Warnings were reported and did not fail the run. Saying
				// "no problems found" underneath them would be a lie.
				return nil
			}

			fmt.Fprintf(e.errOut, "%s: %d migrations, no problems found\n",
				p.Config.Migrations.Dir, countMigrations(p))
			return nil
		},
	}

	cmd.Flags().StringVar(&base, "base", "",
		"also refuse migrations numbered at or below this git ref, for example origin/main")
	cmd.Flags().BoolVar(&strict, "strict", false, "treat warnings as failures")
	return cmd
}

// countMigrations is how many files in the project's migrations directory goose
// would apply — which is not how many files are in it. A README beside them is
// not a migration, and a summary that counted it would be reporting on a
// directory listing rather than on a schema.
func countMigrations(p *project.Project) int {
	names, err := migrationNames(p.MigrationsDir())
	if err != nil {
		return 0
	}
	n := 0
	for _, name := range names {
		if _, ok := migcheck.Version(name); ok {
			n++
		}
	}
	return n
}

// checkMigrationOrder asks git what base already has, and refuses anything this
// branch adds below it.
//
// Every git command runs in the project root, which is what makes the relative
// pathspec mean this project's migrations and not some outer repository's. See
// [git] for why that is not the process's own directory.
//
// A branch that adds no migration is not asked what the ceiling is. The ceiling
// is only interesting when something has to clear it, so the ordinary case — a
// pull request that touches no migration — stops after the two calls
// [addedMigrations] makes rather than paying for a third.
func checkMigrationOrder(p *project.Project, base string) (diag.List, error) {
	dir := p.Config.Migrations.Dir

	added, err := addedMigrations(p.Root, base, dir)
	if err != nil {
		return diag.List{}, err
	}
	if len(added) == 0 {
		return diag.List{}, nil
	}

	names, err := baseMigrations(p.Root, base, dir)
	if err != nil {
		return diag.List{}, err
	}

	return migcheck.CheckOutOfOrder(added, base, migcheck.MaxVersion(names)), nil
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

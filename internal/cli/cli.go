// Package cli implements the rig command line.
//
// Commands mirror the four steps of using rig: sync the database into table
// configuration, edit it, validate it, generate from it. Everything else is in
// service of those.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/diag"

	// The built-in generators register themselves. Importing them here rather
	// than from the command means there is no way to build a rig that has a
	// command line but no generators behind it.
	_ "github.com/simonjanss/rig/internal/gen/allgen"
)

// Version is stamped at build time.
var Version = "dev"

// ErrDiagnostics is returned when a command failed because of reported
// diagnostics. The diagnostics have already been printed, so the top level
// exits quietly rather than printing a second, redundant error.
var ErrDiagnostics = errors.New("diagnostics reported")

// env carries what every command needs: where to look, and where to write.
type env struct {
	dir        string
	configPath string
	format     diag.Format
	color      bool
	out        io.Writer
	errOut     io.Writer

	// now is the clock the API revision is dated from. Nil is time.Now; a test
	// sets it, because the interesting case is what happens tomorrow.
	now func() time.Time
}

// clock is the current time, from whatever this env was given.
func (e *env) clock() time.Time {
	if e.now != nil {
		return e.now()
	}
	return time.Now()
}

// Main runs the command line and returns a process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	return execute(&env{out: stdout, errOut: stderr}, args)
}

// execute is Main once the environment has been decided. It is separate so a
// test can supply one with a clock of its own.
func execute(e *env, args []string) int {
	root := newRoot(e)
	root.SetArgs(args)
	root.SetOut(e.out)
	root.SetErr(e.errOut)

	if err := root.Execute(); err != nil {
		if !errors.Is(err, ErrDiagnostics) {
			fmt.Fprintf(e.errOut, "rig: %v\n", err)
		}
		return 1
	}
	return 0
}

func newRoot(e *env) *cobra.Command {
	var format string
	var noColor bool

	root := &cobra.Command{
		Use:   "rig",
		Short: "Generate a web system from a Postgres schema",
		Long: "rig turns a Postgres schema and a little configuration into models,\n" +
			"repositories, HTTP handlers, live-sync endpoints, and a typed Go client.\n\n" +
			"The usual loop is:\n" +
			"  rig sync       read the database and write one config file per table\n" +
			"  rig validate   check the schema and the configuration\n" +
			"  rig generate   write the code",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       Version,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			f, err := diag.ParseFormat(format)
			if err != nil {
				return err
			}
			e.format = f

			// Color is for a terminal, not for a pipe or a CI log.
			e.color = !noColor && isTerminal(e.out)

			if e.dir == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				e.dir = wd
			}
			return nil
		},
	}

	root.PersistentFlags().StringVarP(&e.dir, "directory", "C", "", "run as if started in this directory")
	root.PersistentFlags().StringVar(&e.configPath, "config", "", "path to rig.yaml (default: search upwards)")
	root.PersistentFlags().StringVar(&format, "format", "text", "diagnostic format: text, json or github")
	root.PersistentFlags().BoolVar(&noColor, "no-color", false, "disable colored output")
	root.PersistentFlags().StringVar(&containerRuntime, "container-bin", "",
		"container engine to use (default: docker, then podman)")

	root.AddCommand(
		newValidateCmd(e),
		newIRCmd(e),
		newSchemaCmd(e),
		newCodesCmd(e),
		newDBCmd(e),
		newInitCmd(e),
		newSetupProjectCmd(e),
		newMigrationCmd(e),
		newSyncCmd(e),
		newGenerateCmd(e),
		newCheckCmd(e),
		newGeneratorsCmd(e),
	)

	return root
}

// isTerminal reports whether w is a character device, which is the only place
// ANSI colors belong.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

// report prints diagnostics and reports whether anything blocks progress.
func (e *env) report(diags *diag.List) error {
	if err := diag.Render(e.errOut, diags, e.format, diag.RenderOptions{
		Color: e.color,
		Hints: true,
	}); err != nil {
		return err
	}
	if diags.HasErrors() {
		return ErrDiagnostics
	}
	return nil
}

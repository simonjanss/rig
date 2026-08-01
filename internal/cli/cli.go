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

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/diag"
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
}

// Main runs the command line and returns a process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	e := &env{out: stdout, errOut: stderr}

	root := newRoot(e)
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)

	if err := root.Execute(); err != nil {
		if !errors.Is(err, ErrDiagnostics) {
			fmt.Fprintf(stderr, "rig: %v\n", err)
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
			"repositories, HTTP handlers, an OpenAPI document, and a TypeScript client.\n\n" +
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

	root.AddCommand(
		newValidateCmd(e),
		newIRCmd(e),
		newSchemaCmd(e),
		newCodesCmd(e),
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

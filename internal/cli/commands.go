package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

func newValidateCmd(e *env) *cobra.Command {
	var schemaPath string
	var strict bool

	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check the schema and its configuration",
		Long: "Reports every problem it can find in one pass, each anchored to the exact\n" +
			"line that caused it. Exits non-zero when anything blocks generation.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, diags := e.loadProject()
			if p == nil {
				return e.report(&diags)
			}

			schema, err := e.resolveSchema(cmd.Context(), p, schemaPath)
			if err != nil {
				return err
			}

			doc, d := compileFrom(p, schema)
			diags.Append(d)

			// --strict makes every warning count, which is what CI wants: a
			// warning nobody ever fails on is a warning nobody ever fixes.
			if strict && diags.Count(diag.SeverityWarning) > 0 && !diags.HasErrors() {
				fmt.Fprintf(e.errOut, "\n%d warnings, and --strict was given\n",
					diags.Count(diag.SeverityWarning))
				_ = e.report(&diags)
				return ErrDiagnostics
			}

			if err := e.report(&diags); err != nil {
				return err
			}

			fmt.Fprintf(e.errOut, "%s: %d tables, %d resources, no problems found\n",
				p.Config.Project.Name, len(doc.Schema.Tables), len(doc.API.Resources))
			return nil
		},
	}

	cmd.Flags().StringVar(&schemaPath, "schema", "", "compile a schema dump instead of reading the database")
	cmd.Flags().BoolVar(&strict, "strict", false, "treat warnings as failures")
	return cmd
}

func newIRCmd(e *env) *cobra.Command {
	var (
		schemaPath string
		outPath    string
		schemaOnly bool
	)

	cmd := &cobra.Command{
		Use:   "ir",
		Short: "Print the compiled document",
		Long: "Writes the intermediate representation every generator reads. The encoding\n" +
			"is canonical, so the document can be committed and diffed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, diags := e.loadProject()
			if p == nil {
				return e.report(&diags)
			}

			schema, err := e.resolveSchema(cmd.Context(), p, schemaPath)
			if err != nil {
				return err
			}

			if schemaOnly {
				out, err := marshalIndent(schema)
				if err != nil {
					return err
				}
				return e.writeOutput(outPath, out)
			}

			doc, d := compileFrom(p, schema)
			diags.Append(d)

			// What was recorded, not what today would be. Looking at a project
			// should not be how its revision gets decided.
			if err := e.readRevision(p.Root, doc); err != nil {
				return err
			}

			// The document is written even when validation failed. It is marked
			// invalid, and generators refuse to run against it, but being able
			// to inspect a broken project is how it gets fixed.
			out, err := ir.Marshal(doc)
			if err != nil {
				return err
			}
			if err := e.writeOutput(outPath, out); err != nil {
				return err
			}

			return e.report(&diags)
		},
	}

	cmd.Flags().StringVar(&schemaPath, "schema", "", "compile a schema dump instead of reading the database")
	cmd.Flags().StringVarP(&outPath, "output", "o", "", "write to a file instead of standard output")
	cmd.Flags().BoolVar(&schemaOnly, "schema-only", false, "print the normalized schema rather than the whole document")
	return cmd
}

func newSchemaCmd(e *env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Write the JSON Schema files editors use",
		Long: "Writes the schemas for rig.yaml and for table configuration into .rig/.\n" +
			"They are the same documents rig validates against, so what your editor\n" +
			"accepts and what rig accepts cannot drift apart.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, diags := e.loadProject()
			if p == nil {
				return e.report(&diags)
			}
			return writeSchemas(e, p)
		},
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "table",
			Short: "Print the table configuration schema",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				b, err := tableconf.Schema()
				if err != nil {
					return err
				}
				return e.writeOutput("", b)
			},
		},
		&cobra.Command{
			Use:   "project",
			Short: "Print the rig.yaml schema",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				b, err := project.Schema()
				if err != nil {
					return err
				}
				return e.writeOutput("", b)
			},
		},
	)

	return cmd
}

// writeSchemas puts both schemas where the editor directive in a configuration
// file expects to find them.
func writeSchemas(e *env, p *project.Project) error {
	dir := p.Path(".rig")
	if err := mkdirAll(dir); err != nil {
		return err
	}

	table, err := tableconf.Schema()
	if err != nil {
		return err
	}
	if err := e.writeOutput(filepath.Join(dir, "table.schema.json"), table); err != nil {
		return err
	}

	proj, err := project.Schema()
	if err != nil {
		return err
	}
	return e.writeOutput(filepath.Join(dir, "rig.schema.json"), proj)
}

// newCodesCmd documents the diagnostics. A code in a CI log should be
// answerable without reading the source.
func newCodesCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "codes",
		Short: "List every diagnostic code",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				c, ok := diag.LookupCode(args[0])
				if !ok {
					return fmt.Errorf("no diagnostic code %q", args[0])
				}
				fmt.Fprintf(e.out, "%s (%s)\n\n%s\n", c.ID, c.Severity, c.Summary)
				if c.Hint != "" {
					fmt.Fprintf(e.out, "\n%s\n", c.Hint)
				}
				return nil
			}

			for _, c := range diag.Codes() {
				fmt.Fprintf(e.out, "%-8s %-8s %s\n", c.ID, c.Severity, c.Summary)
			}
			return nil
		},
	}
}

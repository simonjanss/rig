package cli

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

func newGenerateCmd(e *env) *cobra.Command {
	var (
		schemaPath string
		only       []string
		force      bool
		prune      bool
	)

	cmd := &cobra.Command{
		Use:   "generate",
		Short: "Write the code",
		Long: "Compiles the schema and its configuration, then runs every configured\n" +
			"generator.\n\n" +
			"Files with .gen. in the name are rewritten on every run. Anything else a\n" +
			"generator produces is written once and then belongs to you.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := e.mustProject()
			if err != nil {
				return err
			}

			doc, diags, err := e.compile(cmd.Context(), p, schemaPath)
			if err != nil {
				return err
			}
			if err := e.report(&diags); err != nil {
				return err
			}

			results, deltas, _, err := e.plan(cmd.Context(), p, doc, only)
			if err != nil {
				return err
			}

			if !gen.NeedsWork(deltas) {
				fmt.Fprintln(e.errOut, "everything is already up to date")
				return nil
			}

			gen.Report(e.errOut, p.Root, deltas, false)

			next, err := gen.Write(p.Root, results, deltas, gen.WriteOptions{Force: force, Prune: prune})
			if err != nil {
				return err
			}
			next.IRHash = hashOf(doc)
			if err := next.Save(p.Root); err != nil {
				return err
			}

			if stale := countStatus(deltas, gen.Stale); stale > 0 && !prune {
				fmt.Fprintf(e.errOut,
					"\n%d file(s) no generator produces any more; `rig generate --prune` removes them\n", stale)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&schemaPath, "schema", "", "compile a schema dump instead of reading the database")
	cmd.Flags().StringSliceVar(&only, "only", nil, "run only these generators")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite generated files that were edited by hand")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete files no generator produces any more")
	return cmd
}

func newCheckCmd(e *env) *cobra.Command {
	var (
		schemaPath string
		only       []string
	)

	cmd := &cobra.Command{
		Use:     "check",
		Aliases: []string{"diff"},
		Short:   "Report whether the generated code is up to date",
		Long: "Runs every generator in memory and compares the result to what is on disk,\n" +
			"without writing anything. Exits non-zero on any difference.\n\n" +
			"This is the CI gate: committed generated code that nobody regenerated is\n" +
			"how a schema change quietly stops matching the code that reads it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := e.mustProject()
			if err != nil {
				return err
			}

			doc, diags, err := e.compile(cmd.Context(), p, schemaPath)
			if err != nil {
				return err
			}
			if err := e.report(&diags); err != nil {
				return err
			}

			_, deltas, _, err := e.plan(cmd.Context(), p, doc, only)
			if err != nil {
				return err
			}

			if !gen.NeedsWork(deltas) {
				fmt.Fprintln(e.errOut, "generated code is up to date")
				return nil
			}

			gen.Report(e.errOut, p.Root, deltas, false)
			fmt.Fprintln(e.errOut, "\nRun `rig generate`.")
			return ErrDiagnostics
		},
	}

	cmd.Flags().StringVar(&schemaPath, "schema", "", "compile a schema dump instead of reading the database")
	cmd.Flags().StringSliceVar(&only, "only", nil, "check only these generators")
	return cmd
}

func newGeneratorsCmd(e *env) *cobra.Command {
	return &cobra.Command{
		Use:   "generators",
		Short: "List the generators rig knows about",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			for _, g := range gen.Default.All() {
				fmt.Fprintf(e.out, "%-14s %s\n", g.Name(), g.Description())
			}
			return nil
		},
	}
}

// plan compiles the generator specs, runs them, and diffs the result.
func (e *env) plan(ctx context.Context, p *project.Project, doc *ir.Document, only []string) (
	[]gen.Result, []gen.Delta, *gen.Manifest, error,
) {
	specs, err := generatorSpecs(p, only)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(specs) == 0 {
		return nil, nil, nil, fmt.Errorf(
			"no generators are configured; add a `generators:` block to rig.yaml " +
				"(`rig generators` lists what there is)")
	}

	results, err := gen.Run(ctx, gen.Default, doc, p.Root, specs)
	if err != nil {
		return nil, nil, nil, err
	}

	manifest, err := gen.LoadManifest(p.Root)
	if err != nil {
		return nil, nil, nil, err
	}

	deltas, err := gen.Diff(p.Root, results, manifest)
	if err != nil {
		return nil, nil, nil, err
	}
	return results, deltas, manifest, nil
}

// generatorSpecs turns the configured generators into run specs, honoring
// --only.
func generatorSpecs(p *project.Project, only []string) ([]gen.Spec, error) {
	var specs []gen.Spec

	for _, g := range p.Config.Generators {
		if len(only) > 0 && !slices.Contains(only, g.Name) {
			continue
		}
		specs = append(specs, gen.Spec{
			Name:    g.Name,
			OutDir:  g.OutDir,
			Options: g.Options,
		})
	}

	// A name in --only that matches nothing is a typo, and running a subset of
	// what was asked for is worse than saying so.
	for _, name := range only {
		if !slices.ContainsFunc(specs, func(s gen.Spec) bool { return s.Name == name }) {
			configured := make([]string, 0, len(p.Config.Generators))
			for _, g := range p.Config.Generators {
				configured = append(configured, g.Name)
			}
			return nil, fmt.Errorf("no generator named %q is configured; rig.yaml has %s",
				name, strings.Join(configured, ", "))
		}
	}

	return specs, nil
}

// compile is the shared front half of generate and check.
func (e *env) compile(ctx context.Context, p *project.Project, schemaPath string) (*ir.Document, diag.List, error) {
	schema, err := e.resolveSchema(ctx, p, schemaPath)
	if err != nil {
		return nil, diag.List{}, err
	}
	doc, diags := compileFrom(p, schema)
	return doc, diags, nil
}

func hashOf(doc *ir.Document) string {
	h, err := doc.Hash()
	if err != nil {
		return ""
	}
	return h
}

func countStatus(deltas []gen.Delta, status gen.Status) int {
	n := 0
	for _, d := range deltas {
		if d.Status == status {
			n++
		}
	}
	return n
}

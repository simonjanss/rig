package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/scaffold"
)

func newSetupProjectCmd(e *env) *cobra.Command {
	var (
		skip   []string
		expose []string
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:   "setup-project",
		Short: "Scaffold the authentication foundation",
		Long: "Writes the migrations for tenants, people, accounts, credentials, sessions,\n" +
			"API keys, roles, provider links, and the authentication log.\n\n" +
			"Migrations and nothing else. The tables are ordinary rig — real tables\n" +
			"following the same column conventions as yours — but rig generates no code\n" +
			"for them: their Go types, their stores and their endpoints all live in the\n" +
			"rig/auth module, so a second copy in your project would be a few thousand\n" +
			"lines nothing calls. Use --expose to get a model and a repository for one\n" +
			"anyway. Nothing already present is overwritten.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := e.mustProject()
			if err != nil {
				return err
			}

			skip, err = normalizeSkip(skip)
			if err != nil {
				return err
			}

			dir := p.MigrationsDir()
			if err := mkdirAll(dir); err != nil {
				return err
			}
			next, err := scaffold.NextMigrationNumber(dir)
			if err != nil {
				return err
			}
			existing, err := migrationNames(dir)
			if err != nil {
				return err
			}

			expose, err = normalizeExpose(expose)
			if err != nil {
				return err
			}

			files := scaffold.Foundation(scaffold.FoundationOptions{
				Skip:          skip,
				Expose:        expose,
				FirstNumber:   next,
				MigrationsDir: dir,
				ConfigPath:    p.TableConfigPath,
				Existing:      existing,
			})

			if dryRun {
				for _, f := range files {
					fmt.Fprintf(e.errOut, "would create %s\n", p.Rel(f.Path))
				}
				return nil
			}

			written, skipped, err := scaffold.Write(files)
			if err != nil {
				return err
			}
			for _, path := range written {
				fmt.Fprintf(e.errOut, "created %s\n", p.Rel(path))
			}
			for _, path := range skipped {
				fmt.Fprintf(e.errOut, "kept    %s (already exists)\n", p.Rel(path))
			}

			if len(written) == 0 {
				fmt.Fprintf(e.errOut, "\nNothing to do: the foundation is already in place.\n")
				return nil
			}

			fmt.Fprintf(e.errOut, "\nNext, turn it on in rig.yaml:\n"+
				"  auth:\n"+
				"    enabled: true\n\n"+
				"That block is where the lifetimes, the rate limits, the password policy and\n"+
				"the sign-in providers live — `rig schema project` lists every key. The\n"+
				"server-go generator writes the wiring into the API package it already\n"+
				"generates, so there is no second generator to configure. Then:\n\n"+
				"  rig db up        apply the migrations\n"+
				"  rig validate     check it\n"+
				"  rig generate     write your own tables, and the auth wiring\n\n"+
				"and wiring it up is one call:\n"+
				"  front, err := api.New(pool, api.Hooks{Grants: myGrants(pool)})\n")

			if len(expose) > 0 {
				fmt.Fprintf(e.errOut, "\nAdd this to rig.yaml, or the configuration "+
					"it just wrote names a table rig leaves out:\n"+
					"  auth:\n    expose: [%s]\n", strings.Join(expose, ", "))
			}
			return nil
		},
	}

	cmd.Flags().StringSliceVar(&skip, "skip", nil,
		"parts to leave out: "+strings.Join(scaffold.Parts(), ", "))
	cmd.Flags().StringSliceVar(&expose, "expose", nil,
		"foundation tables to generate a model, repository, and API for anyway")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "list the files without writing them")
	return cmd
}

// normalizeExpose checks that every named table is one the foundation creates.
//
// A typo here would be silent otherwise: the configuration would be written for
// a table nothing creates, and the next `rig validate` would report a missing
// table rather than a misspelt flag.
func normalizeExpose(expose []string) ([]string, error) {
	out := make([]string, 0, len(expose))
	for _, s := range expose {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !slices.Contains(scaffold.Tables(), s) {
			return nil, fmt.Errorf("the foundation has no table %q; choose from %s",
				s, strings.Join(scaffold.Tables(), ", "))
		}
		out = append(out, s)
	}
	return out, nil
}

// migrationNames lists the migration files already in a directory.
func migrationNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// normalizeSkip checks a skip list and reports what it would break.
//
// Skipping a part something else depends on produces SQL that fails halfway
// through `rig db up`, which is a worse way to find out than a message here.
func normalizeSkip(skip []string) ([]string, error) {
	out := make([]string, 0, len(skip))
	for _, s := range skip {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !slices.Contains(scaffold.Parts(), s) {
			return nil, fmt.Errorf("unknown part %q; choose from %s",
				s, strings.Join(scaffold.Parts(), ", "))
		}
		out = append(out, s)
	}

	for _, part := range scaffold.Parts() {
		if slices.Contains(out, part) {
			continue
		}
		for _, need := range scaffold.Requires(part) {
			if slices.Contains(out, need) {
				return nil, fmt.Errorf("%s needs %s, so they cannot be skipped separately",
					part, need)
			}
		}
	}
	return out, nil
}

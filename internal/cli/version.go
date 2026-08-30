package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/simonjanss/rig/internal/version"
)

func newVersionCmd(e *env) *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the rig version",
		Long: "The version of rig, and with --verbose the commit it was built from.\n\n" +
			"This is the version to pin the runtime modules to: rig generates code\n" +
			"that imports github.com/simonjanss/rig/runtime and its siblings, and they\n" +
			"are released together, at one version.",
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			i := version.Get()
			if !verbose {
				fmt.Fprintln(e.out, i.String())
				return nil
			}

			fmt.Fprintf(e.out, "rig      %s\n", i.String())
			if i.Revision != "" {
				dirty := ""
				if i.Modified {
					dirty = " (uncommitted changes)"
				}
				fmt.Fprintf(e.out, "commit   %s%s\n", i.Revision, dirty)
			}
			if i.Time != "" {
				fmt.Fprintf(e.out, "built    %s\n", i.Time)
			}
			if i.Go != "" {
				fmt.Fprintf(e.out, "go       %s\n", i.Go)
			}
			return nil
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "also print the commit and the toolchain")
	return cmd
}

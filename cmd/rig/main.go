// Command rig is the command line: sync a database into table configuration,
// validate it, and generate from it.
//
// Everything it does is in [github.com/simonjanss/rig/internal/cli]. What is
// here is the process — the arguments, the streams, and the exit code — so that
// the command line itself stays testable without one.
package main

import (
	"os"

	"github.com/simonjanss/rig/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}

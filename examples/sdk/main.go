// Command sdk is what using a rig Go client looks like from the outside.
//
// It is a separate module on purpose. The other examples are servers, and they
// keep their generated model, store and API packages under internal/ — which is
// exactly right, and which means nothing outside those modules can reach them.
// A client is the opposite kind of thing: it exists to be imported by somebody
// else's program, so it is generated into ./client rather than ./internal, and
// this program is that somebody else.
//
// Three demonstrations, one per subcommand:
//
//	go run . todo      # the whole lifecycle of a resource, no authentication
//	go run . auth      # signing in, sessions, tenants and API keys
//	go run . import    # a batch job: a CSV of todos, imported through the client
//	go run . files     # uploading, downloading, and a create that carries its file
//
// Each needs the corresponding example running. From the repository root:
//
//	cd examples/todo && rig db up && go run .      # then, here: go run . todo
//	cd examples/auth && rig db up && go run .      # then, here: go run . auth
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/simonjanss/rig/rigclient"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n%s\n", explain(err))
		os.Exit(1)
	}
}

func run() error {
	// Ctrl-C stops the demonstration where it is rather than mid-request: every
	// call below takes this context, which is the habit worth showing.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if len(os.Args) < 2 {
		usage()
		return errors.New("say which demonstration to run")
	}

	switch os.Args[1] {
	case "todo":
		return todoDemo(ctx, os.Args[2:])
	case "auth":
		return authDemo(ctx, os.Args[2:])
	case "import":
		return importDemo(ctx, os.Args[2:])
	case "files":
		return filesDemo(ctx, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("no demonstration called %q", os.Args[1])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `sdk — the rig Go client, demonstrated

  go run . todo [-base-url URL] [-tenant UUID]
      Create, read, search, patch, complete, delete and restore a todo,
      against examples/todo.

  go run . auth [-base-url URL] [-email ADDRESS] [-password SECRET]
      Sign in, call as the session, mint an API key and call as that,
      against examples/auth.

  go run . import [-file PATH] [-workers N] [-dry-run] [-skip-existing]
      Import a CSV of todos: a bounded worker pool, a retry that knows
      which failures are worth retrying, and a report naming the line
      of every row that did not go in.

  go run . files [-base-url URL] [-tenant UUID]
      Upload a file, read its url off the row, download it, ask for part
      of it, create a row and its file in one request, and delete it —
      against examples/todo.
`)
}

// flags is the small set every demonstration shares.
func flags(name string, args []string, baseURL *string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ExitOnError)
	set.StringVar(baseURL, "base-url", *baseURL, "where the example server is listening")
	return set
}

// explain turns a failed call into something worth reading.
//
// A *rigclient.Error already says which failure it was and carries the request
// identifier the server logged it under; what is added here is the one thing the
// error cannot know, which is that the server may simply not be running.
func explain(err error) string {
	var rows *importFailed
	if errors.As(err, &rows) {
		return rows.Error()
	}

	var e *rigclient.Error
	if errors.As(err, &e) {
		out := "the server refused the call: " + e.Error()
		if fields := string(e.Fields); fields != "" && fields != "null" {
			out += "\n  fields: " + fields
		}
		return out
	}
	return err.Error() + "\n\nIs the example server running? See the package comment."
}

// step prints what is about to happen, so the output reads as a transcript
// rather than as a dump.
func step(format string, args ...any) {
	fmt.Printf("\n▸ "+format+"\n", args...)
}

func detail(format string, args ...any) {
	fmt.Printf("  "+format+"\n", args...)
}

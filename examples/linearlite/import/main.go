// Command import creates a todo per CSV row, through the generated Go client,
// with a personal API key.
//
// It exists to be watched: run it with the board open in a browser and the
// columns fill card by card, live over the sync stream, with nothing polling
// anywhere. The -delay flag is the demonstration — a real batch job would
// leave it at zero and add the worker pool and per-row error report
// examples/sdk/import_demo.go walks through. The loop itself is
// importer.Run, shared with the docker test.
//
// Mint the key on the settings page (or with curl against POST /auth/api-keys,
// kind Personal), then:
//
//	go run ./import -key rig_sk_…
//
// The key resolves to its owner inside one tenant, so there is no tenant to
// name and no login to perform: a key is a credential on its own, and every
// row this creates is attributed to the person who minted it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/simonjanss/rig/examples/linearlite/client"
	"github.com/simonjanss/rig/examples/linearlite/importer"
	"github.com/simonjanss/rig/rigclient"
)

func main() {
	var (
		file  = flag.String("file", "import/testdata/todos.csv", "CSV to import: title,description,status,priority")
		base  = flag.String("base", client.DefaultBaseURL, "where the server runs")
		key   = flag.String("key", os.Getenv("LINEARLITE_API_KEY"), "a personal API key ($LINEARLITE_API_KEY)")
		delay = flag.Duration("delay", 600*time.Millisecond, "pause between rows, so the board visibly fills")
	)
	flag.Parse()

	if *key == "" {
		fmt.Fprintln(os.Stderr, "no API key: pass -key or set $LINEARLITE_API_KEY —")
		fmt.Fprintln(os.Stderr, "mint one on the settings page, or:")
		fmt.Fprintln(os.Stderr, `  curl -X POST -H "Authorization: Bearer $ACCESS_TOKEN" -H content-type:application/json \`)
		fmt.Fprintln(os.Stderr, `    -d '{"name":"import","kind":"Personal","scopes":["todo.read","todo.write"]}' \`)
		fmt.Fprintln(os.Stderr, "    "+client.DefaultBaseURL+"/auth/api-keys")
		os.Exit(2)
	}

	c, err := client.New(rigclient.Config{
		BaseURL:    *base,
		Credential: rigclient.APIKey(*key),
		UserAgent:  "linearlite-import/1",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	created, failed, err := importer.Run(context.Background(), c, *file, *delay, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("\n%d created, %d failed\n", created, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

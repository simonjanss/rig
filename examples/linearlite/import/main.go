// Command import creates a todo per CSV row, through the generated Go client,
// with a personal API key.
//
// It exists to be watched: run it with the board open in a browser and the
// columns fill card by card, live over the sync stream, with nothing polling
// anywhere. The -delay flag is the demonstration — a real batch job would
// leave it at zero and add the worker pool and per-row error report
// examples/sdk/import_demo.go walks through.
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
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/simonjanss/rig/examples/linearlite/client"
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

	created, failed, err := run(context.Background(), c, *file, *delay, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("\n%d created, %d failed\n", created, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

// run reads the file and creates a todo per row, pausing between rows. It is a
// function rather than the body of main so the docker test can call it against
// a server of its own, with no delay.
func run(ctx context.Context, c *client.Client, file string, delay time.Duration, out io.Writer) (created, failed int, err error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	r := csv.NewReader(f)
	header, err := r.Read()
	if err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", file, err)
	}
	if want := []string{"title", "description", "status", "priority"}; !slices.Equal(header, want) {
		return 0, 0, fmt.Errorf("%s: header is %v, want %v", file, header, want)
	}

	for line := 2; ; line++ {
		record, err := r.Read()
		if errors.Is(err, io.EOF) {
			return created, failed, nil
		}
		if err != nil {
			return created, failed, fmt.Errorf("line %d: %w", line, err)
		}

		in := client.TodoCreateInput{
			Title:    record[0],
			Status:   client.TodoStatus(record[2]),
			Priority: client.TodoPriority(record[3]),
		}
		if record[1] != "" {
			in.Description = &record[1]
		}

		// The idempotency key is what makes a rerun a no-op instead of a
		// duplicate board: the server records the first answer under it and
		// replays that answer to a retry. Keyed on the file and the line, so
		// re-running after fixing row nine re-sends only row nine's content.
		_, err = c.Todos.Create(ctx, in,
			rigclient.WithIdempotencyKey(fmt.Sprintf("linearlite-import:%s:%d", file, line)),
			rigclient.WithRetry(3),
		)
		if err != nil {
			failed++
			if refused, ok := client.TodoCreateError(err); ok {
				fmt.Fprintf(out, "  line %-3d failed   %-40s %s\n", line, record[0], refused.Fields)
			} else {
				fmt.Fprintf(out, "  line %-3d failed   %-40s %v\n", line, record[0], err)
			}
			continue
		}

		created++
		fmt.Fprintf(out, "  line %-3d created  %s\n", line, record[0])

		if delay > 0 {
			select {
			case <-ctx.Done():
				return created, failed, ctx.Err()
			case <-time.After(delay):
			}
		}
	}
}

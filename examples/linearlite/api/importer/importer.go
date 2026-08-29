// Package importer is the import job's loop, one function so the command in
// import/ and the docker test drive the same code.
package importer

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/simonjanss/rig/examples/linearlite/client"
	"github.com/simonjanss/rig/rigclient"
)

// Run reads the file and creates a todo per row, pausing between rows.
//
// The delay is the demonstration — the board visibly fills while the job runs
// — and zero is what a test or a real batch job passes. Each create carries an
// idempotency key built from the file and the line, which is what makes a
// rerun a no-op instead of a duplicate board: the server records the first
// answer under the key and replays it to a retry, so the report reads the same
// and the board does not grow.
func Run(ctx context.Context, c *client.Client, file string, delay time.Duration, out io.Writer) (created, failed int, err error) {
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

		// Keyed on the file and the line, so re-running after fixing row nine
		// re-sends only row nine's content.
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

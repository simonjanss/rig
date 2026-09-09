package dockerdb

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// scriptRuntime is a [Runtime] that answers from a list rather than from a
// container engine, and records what it was asked.
//
// The errors it returns are built the way [CLIRuntime.Run] builds them —
// "docker run: " and then whatever the engine printed on stderr — because the
// only thing distinguishing a lost port race from any other failure is that
// text. A fake returning a tidier error would test the matching against a
// string no engine produces.
type scriptRuntime struct {
	mu      sync.Mutex
	calls   [][]string
	results []scriptResult
}

type scriptResult struct {
	out    string
	stderr string
}

// Name implements [Runtime].
func (r *scriptRuntime) Name() string { return "docker" }

// Available implements [Runtime].
func (r *scriptRuntime) Available(context.Context) error { return nil }

// Run implements [Runtime].
//
// The script covers `run` and nothing else: every other subcommand succeeds,
// because the only one whose answer any of these tests turns on is the one that
// publishes a port. The last entry repeats once the script runs out, so a test
// that wants "fails every time" writes one.
func (r *scriptRuntime) Run(_ context.Context, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.calls = append(r.calls, args)
	if args[0] != "run" || len(r.results) == 0 {
		return "", nil
	}

	runs := 0
	for _, call := range r.calls {
		if call[0] == "run" {
			runs++
		}
	}
	res := r.results[min(runs-1, len(r.results)-1)]
	if res.stderr != "" {
		return "", fmt.Errorf("docker %s: %s", args[0], res.stderr)
	}
	return res.out, nil
}

// commands is each call as the command line it would have been, for assertions
// that read as what the engine saw.
func (r *scriptRuntime) commands() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]string, 0, len(r.calls))
	for _, args := range r.calls {
		out = append(out, strings.Join(args, " "))
	}
	return out
}

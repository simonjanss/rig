package serve_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/runtime/serve"
)

// The environment a child process reads. A subprocess is the only way to test
// Main: it owns the signal handling and it calls os.Exit, and neither can be
// observed from inside the process doing them.
const (
	childEnv  = "RIG_SERVE_MAIN_CHILD"
	markerEnv = "RIG_SERVE_MAIN_MARKER"
	argEnv    = "RIG_SERVE_MAIN_ARG"
)

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "" {
		os.Exit(m.Run())
	}
	child()
}

// child is a process whose main function is serve.Main, as a project's would be.
//
// Neither path it is driven down reaches a working database, which is the point:
// what is being tested is what Main does on the way out, and the two ways out
// that are not a clean stop are a subcommand that does not exist and one that
// failed. The marker file is how the parent sees that OnExit ran, since a
// process that has exited cannot be asked.
func child() {
	marker := os.Getenv(markerEnv)

	var args []string
	if arg := os.Getenv(argEnv); arg != "" {
		args = []string{arg}
	}
	os.Args = append([]string{"child"}, args...)

	serve.Main(serve.Config{
		// A port with nothing behind it, reached quickly.
		DatabaseURL:    "postgres://rig:rig@127.0.0.1:1/nothing?sslmode=disable",
		Addr:           "127.0.0.1:0",
		MaxStartup:     2 * time.Second,
		ConnectTimeout: time.Second,
		Tasks: map[string]serve.Task{
			"migrate": func(context.Context, *pgxpool.Pool) error { return nil },
		},
		OnExit: func() { _ = os.WriteFile(marker, []byte("ran"), 0o600) },
	}, nil)
}

func runChild(t *testing.T, arg string) (code int, marked bool, out string) {
	t.Helper()

	marker := filepath.Join(t.TempDir(), "exited")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1", markerEnv+"="+marker, argEnv+"="+arg)

	raw, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
		code = exit.ExitCode()
	case err != nil:
		t.Fatalf("running the child: %v\n%s", err, raw)
	}

	_, statErr := os.Stat(marker)
	return code, statErr == nil, string(raw)
}

// A flush written as a deferred call in a main function does not survive
// os.Exit, and Main exits.
//
// OnExit is what runs instead, and it runs on the paths where something went
// wrong — which are the runs whose spans somebody actually wants. Before it
// existed, a `defer process.Close()` in a main function ran when the server
// stopped cleanly and was skipped on every one of these.
func TestOnExitRunsOnTheWayOutOfAFailedSubcommand(t *testing.T) {
	code, marked, out := runChild(t, "not-a-command")

	if code != 2 {
		t.Errorf("exit = %d, want 2 for a subcommand that does not exist\n%s", code, out)
	}
	if !strings.Contains(out, "not-a-command") {
		t.Errorf("the refusal should name what was asked for:\n%s", out)
	}
	if !marked {
		t.Error("OnExit did not run: a deferred flush would have been skipped here too")
	}
}

// The same for a task that ran and failed, which is the one an hourly job hits.
func TestOnExitRunsOnTheWayOutOfAFailedTask(t *testing.T) {
	code, marked, out := runChild(t, "migrate")

	if code != 1 {
		t.Errorf("exit = %d, want 1 for a task that failed\n%s", code, out)
	}
	if !marked {
		t.Error("OnExit did not run after the task failed")
	}
}

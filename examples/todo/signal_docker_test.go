//go:build docker

package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/serve"
)

// The environment a child process reads. Main owns the signal handling and it
// calls os.Exit, so a subprocess is the only place either can be watched.
const (
	childEnv     = "RIG_TODO_SIGNAL_CHILD"
	listeningEnv = "RIG_TODO_SIGNAL_LISTENING"
)

func TestMain(m *testing.M) {
	if os.Getenv(childEnv) == "" {
		os.Exit(m.Run())
	}
	child()
}

// child is a server whose shutdown will not finish.
//
// The close step ignores the context it is handed and has no limit of its own,
// so the whole two-minute budget belongs to it: this is the process somebody is
// watching not make progress, and the one a second signal has to be able to end.
func child() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}
	// The test binary's own flags would otherwise be read as a subcommand.
	os.Args = os.Args[:1]

	serve.Main(serve.Config{
		DatabaseURL: dsn,
		Addr:        "127.0.0.1:0",
		MaxShutdown: 2 * time.Minute,
		Logger:      slog.New(slog.DiscardHandler),
		OnListen: func(net.Addr) {
			_ = os.WriteFile(os.Getenv(listeningEnv), []byte("up"), 0o600)
		},
	}, func(_ context.Context, app *serve.App) (http.Handler, error) {
		app.Close("will not stop", func(context.Context) error {
			select {}
		})
		return newHandler(app.Pool, nil), nil
	})
}

// The first signal starts the shutdown. The second one ends it.
//
// signal.NotifyContext leaves the handler installed until stop is called, so
// without giving it back a second SIGTERM goes into a channel nobody is reading
// and does nothing at all. That is the wrong answer for the case it arrives in:
// somebody watching a drain that is not progressing, whose only other move is to
// find the pid and send SIGKILL from outside.
func TestASecondSignalEndsAShutdownThatWillNotFinish(t *testing.T) {
	listening := filepath.Join(t.TempDir(), "listening")

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), childEnv+"=1", listeningEnv+"="+listening)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	waitFor(t, listening, 30*time.Second, "the child never listened")

	// The first: the shutdown starts, and stays started — the close step it
	// reaches never returns and has two minutes to not return in.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("the child exited on the first signal, before the step that hangs: %v", err)
	case <-time.After(2 * time.Second):
	}

	// The second: the handler has been given back, so this is the default
	// disposition and the process goes.
	began := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if took := time.Since(began); took > 10*time.Second {
			t.Errorf("the second signal took %s to end it", took)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a second SIGTERM did not end a shutdown that was going nowhere")
	}
}

func waitFor(t *testing.T, path string, within time.Duration, complaint string) {
	t.Helper()

	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(complaint)
}

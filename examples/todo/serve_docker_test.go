//go:build docker

package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/serve"
)

// What main does, driven from a test: boot against the real database, answer,
// and stop when asked. The interesting part is the last one — a shutdown that
// hangs, or one that drops the connection instead of finishing the request, is
// the kind of thing nobody notices until a deploy.
func TestTheServerBootsAndShutsDown(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	listening := make(chan net.Addr, 1)
	stopped := make(chan error, 1)
	drained := make(chan struct{})
	closed := make(chan time.Time, 1)

	go func() {
		stopped <- serve.Run(ctx, serve.Config{
			DatabaseURL:   dsn,
			Addr:          "127.0.0.1:0",
			LivenessPath:  "/livez",
			ReadinessPath: "/readyz",
			DrainDelay:    2 * time.Second,
			// Room for the drain delay and the notifier's own five seconds,
			// which mount registers. rig refuses to start if there is not.
			MaxShutdown: 15 * time.Second,
			Logger:      slog.New(slog.DiscardHandler),
			OnListen:    func(a net.Addr) { listening <- a },
		}, func(_ context.Context, app *serve.App) (http.Handler, error) {
			// A dependency of the kind every real service has. What is being
			// checked is when it stops: after the last request, not before.
			app.Drain("consumer", func(context.Context) error {
				close(drained)
				return nil
			})
			app.Close("consumer", func(context.Context) error {
				closed <- time.Now()
				return nil
			})
			return newHandler(app.Pool, nil), nil
		})
	}()

	var addr net.Addr
	select {
	case addr = <-listening:
	case err := <-stopped:
		t.Fatalf("the server stopped before it listened: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the server never listened")
	}

	base := "http://" + addr.String()

	t.Run("both probes answer", func(t *testing.T) {
		// Readiness really asks the database; liveness deliberately does not.
		for _, path := range []string{"/livez", "/readyz"} {
			if got := status(t, base+path); got != http.StatusOK {
				t.Errorf("%s = %d, want 200", path, got)
			}
		}
	})

	t.Run("the routes are mounted", func(t *testing.T) {
		// No tenant header, so the generated handler refuses it — which is
		// proof the route exists and is scoped, rather than a 404.
		if got := status(t, base+"/api/v1/todos"); got != http.StatusUnauthorized {
			t.Errorf("status = %d, want 401", got)
		}
	})

	cancel()

	// The drain is the point of having two probes: readiness goes false while
	// the server is still answering, so whatever routes traffic has a window to
	// look away before anything is refused.
	// A consumer stops fetching at the start of the shutdown, while the server
	// is still finishing what it has. Stopping it afterwards would mean
	// spending the drain window pulling work nobody will finish.
	t.Run("what fetches its own work stops first", func(t *testing.T) {
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Fatal("the drain hook never ran")
		}

		select {
		case at := <-closed:
			t.Fatalf("the close hook ran at %s, before the server stopped", at)
		default:
		}
	})

	t.Run("readiness fails first, and the server keeps serving", func(t *testing.T) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if got := status(t, base+"/readyz"); got == http.StatusServiceUnavailable {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("readiness never turned false")
			}
			time.Sleep(20 * time.Millisecond)
		}

		if got := status(t, base+"/livez"); got != http.StatusOK {
			t.Errorf("liveness = %d while draining, want 200: the process is fine, it is leaving", got)
		}
		// And real requests are still served during the window.
		if got := status(t, base+"/api/v1/todos"); got != http.StatusUnauthorized {
			t.Errorf("the API answered %d while draining, want it still serving", got)
		}
	})

	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("a clean shutdown should not be an error: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop")
	}

	// And the dependency was closed, after the server stopped rather than
	// during, so a request in flight still had it.
	select {
	case <-closed:
	default:
		t.Error("the close hook never ran")
	}

	// And it really stopped: the listener is closed, not merely idle.
	if _, err := http.Get(base + "/livez"); err == nil {
		t.Error("the server should no longer be answering")
	}
}

func status(t *testing.T, url string) int {
	t.Helper()

	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, res.Body)

	return res.StatusCode
}

// A dependency that will not close is worth reporting and is not a server that
// failed: the process served, then did not tidy up. Run says which it was, so
// main can log it without telling the orchestrator the container crashed.
func TestAFailedShutdownIsNotAFailedServer(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}

	ctx, cancel := context.WithCancel(t.Context())
	listening := make(chan net.Addr, 1)
	stopped := make(chan error, 1)
	broken := errors.New("would not close")

	go func() {
		stopped <- serve.Run(ctx, serve.Config{
			DatabaseURL: dsn,
			Addr:        "127.0.0.1:0",
			MaxShutdown: 5 * time.Second,
			Logger:      slog.New(slog.DiscardHandler),
			OnListen:    func(a net.Addr) { listening <- a },
		}, func(_ context.Context, app *serve.App) (http.Handler, error) {
			app.Close("stubborn", func(context.Context) error { return broken })
			return newHandler(app.Pool, nil), nil
		})
	}()

	select {
	case <-listening:
	case err := <-stopped:
		t.Fatalf("the server stopped before it listened: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("the server never listened")
	}

	cancel()

	select {
	case err := <-stopped:
		if !errors.Is(err, broken) {
			t.Fatalf("err = %v, want the failure that happened", err)
		}
		if !serve.Unclean(err) {
			t.Error("a failed teardown should not read as a failed server")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop")
	}
}

// A boot that hangs is worse than one that fails: nothing is serving and
// nothing has said why. MaxStartup turns it into an error with a phase in it.
func TestAStartupThatHangsIsRefused(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	start := time.Now()
	err := serve.Run(t.Context(), serve.Config{
		DatabaseURL: dsn,
		Addr:        "127.0.0.1:0",
		MaxStartup:  200 * time.Millisecond,
		Logger:      slog.New(slog.DiscardHandler),
	}, func(_ context.Context, app *serve.App) (http.Handler, error) {
		// Something slow that does not watch its context: a client dialling a
		// host that will never answer.
		<-release
		return newHandler(app.Pool, nil), nil
	})

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the startup budget to have ended it", err)
	}
	if !strings.Contains(err.Error(), "build the routes") {
		t.Errorf("the error should name the phase that hung: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("took %s to give up", elapsed)
	}
}

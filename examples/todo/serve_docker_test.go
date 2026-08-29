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

// stated is a serve.Config with every value serve refuses to invent, so a test
// below writes only what it is actually about.
//
// serve has no defaults: a config that leaves a timeout or a probe path empty is
// refused before anything listens, which is the point — a value nobody chose is
// found only by what it costs when it is wrong. In a test that trade lands as
// ten fields per case that are not what the case is about, so they are written
// once, here, where they can still be read.
func stated(dsn string) serve.Config {
	return serve.Config{
		DatabaseURL:       dsn,
		Addr:              "127.0.0.1:0",
		Logger:            slog.New(slog.DiscardHandler),
		LivenessPath:      "/livez",
		ReadinessPath:     "/readyz",
		MaxStartup:        30 * time.Second,
		ConnectTimeout:    10 * time.Second,
		ProbeTimeout:      2 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

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

	cfg := stated(dsn)
	cfg.DrainDelay = 2 * time.Second
	// Room for the drain delay and the notifier's own five seconds, which mount
	// registers. rig refuses to start if there is not.
	cfg.MaxShutdown = 15 * time.Second
	cfg.OnListen = func(a net.Addr) { listening <- a }

	go func() {
		stopped <- serve.Run(ctx, cfg, func(_ context.Context, app *serve.App) (http.Handler, error) {
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

// Two listeners in one process, and what the second one is for.
//
// rig's monitoring page is served on a port of its own rather than a route on
// the API's mux, because a bind address is a boundary the kernel keeps and a
// path is not — an allowlist keyed on the connection's address matches
// everything or nothing behind a load balancer, and the interface a socket is
// bound to does not. This example turns the page off, so the handler here is a
// stand-in: what is under test is serve, not observe.
//
// The last part is why it is stopped last. A monitoring page that goes away
// with the API is one you cannot use to watch a drain, which is one of the
// times you most want it.
func TestTheMonitorIsASecondListener(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	listening := make(chan net.Addr, 1)
	monitoring := make(chan net.Addr, 1)
	stopped := make(chan error, 1)

	cfg := stated(dsn)
	cfg.DrainDelay = 2 * time.Second
	cfg.MaxShutdown = 15 * time.Second
	cfg.Monitor = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})
	cfg.MonitorAddr = "127.0.0.1:0"
	cfg.OnListen = func(a net.Addr) { listening <- a }
	cfg.OnMonitorListen = func(a net.Addr) { monitoring <- a }

	go func() {
		stopped <- serve.Run(ctx, cfg, func(_ context.Context, app *serve.App) (http.Handler, error) {
			return newHandler(app.Pool, nil), nil
		})
	}()

	var api, monitor net.Addr
	for range 2 {
		select {
		case api = <-listening:
		case monitor = <-monitoring:
		case err := <-stopped:
			t.Fatalf("the server stopped before both listeners were up: %v", err)
		case <-time.After(15 * time.Second):
			t.Fatal("one of the two listeners never came up")
		}
	}
	if api.String() == monitor.String() {
		t.Fatalf("both listeners bound %s; they are supposed to be two ports", api)
	}

	apiBase, monitorBase := "http://"+api.String(), "http://"+monitor.String()

	t.Run("each port answers only its own", func(t *testing.T) {
		if got := status(t, monitorBase+"/_rig/monitor/"); got != http.StatusTeapot {
			t.Errorf("the monitor port answered %d, want the handler's 418", got)
		}
		// The whole point: the page's path is nothing on the API's listener.
		if got := status(t, apiBase+"/_rig/monitor/"); got != http.StatusNotFound {
			t.Errorf("the API port answered %d for the page's path, want 404", got)
		}
		// And the probes stayed where an orchestrator can reach them.
		if got := status(t, apiBase+"/livez"); got != http.StatusOK {
			t.Errorf("liveness on the API port = %d, want 200", got)
		}
		if got := status(t, monitorBase+"/livez"); got != http.StatusTeapot {
			t.Errorf("the monitor port answered %d for /livez; the probes are not on it", got)
		}
	})

	cancel()

	t.Run("the monitor outlives the drain", func(t *testing.T) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			if status(t, apiBase+"/readyz") == http.StatusServiceUnavailable {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("readiness never turned false")
			}
			time.Sleep(20 * time.Millisecond)
		}

		if got := status(t, monitorBase+"/_rig/monitor/"); got != http.StatusTeapot {
			t.Errorf("the monitor answered %d while the API drained, want it still serving", got)
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

	// Both ports are closed once Run has returned, the monitor's last of all.
	for _, url := range []string{apiBase + "/livez", monitorBase + "/_rig/monitor/"} {
		if _, err := http.Get(url); err == nil {
			t.Errorf("%s still answered after the server stopped", url)
		}
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
		cfg := stated(dsn)
		cfg.MaxShutdown = 5 * time.Second
		cfg.OnListen = func(a net.Addr) { listening <- a }
		stopped <- serve.Run(ctx, cfg, func(_ context.Context, app *serve.App) (http.Handler, error) {
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
	cfg := stated(dsn)
	cfg.MaxStartup = 200 * time.Millisecond
	// And the connection budget with it. A short MaxStartup used to pull this
	// down on its own; nothing adjusts a stated value now, so a config that
	// asked to connect for ten seconds inside a two-hundred-millisecond boot is
	// refused for saying two things that cannot both be true.
	cfg.ConnectTimeout = 100 * time.Millisecond
	cfg.MaxShutdown = 5 * time.Second

	err := serve.Run(t.Context(), cfg, func(_ context.Context, app *serve.App) (http.Handler, error) {
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

// A request that will not finish spends what is left over, and not the closers'
// share of the budget.
//
// This is the failure the leftover in MaxShutdown was always described as
// preventing and, until serveUntil, did not. A long poll or a client that
// stopped reading is an in-flight request that http.Server.Shutdown waits for,
// and Shutdown does not cancel a request's context — so without a deadline of
// its own it waits until the whole budget is gone, and every step registered
// with App.Close then meets one that has already passed. The worst of those is a
// write: rig's own notification engine hands its claims back in exactly such a
// step, and a pass that does not run leaves them held until the claim expires.
func TestAHandlerThatWillNotReturnDoesNotStarveTheTeardown(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = dsnFallback
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	listening := make(chan net.Addr, 1)
	stopped := make(chan error, 1)
	// The step that has to survive the handler, and how much of its own limit it
	// was actually given.
	left := make(chan time.Duration, 1)

	hung := make(chan struct{})
	t.Cleanup(func() { close(hung) })

	go func() {
		cfg := stated(dsn)
		// Four seconds, of which two belong to the closer below. A handler that
		// never returns should get the other two and no more.
		cfg.MaxShutdown = 4 * time.Second
		cfg.OnListen = func(a net.Addr) { listening <- a }
		stopped <- serve.Run(ctx, cfg, func(_ context.Context, app *serve.App) (http.Handler, error) {
			app.CloseWithin("release the claims", 2*time.Second, func(c context.Context) error {
				deadline, ok := c.Deadline()
				if !ok {
					left <- 0
					return nil
				}
				left <- time.Until(deadline)
				return nil
			})

			mux := http.NewServeMux()
			mux.HandleFunc("/hang", func(w http.ResponseWriter, r *http.Request) {
				// Deliberately not watching r.Context(): that is what a poll
				// waiting on something upstream looks like, and it is the case
				// Shutdown cannot do anything about on its own.
				<-hung
			})
			return mux, nil
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

	// One request in flight, going nowhere.
	polling := make(chan struct{})
	go func() {
		close(polling)
		res, err := http.Get("http://" + addr.String() + "/hang")
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
		}
	}()
	<-polling
	time.Sleep(200 * time.Millisecond)

	began := time.Now()
	cancel()

	select {
	case got := <-left:
		// The whole two seconds, near enough: the point is that the closer did
		// not inherit a deadline the handler had already spent.
		if got < 1500*time.Millisecond {
			t.Errorf("the closer was given %s of its 2s", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the closer never ran: the handler took the whole budget")
	}

	select {
	case <-stopped:
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop")
	}

	// And the whole sequence still fits the budget it was given.
	if took := time.Since(began); took > 6*time.Second {
		t.Errorf("the shutdown took %s against a 4s budget", took)
	}
}

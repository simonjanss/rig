//go:build docker

package integration

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/serve"
)

// This example's whole shutdown, driven the way a deployment drives it: through
// serve.Run, with a real subscription open against a sync service that has
// stopped answering.
//
// It is the one arrangement where the shutdown can go wrong in a way none of the
// other tests here would see. A live shape poll is a request the server is
// deliberately not answering yet, so http.Server.Shutdown waits for it; nothing
// in the poll is late, so no timeout applies to it; and Shutdown does not cancel
// a request's context. Without the drain api.Register attaches to Shapes.App,
// one open board spends the entire budget — and then the trace flush, the sweep
// and the notification engine's close, which is where its claims are handed
// back, each find a deadline that has already passed.
//
// The sync service here accepts and says nothing, which is the worst case and
// what a paused container or a partition produces. A healthy Electric answers in
// about twenty seconds, which is bad enough on a budget of forty.
func TestTheServerStopsWithASubscriptionOpen(t *testing.T) {
	// The harness server below reads this through electricURL when it builds its
	// proxy, so it is set before that server exists. The server under test is
	// handed the same address by name.
	upstream, release := newSilentSync(t)
	t.Setenv("ELECTRIC_URL", upstream)

	// The harness server, for the session. It is minted against the same pool,
	// so it is a session on the server under test too.
	fixture := newServer(t)
	fixture.seed(t)
	token := fixture.login(t, app.SeedEmail)

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55444/rig?sslmode=disable"
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listening := make(chan net.Addr, 1)
	stopped := make(chan error, 1)
	// What the first step registered after the server was actually given.
	budget := make(chan time.Duration, 1)

	go func() {
		stopped <- serve.Run(ctx, serve.Config{
			DatabaseURL: dsn,
			Addr:        "127.0.0.1:0",
			Logger:      slog.New(slog.DiscardHandler),
			// serve has no defaults, so a config states all of it or is refused
			// before anything listens. Only the drain delay and the budget are
			// what this test is about; the rest are ordinary values written out
			// because there is nowhere else for them to come from.
			LivenessPath:      "/livez",
			ReadinessPath:     "/readyz",
			MaxStartup:        30 * time.Second,
			ConnectTimeout:    10 * time.Second,
			ProbeTimeout:      2 * time.Second,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       2 * time.Minute,
			DrainDelay:        time.Second,
			MaxShutdown:       api.ShutdownBudget() + time.Second,
			OnListen:          func(a net.Addr) { listening <- a },
		}, func(c context.Context, srv *serve.App) (http.Handler, error) {
			// The generated sequence itself, not a copy of it. api.Main is this
			// and a serve.Config; serve.Run is what a test that wants the
			// process needs, and api.Mount is the half they share — so what is
			// under test here is the order that ships rather than one written
			// out again beside it. No page, the way a task has none.
			handler, err := api.Mount(func(c context.Context, srv *serve.App, _ *observe.Page) (api.Parts, error) {
				return app.New(c, app.Config{
					Pool:   srv.Pool,
					Logger: srv.Logger,
					App:    srv,
					// The sync service that accepts and never answers, named
					// rather than read back out of the environment: this is the
					// one thing the test is about.
					ElectricURL: upstream,
				})
			})(c, srv)
			if err != nil {
				return nil, err
			}

			// Registered after that, so it closes before every step in it —
			// and therefore with the whole of whatever the server left behind.
			srv.CloseWithin("what is left", 2*time.Second, func(c context.Context) error {
				deadline, ok := c.Deadline()
				if !ok {
					budget <- 0
					return nil
				}
				budget <- time.Until(deadline)
				return nil
			})
			return handler, nil
		})
	}()

	var addr net.Addr
	select {
	case addr = <-listening:
	case err := <-stopped:
		t.Fatalf("the server stopped before it listened: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("the server never listened")
	}

	// A subscriber, resuming the way a browser that was already streaming does:
	// an offset that is not the beginning, so nothing in the proxy bounds the
	// wait, and a poll that reaches the silent service and stays there.
	polled := make(chan int, 1)
	go func() {
		req, err := http.NewRequest(http.MethodGet,
			"http://"+addr.String()+"/api/v1/todo/_stream?offset=0_inf&live=true&handle=h", nil)
		if err != nil {
			polled <- 0
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			polled <- 0
			return
		}
		defer res.Body.Close()
		polled <- res.StatusCode
	}()

	// It has to still be waiting when the shutdown starts, or this test is
	// asserting nothing.
	select {
	case code := <-polled:
		t.Fatalf("the poll answered %d before the shutdown; it was never held open", code)
	case <-time.After(2 * time.Second):
	}

	began := time.Now()
	cancel()

	select {
	case got := <-budget:
		// Near enough its whole two seconds. What matters is that it is not
		// zero: a step reached after the budget ran out gets a deadline in the
		// past and gives up without doing anything.
		if got < time.Second {
			t.Errorf("the first close step was given %s of its 2s: the poll took the budget", got)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("no close step ever ran")
	}

	select {
	case err := <-stopped:
		if err != nil {
			t.Errorf("the shutdown reported %v", err)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("the server did not stop")
	}

	// And the subscriber was told to come back rather than left hanging or cut
	// off mid-response.
	select {
	case code := <-polled:
		if code != http.StatusServiceUnavailable {
			t.Errorf("the subscriber got %d, want 503 with a Retry-After", code)
		}
	case <-time.After(10 * time.Second):
		t.Error("the poll outlived the server")
	}

	if took := time.Since(began); took > api.ShutdownBudget() {
		t.Errorf("the shutdown took %s against a %s budget", took, api.ShutdownBudget())
	}

	close(release)
}

// newSilentSync is a sync service that accepts a request and never answers it,
// which is what a hung Electric looks like from the proxy's side. Closing the
// returned channel lets its handlers go.
func newSilentSync(t *testing.T) (url string, release chan struct{}) {
	t.Helper()

	held := make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-held:
		case <-r.Context().Done():
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	return "http://" + ln.Addr().String(), held
}

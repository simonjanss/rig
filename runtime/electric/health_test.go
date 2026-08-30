package electric_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/electric"
)

// health is a stub sync service answering only the health endpoint, with
// whatever this case wants it to say.
func health(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()

	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			t.Errorf("path = %q, want /v1/health", r.URL.Path)
		}
		asked++
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &asked
}

func TestHealthAcceptsAnActiveService(t *testing.T) {
	t.Parallel()

	srv, asked := health(t, http.StatusOK, `{"status":"active"}`)
	p, err := newProxy(electric.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	if err := p.Health(context.Background()); err != nil {
		t.Fatalf("Health = %v, want nil", err)
	}
	if *asked != 1 {
		t.Errorf("asked %d times, want 1", *asked)
	}
}

// The gap this whole method exists for: the service is answering HTTP and has
// not connected to its database, and a shape request sent now hangs rather than
// fails.
func TestHealthRefusesAServiceThatIsUpButNotServing(t *testing.T) {
	t.Parallel()

	srv, _ := health(t, http.StatusOK, `{"status":"waiting"}`)
	p, err := newProxy(electric.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	err = p.Health(context.Background())
	if err == nil {
		t.Fatal("Health = nil, want an error: a 200 alone is not the test")
	}
	// The state it did report, so whoever reads the log knows to wait rather
	// than to go looking for a wrong address.
	if !strings.Contains(err.Error(), "waiting") {
		t.Errorf("error = %q, want it to quote the state the service reported", err)
	}
}

func TestHealthReportsTheStatus(t *testing.T) {
	t.Parallel()

	srv, _ := health(t, http.StatusServiceUnavailable, "no")
	p, err := newProxy(electric.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	err = p.Health(context.Background())
	if err == nil {
		t.Fatal("Health = nil, want an error")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %q, want the status in it", err)
	}
}

func TestHealthReportsAServiceThatIsNotThere(t *testing.T) {
	t.Parallel()

	p, err := newProxy(electric.Config{URL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}

	err = p.Health(context.Background())
	if err == nil {
		t.Fatal("Health = nil, want an error")
	}
	if !strings.Contains(err.Error(), "cannot reach the sync service") {
		t.Errorf("error = %q", err)
	}
}

// The caller's context is the whole bound. Health adds none of its own.
func TestHealthIsBoundedByItsContext(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	p, err := newProxy(electric.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	began := time.Now()
	if err := p.Health(ctx); err == nil {
		t.Fatal("Health = nil, want the context's error")
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("took %s: the context did not bound it", took)
	}
}

// A probe is not a subscription. Whatever it finds, the requests that did happen
// are what SyncReachable answers — otherwise a monitoring page polling every few
// seconds decides whether every subscriber gets live sync.
func TestHealthLeavesTheCircuitAlone(t *testing.T) {
	t.Parallel()

	srv, _ := health(t, http.StatusServiceUnavailable, "no")
	p, err := newProxy(electric.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	for range electric.DefaultBreakerThreshold * 2 {
		if err := p.Health(context.Background()); err == nil {
			t.Fatal("Health = nil, want an error")
		}
	}
	if !p.SyncReachable() {
		t.Error("SyncReachable = false: a failed probe opened the circuit")
	}
}

func TestHealthOnADrainingProxy(t *testing.T) {
	t.Parallel()

	srv, asked := health(t, http.StatusOK, `{"status":"active"}`)
	p, err := newProxy(electric.Config{URL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := p.Health(context.Background()); err == nil {
		t.Fatal("Health = nil, want an error from a proxy that is going away")
	}
	if *asked != 0 {
		t.Errorf("asked %d times, want 0: a shutting-down proxy has nothing to probe for", *asked)
	}
}

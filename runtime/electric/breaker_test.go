package electric_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/electric"
)

// counted is an upstream that says how many times it was asked, which is the
// whole subject of these tests: what the circuit is for is not asking.
type counted struct {
	srv   *httptest.Server
	hits  atomic.Int64
	fail  atomic.Bool
	block chan struct{}
}

func newCounted(t *testing.T) *counted {
	t.Helper()

	c := &counted{}
	c.fail.Store(true)
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.hits.Add(1)

		if c.block != nil {
			select {
			case <-c.block:
			case <-r.Context().Done():
				return
			}
		}
		if c.fail.Load() {
			http.Error(w, "no", http.StatusInternalServerError)
			return
		}
		w.Header().Set("electric-handle", "the-handle")
		w.Header().Set("electric-offset", "0_0")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"headers":{"control":"up-to-date"}}]`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// The point of the circuit: past the threshold the sync service is not asked
// again, so the requests behind the outage do not each pay for finding it.
func TestAnOpenCircuitStopsAskingTheSyncService(t *testing.T) {
	t.Parallel()

	up := newCounted(t)
	p, _ := electric.New(electric.Config{
		URL:              up.srv.URL,
		BreakerThreshold: 3,
		// Long enough that nothing is let through to test the service during
		// this test, so the count is only the failures that opened it.
		BreakerCooldown: time.Hour,
	})
	shape := electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}

	for range 8 {
		if got := serve(t, p, shape, "offset=-1").StatusCode; got != http.StatusOK {
			t.Fatalf("status = %d, want the snapshot", got)
		}
	}

	if got := up.hits.Load(); got != 3 {
		t.Errorf("the sync service was asked %d times, want the 3 that opened the circuit", got)
	}
	if p.SyncReachable() {
		t.Error("SyncReachable is true with the circuit open")
	}
}

// And the requests behind it are answered at once rather than each waiting out
// the deadline, which is what the circuit buys.
func TestAnOpenCircuitAnswersWithoutWaiting(t *testing.T) {
	t.Parallel()

	up := newCounted(t)
	up.block = make(chan struct{})
	t.Cleanup(func() { close(up.block) })

	p, _ := electric.New(electric.Config{
		URL:              up.srv.URL,
		InitialTimeout:   300 * time.Millisecond,
		BreakerThreshold: 1,
		BreakerCooldown:  time.Hour,
	})
	shape := electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}

	// The first request finds the outage, and pays the deadline to do it.
	start := time.Now()
	if got := serve(t, p, shape, "offset=-1").StatusCode; got != http.StatusOK {
		t.Fatalf("status = %d", got)
	}
	if waited := time.Since(start); waited < 300*time.Millisecond {
		t.Fatalf("the first request took %s, so it did not wait for the service", waited)
	}

	// The second is answered from the fallback without one.
	start = time.Now()
	res := serve(t, p, shape, "offset=-1")
	if waited := time.Since(start); waited > 150*time.Millisecond {
		t.Errorf("the second request took %s, so it waited for a service nobody was asking", waited)
	}
	if got := res.Header.Get("X-Rig-Sync-Fallback"); got != "snapshot" {
		t.Errorf("X-Rig-Sync-Fallback = %q", got)
	}
}

// A shape with nothing to fall back to gets the status it always got, and gets
// it immediately. The circuit is about who is asked, not about what is answered.
func TestAnOpenCircuitStillRefusesAShapeWithNoFallback(t *testing.T) {
	t.Parallel()

	up := newCounted(t)
	p, _ := electric.New(electric.Config{
		URL: up.srv.URL, BreakerThreshold: 1, BreakerCooldown: time.Hour,
	})
	shape := electric.Shape{Table: "lesson"}

	// The first is forwarded, because a 5xx the sync service chose says more
	// than a 502 substituted for it.
	if got := serve(t, p, shape, "offset=-1").StatusCode; got != http.StatusInternalServerError {
		t.Fatalf("status = %d, want the 500 forwarded", got)
	}
	if got := serve(t, p, shape, "offset=-1").StatusCode; got != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", got)
	}
	if got := up.hits.Load(); got != 1 {
		t.Errorf("the sync service was asked %d times", got)
	}
}

// One request through the cooldown, not all of them: the rest are answered
// while it finds out, or a recovering service meets the whole herd at once.
func TestOneRequestAtATimeTestsTheService(t *testing.T) {
	t.Parallel()

	up := newCounted(t)
	p, _ := electric.New(electric.Config{
		URL:              up.srv.URL,
		BreakerThreshold: 1,
		BreakerCooldown:  50 * time.Millisecond,
	})
	shape := electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}

	serve(t, p, shape, "offset=-1")
	if got := up.hits.Load(); got != 1 {
		t.Fatalf("the circuit did not open: %d requests reached the service", got)
	}

	time.Sleep(80 * time.Millisecond)
	// This one is let through and fails, which starts the cooldown again.
	serve(t, p, shape, "offset=-1")
	// So this one is not, even though it arrives after the same cooldown the
	// first one waited out.
	serve(t, p, shape, "offset=-1")

	if got := up.hits.Load(); got != 2 {
		t.Errorf("the sync service was asked %d times, want the failure and one test", got)
	}
}

// And the recovery: the request that succeeds closes the circuit, and the ones
// after it are forwarded like any other.
func TestTheRequestThatSucceedsClosesTheCircuit(t *testing.T) {
	t.Parallel()

	up := newCounted(t)
	p, _ := electric.New(electric.Config{
		URL:              up.srv.URL,
		BreakerThreshold: 1,
		BreakerCooldown:  50 * time.Millisecond,
	})
	shape := electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}

	serve(t, p, shape, "offset=-1")
	if p.SyncReachable() {
		t.Fatal("the circuit did not open")
	}

	up.fail.Store(false)
	time.Sleep(80 * time.Millisecond)

	res := serve(t, p, shape, "offset=-1")
	if got := res.Header.Get("X-Rig-Sync-Fallback"); got != "" {
		t.Errorf("the test request was answered from the fallback: %q", got)
	}
	if got := res.Header.Get("electric-handle"); got != "the-handle" {
		t.Errorf("electric-handle = %q, so the answer was not the sync service's", got)
	}
	if !p.SyncReachable() {
		t.Error("SyncReachable is false after a request succeeded")
	}

	// And nothing is being held back any more.
	before := up.hits.Load()
	serve(t, p, shape, "offset=-1")
	if got := up.hits.Load(); got != before+1 {
		t.Error("a request after the circuit closed did not reach the sync service")
	}
}

// Twice per outage rather than once per request, which is what makes this the
// thing to alert on.
func TestOnSyncStateSaysWhenItWentAndWhenItCameBack(t *testing.T) {
	t.Parallel()

	up := newCounted(t)
	var seen []bool
	p, _ := electric.New(electric.Config{
		URL:              up.srv.URL,
		BreakerThreshold: 2,
		BreakerCooldown:  50 * time.Millisecond,
		OnSyncState:      func(_ context.Context, reachable bool) { seen = append(seen, reachable) },
	})
	shape := electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}

	// Two failures to open it, then two more requests that are answered without
	// asking anybody and are therefore not news.
	for range 4 {
		serve(t, p, shape, "offset=-1")
	}
	up.fail.Store(false)
	time.Sleep(80 * time.Millisecond)
	serve(t, p, shape, "offset=-1")
	serve(t, p, shape, "offset=-1")

	if len(seen) != 2 || seen[0] || !seen[1] {
		t.Errorf("OnSyncState saw %v, want one false and then one true", seen)
	}
}

// A page being closed is not an outage. A client that hangs up mid-poll is the
// ordinary end of a subscription, and counting it would let a busy tab open the
// circuit on a sync service that was answering fine.
func TestAClientHangingUpDoesNotOpenTheCircuit(t *testing.T) {
	t.Parallel()

	up := newCounted(t)
	up.block = make(chan struct{})
	t.Cleanup(func() { close(up.block) })

	p, _ := electric.New(electric.Config{
		URL: up.srv.URL, BreakerThreshold: 1, InitialTimeout: -1,
	})
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.Serve(w, r, electric.Shape{Table: "lesson"})
	}))
	t.Cleanup(front.Close)

	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"?offset=0_inf&handle=h&live=true", nil)
		if err != nil {
			t.Fatal(err)
		}
		if res, err := http.DefaultClient.Do(req); err == nil {
			res.Body.Close()
			t.Fatal("the request was answered, so nothing hung up")
		}
		cancel()
	}

	if !p.SyncReachable() {
		t.Error("three closed pages opened the circuit")
	}
}

// Off is off: a project that would rather every request went on asking can say
// so, and then nothing counts and nothing is skipped.
func TestTheCircuitCanBeTurnedOff(t *testing.T) {
	t.Parallel()

	up := newCounted(t)
	p, _ := electric.New(electric.Config{URL: up.srv.URL, BreakerThreshold: -1})
	shape := electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}

	for range 4 {
		serve(t, p, shape, "offset=-1")
	}

	if got := up.hits.Load(); got != 4 {
		t.Errorf("the sync service was asked %d times, want all 4", got)
	}
	if !p.SyncReachable() {
		t.Error("SyncReachable is false with nothing counting")
	}
}

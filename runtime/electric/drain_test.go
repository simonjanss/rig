package electric_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/electric"
)

// A live poll ends when the process is stopping, not only when the sync service
// finally has something to say.
//
// This is the whole reason Drain exists. A subscription is an in-flight request,
// so http.Server.Shutdown waits for it; nothing in the poll is late, so no
// timeout applies to it; and Shutdown does not cancel a request's context.
// Without this the shutdown budget is spent waiting for news.
func TestDrainEndsAPollThatIsStillWaiting(t *testing.T) {
	t.Parallel()

	// A sync service that accepts the request and then says nothing, which is
	// what a live poll looks like from here and what a hung one looks like too.
	held := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-held:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(held)

	proxy, err := electric.New(electric.Config{URL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}

	polling := make(chan struct{})
	answered := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/shape?offset=0_0&handle=h&live=true", nil)
		close(polling)
		proxy.Serve(rec, r, electric.Shape{Table: "todo"})
		answered <- rec.Code
	}()

	<-polling
	// Long enough that the poll is genuinely upstream and waiting, rather than
	// being caught before it started.
	time.Sleep(100 * time.Millisecond)

	began := time.Now()
	if err := proxy.Drain(context.Background()); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if took := time.Since(began); took > 2*time.Second {
		t.Errorf("the drain took %s to release one poll", took)
	}

	select {
	case code := <-answered:
		// Told to come back, not handed a snapshot: there is no server left to
		// read one out of, and the next attempt belongs to another replica.
		if code != http.StatusServiceUnavailable {
			t.Errorf("the subscriber got %d, want 503", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the poll outlived the drain that was supposed to release it")
	}
}

// A request that arrives after the drain is refused rather than started, since
// starting one opens a poll this process will not be here to answer.
func TestADrainedProxyStartsNoNewPoll(t *testing.T) {
	t.Parallel()

	var reached atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached.Store(true)
	}))
	defer upstream.Close()

	proxy, err := electric.New(electric.Config{URL: upstream.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err := proxy.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	proxy.Serve(rec, httptest.NewRequest(http.MethodGet, "/shape?offset=-1", nil), electric.Shape{Table: "todo"})

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("a subscriber told to go away should be told when to come back")
	}
	if reached.Load() {
		t.Error("a drained proxy asked the sync service anyway")
	}
}

// A proxy that never served anything returns at once, and draining twice is
// safe: a shutdown sequence runs from a defer that also covers a failure during
// startup, so it can be reached more than once.
func TestDrainingIsSafeToRepeatAndToDoForNothing(t *testing.T) {
	t.Parallel()

	proxy, err := electric.New(electric.Config{URL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := proxy.Drain(context.Background()); err != nil {
				t.Errorf("drain: %v", err)
			}
		}()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("draining a proxy with nothing open did not return")
	}
}

// The circuit being open is not a way past the drain.
//
// A sync outage is exactly when a shutdown is likely to overlap one, and the
// request that arrives then must not be answered from the database: a snapshot
// is a table read and a large body out of a process whose pool is about to
// close. Come back is the answer on both paths, which is why the drain is
// checked before the circuit and not after it.
func TestADrainedProxyDoesNotFallBackWhileTheCircuitIsOpen(t *testing.T) {
	t.Parallel()

	var read atomic.Bool
	shape := electric.Shape{
		Table:   "todo",
		Key:     []string{"id"},
		Columns: []string{"id"},
		Fallback: func(context.Context) (electric.Snapshot, error) {
			read.Store(true)
			return electric.Snapshot{}, nil
		},
	}

	// Nothing listening, and one failure is enough to open the circuit.
	proxy, err := electric.New(electric.Config{URL: "http://127.0.0.1:1", BreakerThreshold: 1})
	if err != nil {
		t.Fatal(err)
	}
	proxy.Serve(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/shape?offset=-1", nil), shape)
	if proxy.SyncReachable() {
		t.Fatal("the circuit did not open, so this test is asserting nothing")
	}

	if err := proxy.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	read.Store(false)

	rec := httptest.NewRecorder()
	proxy.Serve(rec, httptest.NewRequest(http.MethodGet, "/shape?offset=-1", nil), shape)

	if read.Load() {
		t.Error("a drained proxy answered from the database because the circuit was open")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503", rec.Code)
	}
}

package throttle_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/throttle"
)

func localSetup(t *testing.T, cfg throttle.LocalConfig) (*throttle.Limiter, *tally, *clock) {
	t.Helper()

	c := newClock()
	rec := newTally()
	return throttle.NewRecording(throttle.NewLocal(rec, cfg)).WithClock(c.now), rec, c
}

// The whole point of the local tally: a caller nowhere near their limit should
// not be costing a database round trip per request.
func TestAQuietCallerCostsAlmostNoWrites(t *testing.T) {
	t.Parallel()

	limiter, rec, c := localSetup(t, throttle.LocalConfig{Interval: time.Second})
	key := throttle.Account("acct-1")
	limit := throttle.Limit{Name: "api.account", Max: 1000, Window: time.Minute}

	// A hundred calls inside one Interval.
	for range 100 {
		if _, err := limiter.Take(context.Background(), throttle.Check{Limit: limit, Key: key}); err != nil {
			t.Fatal(err)
		}
		c.advance(time.Millisecond)
	}

	// One write to announce the key, and no more until the Interval is up.
	if got := rec.roundTrips(); got > 1 {
		t.Fatalf("100 calls cost %d round trips; the local tally is not holding anything back", got)
	}
}

// And the other half of that trade: it must still publish, or several replicas
// each quietly counting a fraction of the budget are collectively far over it
// and none of them can tell.
func TestItPublishesOnTheInterval(t *testing.T) {
	t.Parallel()

	limiter, rec, c := localSetup(t, throttle.LocalConfig{Interval: time.Second})
	key := throttle.Account("acct-1")
	limit := throttle.Limit{Name: "api.account", Max: 1000, Window: time.Minute}

	for range 10 {
		if _, err := limiter.Take(context.Background(), throttle.Check{Limit: limit, Key: key}); err != nil {
			t.Fatal(err)
		}
		c.advance(time.Second)
	}

	if got := rec.roundTrips(); got < 9 {
		t.Fatalf("ten calls a second apart published %d times; a replica that never publishes is a limit that never applies", got)
	}
}

// The case the design is most exposed on: an attacker past their limit must not
// be able to turn each of their requests into a database write.
func TestBeingRefusedCostsNoWritePerRequest(t *testing.T) {
	t.Parallel()

	limiter, rec, c := localSetup(t, throttle.LocalConfig{Interval: time.Second})
	key := throttle.IP("203.0.113.7")
	limit := throttle.Limit{Name: "api.ip", Max: 5, Window: time.Minute}

	ctx := context.Background()
	for range limit.Max {
		if _, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key}); err != nil {
			t.Fatal(err)
		}
	}
	settled := rec.roundTrips()

	// Now hammer, well inside one Interval.
	for range 500 {
		d, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			t.Fatal("a request past the limit was allowed")
		}
		c.advance(time.Microsecond)
	}

	if extra := rec.roundTrips() - settled; extra > 1 {
		t.Fatalf("500 refused requests cost %d writes; a limiter that amplifies an attack into database load is worse than none", extra)
	}
}

// Two limiters over one tally stand in for two replicas. The limit must hold
// across them — approximately, and the overshoot must be bounded by what one
// Interval of traffic can hide.
func TestTwoReplicasShareTheBudget(t *testing.T) {
	t.Parallel()

	c := newClock()
	rec := newTally()
	limit := throttle.Limit{Name: "api.tenant", Max: 20, Window: time.Minute}
	key := throttle.Tenant("tenant-1")

	cfg := throttle.LocalConfig{Interval: 10 * time.Millisecond}
	a := throttle.NewRecording(throttle.NewLocal(rec, cfg)).WithClock(c.now)
	b := throttle.NewRecording(throttle.NewLocal(rec, cfg)).WithClock(c.now)

	ctx := context.Background()
	allowed := 0
	for range 100 {
		for _, l := range []*throttle.Limiter{a, b} {
			d, err := l.Take(ctx, throttle.Check{Limit: limit, Key: key})
			if err != nil {
				t.Fatal(err)
			}
			if d.Allowed {
				allowed++
			}
		}
		c.advance(time.Millisecond)
	}

	if allowed < limit.Max {
		t.Fatalf("only %d of a %d budget got through; two replicas should not each enforce a fraction", allowed, limit.Max)
	}
	// Generous, and deliberately so: the contract is "bounded overshoot", not
	// exactness. What it pins is that the limit applies at all — 200 requests
	// against a budget of 20 must not mostly succeed.
	if allowed > 2*limit.Max {
		t.Fatalf("%d of 200 requests got through against a limit of %d; the replicas are not reconciling", allowed, limit.Max)
	}
}

func TestARolloverCarriesUnpublishedCountsForward(t *testing.T) {
	t.Parallel()

	limiter, rec, c := localSetup(t, throttle.LocalConfig{Interval: time.Hour})
	key := throttle.Account("acct-1")
	limit := throttle.Limit{Name: "api.account", Max: 100, Window: time.Minute}

	ctx := context.Background()
	c.at = c.at.Truncate(time.Minute)
	for range 10 {
		if _, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key}); err != nil {
			t.Fatal(err)
		}
	}

	// Over the boundary. The ten calls above belong to a bucket that is now the
	// weighted previous one, and dropping them would hand out a fresh budget.
	c.advance(time.Minute)
	d, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if d.Used <= 1 {
		t.Fatalf("the first call of the new bucket counted %d; the previous bucket's unpublished calls were dropped", d.Used)
	}
	if rec.roundTrips() == 0 {
		t.Fatal("nothing was ever published")
	}
}

// held is a recorder whose first call blocks, so a test can hold a flush open
// across a bucket boundary. The total it eventually answers with is the shape of
// the trap: a large number about a window that has since ended.
type held struct {
	release chan struct{}

	mu    sync.Mutex
	calls int
}

func (h *held) Incr(_ context.Context, limit throttle.Limit, _ throttle.Key, now time.Time, delta int) (int, time.Time, error) {
	h.mu.Lock()
	h.calls++
	first := h.calls == 1
	h.mu.Unlock()

	resetAt := now.UTC().Truncate(limit.Window).Add(limit.Window)
	if first {
		<-h.release
		return 100, resetAt, nil
	}
	return delta, resetAt, nil
}

// A flush that lands after its bucket has rolled over must not be believed.
//
// The total it carries is about a window nobody is deciding from any more, and
// global only ever rises — so writing it would hand the new bucket the old one's
// whole count and keep the key refused for the rest of it, from memory, on
// evidence that expired.
func TestAFlushThatLandsAfterARolloverIsNotBelieved(t *testing.T) {
	t.Parallel()

	rec := &held{release: make(chan struct{})}
	local := throttle.NewLocal(rec, throttle.LocalConfig{Interval: time.Millisecond})
	limit := throttle.Limit{Name: "api.ip", Max: 10, Window: time.Minute}
	key := throttle.IP("203.0.113.7")
	ctx := context.Background()

	base := time.Now().UTC().Truncate(time.Minute)
	before, after := base.Add(59*time.Second), base.Add(time.Minute)

	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		if _, _, err := local.Incr(ctx, limit, key, before, 1); err != nil {
			t.Error(err)
		}
	}()

	// The first call of the new bucket, while the one before it is still out.
	// Retried rather than slept on: what has to be true is that the flush is in
	// flight, and the recorder having been entered is how that is known.
	for rec.roundTrips() == 0 {
		runtime.Gosched()
	}
	if _, _, err := local.Incr(ctx, limit, key, after, 1); err != nil {
		t.Fatal(err)
	}

	close(rec.release)
	<-flushed

	total, _, err := local.Incr(ctx, limit, key, after.Add(time.Second), 1)
	if err != nil {
		t.Fatal(err)
	}
	if total > limit.Max {
		t.Fatalf("the new bucket counts %d against a limit of %d; the stale flush was written to it",
			total, limit.Max)
	}
}

func (h *held) roundTrips() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.calls
}

// A database that is refusing writes is exactly when the local count is the
// only count there is, so the increments must not be thrown away with the error.
func TestAFailedFlushKeepsTheCount(t *testing.T) {
	t.Parallel()

	c := newClock()
	rec := newTally()
	limiter := throttle.NewRecording(throttle.NewLocal(rec, throttle.LocalConfig{Interval: time.Millisecond})).WithClock(c.now)
	limit := throttle.Limit{Name: "api.account", Max: 4, Window: time.Minute}
	key := throttle.Account("acct-1")
	ctx := context.Background()

	rec.err = errors.New("connection refused")
	for range 6 {
		if _, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key}); err == nil {
			t.Fatal("the recorder was failing and Take did not say so")
		}
		c.advance(time.Millisecond)
	}

	// The database comes back. Everything that happened while it was away is
	// still counted, so the caller is over their limit rather than starting
	// fresh.
	rec.err = nil
	d, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatalf("7 calls against a limit of %d was allowed; the outage reset the count", limit.Max)
	}
}

func TestPastMaxKeysItDegradesRatherThanGrowing(t *testing.T) {
	t.Parallel()

	c := newClock()
	rec := newTally()
	limiter := throttle.NewRecording(throttle.NewLocal(rec, throttle.LocalConfig{
		Interval: time.Hour,
		MaxKeys:  4,
	})).WithClock(c.now)
	limit := throttle.Limit{Name: "api.ip", Max: 100, Window: time.Minute}

	ctx := context.Background()
	for i := range 50 {
		key := throttle.IP("198.51.100." + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		if _, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key}); err != nil {
			t.Fatal(err)
		}
	}

	// Every call past the cap goes straight through, which is the point: slow
	// and correct, rather than a map whose size is chosen by the caller.
	if got := rec.roundTrips(); got < 40 {
		t.Fatalf("only %d of 50 calls past a cap of 4 reached the recorder", got)
	}
}

// And what keeps the map under that cap: a state is dropped once its bucket is
// too old to be counted, which is two of its own windows and not two of the
// longest window any limit might have. The difference decides when the cap is
// reached, and past the cap every new caller pays a write per request.
func TestADeadKeyIsPrunedWithinTwoOfItsOwnWindows(t *testing.T) {
	t.Parallel()

	c := newClock()
	rec := newTally()
	limiter := throttle.NewRecording(throttle.NewLocal(rec, throttle.LocalConfig{
		Interval: time.Second,
		MaxKeys:  2,
	})).WithClock(c.now)
	limit := throttle.Limit{Name: "api.ip", Max: 1000, Window: time.Minute}
	ctx := context.Background()

	take := func(value string) {
		t.Helper()
		if _, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: throttle.IP(value)}); err != nil {
			t.Fatal(err)
		}
	}

	take("198.51.100.1")

	// Three windows on, the first caller's bucket cannot be counted by anything.
	// Its slot has to be free, or two live callers cannot both be held.
	c.advance(3 * time.Minute)
	take("198.51.100.2")
	take("198.51.100.3")

	settled := rec.roundTrips()
	for range 10 {
		take("198.51.100.3")
		c.advance(time.Millisecond)
	}

	if extra := rec.roundTrips() - settled; extra > 0 {
		t.Fatalf("ten calls from a held caller cost %d writes; the map is full of keys "+
			"nothing can count, so live callers are going straight to the recorder", extra)
	}
}

func TestConcurrentCallsAreAllCounted(t *testing.T) {
	t.Parallel()

	c := newClock()
	rec := newTally()
	local := throttle.NewLocal(rec, throttle.LocalConfig{Interval: 0})
	limit := throttle.Limit{Name: "api.account", Max: 1 << 30, Window: time.Minute}
	key := throttle.Account("acct-1")

	const goroutines, each = 8, 200

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if _, _, err := local.Incr(context.Background(), limit, key, c.at, 1); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Drain whatever is still held locally, then ask the recorder directly.
	total, _, err := local.Incr(context.Background(), limit, key, c.at, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := goroutines * each; total != want {
		t.Fatalf("counted %d of %d concurrent calls", total, want)
	}
}

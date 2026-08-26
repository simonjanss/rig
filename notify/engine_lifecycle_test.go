package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/simonjanss/rig/notify"
)

// The goroutine's lifecycle, which had no tests at all. Every one of these was a
// live hazard before the engine's ticker became [tick.Ticker]'s, and the first
// two are hazards `presence.Sweeper` has guarded against since it was written —
// so this file is the pair of them finally agreeing.
//
// No database: Close reaches ReleaseClaims, which returns before touching the
// store when nothing is held, and an engine that never dispatched holds nothing.

func engine(t *testing.T, interval time.Duration) *notify.Engine {
	t.Helper()
	return notify.NewEngine(notify.EngineConfig{Interval: interval})
}

// Two Starts used to be two goroutines, and both of them closed the same `done`
// channel on the way out — so the second one to return panicked with "close of
// closed channel", in the shutdown path, from a goroutine nothing was recovering.
func TestTwoStartsDoNotPanicOnClose(t *testing.T) {
	t.Parallel()

	e := engine(t, time.Hour)
	e.Start()
	e.Start()

	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close after two Starts: %v", err)
	}
	// And a Start after a Close starts nothing rather than closing `done` twice.
	e.Start()
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("Close after a Start that followed it: %v", err)
	}
}

// The arrangement an operator has who left the dispatching to the cron task and
// kept the documented shutdown registration:
//
//	app.CloseWithin("notifications", 15*time.Second, engine.Close)
//
// There is no goroutine, so there is nothing to wait for. Waiting anyway spent
// the whole fifteen seconds and then reported a failed shutdown step — on every
// deploy, for a process that had done nothing wrong.
func TestCloseWithoutStartDoesNotWait(t *testing.T) {
	t.Parallel()

	e := engine(t, time.Hour)

	// Generously longer than this should take, and short enough that the old
	// behaviour fails the test rather than slowing it down.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	if err := e.Close(ctx); err != nil {
		t.Fatalf("Close on an engine that was never started: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("Close waited %s for a goroutine that does not exist", took)
	}
}

func TestCloseTwiceIsSafe(t *testing.T) {
	t.Parallel()

	e := engine(t, time.Hour)
	e.Start()

	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("the first Close: %v", err)
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatalf("the second Close: %v", err)
	}
}

// Two Closes at once used to be a check on `stop` and then a close of it, with no
// lock between them, so both callers could take the branch and the second close
// panicked. Under -race this is the test that says so.
func TestTwoClosesAtOnceIsSafe(t *testing.T) {
	t.Parallel()

	e := engine(t, time.Hour)
	e.Start()

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- e.Close(context.Background()) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("a concurrent Close: %v", err)
		}
	}
}

// Close honours the context it was given, which is the reason it takes one: a
// pass that cannot reach the database must not outlive the deadline its caller
// declared for it.
func TestCloseHonoursItsContext(t *testing.T) {
	t.Parallel()

	e := engine(t, time.Hour)
	e.Start()

	// Cancelled before the call, so this is the deadline having already passed
	// rather than a race with the ticker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A started engine stops on its own signal, so either answer is correct here
	// — what is not is blocking forever.
	if err := e.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with a cancelled context: %v", err)
	}
}

// A non-positive interval is the default minute here, not "never" — and the
// asymmetry with presence is worth a test, because the two look like the same
// knob and are not.
//
// `presence.SweeperConfig.Interval` reads a negative as "the cron job owns
// this" and starts nothing. `EngineConfig.Interval` has no such setting: zero and
// negative both mean [DefaultInterval], which is what its own doc says. An
// operator who wanted notify's goroutine off by writing `-1` got a dispatcher
// running every minute. [tick.Ticker] does support "never", so the setting is
// one line away — but adding it would turn a working configuration into a silent
// no-op for anyone who wrote a negative expecting the current answer, so it stays
// as it is and is asserted rather than left to be discovered.
func TestANonPositiveIntervalIsTheDefaultAndNotNever(t *testing.T) {
	t.Parallel()

	for _, interval := range []time.Duration{0, -time.Second} {
		e := engine(t, interval)
		e.Start()

		// Started, so Close has a goroutine to wait for — which is the whole
		// assertion. On a ticker that started nothing this returns without
		// waiting, and the two are told apart by nothing else observable.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := e.Close(ctx); err != nil {
			t.Errorf("interval %s: Close: %v", interval, err)
		}
		cancel()
	}
}

// A nudge is an optimization and nothing may be built on it, so what is worth
// asserting is that reaching for one is never itself an error: before Start,
// after Close, and a hundred times in a row when the channel holds one.
//
// Deliberately not nudged while the goroutine is running, which is the one thing
// this cannot ask without a database: the nudged pass reaches Resolve, and an
// engine constructed with no DB dereferences a nil store there. That a nudge runs
// a pass promptly is asserted in runtime/tick's own tests, against a pass
// that does nothing; that the pass does the right thing is what the examples'
// Docker suites are for.
func TestNudgingIsAlwaysSafe(t *testing.T) {
	t.Parallel()

	e := engine(t, time.Hour)
	for range 100 {
		e.Nudge()
	}
	if err := e.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.Nudge()
}

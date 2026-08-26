package tick_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/tick"
)

// The four safety properties, one test each, because each of them is a bug
// somebody has had in a hand-rolled ticker rather than a hypothetical. The first
// three are `presence.Sweeper`'s own lifecycle tests, moved here — that is the
// point of the type existing, and `presence` keeps them as well, asserting the
// same properties through the wrapper.

func ticker(pass func(context.Context)) *tick.Ticker {
	return tick.New(tick.Config{Interval: time.Hour, Pass: pass})
}

func nothing(context.Context) {}

// The arrangement an operator has who left the work to a cron job and kept the
// shutdown registration: there is no goroutine, so there is nothing to wait for.
// Waiting anyway would hold shutdown open for the whole registered timeout and
// then report a failure — on every deploy.
func TestCloseWithoutStartDoesNotWait(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ticker(nothing).Close(ctx); err != nil {
		t.Fatalf("Close on a ticker that was never started: %v", err)
	}
}

// The other half, and the reason Close takes a context at all: a pass that cannot
// reach the database must not outlive the deadline its caller declared.
func TestCloseHonoursItsContext(t *testing.T) {
	t.Parallel()

	tk := ticker(nothing)
	tk.Start()

	// Cancelled before the call, so this is the deadline having already passed
	// rather than a race with the ticker.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A started ticker stops on its own signal, so either answer is correct here
	// — what is not is blocking forever, which is what this test fails on.
	if err := tk.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Close with a cancelled context: %v", err)
	}
	if err := tk.Close(context.Background()); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// Guards the pair above: two goroutines would both close `done` on the way out,
// which is a panic in a shutdown path.
func TestStartIsIdempotent(t *testing.T) {
	t.Parallel()

	tk := ticker(nothing)
	tk.Start()
	tk.Start()

	if err := tk.Close(context.Background()); err != nil {
		t.Fatalf("Close after two Starts: %v", err)
	}
	// A Start after a Close starts nothing rather than closing `done` twice.
	tk.Start()
	if err := tk.Close(context.Background()); err != nil {
		t.Fatalf("Close after a Start that followed it: %v", err)
	}
}

// The fourth property, which no hand-rolled version had: Close is a check on
// `closed` and then a close of a channel, so two callers can both take the
// branch. Under -race this is the test that says so.
func TestTwoClosesAtOnceIsSafe(t *testing.T) {
	t.Parallel()

	tk := ticker(nothing)
	tk.Start()

	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- tk.Close(context.Background()) }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Errorf("a concurrent Close: %v", err)
		}
	}
}

// A non-positive interval starts nothing and is not an error: it is how an
// operator says a cron job owns this. Then Close has nothing to wait for, which
// is the same path as never having started.
func TestANonPositiveIntervalStartsNothing(t *testing.T) {
	t.Parallel()

	for _, interval := range []time.Duration{0, -time.Second} {
		var passes atomic.Int64
		tk := tick.New(tick.Config{
			Interval: interval,
			Pass:     func(context.Context) { passes.Add(1) },
		})
		tk.Start()
		tk.Nudge() // Not even a nudge starts an unstarted ticker.

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		if err := tk.Close(ctx); err != nil {
			t.Errorf("interval %s: Close: %v", interval, err)
		}
		cancel()
		if n := passes.Load(); n != 0 {
			t.Errorf("interval %s ran %d passes", interval, n)
		}
	}
}

// A nil Pass is the same: there is nothing to run, so there is no goroutine.
func TestANilPassStartsNothing(t *testing.T) {
	t.Parallel()

	tk := tick.New(tick.Config{Interval: time.Hour})
	tk.Start()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tk.Close(ctx); err != nil {
		t.Fatalf("Close on a ticker with no Pass: %v", err)
	}
}

// A nudge runs a pass now rather than at the next tick, which is the whole reason
// it exists — the interval here is an hour, so a pass at all is the assertion.
func TestNudgeRunsAPassNow(t *testing.T) {
	t.Parallel()

	ran := make(chan struct{}, 1)
	tk := ticker(func(context.Context) {
		select {
		case ran <- struct{}{}:
		default:
		}
	})
	tk.Start()
	defer func() { _ = tk.Close(context.Background()) }()

	tk.Nudge()
	select {
	case <-ran:
	case <-time.After(5 * time.Second):
		t.Fatal("a nudge did not run a pass inside five seconds")
	}
}

// Nudges are dropped rather than queued, and a nudge on a ticker that has not
// started or has closed is not an error. Nothing may be built on a nudge, so the
// only thing to assert is that reaching for one is always safe.
func TestNudgingIsAlwaysSafe(t *testing.T) {
	t.Parallel()

	tk := ticker(nothing)
	for range 100 {
		tk.Nudge() // Before Start.
	}
	tk.Start()
	for range 100 {
		tk.Nudge()
	}
	if err := tk.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	tk.Nudge() // And after Close.
}

// PassTimeout bounds one pass when it is set.
func TestAPassGetsItsOwnTimeoutWhenOneIsSet(t *testing.T) {
	t.Parallel()

	deadlines := make(chan time.Duration, 1)
	tk := tick.New(tick.Config{
		Interval:    time.Hour,
		PassTimeout: 42 * time.Second,
		Pass: func(ctx context.Context) {
			deadline, ok := ctx.Deadline()
			if !ok {
				deadlines <- 0
				return
			}
			deadlines <- time.Until(deadline).Round(time.Second)
		},
	})
	tk.Start()
	defer func() { _ = tk.Close(context.Background()) }()

	tk.Nudge()
	select {
	case got := <-deadlines:
		if got != 42*time.Second {
			t.Errorf("the pass had %s left, want 42s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no pass ran")
	}
}

// And a zero PassTimeout is unbounded, which is the default and the one that
// matters: work bounding itself by a claim lease longer than the interval would
// be cut off mid-write — and cut off before the statement that gives the claims
// back — by a timeout imposed here.
func TestAZeroPassTimeoutIsUnbounded(t *testing.T) {
	t.Parallel()

	bounded := make(chan bool, 1)
	tk := ticker(func(ctx context.Context) {
		_, ok := ctx.Deadline()
		select {
		case bounded <- ok:
		default:
		}
	})
	tk.Start()
	defer func() { _ = tk.Close(context.Background()) }()

	tk.Nudge()
	select {
	case ok := <-bounded:
		if ok {
			t.Error("the pass was given a deadline nobody asked for")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no pass ran")
	}
}

// Close waits for the pass in flight rather than returning while a write is
// still going, which is why it is registered before the pool closes.
func TestCloseWaitsForThePassInFlight(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	var finished atomic.Bool
	tk := ticker(func(context.Context) {
		select {
		case started <- struct{}{}:
		default:
			return
		}
		time.Sleep(100 * time.Millisecond)
		finished.Store(true)
	})
	tk.Start()

	tk.Nudge()
	<-started

	if err := tk.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !finished.Load() {
		t.Error("Close returned while the pass was still running")
	}
}

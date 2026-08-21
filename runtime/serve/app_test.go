package serve

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// The last thing built is the first thing closed. Anything else tears a
// dependency down while something that needs it is still running.
func TestCloseRunsInReverse(t *testing.T) {
	var order []string
	app := &App{Logger: quiet()}

	app.Close("pool-of-something", func(context.Context) error {
		order = append(order, "first-built")
		return nil
	})
	app.Close("consumer", func(context.Context) error {
		order = append(order, "second-built")
		return nil
	})
	app.CloseFunc("exporter", func() error {
		order = append(order, "third-built")
		return nil
	})

	if err := app.runClose(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}

	want := []string{"third-built", "second-built", "first-built"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order = %v, want %v", order, want)
			break
		}
	}
}

// A shutdown cannot be abandoned halfway: the rest of the process still has to
// come down, and the failure still has to be reported.
func TestOneFailedCloseDoesNotStopTheRest(t *testing.T) {
	broken := errors.New("would not close")
	ran := false

	app := &App{Logger: quiet()}
	app.Close("stubborn", func(context.Context) error { return broken })
	app.Close("fine", func(context.Context) error { ran = true; return nil })

	err := app.runClose(t.Context(), time.Second)

	if !errors.Is(err, broken) {
		t.Errorf("err = %v, want the failure reported", err)
	}
	if !ran {
		t.Error("the remaining dependencies should still have been closed")
	}
}

// The teardown runs from a defer that also covers a failure during startup, so
// it has to be safe to reach twice.
func TestCloseHappensOnce(t *testing.T) {
	calls := 0
	app := &App{Logger: quiet()}
	app.Close("thing", func(context.Context) error { calls++; return nil })

	_ = app.runClose(t.Context(), time.Second)
	_ = app.runClose(t.Context(), time.Second)

	if calls != 1 {
		t.Errorf("closed %d times, want 1", calls)
	}
}

// Cancelling the server is what starts the shutdown, so a teardown derived from
// that context would have no time to do anything.
func TestTheTeardownOutlivesWhatCancelledIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var deadline time.Time
	app := &App{Logger: quiet()}
	app.Close("thing", func(c context.Context) error {
		if err := c.Err(); err != nil {
			t.Errorf("the closing context is already done: %v", err)
		}
		deadline, _ = c.Deadline()
		return nil
	})

	if err := app.runClose(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if deadline.IsZero() {
		t.Error("a teardown with no deadline can hang forever")
	}
}

// Drain is the other end: it runs before the server stops, in the order things
// were registered, and its failures are reported rather than swallowed.
func TestDrainRunsForward(t *testing.T) {
	var order []string
	failed := errors.New("no")

	app := &App{Logger: quiet()}
	app.Drain("consumer", func(context.Context) error {
		order = append(order, "consumer")
		return failed
	})
	app.Drain("scheduler", func(context.Context) error {
		order = append(order, "scheduler")
		return nil
	})

	err := app.runDrain(t.Context())

	if !errors.Is(err, failed) {
		t.Errorf("err = %v", err)
	}
	if len(order) != 2 || order[0] != "consumer" || order[1] != "scheduler" {
		t.Errorf("order = %v, want [consumer scheduler]", order)
	}
}

func TestNilStepsAreIgnored(t *testing.T) {
	app := &App{Logger: quiet()}
	app.Close("nothing", nil)
	app.CloseFunc("nothing", nil)
	app.Drain("nothing", nil)

	if err := app.runClose(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := app.runDrain(t.Context()); err != nil {
		t.Fatal(err)
	}
}

// A step that ignores the context it was handed must not hold the process open,
// and must not take the steps after it down with it.
func TestAStepThatWillNotStopIsAbandoned(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	after := false
	app := &App{Logger: quiet()}

	// Registered first, so it runs last: the point is that it still runs.
	app.Close("after", func(context.Context) error { after = true; return nil })
	app.CloseWithin("stuck", 50*time.Millisecond, func(context.Context) error {
		<-release // deliberately deaf to the context
		return nil
	})

	start := time.Now()
	err := app.runClose(t.Context(), time.Minute)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("waited %s on a step that never returns", elapsed)
	}
	if !after {
		t.Error("one stuck step should not stop the rest from closing")
	}
}

// Its own limit is on top of the sequence budget, not instead of it: whichever
// runs out first wins.
func TestTheSequenceBudgetStillApplies(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })

	app := &App{Logger: quiet()}
	// An hour of its own, inside a sequence with 50ms left.
	app.CloseWithin("slow", time.Hour, func(context.Context) error {
		<-release
		return nil
	})

	start := time.Now()
	err := app.runClose(t.Context(), 50*time.Millisecond)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want a deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %s: the sequence budget should have ended it", elapsed)
	}
}

// A step that finishes inside its limit is not disturbed by having one.
func TestALimitDoesNotHurryAStepThatFinishes(t *testing.T) {
	app := &App{Logger: quiet()}

	var deadline time.Time
	app.CloseWithin("quick", 30*time.Second, func(c context.Context) error {
		deadline, _ = c.Deadline()
		return nil
	})

	if err := app.runClose(t.Context(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if time.Until(deadline) > 31*time.Second {
		t.Errorf("the step's own limit should have applied, got %s", time.Until(deadline))
	}
}

// The parts have to fit inside the stated whole, and the answer is wanted at
// startup — during a shutdown it is too late to change anything.
func TestTheShutdownPartsMustFitTheMaximum(t *testing.T) {
	app := &App{Logger: quiet()}
	app.DrainWithin("consumer", 2*time.Second, noop)
	app.CloseWithin("exporter", 20*time.Second, noop)

	err := app.checkShutdown(t.Context(), 20*time.Second, 5*time.Second)
	if err == nil {
		t.Fatal("27s of steps inside a 20s maximum should not start")
	}

	// The message has to be actionable: the total, the maximum, and which step
	// asked for what.
	for _, want := range []string{"27s", "20s", "drain delay 5s", "drain consumer 2s", "close exporter 20s"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
}

func TestAShutdownThatFitsIsAccepted(t *testing.T) {
	app := &App{Logger: quiet()}
	app.CloseWithin("exporter", 5*time.Second, noop)
	app.Close("cache", noop) // no limit of its own: takes what is left

	if err := app.checkShutdown(t.Context(), 20*time.Second, 2*time.Second); err != nil {
		t.Errorf("7s of declared steps inside 20s should be fine: %v", err)
	}
}

// A step with no limit of its own asks for nothing, so it cannot make the sum
// overflow — it takes whatever the declared ones leave.
func TestUndeclaredStepsDoNotCountAgainstTheMaximum(t *testing.T) {
	app := &App{Logger: quiet()}
	for range 50 {
		app.Close("whatever", noop)
	}

	if err := app.checkShutdown(t.Context(), time.Second, 0); err != nil {
		t.Errorf("steps without their own limits should not need budgeting: %v", err)
	}
}

func noop(context.Context) error { return nil }

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

// And a whole nobody stated is refused the same way, with the same parts, at the
// same moment. This is the number that goes into terminationGracePeriodSeconds,
// and the alternative to reading it off this refusal is adding up constants
// across three files by hand.
func TestAnUnstatedMaximumIsRefusedWithTheNumberToState(t *testing.T) {
	app := &App{Logger: quiet()}
	app.DrainWithin("consumer", 2*time.Second, noop)
	app.CloseWithin("exporter", 5*time.Second, noop)

	err := app.checkShutdown(t.Context(), 0, 3*time.Second)
	if err == nil {
		t.Fatal("a shutdown with no stated maximum should not start")
	}

	for _, want := range []string{"MaxShutdown", "10s", "drain delay 3s", "drain consumer 2s", "close exporter 5s", "terminationGracePeriodSeconds"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should mention %q: %v", want, err)
		}
	}
}

// Including a project that registered nothing of its own. What the budget buys
// there is entirely the requests in flight, and how long those are worth waiting
// for is not this package's guess to make.
func TestAnUnstatedMaximumIsRefusedEvenWithNothingRegistered(t *testing.T) {
	app := &App{Logger: quiet()}

	err := app.checkShutdown(t.Context(), 0, 0)
	if err == nil {
		t.Fatal("a shutdown with no stated maximum should not start")
	}
	if !strings.Contains(err.Error(), "MaxShutdown") {
		t.Errorf("the error should name the field: %v", err)
	}
	// The parts list is empty here, and an empty one must not be printed as an
	// empty parenthesis with nothing in it.
	if strings.Contains(err.Error(), "()") {
		t.Errorf("the message should not print an empty list of parts: %v", err)
	}
}

// The refusal above is reached with the teardown already deferred, so the steps
// the mount function registered get closed under a budget that was never
// stated. They have to get time rather than an expired context: otherwise the
// one message that matters arrives joined to a paragraph of "gave up waiting"
// from steps that were never the problem.
func TestAZeroBudgetGivesTheStepsTimeRatherThanNone(t *testing.T) {
	app := &App{Logger: quiet()}

	ran := false
	app.CloseWithin("exporter", 5*time.Second, func(ctx context.Context) error {
		ran = true
		return ctx.Err()
	})

	if err := app.runClose(t.Context(), 0); err != nil {
		t.Errorf("a step under an unstated budget should still get its own time: %v", err)
	}
	if !ran {
		t.Error("the step never ran")
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

// The leftover checkShutdown reasons about is real time, not a description.
//
// serveUntil is what makes it so: a handler that will not return spends what is
// left after the close steps have been set aside, and not a second more. Without
// it the steps that run after the server each meet a deadline that has already
// passed, and the worst of them is a write.
func TestTheCloseStepsAreReservedOutOfTheBudget(t *testing.T) {
	app := &App{Logger: quiet()}
	app.CloseWithin("engine", 15*time.Second, func(context.Context) error { return nil })
	app.CloseWithin("sweeper", 5*time.Second, func(context.Context) error { return nil })
	app.Close("no limit of its own", func(context.Context) error { return nil })

	if got := app.reserved(); got != 20*time.Second {
		t.Errorf("reserved = %s, want 20s: a step with no limit of its own reserves nothing", got)
	}

	// A budget of 35s, of which 20 belongs to the steps above.
	app.until = time.Now().Add(35 * time.Second)
	if left := time.Until(serveUntil(app)); left < 14*time.Second || left > 15*time.Second {
		t.Errorf("the requests in flight got %s, want about 15s", left)
	}
}

// A budget the steps have entirely spoken for is a warning checkShutdown has
// already given, not a reason to hand http.Server a deadline in the past and
// find out what it does with one.
func TestAFullySpokenForBudgetStillStopsTheServerNow(t *testing.T) {
	app := &App{Logger: quiet()}
	app.CloseWithin("everything", 10*time.Second, func(context.Context) error { return nil })
	app.until = time.Now().Add(5 * time.Second)

	if got := serveUntil(app); got.Before(time.Now().Add(-time.Second)) {
		t.Errorf("serveUntil = %s, want no earlier than now", got)
	}
}

// A close step gets the whole of its own timeout even when the server before it
// used every second it was allowed.
func TestAHandlerThatWillNotReturnDoesNotStarveTheTeardown(t *testing.T) {
	got := make(chan time.Duration, 1)

	app := &App{Logger: quiet()}
	app.CloseWithin("flush", 200*time.Millisecond, func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			got <- 0
			return nil
		}
		got <- time.Until(deadline)
		return nil
	})

	// The whole budget, of which the step above reserved 200ms. Pretend the
	// server spent the rest.
	app.until = time.Now().Add(200 * time.Millisecond)

	if err := app.runClose(t.Context(), 300*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if left := <-got; left < 150*time.Millisecond {
		t.Errorf("the flush was given %s of its 200ms", left)
	}
}

// A dependency the mount function registered fails readiness by name, so the
// 503 body says which one rather than only that something is wrong.
func TestAReadyCheckIsNamedWhenItFails(t *testing.T) {
	app := &App{Logger: quiet()}
	app.Ready("sync service", func(context.Context) error {
		return errors.New("electric: the sync service answered 503")
	})

	err := runChecks(t.Context(), app)
	if err == nil {
		t.Fatal("runChecks = nil, want the check's error")
	}
	if !strings.Contains(err.Error(), "sync service") {
		t.Errorf("error = %q, want the dependency named", err)
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %q, want the cause kept", err)
	}
}

// Nil is nothing to check rather than something to blow up on, the same answer
// Drain and Close give a nil function.
func TestANilReadyCheckIsNotRegistered(t *testing.T) {
	app := &App{Logger: quiet()}
	app.Ready("nothing", nil)

	if got := len(app.readyChecks()); got != 0 {
		t.Errorf("registered %d checks, want 0", got)
	}
	if err := runChecks(t.Context(), app); err != nil {
		t.Errorf("runChecks = %v, want nil", err)
	}
}

// A passing readiness check says what it checked. "ready" alone cannot tell an
// instance that asked its dependencies from one that has none registered, and
// that is the difference somebody reading this endpoint came to find out.
func TestTheReadyBodyNamesWhatWasChecked(t *testing.T) {
	app := &App{Logger: quiet()}
	app.Ready("sync service", func(context.Context) error { return nil })

	body := readyBody(app)
	for _, want := range []string{"ready", "database", "sync service"} {
		if !strings.Contains(body, want) {
			t.Errorf("body = %q, want %q in it", body, want)
		}
	}

	// And with nothing registered it still names the one dependency serve
	// always checks itself.
	if body := readyBody(&App{}); !strings.Contains(body, "database") {
		t.Errorf("body = %q, want the pool ping named", body)
	}
}

// steps is a [Shutdown] written by hand, which is what a hand-written main has
// and what these tests use in place of the generated struct.
type steps []Step

func (s steps) Steps() []Step { return s }

// A number in Config.Shutdown replaces the one the step was registered with.
//
// The registration is generated code — `api.StartPresenceSweeper` and the four
// beside it — so this is the only place a deployment can disagree with it, and
// the disagreement has to reach the step rather than only the arithmetic: what
// it buys is a flush that gives up sooner, not a smaller number in a log line.
func TestConfigShutdownResizesTheStepItNames(t *testing.T) {
	app := &App{Logger: quiet()}
	app.limit(steps{{Name: "presence", Timeout: 2 * time.Second}})

	app.CloseWithin("presence", 5*time.Second, func(context.Context) error { return nil })
	app.DrainWithin("shapes", 5*time.Second, func(context.Context) error { return nil })

	if got := app.stop[0].Timeout; got != 2*time.Second {
		t.Errorf("the presence step got %s, want the 2s the config asked for", got)
	}
	if got := app.drain[0].Timeout; got != 5*time.Second {
		t.Errorf("a step nothing sized got %s, want the 5s it was registered with", got)
	}

	// And the arithmetic follows, because it is read off the same steps: a
	// deployment that shortens a step is one whose budget has room it did not
	// have before.
	if total, _ := app.declared(0); total != 7*time.Second {
		t.Errorf("the declared total is %s, want 7s — the resized step and the untouched one", total)
	}
}

// A zero is "I did not say", not "no limit at all".
//
// It is the whole reason the generated set is a struct: most of its fields are
// zero most of the time, and a zero that reached the step would turn every
// step the deployment did not mention into one bounded only by what is left of
// the budget — which is the opposite of what sizing one asks for.
func TestAZeroInConfigShutdownLeavesTheStepAlone(t *testing.T) {
	app := &App{Logger: quiet()}
	app.limit(steps{{Name: "traces"}, {Name: "auth", Timeout: 0}})

	app.CloseWithin("traces", 5*time.Second, func(context.Context) error { return nil })

	if got := app.stop[0].Timeout; got != 5*time.Second {
		t.Errorf("a step sized with a zero got %s, want the 5s it was registered with", got)
	}
}

// A step nobody registers is refused before the server listens.
//
// The same failure the configuration blocks are refused for — a number
// somebody set and believed in, read by nothing — and worth more here than
// elsewhere, because what is believed is about a shutdown. Nobody watches one
// until the day it drops requests, and by then the evidence is a killed
// process rather than a value that did nothing.
func TestConfigShutdownIsRefusedWhenNothingRegistersTheStep(t *testing.T) {
	app := &App{Logger: quiet()}
	app.limit(steps{{Name: "notifications", Timeout: 10 * time.Second}})
	app.CloseWithin("presence", 5*time.Second, func(context.Context) error { return nil })

	err := app.checkShutdown(context.Background(), time.Minute, 0)
	if err == nil {
		t.Fatal("a shutdown step nothing registers was accepted")
	}
	for _, want := range []string{"notifications", "presence"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// And one that does reach a step is not refused, however many were left out.
func TestConfigShutdownIsAcceptedWhenTheStepIsThere(t *testing.T) {
	app := &App{Logger: quiet()}
	app.limit(steps{{Name: "presence", Timeout: 2 * time.Second}})
	app.CloseWithin("presence", 5*time.Second, func(context.Context) error { return nil })
	app.DrainWithin("shapes", 5*time.Second, func(context.Context) error { return nil })

	if err := app.checkShutdown(context.Background(), time.Minute, 0); err != nil {
		t.Errorf("a shutdown that sizes a step this server has was refused: %v", err)
	}
}

// The unbounded half of a two-step name stays unbounded.
//
// `api.StartNotificationEngine` registers both halves of the engine under
// "notifications": a drain that stops it claiming, unbounded on purpose, and a
// close that finishes what it claimed within fifteen seconds. A number that
// reached both would be counted twice by [App.declared] while the generated
// Shutdown.Budget counts it once — so shortening the engine would grow the
// total its own budget has to hold, and a set built with Budget would be
// refused by the check that reads it.
func TestConfigShutdownLeavesTheUnboundedHalfOfAStepAlone(t *testing.T) {
	app := &App{Logger: quiet()}
	app.limit(steps{{Name: "notifications", Timeout: 10 * time.Second}})

	app.Drain("notifications", func(context.Context) error { return nil })
	app.CloseWithin("notifications", 15*time.Second, func(context.Context) error { return nil })
	app.DrainWithin("shapes", 5*time.Second, func(context.Context) error { return nil })

	if got := app.drain[0].Timeout; got != 0 {
		t.Errorf("the engine's drain got %s, want the no limit it was registered with", got)
	}
	if got := app.stop[0].Timeout; got != 10*time.Second {
		t.Errorf("the engine's close got %s, want the 10s the config asked for", got)
	}

	// Ten for the engine and five for the subscriptions, and the drain that
	// declared nothing still declares nothing. The same numbers the generated
	// Shutdown.Budget adds the headroom to.
	if total, _ := app.declared(0); total != 15*time.Second {
		t.Errorf("the declared total is %s, want 15s — the engine counted once", total)
	}
}

// And a name that only ever reached the unbounded form is refused, because
// that number went nowhere.
func TestConfigShutdownIsRefusedWhenTheStepItNamesHasNoLimit(t *testing.T) {
	app := &App{Logger: quiet()}
	app.limit(steps{{Name: "store", Timeout: 2 * time.Second}})
	app.Close("store", func(context.Context) error { return nil })
	app.CloseWithin("presence", 5*time.Second, func(context.Context) error { return nil })

	err := app.checkShutdown(context.Background(), time.Minute, 0)
	if err == nil {
		t.Fatal("a shutdown step with no limit to replace was sized and accepted")
	}
	for _, want := range []string{"store", "presence"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

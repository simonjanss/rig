package notify

import (
	"strings"
	"testing"
	"time"
)

// An internal test, because the schedule is the thing worth asserting on and
// nextAttemptAt is where it lives. Reaching it through Dispatch would need a
// database to observe a deliver_at that this can read directly.

var epoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// floor and ceiling are the two ends of the spread. A real jitter returns
// something in [0, n), so pinning it to each end is how a range becomes an
// assertion — which is the whole reason EngineConfig.Jitter is a field.
func floor(int64) int64     { return 0 }
func ceiling(n int64) int64 { return n - 1 }

func engineFor(t *testing.T, jitter func(int64) int64, base, ceil time.Duration) *Engine {
	t.Helper()
	return NewEngine(EngineConfig{
		Config:      Config{Now: func() time.Time { return epoch }},
		BackoffBase: base,
		BackoffCap:  ceil,
		Jitter:      jitter,
	})
}

// The schedule the defaults add up to, asserted at its floor so the doubling and
// the ceiling are both visible as exact numbers. This is the table that says
// "about eight hours", and it is here so that changing one of the three
// constants cannot quietly change the total.
func TestTheDefaultScheduleDoublesThenFlattens(t *testing.T) {
	t.Parallel()

	e := engineFor(t, floor, DefaultBackoffBase, DefaultBackoffCap)

	want := []time.Duration{
		time.Minute,      // attempt 1 failed
		2 * time.Minute,  // 2
		4 * time.Minute,  // 3
		8 * time.Minute,  // 4
		16 * time.Minute, // 5 — where the old five-attempt schedule stopped
		32 * time.Minute, // 6
		time.Hour,        // 7, capped from 64m
		time.Hour,        // 8
	}
	var total time.Duration
	for i, w := range want {
		attempts := i + 1
		got := e.nextAttemptAt(attempts, 0).Sub(epoch)
		if got != w {
			t.Errorf("attempt %d waits %s, want %s", attempts, got, w)
		}
		total += w
	}
	// And the tail, so the eight hours is checked rather than asserted in prose.
	// It stops one short of max_attempts because the last failure is a failure
	// rather than a wait: mark reaches the cap and writes Failed without asking
	// when to come back.
	for attempts := len(want) + 1; attempts < DefaultMaxAttempts; attempts++ {
		got := e.nextAttemptAt(attempts, 0).Sub(epoch)
		if got != DefaultBackoffCap {
			t.Errorf("attempt %d waits %s, want the cap %s", attempts, got, DefaultBackoffCap)
		}
		total += got
	}
	// Thirteen waits for fourteen attempts. Eight hours and three minutes, and
	// the point of the number is that it is measured in hours where the old
	// five-attempt schedule was measured in minutes.
	if want := 8*time.Hour + 3*time.Minute; total != want {
		t.Errorf("the schedule spans %s, want %s", total, want)
	}
}

// The spread only ever lengthens a wait. That is the departure from rigclient,
// which subtracts, and it is what keeps backoff_base a floor and the table above
// exact rather than average.
func TestTheSpreadNeverShortensAWait(t *testing.T) {
	t.Parallel()

	low := engineFor(t, floor, time.Minute, time.Hour)
	high := engineFor(t, ceiling, time.Minute, time.Hour)

	for attempts := 1; attempts <= DefaultMaxAttempts; attempts++ {
		nominal := low.nextAttemptAt(attempts, 0).Sub(epoch)
		spread := high.nextAttemptAt(attempts, 0).Sub(epoch)

		if spread < nominal {
			t.Errorf("attempt %d: the spread shortened %s to %s", attempts, nominal, spread)
		}
		// Half the wait and no more, so a schedule cannot drift into the next
		// order of magnitude.
		if max := nominal + nominal/2; spread > max {
			t.Errorf("attempt %d: the spread took %s past %s", attempts, spread, max)
		}
	}
}

// A Retry-After is a boundary, and adding rather than subtracting is what keeps
// it one: the wait is never shorter than the provider asked for. It is spread all
// the same, because a hundred rows carrying one provider's boundary cannot all
// return at that boundary without rebuilding the herd it was asked to prevent.
func TestARetryAfterIsHonouredAsAFloor(t *testing.T) {
	t.Parallel()

	const asked = 10 * time.Minute
	for _, tc := range []struct {
		name   string
		jitter func(int64) int64
	}{{"at the floor", floor}, {"at the ceiling", ceiling}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// A base and cap that would both give a different answer, so what is
			// being read is the request and not the schedule.
			got := engineFor(t, tc.jitter, time.Second, 2*time.Second).
				nextAttemptAt(9, asked).Sub(epoch)
			if got < asked {
				t.Errorf("came back after %s, which is before the %s asked for", got, asked)
			}
			if max := asked + asked/2; got > max {
				t.Errorf("came back after %s, past the %s ceiling", got, max)
			}
		})
	}
}

// A request replaces this attempt's wait and does not move where the doubling had
// got to: a provider asking for ten minutes once is not the same as the outage
// having lasted ten minutes.
func TestARetryAfterDoesNotResetTheSchedule(t *testing.T) {
	t.Parallel()

	e := engineFor(t, floor, time.Minute, time.Hour)
	if got := e.nextAttemptAt(4, 30*time.Second).Sub(epoch); got != 30*time.Second {
		t.Fatalf("the request was not honoured: %s", got)
	}
	// The next failure is still the fifth attempt's wait, not the second's.
	if got, want := e.nextAttemptAt(5, 0).Sub(epoch), 16*time.Minute; got != want {
		t.Errorf("after a request, attempt 5 waits %s, want %s", got, want)
	}
}

// A cap below the base is a schedule that reads as exponential and behaves as
// fixed, so it is refused where somebody can still change one of the two numbers
// — the same argument as the claim_ttl and send_timeout refusals beside it.
func TestACapBelowTheBaseIsRefused(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("a backoff_cap below backoff_base was accepted")
		}
		msg, _ := r.(string)
		for _, want := range []string{"backoff_cap", "backoff_base", "30s", "1m"} {
			if !strings.Contains(msg, want) {
				t.Errorf("the panic does not mention %q: %s", want, msg)
			}
		}
	}()

	NewEngine(EngineConfig{BackoffBase: time.Minute, BackoffCap: 30 * time.Second})
}

// The defaults have to satisfy every refusal above. A default set this package
// panics on is a panic on a configuration nobody wrote, which is the worst place
// for one of these checks to be wrong.
func TestTheDefaultsAreAcceptedByTheirOwnChecks(t *testing.T) {
	t.Parallel()

	e := NewEngine(EngineConfig{})
	if e.backoffCap != DefaultBackoffCap {
		t.Errorf("backoffCap defaulted to %s, want %s", e.backoffCap, DefaultBackoffCap)
	}
	if e.maxAttempts != DefaultMaxAttempts {
		t.Errorf("maxAttempts defaulted to %d, want %d", e.maxAttempts, DefaultMaxAttempts)
	}
	// And the default jitter is installed, because a nil one is a panic on the
	// first failed send rather than at construction.
	if e.jitter == nil {
		t.Error("no jitter was installed, so the first retry would panic")
	}
}

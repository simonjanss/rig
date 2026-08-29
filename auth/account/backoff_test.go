package account

import (
	"testing"
	"time"
)

// An internal test, because the schedule is the thing worth asserting on and
// nextMailAttemptAt is where it lives. Reaching it through DispatchMail would
// observe a deliver_at, which is a database's answer to a question this can ask
// directly.
//
// notify's backoff_test.go asserts the same four properties of the same
// arithmetic. Until now this half had none: nextMailAttemptAt was reached only
// through TestAFailedSendLeavesTheRowForTheNextPass, which checks that a retry
// was scheduled and not when.

var mailEpoch = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// floor and ceiling are the two ends of the spread. A real jitter returns
// something in [0, n), so pinning it to each end is how a range becomes an
// assertion — which is the whole reason MailOptions.Jitter is a field.
func floor(int64) int64     { return 0 }
func ceiling(n int64) int64 { return n - 1 }

// Constructed directly rather than through New, which would want a Store, a
// session manager and an identity store to reach two fields. resolveMail's
// refusals are asserted next door, in dispatch_test.go.
func mailerFor(jitter func(int64) int64, base, ceil time.Duration) *Service {
	return &Service{
		now: func() time.Time { return mailEpoch },
		mail: MailOptions{
			MaxAttempts: DefaultMailMaxAttempts,
			BackoffBase: base,
			BackoffCap:  ceil,
			Jitter:      jitter,
		},
	}
}

// The schedule the defaults add up to, asserted at its floor so the doubling and
// the ceiling are both visible as exact numbers. This is the table that says
// "about eight hours", and it is here so that changing one of the three constants
// cannot quietly change the total — in either module, since the two sets of
// defaults are deliberately the same numbers.
func TestTheDefaultMailScheduleDoublesThenFlattens(t *testing.T) {
	t.Parallel()

	s := mailerFor(floor, DefaultMailBackoffBase, DefaultMailBackoffCap)

	want := []time.Duration{
		time.Minute,      // attempt 1 failed
		2 * time.Minute,  // 2
		4 * time.Minute,  // 3
		8 * time.Minute,  // 4
		16 * time.Minute, // 5
		32 * time.Minute, // 6
		time.Hour,        // 7, capped from 64m
		time.Hour,        // 8
	}
	var total time.Duration
	for i, w := range want {
		attempts := i + 1
		got := s.nextMailAttemptAt(attempts, 0).Sub(mailEpoch)
		if got != w {
			t.Errorf("attempt %d waits %s, want %s", attempts, got, w)
		}
		total += w
	}
	// And the tail, so the eight hours is checked rather than asserted in prose.
	// It stops one short of MaxAttempts because the last failure is a failure
	// rather than a wait: markMail reaches the cap and writes Failed without
	// asking when to come back.
	for attempts := len(want) + 1; attempts < DefaultMailMaxAttempts; attempts++ {
		got := s.nextMailAttemptAt(attempts, 0).Sub(mailEpoch)
		if got != DefaultMailBackoffCap {
			t.Errorf("attempt %d waits %s, want the cap %s",
				attempts, got, DefaultMailBackoffCap)
		}
		total += got
	}
	if want := 8*time.Hour + 3*time.Minute; total != want {
		t.Errorf("the schedule spans %s, want %s", total, want)
	}
}

// The spread only ever lengthens a wait. Nobody is blocked on a queued mail, so
// the nominal schedule stays a floor and BackoffBase keeps meaning what it says.
func TestTheMailSpreadNeverShortensAWait(t *testing.T) {
	t.Parallel()

	low := mailerFor(floor, time.Minute, time.Hour)
	high := mailerFor(ceiling, time.Minute, time.Hour)

	for attempts := 1; attempts <= DefaultMailMaxAttempts; attempts++ {
		nominal := low.nextMailAttemptAt(attempts, 0).Sub(mailEpoch)
		spread := high.nextMailAttemptAt(attempts, 0).Sub(mailEpoch)

		if spread < nominal {
			t.Errorf("attempt %d: the spread shortened %s to %s", attempts, nominal, spread)
		}
		// Half the wait and no more, so a schedule cannot drift into the next
		// order of magnitude.
		if most := nominal + nominal/2; spread > most {
			t.Errorf("attempt %d: the spread took %s past %s", attempts, spread, most)
		}
	}
}

// A Retry-After is a boundary, and adding rather than subtracting is what keeps
// it one: the wait is never shorter than the provider asked for. It is spread all
// the same, because a hundred rows carrying one provider's boundary cannot all
// return at that boundary without rebuilding the herd it was asked to prevent.
func TestAMailRetryAfterIsHonouredAsAFloor(t *testing.T) {
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
			got := mailerFor(tc.jitter, time.Second, 2*time.Second).
				nextMailAttemptAt(9, asked).Sub(mailEpoch)
			if got < asked {
				t.Errorf("came back after %s, which is before the %s asked for", got, asked)
			}
			if most := asked + asked/2; got > most {
				t.Errorf("came back after %s, past the %s ceiling", got, most)
			}
		})
	}
}

// A request replaces this attempt's wait and does not move where the doubling had
// got to: a provider asking for ten minutes once is not the same as the outage
// having lasted ten minutes.
func TestAMailRetryAfterDoesNotResetTheSchedule(t *testing.T) {
	t.Parallel()

	s := mailerFor(floor, time.Minute, time.Hour)
	if got := s.nextMailAttemptAt(4, 30*time.Second).Sub(mailEpoch); got != 30*time.Second {
		t.Fatalf("the request was not honoured: %s", got)
	}
	// The next failure is still the fifth attempt's wait, not the second's.
	if got, want := s.nextMailAttemptAt(5, 0).Sub(mailEpoch), 16*time.Minute; got != want {
		t.Errorf("after a request, attempt 5 waits %s, want %s", got, want)
	}
}

// The two queues' defaults are the same numbers, and the doc comments on both
// sets say so. Asserted rather than trusted, because the pairing is the whole
// reason an operator can tune one dispatcher and not have to learn a second set
// of arithmetic for the other — and nothing else would notice the two drifting
// apart. The comparison against notify's own constants cannot live here, since
// rig/auth does not import rig/notify; this is the half that can.
func TestTheMailDefaultsAreTheEightHourSchedule(t *testing.T) {
	t.Parallel()

	if DefaultMailBackoffBase != time.Minute {
		t.Errorf("DefaultMailBackoffBase = %s, want 1m", DefaultMailBackoffBase)
	}
	if DefaultMailBackoffCap != time.Hour {
		t.Errorf("DefaultMailBackoffCap = %s, want 1h", DefaultMailBackoffCap)
	}
	if DefaultMailMaxAttempts != 14 {
		t.Errorf("DefaultMailMaxAttempts = %d, want 14", DefaultMailMaxAttempts)
	}
	if DefaultMailClaimTTL != 5*time.Minute {
		t.Errorf("DefaultMailClaimTTL = %s, want 5m", DefaultMailClaimTTL)
	}
	if MinMailClaimTTL != time.Minute {
		t.Errorf("MinMailClaimTTL = %s, want 1m", MinMailClaimTTL)
	}
	if DefaultMailSendTimeout != 30*time.Second {
		t.Errorf("DefaultMailSendTimeout = %s, want 30s", DefaultMailSendTimeout)
	}
}

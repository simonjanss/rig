package outbox

import (
	"context"
	"time"
)

// Backoff is the retry ladder: a floor, a ceiling, and the spread that keeps a
// hundred rows from meeting one provider at the same instant.
//
// A value rather than three parameters, because the three are one decision and a
// call site that passed two of them is a call site somebody could pass one of
// them to. Both durations must already be resolved — a zero Base schedules every
// retry for now — and the invariant that Cap is not below Base is the caller's to
// enforce, where it can still name which of the two numbers to change.
type Backoff struct {
	// Base is the first wait, and what every subsequent one doubles from.
	Base time.Duration
	// Cap bounds one wait, so a long schedule's tail is a series of knocks rather
	// than a sleep nobody will be awake for.
	Cap time.Duration
	// Jitter is where the spread comes from: given n, something in [0, n).
	//
	// It exists to be replaced in a test and for nothing else. A nil one panics
	// on the first failed send, which is a constructor that forgot to resolve it
	// — and a silent zero spread would be the herd this exists to prevent.
	Jitter func(int64) int64
}

// Next is when to try again: the doubling, capped, spread.
//
// The spread is added on top of the wait rather than taken out of it, so the
// nominal schedule stays a floor and Base keeps meaning what it says. A
// non-positive asked means the ladder; a positive one replaces this attempt's wait
// without moving where the doubling had got to, so a provider that asks for ten
// minutes once does not reset the schedule.
func (b Backoff) Next(now time.Time, attempts int, asked time.Duration) time.Time {
	wait := asked
	if wait <= 0 {
		wait = b.Base
		for range attempts - 1 {
			// Tested before doubling rather than clamped after, so a long
			// schedule cannot overflow past the ceiling on its way to being
			// clamped back under it.
			if wait >= b.Cap {
				break
			}
			wait *= 2
		}
		wait = min(wait, b.Cap)
	}
	// The +1 makes the bound positive for every wait, including one short enough
	// that half of it truncates to zero.
	return now.Add(wait + time.Duration(b.Jitter(int64(wait/2)+1)))
}

// RoomFor reports whether pass has room for one whole send.
//
// The question is whether the *next* send fits, not whether the budget has
// already run out: a send started with a millisecond left runs to the pass
// deadline and no further, which is a call still in flight as the lease expires.
// So a pass ends with the send timeout unspent, and that is the point.
func RoomFor(pass context.Context, send time.Duration) bool {
	if pass.Err() != nil {
		return false
	}
	deadline, ok := pass.Deadline()
	return !ok || time.Until(deadline) >= send
}

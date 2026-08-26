// Package outbox is the retry vocabulary and the retry arithmetic that rig's two
// queues share.
//
// Two of them, and only ever two: notify's delivery table and auth's mail
// outbox. Both had their own copy of everything here, and both said so in
// comments — "this is notify.Permanent's twin", "notify's nextAttemptAt, to the
// line". This is those comments becoming a compiler-checked fact.
//
// # What is here, and what is deliberately not
//
// Here: what a provider's answer means, when to come back, whether a pass has
// room for another send, and which leases this process is holding. Every one of
// those had no room to differ, and the two copies did not differ.
//
// Not here: the claim-send-mark pass itself. notify groups a digest and marks N
// deliveries per message; auth reads three stores and rotates a token per row;
// and their release statements are scoped differently — notify releases the ids
// it tracked, auth releases everything it ever claimed. A shared pass would be
// one shape with two bodies and a parameter list longer than either, which is
// worse than two passes that each read straight through.
//
// It is also why this can exist at all: a leaf over uuid, in `runtime`, which
// both service modules already depend on and neither depends on the other.
package outbox

import (
	"errors"
	"time"
)

// Permanent wraps an error a retry cannot fix, so the row is marked Failed on
// this attempt rather than spending the rest of its budget.
//
// The case it is for is a provider refusing the recipient rather than refusing
// the request: an address that does not exist, a device token the vendor has
// revoked, a message the provider will never accept. Fourteen attempts over eight
// hours at any of those is the same answer fourteen times.
//
// Use it only when the provider's answer is about the recipient. A 500, a
// timeout, a connection refused and a 429 are all about the provider, and all of
// them are what the ordinary schedule is for — wrapping one of those here turns a
// provider's bad ten minutes into permanently undelivered mail.
//
// Permanent(nil) is nil. That is not a courtesy: a pass decides a send succeeded
// by testing the error against nil, so a helper that manufactured a non-nil
// wrapper from a nil error would mark every successful send as a permanent
// failure. Any helper here that can be handed a nil says what it does with one.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return &permanentError{err: err}
}

// RetryAfter wraps an error with the earliest a retry is worth making, which is
// most often a 429 or a 503 carrying a Retry-After header.
//
// The interval replaces the computed backoff for this attempt only — it does not
// change where the doubling had got to, so a provider that asks for ten minutes
// once does not reset the schedule. It is honoured as a floor rather than exactly,
// because a hundred rows carrying one provider's boundary cannot all return at
// that boundary without rebuilding the herd the spread exists to prevent.
//
// It does not extend the attempt budget. A provider asking to be retried after
// longer than the remaining attempts can cover will still run out of them, and
// that is the intended reading of a maximum: a stop, not a negotiation.
//
// RetryAfter(nil, d) is nil, for the reason [Permanent] gives. A d of zero or less
// is a plain wrap, so the ordinary backoff applies — a provider that sent a
// Retry-After of 0 is saying "now", and "now" for a queue is "next pass".
func RetryAfter(err error, d time.Duration) error {
	if err == nil {
		return nil
	}
	if d <= 0 {
		return err
	}
	return &retryAfterError{err: err, after: d}
}

// IsPermanent reports whether err asks not to be retried.
//
// It reads through wrapping, so an error a sender passed [Permanent] and then
// annotated with fmt.Errorf still answers true.
func IsPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// RetryAfterOf is the interval err asks to be retried after, and whether it asked
// at all.
//
// A permanent error is not retried at all, so this answers false for one even if
// [RetryAfter] was also applied: "do not retry" and "retry at this time" cannot
// both be honoured, and refusing to retry is the stronger of the two. Reading
// them in the other order would let a sender that wrapped both keep alive a row
// it had already said was hopeless.
func RetryAfterOf(err error) (time.Duration, bool) {
	if IsPermanent(err) {
		return 0, false
	}
	var r *retryAfterError
	if errors.As(err, &r) {
		return r.after, true
	}
	return 0, false
}

// The two wrappers are unexported, and there is nothing to read off them that the
// four functions above do not answer. Both carry Unwrap, so errors.Is and
// errors.As reach the provider's own error through them: a sender that wraps a
// sentinel loses nothing by classifying it.

type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

type retryAfterError struct {
	err   error
	after time.Duration
}

func (e *retryAfterError) Error() string { return e.err.Error() }
func (e *retryAfterError) Unwrap() error { return e.err }

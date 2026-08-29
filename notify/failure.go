package notify

import (
	"time"

	"github.com/simonjanss/rig/runtime/outbox"
)

// What a Sender can say about a failure beyond the fact of it.
//
// A [Sender] returns a bare error, and for most of this package's life that was
// the whole vocabulary: everything failed the same way, and everything was
// retried on the same schedule. Which is wrong in both directions at once. A
// provider that answers "that address does not exist" is answering permanently,
// and spending fourteen attempts over eight hours on it buys nothing but log
// lines. A provider that answers 429 with a Retry-After is saying exactly when
// to come back, and guessing instead is either too early — refused again — or
// too late.
//
// The alternative to these helpers was a second return value on [Sender.Send],
// and it was refused for the reason the rest of that interface's documentation
// gives: everything past Send belongs to the application, and rig's leverage
// there is cooperative rather than enforced. A wrapped error is optional in a
// way a signature is not. A Sender written before this file existed keeps
// working, unchanged, and gets the ordinary schedule — which is the right
// default, because "retry it" is the safe answer when nobody said otherwise.

// Permanent wraps an error a retry cannot fix, so the delivery is marked Failed
// on this attempt rather than spending the rest of its budget.
//
// The case it is for is a provider refusing the recipient rather than refusing
// the request: an address that does not exist, a device token the vendor has
// revoked, a message the provider will never accept. Fourteen attempts over
// eight hours at any of those is the same answer fourteen times.
//
// Use it only when the provider's answer is about the recipient. A 500, a
// timeout, a connection refused and a 429 are all about the provider, and all of
// them are what the ordinary schedule is for — wrapping one of those here turns
// a provider's bad ten minutes into permanently undelivered mail.
//
// Permanent(nil) is nil. That is not a courtesy: [Engine.Dispatch] decides a
// send succeeded by testing the error against nil, so a helper that manufactured
// a non-nil wrapper from a nil error would mark every successful send as a
// permanent failure. Any helper here that can be handed a nil has to say what it
// does with one.
func Permanent(err error) error { return outbox.Permanent(err) }

// RetryAfter wraps an error with the earliest a retry is worth making, which is
// most often a 429 or a 503 carrying a Retry-After header.
//
// The interval replaces the computed backoff for this attempt only — it does not
// change where the doubling had got to, so a provider that asks for ten minutes
// once does not reset the schedule. It is honoured as a floor rather than
// exactly: see [Engine] on why a hundred rows carrying one provider's boundary
// cannot all return at that boundary.
//
// It does not extend the attempt budget. A provider asking to be retried after
// longer than the remaining attempts can cover will still run out of them, and
// that is the intended reading of max_attempts: a stop, not a negotiation.
//
// RetryAfter(nil, d) is nil, for the reason [Permanent] gives. A d of zero or
// less is a plain wrap, so the ordinary backoff applies — a provider that sent a
// Retry-After of 0 is saying "now", and "now" for a queue is "next pass".
func RetryAfter(err error, d time.Duration) error { return outbox.RetryAfter(err, d) }

// IsPermanent reports whether err asks not to be retried.
//
// It reads through wrapping, so an error a [Sender] passed [Permanent] and then
// annotated with fmt.Errorf still answers true.
func IsPermanent(err error) bool { return outbox.IsPermanent(err) }

// RetryAfterOf is the interval err asks to be retried after, and whether it
// asked at all.
//
// A permanent error is not retried at all, so this answers false for one even if
// [RetryAfter] was also applied: "do not retry" and "retry at this time" cannot
// both be honoured, and refusing to retry is the stronger of the two. Reading
// them in the other order would let a Sender that wrapped both keep a delivery
// alive that it had already said was hopeless.
func RetryAfterOf(err error) (time.Duration, bool) { return outbox.RetryAfterOf(err) }

// The wrappers themselves live in [runtime/outbox], shared with auth's mail
// outbox, and there is nothing to read off them that the four functions above do
// not answer. Both carry Unwrap, so errors.Is and errors.As reach the provider's
// own error through them: a Sender that wraps a sentinel loses nothing by
// classifying it.
//
// One consequence of sharing them, worth stating rather than discovering: the two
// queues now speak one vocabulary, so `notify.IsPermanent` answers true for an
// error `account.PermanentMailError` wrapped, and the reverse. It used to answer
// false both ways. Nothing depends on either answer, and true is arguably the
// more correct one — a sender that reached for *a* permanent-refusal helper meant
// permanent refusal.

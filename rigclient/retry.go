package rigclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"time"
)

// Retry is how a call the server could not answer is sent again, and how long
// the client waits in between.
//
// It applies to the calls where sending the same request twice cannot mean two
// different things. A read and a delete are that by nature. A create and an
// update are made so: each one goes out named by an idempotency key, and a rig
// server that sees the same key twice answers the second one with what it
// answered the first rather than doing the work again.
//
// An upload is not, and is the one write this never repeats. See [Op.writes].
type Retry struct {
	// Attempts is how many times a call is sent, the first try included. Zero
	// takes [DefaultAttempts]; one sends it once and reports whatever came back.
	//
	// Four is three retries, and the first of them is immediate — see
	// [Retry.Base]. Beyond that a caller is waiting seconds on a call that was
	// probably going to fail anyway, and the calls worth waiting longer on are
	// the ones a caller knows about, which is what [WithRetry] is for.
	Attempts int

	// Base is the first backoff window, doubling per attempt after it. Zero
	// takes [DefaultRetryBase].
	//
	// The first retry does not use it: it goes out immediately, because the
	// commonest retryable failure there is — a pooled connection the server had
	// already closed — is fixed by opening another one, and waiting a second
	// first would only be slower. Everything after that waits.
	//
	// A wait is half the window plus a random share of the other half, rather
	// than the window. The failures worth retrying arrive in crowds — one
	// instance drained, one limiter window closed — and a hundred callers who
	// all waited exactly one second come back together and refuse the server
	// together, which is the thing the interval was for.
	Base time.Duration

	// Cap bounds one backoff window, however many attempts have already failed.
	// Zero takes [DefaultRetryCap].
	//
	// It does not bind at the default attempt count, which is why it is a field
	// rather than a number anybody has to choose: a caller who raises Attempts
	// to eight is asking for eight tries, not for a two-minute sleep in the
	// middle of them.
	Cap time.Duration
}

// DefaultAttempts is how many times a repeatable call is sent when
// [Config.Retry] says nothing: the first try and three more.
const DefaultAttempts = 4

// DefaultRetryBase is the first backoff window, doubling per attempt after it.
//
// With the default attempt count that is an immediate retry, then about a
// second, then about two — call it five seconds before a failing call gives up,
// which is long enough to cross a rolling deploy's window and short enough that
// somebody waiting on the answer has not gone to look at the logs yet.
const DefaultRetryBase = time.Second

// DefaultRetryCap bounds one backoff window. See [Retry.Cap].
const DefaultRetryCap = 2 * time.Second

// MaxRetryAfter is the longest a Retry-After will be slept through.
//
// A server asking for an hour is asking for something a client library cannot
// agree to on somebody's behalf: whether this program has an hour is a question
// about the program. So a longer interval is not waited for — the call comes
// back with the refusal and [Error.RetryAfter] on it, which is the same
// information handed to somebody who can act on it.
const MaxRetryAfter = 30 * time.Second

// The three accessors below read a field or its default, rather than New
// filling the zero values in once. A [CallOption] overrides one field of a Retry
// the caller already configured, and normalising at construction would make the
// other two read as choices somebody made.

func (r Retry) attempts() int {
	if r.Attempts <= 0 {
		return DefaultAttempts
	}
	return r.Attempts
}

func (r Retry) base() time.Duration {
	if r.Base <= 0 {
		return DefaultRetryBase
	}
	return r.Base
}

func (r Retry) window() time.Duration {
	if r.Cap <= 0 {
		return DefaultRetryCap
	}
	return r.Cap
}

// delay is how long to wait before the next attempt, where attempt is the one
// that just failed.
//
// after is what the server asked for, and it wins outright: Retry-After is not a
// hint, it is the interval after which the request stops being refused, and
// guessing a shorter one spends the next attempt on the same refusal. It is not
// jittered, because it is a boundary rather than a guess — and it also cancels
// the immediate first retry, because a server that has just said when to come
// back has answered that question.
//
// It is returned whole rather than clamped to [MaxRetryAfter]. Clamping would
// mean going back before the server said to, which is a request refused twice;
// an interval too long to agree to is one the caller is told about instead. The
// loop in [Runtime.do] is what declines it.
func (r Retry) delay(attempt int, after time.Duration, jitter func(int64) int64) time.Duration {
	if after > 0 {
		return after
	}
	if attempt <= 1 {
		return 0
	}

	window := r.base()
	for range attempt - 2 {
		if window >= r.window() {
			break
		}
		window *= 2
	}
	window = min(window, r.window())

	// Half the window, plus a random share of the other half. Full jitter — a
	// wait anywhere in [0, window] — spreads a crowd better still, but it also
	// means a retry that fires almost immediately after the one that just
	// failed, and the schedule here is short enough that the difference matters.
	half := window / 2
	return half + time.Duration(jitter(int64(half)+1))
}

// retryable reports whether a status is worth sending the same request for
// again.
//
// A list rather than status >= 500. A 501 is excluded deliberately: it is the
// QUERY fallback's own signal — see [Op.Fallback] — and a method nobody in the
// chain has heard of will not have been heard of a second later. A 505 is about
// the protocol, and no wait fixes that either.
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// idempotent reports whether this operation may be sent twice.
//
// It is asked of the operation the generated method built, before [Op.asPost]
// rewrites a refused QUERY: a deployment whose proxy refuses QUERY is exactly
// the one whose searches most need retrying, and keying on the wire method
// would quietly take that away.
//
// A delete is in, on the strength of what it leaves behind rather than what it
// answers. If the first attempt succeeded and its answer was lost, the second
// one is a 404 — so a delete that worked can come back [IsNotFound]. The row is
// gone either way, and a duplicated create is not recoverable that way, which is
// the whole of the distinction.
//
// A write is not in this list and does not need to be: it becomes repeatable by
// carrying an idempotency key, which is a property of the call rather than of
// the method. See [Op.writes] and [WithIdempotencyKey].
func (op Op) idempotent() bool {
	switch op.Method {
	case http.MethodGet, http.MethodHead, http.MethodDelete, MethodQuery:
		return true
	}
	return false
}

// writes reports whether this operation can produce something a second one would
// duplicate, and so whether it is worth naming with an idempotency key.
//
// Asked before [Op.asPost] rewrites a refused QUERY, so a search that has to go
// out as a POST is still a read and is still not given a key.
//
// A form is excluded, and that is about the server rather than about the client.
// An upload route is the one write a rig server does not record against a key:
// its body is still arriving when the service is called, and a transaction held
// open for a transfer is a pooled connection held open for a transfer. So a key
// generated here would name a write nobody wrote down, and repeating it would
// store the file twice.
//
// It costs a create that carries a file its retry, which the server would have
// deduplicated — the SDK cannot tell that route from the upload route, and
// guessing wrong in this direction stores a second file. [WithIdempotencyKey]
// still names such a write for a caller who re-sends it themselves; what it does
// not do is make the SDK re-send it.
func (op Op) writes() bool {
	if op.Multipart != nil {
		return false
	}
	switch op.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return true
	}
	return false
}

// idempotencyKey is what names this write, generating one if the caller did not.
//
// Generated rather than asked for, because a key a caller has to remember is a
// key most callers will not send, and then the retry they were counting on is
// the one thing this SDK will not do for them. It is per call and not per
// attempt: the point is for the second send to be recognisable as the first.
//
// A caller's own key is left alone and is worth having — one derived from the
// data, the way an import job names a row by its line, deduplicates a re-run of
// the whole job, which a fresh random name cannot.
func (c *call) idempotencyKey() string {
	if key := c.header.Get("Idempotency-Key"); key != "" {
		return key
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Without a name there is nothing to make the second send the same
		// write, so the call goes out unkeyed and is not retried. Refusing the
		// call instead would turn a source of randomness having a bad day into
		// an outage.
		return ""
	}
	key := hex.EncodeToString(b[:])
	c.header.Set("Idempotency-Key", key)
	return key
}

// budget is what is left of the time this call was given, retries and backoff
// included.
//
// A deadline rather than a derived context, because [Runtime.do] returns a
// response whose body is still open: a context cancelled when do returns would
// kill the decode in [Do] and the caller's own read in [DoContent]. So the
// bound is carried as a time and spent as [http.Client.Timeout] on each retry.
//
// The clock is [Config.Now], which is what makes it testable without waiting.
// A context's deadline is deliberately not folded in: it is kept by the machine
// clock while this one may be a test's, and the context stops everything
// regardless — the wait watches it, and the request carries it.
type budget struct {
	deadline time.Time
	now      func() time.Time
}

// left is what remains, and whether there was a bound at all. A caller who
// supplied an http.Client with no timeout gets no deadline and no ceiling.
func (b budget) left() (time.Duration, bool) {
	if b.deadline.IsZero() {
		return 0, false
	}
	return b.deadline.Sub(b.now()), true
}

// allows reports whether waiting d still leaves time to send anything
// afterwards.
//
// It is the caller's own timeout and nothing else. A backoff longer than this
// is a [Retry] somebody configured, and configuration is not something to
// second-guess; a Retry-After longer than this is a server asking, and asking
// is what [MaxRetryAfter] answers.
func (b budget) allows(d time.Duration) bool {
	left, bounded := b.left()
	if !bounded {
		return true
	}
	return d < left
}

// leash is what to bound the next attempt by, or zero to leave the client's own
// timeout alone.
func (b budget) leash() time.Duration {
	left, bounded := b.left()
	if !bounded {
		return 0
	}
	return max(left, time.Nanosecond)
}

// budgetFor is the deadline this call has to finish inside.
//
// The timeout that was already on the call becomes the budget for the whole of
// it. [WithTimeout] says it bounds this call and [Config.HTTPClient] says it
// limits the whole exchange, and reading either as a per-attempt ceiling would
// turn a ten-minute upload into thirty minutes without anybody changing a line.
func (rt *Runtime) budgetFor(c *call) budget {
	timeout := c.timeout
	if timeout <= 0 && rt.http != nil {
		timeout = rt.http.Timeout
	}
	b := budget{now: rt.now}
	if timeout > 0 {
		b.deadline = rt.now().Add(timeout)
	}
	return b
}

// backoff puts a form body back where it started and then waits out the interval
// before the next attempt.
//
// A nil answer means go round again. Anything else is what the call returns
// instead of retrying, and it carries cause either way: a body that cannot be
// produced a second time comes back as [ErrCannotRetry] joined to whatever
// prompted the retry, and a caller who stopped waiting gets both the refusal and
// their own cancellation. Neither fact is the whole story on its own.
//
// The rewind is before the wait so that a call which was never going to be
// retriable comes back now rather than a backoff window later.
//
// It does not touch the attempt counter. Which of the three re-sends spends one
// is the thing [Runtime.call] is careful to keep visible, and burying the
// increment in here would be the opposite of that.
func backoff(ctx context.Context, op Op, marks []int64, wait time.Duration, cause error) error {
	if rewound := op.Multipart.rewind(marks); rewound != nil {
		return errors.Join(cause, rewound)
	}
	return waitFor(ctx, wait, cause)
}

// waitFor sleeps, or gives up early because the caller stopped caring.
//
// The context is checked before the timer is armed rather than only inside the
// select, so a call whose context is already done returns at once instead of
// racing a channel that is also ready.
//
// A cancellation during a wait carries the refusal out with it. Both facts are
// true — the server refused, and the caller stopped waiting — and a caller needs
// both to tell "we gave up on a rate limit" from "we gave up on a 500". It is
// the same reasoning [ErrCannotRetry] is joined under.
func waitFor(ctx context.Context, d time.Duration, refusal error) error {
	if err := ctx.Err(); err != nil {
		return errors.Join(refusal, err)
	}
	if d <= 0 {
		return nil
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return errors.Join(refusal, ctx.Err())
	case <-timer.C:
		return nil
	}
}

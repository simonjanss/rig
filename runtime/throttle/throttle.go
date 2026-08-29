// Package throttle rate-limits by counting what already happened.
//
// There is no counter to keep in sync and no store to add. A limiter asks how
// many failures a key has recorded inside a window and decides from the answer,
// which makes it correct across replicas without coordination, correct after a
// restart, and self-healing: counters age out on their own, and a success
// clears the window rather than leaving somebody locked out by four typos.
//
// The cost is a query per check. That is the right trade for the places it is
// used — login, password reset, refresh — where the alternative is a shared
// cache that has to be deployed, monitored, and reasoned about during an
// incident, in exchange for microseconds on a path that already runs argon2id.
package throttle

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// Limit is how many failures of one kind a single subject may accumulate.
type Limit struct {
	// Name identifies the limit in errors and headers, for example
	// "login.email".
	Name string
	// Event is what is being counted. For the generated auth log it is an
	// event name such as "LoginFailed".
	Event string
	// ClearedBy is the event that resets the window, if any.
	//
	// It is what keeps a limit from punishing the wrong thing: somebody who
	// mistypes a password four times, gets it right, and mistypes once more is
	// not locked out, because the success wiped the earlier failures. Empty
	// means nothing clears the window — right for a limit whose event is not a
	// failure at all, such as bounding how often one session may refresh.
	ClearedBy string
	// Max is how many are allowed inside Window. The Max+1st is refused.
	Max int
	// Window is how far back the count reaches.
	Window time.Duration
}

// Key is what a limit is counted against.
//
// Kind matters as much as Value: an email limit and an IP limit are separate
// budgets, and counting them together would let one attacker exhaust both.
type Key struct {
	Kind  string
	Value string
}

// Check pairs a limit with the key to apply it to.
type Check struct {
	Limit Limit
	Key   Key
}

// Counter reports what a key has already done.
//
// Counting is not writing: a Counter never records anything. The caller writes
// its own log — which it was going to write anyway, for the audit trail — and
// the limiter reads it. That is what makes the two impossible to get out of
// step.
type Counter interface {
	// Count returns how many of the limit's events happened for the key
	// strictly after since, and when the earliest of them was. The earliest is
	// the zero time when the count is zero.
	//
	// An occurrence of the limit's ClearedBy event resets the window: anything
	// at or before the most recent one does not count.
	//
	// The lower bound excludes since so that a window closes when it says it
	// will. An event the limiter promised would expire at 12:15 must not still
	// be counted at 12:15, or the Retry-After it handed out was a lie.
	Count(ctx context.Context, limit Limit, key Key, since time.Time) (n int, earliest time.Time, err error)
}

// Recorder spends a slot and reports what the key has spent, in one round trip.
//
// It is the other half of [Counter], for the limits whose event nobody would
// want in an audit trail. A login failure is worth a row on its own merits and
// the limiter reads it for free; an ordinary API call is not, and there are a
// great many more of them. So here the request *is* the event: it has to be
// recorded to be counted, and the recording and the counting are one statement
// because two would race.
//
// The returned total is the whole window, not the delta — a caller that batches
// locally still needs the global answer to decide with.
type Recorder interface {
	// Incr adds delta events for the key at now and returns the total inside
	// the limit's window, with the instant that window's oldest part expires.
	//
	// delta is a count rather than an implied one so that a caller which held
	// events back can flush them together. Zero is legal and asks the question
	// without spending anything.
	Incr(ctx context.Context, limit Limit, key Key, now time.Time, delta int) (total int, resetAt time.Time, err error)
}

// Limiter applies limits.
type Limiter struct {
	// counter answers Allow, recorder answers Take. A limiter has whichever of
	// the two its caller built it with, and asking it the other question is an
	// error rather than a permissive default — see [Limiter.Take].
	counter  Counter
	recorder Recorder
	// now is swappable so a test can age a window without sleeping through it.
	now func() time.Time
}

// New builds a limiter over a counter.
func New(counter Counter) *Limiter {
	return &Limiter{counter: counter, now: time.Now}
}

// NewRecording builds a limiter over a recorder, for [Limiter.Take].
func NewRecording(recorder Recorder) *Limiter {
	return &Limiter{recorder: recorder, now: time.Now}
}

// WithClock returns a copy that reads the time from fn. It exists for tests: a
// sliding window is exactly the kind of thing whose tests should not sleep.
func (l *Limiter) WithClock(fn func() time.Time) *Limiter {
	out := *l
	out.now = fn
	return &out
}

// fold runs per over every check and keeps the tightest answer it gets.
//
// One clock reading for the whole call rather than one per check. Two limits
// read a microsecond apart would measure their windows from different instants,
// and the Retry-After that came back would be derived from a now that the
// decision beside it never saw.
//
// Every check runs, including after one has answered no. Why that matters is not
// the same for the two verbs, and each says so on its own doc — see
// [Limiter.Allow] and [Limiter.Take].
func (l *Limiter) fold(
	ctx context.Context,
	checks []Check,
	per func(context.Context, time.Time, Check) (Decision, error),
) (Decision, error) {
	now := l.now()

	var (
		worst   Decision
		decided bool
	)

	for _, c := range checks {
		d, err := per(ctx, now, c)
		if err != nil {
			return Decision{}, err
		}
		if !decided || d.tighterThan(worst) {
			worst, decided = d, true
		}
	}

	if !decided {
		// No checks is not an error; it is a caller who configured no limits.
		return Decision{Allowed: true}, nil
	}
	return worst, nil
}

// Allow evaluates every check and reports the outcome.
//
// Every check runs, even after one fails, because the headers should describe
// the tightest constraint the caller is actually under rather than whichever
// one happened to be listed first.
func (l *Limiter) Allow(ctx context.Context, checks ...Check) (Decision, error) {
	if l.counter == nil {
		return Decision{}, errors.New("throttle: Allow needs a limiter built with New")
	}
	return l.fold(ctx, checks, l.evaluate)
}

func (l *Limiter) evaluate(ctx context.Context, now time.Time, c Check) (Decision, error) {
	since := now.Add(-c.Limit.Window)

	n, earliest, err := l.counter.Count(ctx, c.Limit, c.Key, since)
	if err != nil {
		return Decision{}, err
	}

	d := Decision{
		Allowed:   n < c.Limit.Max,
		Limit:     c.Limit,
		Key:       c.Key,
		Used:      n,
		Remaining: max(c.Limit.Max-n, 0),
	}
	if d.Allowed {
		return d, nil
	}

	// A slot frees when the earliest failure leaves the window.
	//
	// Somebody far over the limit is told to come back a little too early and
	// is refused again, which is the right way to be wrong: the exact answer
	// needs a second query on every rejected request, and rejected requests are
	// the ones least worth spending a query on.
	d.ResetAt = earliest.Add(c.Limit.Window)
	d.RetryAfter = max(d.ResetAt.Sub(now), time.Second)
	return d, nil
}

// Take spends a slot against every check and reports the outcome.
//
// It is [Limiter.Allow] for the limits that have to be written down to be
// counted. The difference that matters is what the number means: Allow's count
// is what happened *before* this request, so it refuses at Max; Take's count
// includes this request, so it refuses at Max+1. Both let exactly Max through.
//
// Every check is spent, including after one of them has already refused. A
// refused request is still a request, and skipping the rest would let somebody
// sitting over one budget stay under all the others indefinitely — the count on
// the key they were not breaching would stop moving precisely because they were
// breaching another.
func (l *Limiter) Take(ctx context.Context, checks ...Check) (Decision, error) {
	if l.recorder == nil {
		// Not a permissive default. A limiter that answered "allowed" here would
		// be indistinguishable from a configured one until somebody attacked it.
		return Decision{}, errors.New("throttle: Take needs a limiter built with NewRecording")
	}
	return l.fold(ctx, checks, l.spend)
}

func (l *Limiter) spend(ctx context.Context, now time.Time, c Check) (Decision, error) {
	n, resetAt, err := l.recorder.Incr(ctx, c.Limit, c.Key, now, 1)
	if err != nil {
		return Decision{}, err
	}

	d := Decision{
		// n counts this request, which is why this is not the `<` in evaluate.
		Allowed:   n <= c.Limit.Max,
		Limit:     c.Limit,
		Key:       c.Key,
		Used:      n,
		Remaining: max(c.Limit.Max-n, 0),
	}
	if d.Allowed {
		return d, nil
	}

	// The recorder knows when the window it counted over runs out, so unlike
	// evaluate there is nothing to derive: it counted, so it can say.
	d.ResetAt = resetAt
	d.RetryAfter = max(resetAt.Sub(now), time.Second)
	return d, nil
}

// Decision is the outcome of a set of checks.
type Decision struct {
	Allowed bool
	// Limit and Key are whichever check this decision came from: the one that
	// refused, or the one closest to refusing.
	Limit Limit
	Key   Key

	Used      int
	Remaining int

	// RetryAfter is how long until a slot frees. Zero when allowed.
	RetryAfter time.Duration
	ResetAt    time.Time
}

// tighterThan reports whether d constrains the caller more than other.
func (d Decision) tighterThan(other Decision) bool {
	if d.Allowed != other.Allowed {
		return !d.Allowed
	}
	if !d.Allowed {
		return d.RetryAfter > other.RetryAfter
	}
	return d.Remaining < other.Remaining
}

// Err returns the error to answer with, or nil when the request may proceed.
//
// The message says how long to wait and nothing about why the limit exists. A
// login limiter that says "too many attempts for this email address" has just
// confirmed the address is worth attacking.
func (d Decision) Err() error {
	if d.Allowed {
		return nil
	}
	return &Refusal{
		err:      rigerr.RateLimited("too many attempts; try again in %s", humanize(d.RetryAfter)),
		decision: d,
	}
}

// Refusal is a rate-limit error that still knows the decision behind it.
//
// The wait has to survive the trip out to the HTTP layer, which is several
// packages away from the limiter and has no way to ask again. Carrying it on
// the error is what lets the response set Retry-After without every handler in
// between knowing about rate limits.
type Refusal struct {
	err      *rigerr.Error
	decision Decision
}

// Error implements error.
func (r *Refusal) Error() string { return r.err.Error() }

// Unwrap exposes the typed error, so [rigerr.CodeOf] and [rigerr.Is] see a
// rate-limit error rather than an unrecognized one.
func (r *Refusal) Unwrap() error { return r.err }

// RetryAfter is how long until a slot frees.
func (r *Refusal) RetryAfter() time.Duration { return r.decision.RetryAfter }

// Decision is the whole answer, for a caller that wants the headers.
func (r *Refusal) Decision() Decision { return r.decision }

// RefusalOf finds the rate-limit refusal in an error chain, if there is one.
func RefusalOf(err error) (*Refusal, bool) {
	var r *Refusal
	return r, errors.As(err, &r)
}

// SetHeaders describes the limit to the client.
//
// Retry-After is the one every client already understands. The RateLimit-*
// headers let a well-behaved one slow down before it is refused, which is the
// whole point of telling it anything.
func (d Decision) SetHeaders(h http.Header) {
	if d.Limit.Max > 0 {
		h.Set("RateLimit-Limit", strconv.Itoa(d.Limit.Max))
		h.Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
	}
	if d.Allowed {
		return
	}

	seconds := int(d.RetryAfter.Round(time.Second) / time.Second)
	h.Set("Retry-After", strconv.Itoa(max(seconds, 1)))
	h.Set("RateLimit-Reset", strconv.Itoa(max(seconds, 1)))
}

// humanize renders a wait the way a person would say it.
func humanize(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(max(int(d.Round(time.Second)/time.Second), 1)) + " seconds"
	case d < time.Hour:
		return strconv.Itoa(int(d.Round(time.Minute)/time.Minute)) + " minutes"
	default:
		return strconv.Itoa(int(d.Round(time.Hour)/time.Hour)) + " hours"
	}
}

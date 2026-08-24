package rigclient

import (
	"net/http"
	"strconv"
	"time"
)

// The headers a rig server describes a caller's budget with. They go out on
// every response, not only on a refusal, which is the whole point: a client that
// can see it is at 900 of 1000 can slow down before it is refused.
const (
	headerRateLimitLimit     = "RateLimit-Limit"
	headerRateLimitRemaining = "RateLimit-Remaining"
	headerRateLimitReset     = "RateLimit-Reset"
)

// RateLimitStatus is what a response said about the caller's budget.
//
// It is the tightest limit the caller is under rather than a list of every one
// that applied: the server evaluates each and reports the one closest to
// refusing, because that is the only number a client can act on.
type RateLimitStatus struct {
	// Op is the operation that was called, as the document names it —
	// "listTodos". A gauge keyed on it says which call is spending the budget.
	Op string

	// Limit is how many calls the window allows, and Remaining how many are
	// left. Remaining is zero on the response that was refused.
	Limit     int
	Remaining int

	// ResetAfter is how long until the window frees, and is only stated on a
	// refusal — an allowed response says how much is left but not when it comes
	// back. Zero means the server did not say.
	ResetAfter time.Duration

	// Refused is true when this response was the 429 rather than a call that
	// went through.
	Refused bool
}

// Used is how much of the budget has been spent.
func (s RateLimitStatus) Used() int { return max(s.Limit-s.Remaining, 0) }

// Fraction is how much of the budget is spent, from 0 to 1.
//
// It is the number worth alerting on, and it is here rather than left to the
// caller because the obvious arithmetic has a divide-by-zero in it: a response
// from a server with no limit configured carries no headers and leaves Limit at
// zero.
func (s RateLimitStatus) Fraction() float64 {
	if s.Limit <= 0 {
		return 0
	}
	return float64(s.Used()) / float64(s.Limit)
}

// rateLimitOf reads the status out of a response, and reports whether the server
// said anything at all.
//
// A server with no throttle block sends none of these headers, and a caller that
// treated the zero value as "no budget left" would be reading a limit of zero
// out of a server that has no limits.
func rateLimitOf(op string, res *http.Response) (RateLimitStatus, bool) {
	raw := res.Header.Get(headerRateLimitLimit)
	if raw == "" {
		return RateLimitStatus{}, false
	}
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return RateLimitStatus{}, false
	}

	s := RateLimitStatus{
		Op:      op,
		Limit:   limit,
		Refused: res.StatusCode == http.StatusTooManyRequests,
	}
	// Remaining is sent with Limit, but it is read separately rather than
	// assumed: a proxy that rewrote one and not the other should produce a
	// missing number, not a confident wrong one.
	if n, err := strconv.Atoi(res.Header.Get(headerRateLimitRemaining)); err == nil {
		s.Remaining = n
	}
	if n, err := strconv.Atoi(res.Header.Get(headerRateLimitReset)); err == nil && n > 0 {
		s.ResetAfter = time.Duration(n) * time.Second
	}
	return s, true
}

// observeRateLimit hands the status to the configured callback.
//
// Every response, including the refused ones and the ones a retry will replace:
// each is a real thing the server said, and a client watching its budget wants
// the observation rather than a summary of the call.
func (rt *Runtime) observeRateLimit(op string, res *http.Response) {
	if rt.onRateLimit == nil || res == nil {
		return
	}
	if s, ok := rateLimitOf(op, res); ok {
		rt.onRateLimit(s)
	}
}

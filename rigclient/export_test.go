package rigclient

import "time"

// Delay is [Retry.delay], reachable from the external test package beside this
// one.
//
// The seam is here rather than on the type because a caller has no use for it:
// how long the SDK waits is a decision it makes, not a number anybody else
// reproduces. What a test needs is to hand in the randomness and get the
// arithmetic back.
func Delay(r Retry, attempt int, after time.Duration, jitter func(int64) int64) time.Duration {
	return r.delay(attempt, after, jitter)
}

// RetryAfter is the header parser, for the two forms it has to read.
func RetryAfter(value string, now time.Time) time.Duration { return retryAfter(value, now) }

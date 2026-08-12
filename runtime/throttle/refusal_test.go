package throttle

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/rigerr"
)

// The refusal travels several packages from the limiter to the response, and
// the wait has to survive the trip: a 429 with no Retry-After leaves a client
// with nothing to do but guess, and clients that guess retry immediately.
func TestARefusalCarriesItsWaitOutThroughTheErrorChain(t *testing.T) {
	t.Parallel()

	d := Decision{
		Allowed:    false,
		Limit:      Limit{Max: 5, Window: 15 * time.Minute},
		Remaining:  0,
		RetryAfter: 90 * time.Second,
	}
	err := d.Err()

	// It reads as a rate limit to anything that only knows rigerr.
	if code := rigerr.CodeOf(err); code != rigerr.CodeRateLimited {
		t.Errorf("code = %q, want RateLimited", code)
	}
	if rigerr.StatusOf(err) != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rigerr.StatusOf(err))
	}

	// And the decision survives being wrapped by the layers in between.
	found, ok := RefusalOf(fmt.Errorf("logging in: %w", err))
	if !ok {
		t.Fatal("the refusal should be reachable through a wrap")
	}
	if found.RetryAfter() != 90*time.Second {
		t.Errorf("RetryAfter = %s", found.RetryAfter())
	}
	if found.Decision().Limit.Max != 5 {
		t.Errorf("the whole decision should come with it: %+v", found.Decision())
	}
	if found.Error() == "" {
		t.Error("it should still read as an error")
	}

	if _, ok := RefusalOf(errors.New("something else")); ok {
		t.Error("an ordinary error is not a refusal")
	}
	if _, ok := RefusalOf(nil); ok {
		t.Error("nothing is not a refusal")
	}
}

// Retry-After is the header every client already understands; the RateLimit-*
// ones let a well-behaved client slow down before it is refused, which is the
// whole point of telling it anything.
func TestSetHeadersDescribesTheLimit(t *testing.T) {
	t.Parallel()

	refused := Decision{
		Limit:      Limit{Max: 5, Window: time.Minute},
		Remaining:  0,
		RetryAfter: 42 * time.Second,
	}

	h := http.Header{}
	refused.SetHeaders(h)

	for header, want := range map[string]string{
		"RateLimit-Limit":     "5",
		"RateLimit-Remaining": "0",
		"Retry-After":         "42",
		"RateLimit-Reset":     "42",
	} {
		if got := h.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}

	// An allowed request is told how much room is left and nothing else: a
	// Retry-After on a request that was not refused is a contradiction.
	allowed := Decision{Allowed: true, Limit: Limit{Max: 5}, Remaining: 3}
	h = http.Header{}
	allowed.SetHeaders(h)

	if got := h.Get("RateLimit-Remaining"); got != "3" {
		t.Errorf("remaining = %q", got)
	}
	if h.Get("Retry-After") != "" {
		t.Error("an allowed request has nothing to wait for")
	}
}

// Zero seconds reads as "try again now", which is what a client does anyway
// and what produces the retry storm the limit exists to stop.
func TestARetryAfterIsNeverZero(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	Decision{Limit: Limit{Max: 1}, RetryAfter: 200 * time.Millisecond}.SetHeaders(h)

	if got := h.Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want at least 1", got)
	}
}

// A limit with no maximum is one that was never configured, and reporting it
// as "0 remaining of 0" would tell a client it is permanently refused.
func TestAnUnconfiguredLimitSaysNothing(t *testing.T) {
	t.Parallel()

	h := http.Header{}
	Decision{Allowed: true, Limit: Limit{}}.SetHeaders(h)

	if h.Get("RateLimit-Limit") != "" {
		t.Errorf("headers = %v, want none", h)
	}
}

// The message is read by a person deciding whether they are locked out for a
// moment or for the afternoon.
func TestTheWaitIsSaidTheWayAPersonWouldSayIt(t *testing.T) {
	t.Parallel()

	for wait, want := range map[time.Duration]string{
		500 * time.Millisecond: "1 seconds",
		30 * time.Second:       "30 seconds",
		90 * time.Second:       "2 minutes",
		45 * time.Minute:       "45 minutes",
		2 * time.Hour:          "2 hours",
	} {
		if got := humanize(wait); got != want {
			t.Errorf("humanize(%s) = %q, want %q", wait, got, want)
		}
	}

	err := Decision{RetryAfter: 2 * time.Hour}.Err()
	if !strings.Contains(err.Error(), "2 hours") {
		t.Errorf("the refusal should say how long: %v", err)
	}

	// And nothing about why the limit exists: "too many attempts for this
	// email address" has just confirmed the address is worth attacking.
	if strings.Contains(err.Error(), "email") {
		t.Errorf("the message should not describe the limit: %v", err)
	}

	// An allowed decision has nothing to report.
	if allowed := (Decision{Allowed: true}).Err(); allowed != nil {
		t.Errorf("an allowed request is not an error: %v", allowed)
	}
}

// Every key kind has a constructor, because building one by hand is how the
// kind and the value end up swapped.
func TestTheKeyConstructors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key  Key
		kind string
	}{
		{Email("sam@example.com"), KeyEmailAddress},
		{IP("203.0.113.4"), KeyIPAddress},
		{Account("11111111-1111-1111-1111-111111111111"), KeyAccount},
		{APIKey("7f3a"), KeyAPIKey},
		{TokenFamily("22222222-2222-2222-2222-222222222222"), KeyTokenFamily},
	} {
		if tc.key.Kind != tc.kind {
			t.Errorf("kind = %q, want %q", tc.key.Kind, tc.kind)
		}
		if tc.key.Value == "" {
			t.Errorf("%s carries no value", tc.kind)
		}
	}
}

// The counting query is built once from configuration, and the two halves of
// it — the events and whatever cleared them — read the same columns from
// different aliases. Unqualified, the subquery would be ambiguous.
func TestTheQueryQualifiesBothSidesOfAClearingLimit(t *testing.T) {
	t.Parallel()

	p := NewPostgres(nil, PostgresConfig{})

	plain := p.sql("t.email_address = $1", Limit{Max: 5, Window: time.Minute})
	if !strings.Contains(plain, "FROM rig_auth_log t") {
		t.Errorf("the default table should be rig_auth_log:\n%s", plain)
	}
	if strings.Contains(plain, "greatest(") {
		t.Errorf("a limit nothing clears needs no floor subquery:\n%s", plain)
	}

	cleared := p.sql("email_address = $1", Limit{
		Max: 5, Window: time.Minute, ClearedBy: "LoginSucceeded",
	})
	if !strings.Contains(cleared, "greatest(") {
		t.Errorf("a clearing event should raise the floor:\n%s", cleared)
	}
	if !strings.Contains(cleared, "c.email_address") || !strings.Contains(cleared, "t.email_address") {
		t.Errorf("both aliases should be qualified:\n%s", cleared)
	}
}

// A key kind nothing knows how to match cannot be counted, and counting zero
// would silently disable the limit.
func TestAnUnknownKeyKindIsAnError(t *testing.T) {
	t.Parallel()

	p := NewPostgres(nil, PostgresConfig{})

	_, _, err := p.Count(t.Context(), Limit{Max: 1, Window: time.Minute},
		Key{Kind: "invented", Value: "x"}, time.Now())
	if err == nil {
		t.Fatal("a key nothing matches should be an error, not an empty count")
	}
	if !strings.Contains(err.Error(), "invented") {
		t.Errorf("the error should name the kind: %v", err)
	}
}

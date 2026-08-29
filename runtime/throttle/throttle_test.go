package throttle_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// clock is a hand-wound time source. A sliding window whose tests sleep is a
// test suite that gets deleted the first time it flakes.
type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

func newClock() *clock {
	return &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func setup(t *testing.T) (*throttle.Limiter, *throttle.Memory, *clock) {
	t.Helper()

	c := newClock()
	counter := throttle.NewMemory()
	return throttle.New(counter).WithClock(c.now), counter, c
}

var loginByEmail = throttle.Standard().LoginByEmail

func TestUnderTheLimitIsAllowed(t *testing.T) {
	t.Parallel()

	limiter, counter, c := setup(t)
	key := throttle.Email("sam@example.com")

	for range 4 {
		counter.Record(throttle.EventLoginFailed, key, c.at)
	}

	d, err := limiter.Allow(context.Background(), throttle.Check{Limit: loginByEmail, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Error("four failures against a limit of five should still be allowed")
	}
	if d.Remaining != 1 {
		t.Errorf("remaining = %d, want 1", d.Remaining)
	}
	if d.Err() != nil {
		t.Errorf("an allowed decision should have no error: %v", d.Err())
	}
}

func TestTheLimitRefuses(t *testing.T) {
	t.Parallel()

	limiter, counter, c := setup(t)
	key := throttle.Email("sam@example.com")

	for range 5 {
		counter.Record(throttle.EventLoginFailed, key, c.at)
	}

	d, err := limiter.Allow(context.Background(), throttle.Check{Limit: loginByEmail, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("the sixth attempt should be refused")
	}
	if d.Remaining != 0 {
		t.Errorf("remaining = %d, want 0", d.Remaining)
	}

	// A refusal a client cannot act on is just a wall.
	if d.RetryAfter != 15*time.Minute {
		t.Errorf("retry after = %s, want the full window", d.RetryAfter)
	}
	if !rigerr.Is(d.Err(), rigerr.CodeRateLimited) {
		t.Errorf("error = %v, want a rate-limit error", d.Err())
	}

	// And it must not say what it is counting. "too many attempts for this
	// email address" confirms the address is worth attacking.
	if msg := d.Err().Error(); strings.Contains(msg, "sam@example.com") {
		t.Errorf("the message names the subject: %q", msg)
	}
}

func TestTheWindowSlides(t *testing.T) {
	t.Parallel()

	limiter, counter, c := setup(t)
	key := throttle.Email("sam@example.com")

	// Five failures, the first one a minute before the rest.
	counter.Record(throttle.EventLoginFailed, key, c.at)
	c.advance(time.Minute)
	for range 4 {
		counter.Record(throttle.EventLoginFailed, key, c.at)
	}

	refused, _ := limiter.Allow(context.Background(), throttle.Check{Limit: loginByEmail, Key: key})
	if refused.Allowed {
		t.Fatal("five failures should be refused")
	}
	// The oldest leaves the window a minute before the others do.
	if refused.RetryAfter != 14*time.Minute {
		t.Errorf("retry after = %s, want 14m", refused.RetryAfter)
	}

	// Once it has, there is room again — nobody had to clear anything.
	c.advance(14 * time.Minute)
	allowed, _ := limiter.Allow(context.Background(), throttle.Check{Limit: loginByEmail, Key: key})
	if !allowed.Allowed {
		t.Error("the oldest failure has aged out, so a slot should be free")
	}
}

// Four typos and then the right password is a person having a bad morning, not
// an attack. Locking them out on the fifth attempt punishes the wrong thing.
func TestASuccessClearsTheWindow(t *testing.T) {
	t.Parallel()

	limiter, counter, c := setup(t)
	key := throttle.Email("sam@example.com")

	for range 4 {
		counter.Record(throttle.EventLoginFailed, key, c.at)
	}

	c.advance(time.Second)
	counter.Record(throttle.EventLoginSucceeded, key, c.at)

	c.advance(time.Second)
	counter.Record(throttle.EventLoginFailed, key, c.at)

	d, _ := limiter.Allow(context.Background(), throttle.Check{Limit: loginByEmail, Key: key})
	if !d.Allowed {
		t.Fatal("failures before a success should not count")
	}
	if d.Used != 1 {
		t.Errorf("used = %d, want only the failure after the success", d.Used)
	}
}

// Two budgets, deliberately: one attacker must not be able to lock a victim
// out, and one botnet must not be able to spray every address it knows.
func TestTheTightestLimitWins(t *testing.T) {
	t.Parallel()

	limiter, counter, c := setup(t)
	std := throttle.Standard()

	var (
		email = throttle.Email("sam@example.com")
		ip    = throttle.IP("203.0.113.10")
	)

	// One address hammered from one source: the email limit trips first.
	for range 5 {
		counter.Record(throttle.EventLoginFailed, email, c.at)
		counter.Record(throttle.EventLoginFailed, ip, c.at)
	}

	d, _ := limiter.Allow(context.Background(),
		throttle.Check{Limit: std.LoginByEmail, Key: email},
		throttle.Check{Limit: std.LoginByIP, Key: ip},
	)
	if d.Allowed {
		t.Fatal("the email limit should have tripped")
	}
	if d.Limit.Name != "login.email" {
		t.Errorf("the decision came from %q, want login.email", d.Limit.Name)
	}

	// A different address from the same source is still fine — one account
	// being under attack does not take the whole office offline.
	other := throttle.Email("robin@example.com")
	d, _ = limiter.Allow(context.Background(),
		throttle.Check{Limit: std.LoginByEmail, Key: other},
		throttle.Check{Limit: std.LoginByIP, Key: ip},
	)
	if !d.Allowed {
		t.Error("five failures is nowhere near the IP limit of fifty")
	}
}

func TestOneSourceSprayingManyAccountsIsThrottled(t *testing.T) {
	t.Parallel()

	limiter, counter, c := setup(t)
	std := throttle.Standard()
	ip := throttle.IP("203.0.113.10")

	// Fifty accounts, one attempt each: every email limit is untouched.
	for range 50 {
		counter.Record(throttle.EventLoginFailed, ip, c.at)
	}

	d, _ := limiter.Allow(context.Background(),
		throttle.Check{Limit: std.LoginByEmail, Key: throttle.Email("nobody@example.com")},
		throttle.Check{Limit: std.LoginByIP, Key: ip},
	)
	if d.Allowed {
		t.Fatal("the IP limit exists precisely for this")
	}
	if d.Limit.Name != "login.ip" {
		t.Errorf("the decision came from %q, want login.ip", d.Limit.Name)
	}
}

func TestHeaders(t *testing.T) {
	t.Parallel()

	limiter, counter, c := setup(t)
	key := throttle.Email("sam@example.com")
	for range 5 {
		counter.Record(throttle.EventLoginFailed, key, c.at)
	}

	d, _ := limiter.Allow(context.Background(), throttle.Check{Limit: loginByEmail, Key: key})

	h := http.Header{}
	d.SetHeaders(h)

	if got := h.Get("Retry-After"); got != "900" {
		t.Errorf("Retry-After = %q, want 900", got)
	}
	if got := h.Get("RateLimit-Limit"); got != "5" {
		t.Errorf("RateLimit-Limit = %q, want 5", got)
	}
	if got := h.Get("RateLimit-Remaining"); got != "0" {
		t.Errorf("RateLimit-Remaining = %q, want 0", got)
	}

	// An allowed request still says how much room is left, so a well-behaved
	// client can slow down before it is refused.
	c.advance(20 * time.Minute)
	ok, _ := limiter.Allow(context.Background(), throttle.Check{Limit: loginByEmail, Key: key})
	h = http.Header{}
	ok.SetHeaders(h)

	if got := h.Get("RateLimit-Remaining"); got != "5" {
		t.Errorf("RateLimit-Remaining = %q, want 5", got)
	}
	if got := h.Get("Retry-After"); got != "" {
		t.Errorf("an allowed request should carry no Retry-After, got %q", got)
	}
}

func TestNoChecksAllows(t *testing.T) {
	t.Parallel()

	limiter, _, _ := setup(t)

	d, err := limiter.Allow(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Error("a caller who configured no limits is not rate limited")
	}
}

// A limiter that cannot read its counter must not quietly allow everything: the
// caller decides whether to fail open, and cannot decide what it is not told.
func TestACounterFailurePropagates(t *testing.T) {
	t.Parallel()

	want := errors.New("connection refused")
	limiter := throttle.New(brokenCounter{want})

	if _, err := limiter.Allow(context.Background(),
		throttle.Check{Limit: loginByEmail, Key: throttle.Email("sam@example.com")}); !errors.Is(err, want) {
		t.Errorf("err = %v, want the counter's failure", err)
	}
}

type brokenCounter struct{ err error }

func (b brokenCounter) Count(context.Context, throttle.Limit, throttle.Key, time.Time) (int, time.Time, error) {
	return 0, time.Time{}, b.err
}

// A key kind nobody mapped is a configuration mistake, and counting zero for it
// would make the limit look configured while enforcing nothing.
func TestAnUnmappedKeyKindIsAnError(t *testing.T) {
	t.Parallel()

	counter := throttle.NewPostgres(nil, throttle.DefaultPostgresConfig())

	_, _, err := counter.Count(context.Background(), loginByEmail,
		throttle.Key{Kind: "hostname", Value: "example.com"}, time.Now())
	if err == nil {
		t.Fatal("an unmapped key kind should be refused")
	}
	if !strings.Contains(err.Error(), "hostname") {
		t.Errorf("the error should name the kind: %v", err)
	}
}

// One valid login from a shared office address must not wipe the record of the
// failures coming from the same place.
func TestTheIPLimitIsNotClearedByASuccess(t *testing.T) {
	t.Parallel()

	limiter, counter, c := setup(t)
	std := throttle.Standard()
	ip := throttle.IP("203.0.113.10")

	for range 50 {
		counter.Record(throttle.EventLoginFailed, ip, c.at)
	}
	c.advance(time.Second)
	counter.Record(throttle.EventLoginSucceeded, ip, c.at)

	d, _ := limiter.Allow(context.Background(), throttle.Check{Limit: std.LoginByIP, Key: ip})
	if d.Allowed {
		t.Error("a success from one address should not clear that address's window")
	}
}

// The longest window is what anything deleting from the log has to clear, so it
// comes back with the name of the limit that set it: a refusal that says which
// limit it would have broken is one somebody can act on.
func TestTheLongestWindow(t *testing.T) {
	t.Parallel()

	longest, name := throttle.Standard().LongestWindow()
	if longest != time.Hour {
		t.Errorf("longest window = %s, want 1h", longest)
	}
	// Two of the standard limits are an hour; the first of them wins, and what
	// matters is that it is one of them rather than which.
	if name != "password.reset" && name != "verification.resend" {
		t.Errorf("longest window belongs to %q, want one of the hourly limits", name)
	}

	// A project that widened one is measured against what it widened it to.
	d := throttle.Standard()
	d.LoginByEmail.Window = 30 * 24 * time.Hour
	if longest, name = d.LongestWindow(); longest != 30*24*time.Hour || name != "login.email" {
		t.Errorf("longest window = %s (%s), want 720h (login.email)", longest, name)
	}

	// All is the set, and it has to stay the whole set: a limit missing from it
	// is a window nothing checks against.
	if n := len(throttle.Standard().All()); n != 6 {
		t.Errorf("All returned %d limits, want the six Defaults has", n)
	}
}

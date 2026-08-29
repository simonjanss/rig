package throttle_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/throttle"
)

// tally is an in-process stand-in for the rig_throttle table. It implements the
// same weighted-bucket arithmetic the SQL does, so a test of what sits in front
// of a Recorder is a test of what will happen in production.
type tally struct {
	mu      sync.Mutex
	buckets map[string]int
	calls   int
	err     error
}

func newTally() *tally { return &tally{buckets: map[string]int{}} }

func (t *tally) key(limit throttle.Limit, k throttle.Key, at time.Time) string {
	return limit.Name + "\x00" + k.Kind + "\x00" + k.Value + "\x00" + at.UTC().Format(time.RFC3339Nano)
}

func (t *tally) Incr(_ context.Context, limit throttle.Limit, k throttle.Key, now time.Time, delta int) (int, time.Time, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.calls++
	if t.err != nil {
		return 0, time.Time{}, t.err
	}

	now = now.UTC()
	bucket := now.Truncate(limit.Window)
	prev := bucket.Add(-limit.Window)

	t.buckets[t.key(limit, k, bucket)] += delta

	weight := 1 - float64(now.Sub(bucket))/float64(limit.Window)
	total := t.buckets[t.key(limit, k, bucket)] + int(float64(t.buckets[t.key(limit, k, prev)])*weight+0.5)
	return total, bucket.Add(limit.Window), nil
}

func (t *tally) roundTrips() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

var perAccount = throttle.Limit{Name: "api.account", Max: 10, Window: time.Minute}

func TestTakeAllowsExactlyMax(t *testing.T) {
	t.Parallel()

	c := newClock()
	limiter := throttle.NewRecording(newTally()).WithClock(c.now)
	key := throttle.Account("acct-1")

	for i := range perAccount.Max {
		d, err := limiter.Take(context.Background(), throttle.Check{Limit: perAccount, Key: key})
		if err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
		if !d.Allowed {
			t.Fatalf("call %d of %d was refused", i+1, perAccount.Max)
		}
	}

	d, err := limiter.Take(context.Background(), throttle.Check{Limit: perAccount, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatalf("call %d was allowed, the limit is %d", perAccount.Max+1, perAccount.Max)
	}
	if d.RetryAfter <= 0 {
		t.Errorf("a refusal with no wait: %v", d.RetryAfter)
	}
}

// The off-by-one between the two verbs is the thing most likely to be wrong, so
// it gets a test that names it: Allow counts what happened before this request
// and Take counts this one too, and both have to let exactly Max through.
func TestTakeAndAllowAgreeOnHowManyGetThrough(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	limit := throttle.Limit{Name: "x", Event: "E", Max: 3, Window: time.Minute}

	c := newClock()
	counter := throttle.NewMemory()
	reading := throttle.New(counter).WithClock(c.now)
	writing := throttle.NewRecording(newTally()).WithClock(c.now)
	key := throttle.Account("acct-1")

	var allowed, taken int
	for range limit.Max + 3 {
		if d, err := reading.Allow(ctx, throttle.Check{Limit: limit, Key: key}); err == nil && d.Allowed {
			allowed++
			counter.Record(limit.Event, key, c.at)
		}
		if d, err := writing.Take(ctx, throttle.Check{Limit: limit, Key: key}); err == nil && d.Allowed {
			taken++
		}
	}

	if allowed != limit.Max || taken != limit.Max {
		t.Fatalf("Allow let %d through and Take let %d through; the limit is %d", allowed, taken, limit.Max)
	}
}

// Every check is spent even after one has refused. Otherwise somebody sitting
// over their per-account limit would stop accumulating against their tenant's,
// and could stay under every other budget by staying over one.
func TestTakeSpendsEveryCheckEvenAfterARefusal(t *testing.T) {
	t.Parallel()

	tight := throttle.Limit{Name: "tight", Max: 1, Window: time.Minute}
	loose := throttle.Limit{Name: "loose", Max: 100, Window: time.Minute}

	c := newClock()
	rec := newTally()
	limiter := throttle.NewRecording(rec).WithClock(c.now)
	key := throttle.Account("acct-1")

	ctx := context.Background()
	for range 5 {
		_, err := limiter.Take(ctx,
			throttle.Check{Limit: tight, Key: key},
			throttle.Check{Limit: loose, Key: key})
		if err != nil {
			t.Fatal(err)
		}
	}

	d, err := limiter.Take(ctx, throttle.Check{Limit: loose, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if d.Used != 6 {
		t.Fatalf("the loose limit counted %d of 6 calls — refusals on the other check stopped counting here", d.Used)
	}
}

func TestTakeReturnsTheTightestCheck(t *testing.T) {
	t.Parallel()

	tight := throttle.Limit{Name: "tight", Max: 1, Window: time.Minute}
	loose := throttle.Limit{Name: "loose", Max: 100, Window: time.Minute}

	c := newClock()
	limiter := throttle.NewRecording(newTally()).WithClock(c.now)
	key := throttle.Account("acct-1")

	ctx := context.Background()
	_, _ = limiter.Take(ctx, throttle.Check{Limit: loose, Key: key}, throttle.Check{Limit: tight, Key: key})

	d, err := limiter.Take(ctx,
		throttle.Check{Limit: loose, Key: key},
		throttle.Check{Limit: tight, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("the tight limit should have refused")
	}
	if d.Limit.Name != "tight" {
		t.Fatalf("the decision names %q, not the limit that refused", d.Limit.Name)
	}
}

func TestTheWindowSlidesRatherThanStepping(t *testing.T) {
	t.Parallel()

	c := newClock()
	limiter := throttle.NewRecording(newTally()).WithClock(c.now)
	key := throttle.Account("acct-1")
	ctx := context.Background()

	// Spend the whole budget at the very end of a bucket.
	c.at = c.at.Truncate(time.Minute).Add(59 * time.Second)
	for range perAccount.Max {
		if _, err := limiter.Take(ctx, throttle.Check{Limit: perAccount, Key: key}); err != nil {
			t.Fatal(err)
		}
	}

	// One second later the bucket has rolled. A fixed window would hand out a
	// whole fresh budget here, which is the boundary burst this design exists
	// to avoid: the previous bucket still counts almost in full.
	c.advance(time.Second)
	d, err := limiter.Take(ctx, throttle.Check{Limit: perAccount, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("a fresh budget one second after spending the last one — the window stepped instead of sliding")
	}

	// Late in the new bucket the old one has decayed and the budget is back.
	c.advance(58 * time.Second)
	d, err = limiter.Take(ctx, throttle.Check{Limit: perAccount, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if !d.Allowed {
		t.Fatal("the old bucket never decayed out of the window")
	}
}

func TestTakeWithoutARecorderIsAnError(t *testing.T) {
	t.Parallel()

	limiter := throttle.New(throttle.NewMemory())
	d, err := limiter.Take(context.Background(), throttle.Check{Limit: perAccount, Key: throttle.Account("a")})
	if err == nil {
		t.Fatal("a limiter with no recorder answered Take")
	}
	if d.Allowed {
		t.Fatal("the failed decision was allowed, which is the permissive default this must not have")
	}
	if !strings.Contains(err.Error(), "NewRecording") {
		t.Errorf("the error does not say what to do: %v", err)
	}
}

func TestAllowWithoutACounterIsAnError(t *testing.T) {
	t.Parallel()

	limiter := throttle.NewRecording(newTally())
	d, err := limiter.Allow(context.Background(), throttle.Check{Limit: perAccount, Key: throttle.Account("a")})
	if err == nil {
		t.Fatal("a limiter with no counter answered Allow")
	}
	if d.Allowed {
		t.Fatal("the failed decision was allowed, which is the permissive default this must not have")
	}
}

func TestTakePropagatesTheRecorderError(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection refused")
	rec := newTally()
	rec.err = boom

	limiter := throttle.NewRecording(rec)
	if _, err := limiter.Take(context.Background(), throttle.Check{Limit: perAccount, Key: throttle.Account("a")}); !errors.Is(err, boom) {
		t.Fatalf("the recorder's error did not reach the caller: %v", err)
	}
}

// One clock reading for the whole call, however many checks it carries. Two
// limits read a microsecond apart would measure their windows from different
// instants, and the tightest of them would hand out a Retry-After derived from a
// now that the decision beside it never saw.
func TestOneClockReadingPerCall(t *testing.T) {
	t.Parallel()

	c := newClock()
	var readings int
	limiter := throttle.NewRecording(newTally()).WithClock(func() time.Time {
		readings++
		return c.now()
	})

	checks := []throttle.Check{
		{Limit: perAccount, Key: throttle.Account("acct-1")},
		{Limit: throttle.Limit{Name: "api.tenant", Max: 100, Window: time.Minute},
			Key: throttle.Account("acct-1")},
		{Limit: throttle.Limit{Name: "api.ip", Max: 50, Window: time.Hour},
			Key: throttle.Account("acct-1")},
	}
	if _, err := limiter.Take(context.Background(), checks...); err != nil {
		t.Fatal(err)
	}

	if readings != 1 {
		t.Errorf("the clock was read %d times for %d checks, want once for the call",
			readings, len(checks))
	}
}

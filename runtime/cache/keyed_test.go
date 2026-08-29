package cache_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/cache"
)

// The four decisions [cache.Keyed] exists to spell once, asserted once. Each was
// written out four times in auth before this type, and the fourth copy is where a
// decision like this drifts.

var errMiss = errors.New("keyed_test: no such thing")

// served is a cache attached to no channel: it holds and forgets in this process
// alone, which is what [cache.Keyed.ServeLocally] names.
func served[V any](cfg cache.KeyedConfig[V]) *cache.Keyed[V] {
	k := cache.NewKeyed(cfg)
	k.ServeLocally()
	return k
}

// A nil receiver reads through. That is what a cache somebody decided against
// looks like, and it is why no call site withdrawing a value needs a condition.
func TestANilKeyedReadsThrough(t *testing.T) {
	t.Parallel()

	var k *cache.Keyed[string]
	calls := 0
	for range 3 {
		got, err := k.Load("a", func() (string, error) { calls++; return "v", nil })
		if err != nil || got != "v" {
			t.Fatalf("Load = %q, %v", got, err)
		}
	}
	if calls != 3 {
		t.Errorf("a nil cache answered from somewhere: %d calls for 3 loads", calls)
	}

	// And every other method is a no-op rather than a panic.
	if err := k.Forget(context.Background(), nil, "a"); err != nil {
		t.Errorf("Forget on nil: %v", err)
	}
	if err := k.ForgetOrDrop(context.Background(), "a"); err != nil {
		t.Errorf("ForgetOrDrop on nil: %v", err)
	}
	if err := k.Clear(context.Background(), nil); err != nil {
		t.Errorf("Clear on nil: %v", err)
	}
	k.Drop("a")
	if n := k.Len(); n != 0 {
		t.Errorf("Len on nil = %d", n)
	}
}

// A cache that has not been attached to anything holds nothing, because an answer
// that cannot be withdrawn is not one to keep. Fail-safe: forgetting to serve it
// costs latency rather than correctness.
func TestAnUnattachedKeyedHoldsNothing(t *testing.T) {
	t.Parallel()

	k := cache.NewKeyed(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	calls := 0
	for range 3 {
		if _, err := k.Load("a", func() (string, error) { calls++; return "v", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Errorf("an unattached cache held an answer: %d calls for 3 loads", calls)
	}
	if n := k.Len(); n != 0 {
		t.Errorf("it holds %d entries", n)
	}
}

// And once served it holds.
func TestServeLocallyHolds(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	calls := 0
	for range 3 {
		if _, err := k.Load("a", func() (string, error) { calls++; return "v", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Errorf("three loads asked the store %d times, want 1", calls)
	}
}

// A failure is never held. That is the seam a miss borrows: a loader that turns
// "no such row" into a sentinel produces the answer without anything remembering
// it — which matters most where the key is attacker-supplied, because caching
// "no such token" would let anybody fill the map with invented identifiers and
// evict every real entry in it.
func TestAMissIsNotHeld(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	calls := 0
	for range 3 {
		_, err := k.Load("nope", func() (string, error) { calls++; return "", errMiss })
		if !errors.Is(err, errMiss) {
			t.Fatalf("Load = %v, want the sentinel", err)
		}
	}
	if calls != 3 {
		t.Errorf("a miss was held: %d calls for 3 loads", calls)
	}
	if n := k.Len(); n != 0 {
		t.Errorf("the map holds %d entries after three misses", n)
	}
}

// A held value is copied on the way out. Without a cache every caller got its own
// row out of the store, so with one every caller has to get its own value — or
// the cache would have changed something other than where the read happened.
func TestTheValueIsClonedOnTheWayOut(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[[]string]{
		Topic: "t", TTL: time.Hour,
		Clone: func(v []string) []string { return slices.Clone(v) },
	})
	load := func() ([]string, error) {
		return k.Load("a", func() ([]string, error) { return []string{"read"}, nil })
	}

	first, err := load()
	if err != nil {
		t.Fatal(err)
	}
	first[0] = "written through"

	second, err := load()
	if err != nil {
		t.Fatal(err)
	}
	if second[0] != "read" {
		t.Errorf("one caller wrote through the held value: the next got %q", second[0])
	}
}

// A nil Clone is identity, which is right for a value with nothing a caller could
// write through — `struct{}` being the one in auth.
func TestANilCloneIsIdentity(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[struct{}]{Topic: "t", TTL: time.Hour})
	if _, err := k.Load("a", func() (struct{}, error) { return struct{}{}, nil }); err != nil {
		t.Fatal(err)
	}
	if n := k.Len(); n != 1 {
		t.Errorf("holds %d, want 1", n)
	}
}

// Drop forgets in this process only. The wrong tool for a revocation and the right
// one for a caller with no transaction to publish on.
func TestDropForgetsLocally(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	calls := 0
	load := func() { _, _ = k.Load("a", func() (string, error) { calls++; return "v", nil }) }

	load()
	load()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	k.Drop("a")
	load()
	if calls != 2 {
		t.Errorf("after Drop the next load asked the store %d times, want 2", calls)
	}
}

// Drop is variadic, which is what absorbs session's "revoke a family" — a list of
// identifiers rather than one.
func TestDropIsVariadic(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	for _, key := range []string{"a", "b", "c"} {
		if _, err := k.Load(key, func() (string, error) { return "v", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if n := k.Len(); n != 3 {
		t.Fatalf("holds %d, want 3", n)
	}
	k.Drop("a", "b", "c")
	if n := k.Len(); n != 0 {
		t.Errorf("holds %d after dropping all three, want 0", n)
	}
}

// With no transaction on the context, ForgetOrDrop drops locally: a store that is
// not Postgres has no channel to publish on and no other replica to reach.
func TestForgetOrDropWithNoTransactionDropsLocally(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	calls := 0
	load := func() { _, _ = k.Load("a", func() (string, error) { calls++; return "v", nil }) }

	load()
	load()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	if err := k.ForgetOrDrop(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	load()
	if calls != 2 {
		t.Errorf("after ForgetOrDrop the next load asked the store %d times, want 2", calls)
	}
}

// A cache served on no channel has nothing to publish on, so Forget and Clear
// drop what they hold directly rather than doing nothing.
//
// Publishing nowhere *and* keeping the value is the one arrangement where a
// withdrawal silently does not happen — and it is the arrangement a
// single-instance deployment is in, which is what [cache.Keyed.ServeLocally]
// names. A cache that holds has to be a cache that can forget.
func TestForgetOnALocalCacheDropsLocally(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	calls := 0
	load := func(key string) {
		_, _ = k.Load(key, func() (string, error) { calls++; return "v", nil })
	}

	load("a")
	load("a")
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}

	// The db is nil on purpose: there is no channel here, so nothing may reach
	// for one.
	if err := k.Forget(context.Background(), nil, "a"); err != nil {
		t.Fatalf("Forget: %v", err)
	}
	load("a")
	if calls != 2 {
		t.Errorf("after Forget the next load asked the store %d times, want 2", calls)
	}

	load("b")
	if n := k.Len(); n != 2 {
		t.Fatalf("holds %d, want 2", n)
	}
	if err := k.Clear(context.Background(), nil); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	if n := k.Len(); n != 0 {
		t.Errorf("holds %d after Clear, want 0", n)
	}
}

// And a cache that was never served has nothing to drop either way, so both are
// no-ops rather than a nil dereference.
func TestForgetOnAnUnservedCacheIsANoOp(t *testing.T) {
	t.Parallel()

	k := cache.NewKeyed(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	if err := k.Forget(context.Background(), nil, "a"); err != nil {
		t.Errorf("Forget: %v", err)
	}
	if err := k.Clear(context.Background(), nil); err != nil {
		t.Errorf("Clear: %v", err)
	}
}

// Zero TTL caches nothing, so "off" stays off. A caller with a default of its own
// resolves it before it gets here.
func TestAZeroTTLHoldsNothing(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[string]{Topic: "t"})
	calls := 0
	for range 3 {
		if _, err := k.Load("a", func() (string, error) { calls++; return "v", nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 3 {
		t.Errorf("a zero TTL held an answer: %d calls for 3 loads", calls)
	}
}

// Many goroutines asking at once get one answer, which is [cache.Map]'s property
// and has to survive the wrapper. Run under -race.
func TestManyGoroutinesGetOneAnswerThroughAKeyed(t *testing.T) {
	t.Parallel()

	k := served(cache.KeyedConfig[string]{Topic: "t", TTL: time.Hour})
	var mu sync.Mutex
	calls := 0

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = k.Load("a", func() (string, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				return "v", nil
			})
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("fifty concurrent loads asked the store %d times, want 1", calls)
	}
}

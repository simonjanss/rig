package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/cache"
)

// [cache.RowCache] used to be a hundred lines emitted into every generated
// store, where the only thing that could check it was a golden file — a
// comparison that proves the text did not move and says nothing about what it
// does. These are the properties.
//
// What cannot be asserted here is the half that needs Postgres: that a
// withdrawal published on the writing transaction reaches a second replica, and
// is thrown away when that transaction rolls back. `internal/authtest` asks that
// of [cache.Keyed], which is what this is built on, and the generated stores'
// own Docker suites ask it end to end.

func rowCache(ttl time.Duration) *cache.RowCache[string] {
	return cache.NewRowCache[string](cache.RowCacheConfig{
		Topic: "thing", TTL: ttl, MaxEntries: 100,
	})
}

// servedLocally is a cache that holds with no channel behind it — see
// [cache.ServeLocallyForTest], which is the only way to reach that state and
// says why it is not on the type.
func servedLocally(t *testing.T) *cache.RowCache[string] {
	t.Helper()

	c := rowCache(time.Hour)
	cache.ServeLocallyForTest(c)
	return c
}

// The load-bearing default, and the one that separates this from
// [cache.Keyed.ServeLocally]: a row cache attached to no channel holds nothing.
//
// A `cache:` block is over the application's own rows, written by handlers that
// run everywhere. A plain time-to-live over those is the trade the whole
// mechanism exists to refuse, so an unserved cache reads through — which costs
// queries — rather than holding what nothing can withdraw.
func TestAnUnservedRowCacheHoldsNothing(t *testing.T) {
	t.Parallel()

	c := rowCache(time.Hour)
	calls := 0
	load := func() { _, _ = c.Load("a", func() (string, error) { calls++; return "v", nil }) }

	load()
	load()
	if calls != 2 {
		t.Errorf("an unserved cache asked the store %d times, want 2", calls)
	}
}

// A nil bus is the same as never serving it. There is no arrangement in which a
// row cache holds a row it cannot withdraw.
func TestServingARowCacheOnNoBusLeavesItDead(t *testing.T) {
	t.Parallel()

	c := rowCache(time.Hour)
	c.Serve(nil)

	calls := 0
	load := func() { _, _ = c.Load("a", func() (string, error) { calls++; return "v", nil }) }

	load()
	load()
	if calls != 2 {
		t.Errorf("a cache served on no bus asked the store %d times, want 2", calls)
	}
}

// A nil receiver reads through, so a store built without caching needs no
// condition around any of this.
func TestANilRowCacheReadsThroughAndForgetsNothing(t *testing.T) {
	t.Parallel()

	var c *cache.RowCache[string]

	v, err := c.Load("a", func() (string, error) { return "v", nil })
	if err != nil || v != "v" {
		t.Fatalf("Load = (%q, %v)", v, err)
	}
	if err := c.Forget(context.Background(), "a"); err != nil {
		t.Errorf("Forget on a nil cache = %v", err)
	}
	c.Serve(nil)
}

// With no transaction on the context there is nothing to publish on, and the
// local drop is all there is. [dbx.AfterCommit] runs it immediately, which is
// the whole of what is available and the whole of what is needed.
func TestForgetWithNoTransactionStillDropsLocally(t *testing.T) {
	t.Parallel()

	c := servedLocally(t)
	calls := 0
	load := func() { _, _ = c.Load("a", func() (string, error) { calls++; return "v", nil }) }

	load()
	load()
	if calls != 1 {
		t.Fatalf("a served cache asked the store %d times, want 1", calls)
	}

	if err := c.Forget(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	load()
	if calls != 2 {
		t.Errorf("after Forget the next load asked the store %d times, want 2", calls)
	}
}

// Forgetting a key nothing holds is not an error, which is what a delete of a
// row nobody had read looks like.
func TestForgettingAKeyThatIsNotHeldIsFine(t *testing.T) {
	t.Parallel()

	if err := servedLocally(t).Forget(context.Background(), "never-read"); err != nil {
		t.Errorf("Forget = %v", err)
	}
}

func TestAZeroTTLRowCacheHoldsNothing(t *testing.T) {
	t.Parallel()

	c := cache.NewRowCache[string](cache.RowCacheConfig{Topic: "thing", MaxEntries: 100})
	calls := 0
	load := func() { _, _ = c.Load("a", func() (string, error) { calls++; return "v", nil }) }

	load()
	load()
	if calls != 2 {
		t.Errorf("a zero-TTL cache asked the store %d times, want 2", calls)
	}
}

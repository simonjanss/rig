package cache_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/cache"
)

// clock is a hand-wound one, because what is worth testing about a cache is the
// edge of its window and a test that sleeps to reach it fails on a loaded
// machine.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock() *clock {
	return &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// counted is a loader that says how many times it was actually called.
func counted(v string, n *int) func() (string, error) {
	return func() (string, error) {
		*n++
		return v, nil
	}
}

func TestALoadInsideTheWindowAsksOnce(t *testing.T) {
	t.Parallel()

	clk := newClock()
	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: clk.now})

	asked := 0
	for range 5 {
		got, err := m.Load("k", counted("answer", &asked))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got != "answer" {
			t.Fatalf("got %q, want %q", got, "answer")
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times, want 1", asked)
	}
}

func TestALoadPastTheWindowAsksAgain(t *testing.T) {
	t.Parallel()

	clk := newClock()
	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: clk.now})

	asked := 0
	if _, err := m.Load("k", counted("first", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}

	// One nanosecond inside the window is still inside it: the entry is good
	// until its expiry, not up to it.
	clk.advance(time.Minute - 1)
	if _, err := m.Load("k", counted("first", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if asked != 1 {
		t.Fatalf("asked %d times before the window closed, want 1", asked)
	}

	clk.advance(1)
	got, err := m.Load("k", counted("second", &asked))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "second" {
		t.Errorf("got %q after the window closed, want %q", got, "second")
	}
	if asked != 2 {
		t.Errorf("asked %d times, want 2", asked)
	}
}

// A zero time-to-live is how a project turns the cache off by changing a number
// rather than by unpicking its wiring, so it has to be a pass-through and not an
// entry that expires immediately.
func TestAZeroTTLAsksEveryTime(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: 0, Now: newClock().now})

	asked := 0
	for range 3 {
		if _, err := m.Load("k", counted("answer", &asked)); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if asked != 3 {
		t.Errorf("asked %d times, want 3", asked)
	}
	if m.Len() != 0 {
		t.Errorf("held %d entries, want none", m.Len())
	}
}

// A database that was unreachable for one request does not get to decide the
// answer for the rest of the window.
func TestAFailureIsNeverKept(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})

	sad := errors.New("the database is having a moment")
	if _, err := m.Load("k", func() (string, error) { return "", sad }); !errors.Is(err, sad) {
		t.Fatalf("got %v, want %v", err, sad)
	}
	if m.Len() != 0 {
		t.Fatalf("held %d entries after a failure, want none", m.Len())
	}

	asked := 0
	got, err := m.Load("k", counted("answer", &asked))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "answer" || asked != 1 {
		t.Errorf("got %q after %d asks, want %q after 1", got, asked, "answer")
	}
}

// The value is handed back as it was stored rather than copied, so a V holding a
// slice shares one backing array with every caller inside the window. Pinned
// because it is a property callers have to work around — by cloning inside the
// loader — rather than one they can discover.
func TestAValueIsSharedRatherThanCopied(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[[]string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})
	read := func() ([]string, error) { return []string{"note.read"}, nil }

	first, err := m.Load("k", read)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	first[0] = "note.write"

	second, err := m.Load("k", read)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if second[0] != "note.write" {
		t.Errorf("got %q on the second load, want %q: one caller's edit reaches the next",
			second[0], "note.write")
	}
}

func TestForgettingAKeyMakesTheNextLoadAsk(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})

	asked := 0
	if _, err := m.Load("kept", counted("a", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := m.Load("dropped", counted("b", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}

	m.Forget("dropped")

	if _, err := m.Load("dropped", counted("b", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if asked != 3 {
		t.Errorf("asked %d times, want 3", asked)
	}

	// And the key nobody forgot is still there. Forget takes one key, even
	// though it also discards loads in flight.
	before := asked
	if _, err := m.Load("kept", counted("a", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if asked != before {
		t.Errorf("forgetting one key also dropped another")
	}
}

// Forgetting something that was never held is what most replicas do with any
// given notification.
func TestForgettingWhatWasNeverHeldIsFine(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})
	m.Forget("never-seen")
	m.Clear()
}

func TestClearingDropsEverything(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})

	asked := 0
	for _, k := range []string{"a", "b", "c"} {
		if _, err := m.Load(k, counted(k, &asked)); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if m.Len() != 3 {
		t.Fatalf("held %d entries, want 3", m.Len())
	}

	m.Clear()

	if m.Len() != 0 {
		t.Errorf("held %d entries after a clear, want none", m.Len())
	}
	if _, err := m.Load("a", counted("a", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if asked != 4 {
		t.Errorf("asked %d times, want 4", asked)
	}
}

// The race this cache exists to survive: a role changes while somebody is
// reading the old answer, so the notification is applied before the read comes
// back. Without the generation count the stale answer is written into a map that
// has just been told to forget it and then survives its whole window — the one
// request that mattered being the one the cache gets wrong.
func TestAKeyForgottenDuringALoadIsNotKept(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})

	got, err := m.Load("k", func() (string, error) {
		m.Forget("k")
		return "stale", nil
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// The caller is answered: what was read was true when it was read. What is
	// in doubt is only whether it may be reused.
	if got != "stale" {
		t.Errorf("got %q, want %q", got, "stale")
	}
	if m.Len() != 0 {
		t.Fatalf("held %d entries, want none", m.Len())
	}

	asked := 0
	again, err := m.Load("k", counted("fresh", &asked))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if again != "fresh" || asked != 1 {
		t.Errorf("got %q after %d asks, want %q after 1", again, asked, "fresh")
	}
}

// A Clear landing mid-load is the same hazard arriving by the other route: it is
// what a bus does the moment it reconnects, and the reconnect is exactly when a
// read in flight is most likely to predate something.
func TestAClearDuringALoadIsNotKept(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})

	if _, err := m.Load("k", func() (string, error) {
		m.Clear()
		return "stale", nil
	}); err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Len() != 0 {
		t.Errorf("held %d entries, want none", m.Len())
	}
}

// Past the bound the whole map goes, rather than the process growing to a size
// whoever is calling gets to choose.
func TestTheMapIsBounded(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, MaxEntries: 2, Now: newClock().now})

	asked := 0
	for _, k := range []string{"a", "b"} {
		if _, err := m.Load(k, counted(k, &asked)); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if m.Len() != 2 {
		t.Fatalf("held %d entries, want 2", m.Len())
	}

	if _, err := m.Load("c", counted("c", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Len() != 1 {
		t.Fatalf("held %d entries at the bound, want the map dropped and 1 kept", m.Len())
	}

	// The observable consequence, which is the only thing worth asserting: what
	// was dropped is asked for again.
	if _, err := m.Load("a", counted("a", &asked)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if asked != 4 {
		t.Errorf("asked %d times, want 4", asked)
	}
}

// A cache with no invalidation reaching it serves nothing, because permissions
// nobody can withdraw are worse than permissions that take a round trip.
func TestACacheThatIsNotLiveAsksEveryTime(t *testing.T) {
	t.Parallel()

	var live atomic.Bool
	m := cache.NewMap[string](cache.MapConfig{
		TTL:  time.Minute,
		Live: live.Load,
		Now:  newClock().now,
	})

	asked := 0
	for range 3 {
		if _, err := m.Load("k", counted("answer", &asked)); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if asked != 3 {
		t.Errorf("asked %d times while not live, want 3", asked)
	}
	if m.Len() != 0 {
		t.Errorf("held %d entries while not live, want none", m.Len())
	}

	live.Store(true)

	for range 3 {
		if _, err := m.Load("k", counted("answer", &asked)); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if asked != 4 {
		t.Errorf("asked %d times once live, want 4", asked)
	}
}

// A nil Live is a cache with no invalidation channel at all: a time-to-live and
// nothing else, which is what a single-process deployment configures.
func TestANilLiveCaches(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})

	asked := 0
	for range 3 {
		if _, err := m.Load("k", counted("answer", &asked)); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times, want 1", asked)
	}
}

// Worth its keep only under -race, which `make test` does not pass. The comment
// is the reminder: run it by hand when this file changes.
func TestManyGoroutinesGetOneAnswer(t *testing.T) {
	t.Parallel()

	m := cache.NewMap[string](cache.MapConfig{TTL: time.Minute, Now: newClock().now})

	var asked atomic.Int64
	var wg sync.WaitGroup
	for i := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("k%d", i%4)
			for range 32 {
				got, err := m.Load(key, func() (string, error) {
					asked.Add(1)
					return key, nil
				})
				if err != nil {
					t.Errorf("load: %v", err)
					return
				}
				if got != key {
					t.Errorf("got %q for %q", got, key)
					return
				}
			}
			m.Forget(key)
		}()
	}
	wg.Wait()

	// Not an exact count: concurrent misses on a cold key all ask, which is
	// documented and is what the same requests cost with no cache at all. What
	// matters is that it is bounded well under the 2048 loads.
	if n := asked.Load(); n > 512 {
		t.Errorf("asked %d times for 2048 loads of 4 keys, want far fewer", n)
	}
}

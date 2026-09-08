package cache

import (
	"sync"
	"testing"
	"time"
)

// The register of loads in flight, asserted from inside the package because
// there is nothing exported that can see it — and it has to be asserted
// somewhere, because "bounded by the loads running right now" is the claim that
// stands in for the eviction policy [Map]'s doc comment used to say a per-key
// count would need. A leak here is not a slow map; it is a map that grows for
// the life of the process with nothing to trim it.

func TestTheRegisterOfLoadsInFlightEmptiesItself(t *testing.T) {
	t.Parallel()

	m := NewMap[string](MapConfig{TTL: time.Minute})

	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := string(rune('a' + i%8))
			for range 8 {
				if _, err := m.Load(key, func() (string, error) { return key, nil }); err != nil {
					t.Errorf("load: %v", err)
					return
				}
				m.Forget(key)
			}
		}()
	}
	wg.Wait()

	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.flights) != 0 {
		t.Errorf("%d loads still registered after every one of them returned", len(m.flights))
	}
}

// A loader that panics is the case the defer is for, and the only one where
// leaving a registration behind is invisible: the process survives — a server
// recovers per request, and [dbx] expects a swallowed panic to leave the process
// running — so the registration stays for the life of the process, and the
// bound that stands in for this map's eviction policy goes with it.
func TestALoaderThatPanicsLeavesNothingInFlight(t *testing.T) {
	t.Parallel()

	m := NewMap[string](MapConfig{TTL: time.Minute})

	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic did not come back out of Load")
			}
		}()
		_, _ = m.Load("k", func() (string, error) { panic("cache: the loader gave up") })
	}()

	m.mu.RLock()
	registered := len(m.flights)
	m.mu.RUnlock()
	if registered != 0 {
		t.Errorf("%d loads still registered after one panicked", registered)
	}

	// Not the leak — the count above is the only witness to that — but whether
	// the map came out of a panic usable at all. A loader that died with m.mu
	// held is the other way this goes wrong, and it would hang here rather
	// than fail.
	asked := 0
	load := func() (string, error) { asked++; return "v", nil }
	for range 2 {
		if _, err := m.Load("k", load); err != nil {
			t.Fatalf("load: %v", err)
		}
	}
	if asked != 1 {
		t.Errorf("asked %d times after a panic on this key, want 1", asked)
	}
}

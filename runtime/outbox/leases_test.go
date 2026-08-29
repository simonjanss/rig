package outbox_test

import (
	"slices"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/outbox"
)

// The zero value is usable, so neither dispatcher needs a constructor line for
// this in a struct that has enough of them.
func TestTheZeroValueIsUsable(t *testing.T) {
	t.Parallel()

	var l outbox.Leases
	if n := l.Len(); n != 0 {
		t.Errorf("Len = %d on a zero value", n)
	}
	if ids := l.IDs(); len(ids) != 0 {
		t.Errorf("IDs = %v on a zero value", ids)
	}
	// Dropping and clearing what was never held is not an error.
	l.Drop(uuid.New())
	l.Clear()

	id := uuid.New()
	l.Hold(id)
	if n := l.Len(); n != 1 {
		t.Errorf("Len = %d after one Hold, want 1", n)
	}
}

func TestHoldAndDropAreVariadic(t *testing.T) {
	t.Parallel()

	var l outbox.Leases
	ids := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	l.Hold(ids...)
	if n := l.Len(); n != 3 {
		t.Fatalf("Len = %d, want 3", n)
	}

	l.Drop(ids[0], ids[1])
	if n := l.Len(); n != 1 {
		t.Errorf("Len = %d after dropping two, want 1", n)
	}
	if got := l.IDs(); len(got) != 1 || got[0] != ids[2] {
		t.Errorf("IDs = %v, want just %v", got, ids[2])
	}
}

// Holding the same id twice holds it once, which is what a pass re-claiming a row
// it already had should cost.
func TestHoldingTwiceHoldsOnce(t *testing.T) {
	t.Parallel()

	var l outbox.Leases
	id := uuid.New()
	l.Hold(id, id, id)
	if n := l.Len(); n != 1 {
		t.Errorf("Len = %d, want 1", n)
	}
}

// The returned slice is the caller's: a dispatcher passes it straight into a
// statement, and something writing through it into the map would be a lease this
// process thinks it does not hold.
func TestIDsIsASnapshot(t *testing.T) {
	t.Parallel()

	var l outbox.Leases
	ids := []uuid.UUID{uuid.New(), uuid.New()}
	l.Hold(ids...)

	got := l.IDs()
	slices.Reverse(got)
	got[0] = uuid.New()

	after := l.IDs()
	if len(after) != 2 {
		t.Fatalf("the map holds %d after the snapshot was written through, want 2", len(after))
	}
	for _, want := range ids {
		if !slices.Contains(after, want) {
			t.Errorf("%v is no longer held", want)
		}
	}
}

func TestClearDropsEverything(t *testing.T) {
	t.Parallel()

	var l outbox.Leases
	l.Hold(uuid.New(), uuid.New(), uuid.New())
	l.Clear()
	if n := l.Len(); n != 0 {
		t.Errorf("Len = %d after Clear, want 0", n)
	}
}

// Two passes can run at once — an in-process goroutine and a cron task both
// calling the same dispatch — so every method has to be safe together. Run under
// -race; without the lock this is where it shows.
func TestConcurrentUse(t *testing.T) {
	t.Parallel()

	var l outbox.Leases
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := uuid.New()
			l.Hold(id)
			_ = l.IDs()
			_ = l.Len()
			l.Drop(id)
		}()
	}
	wg.Wait()

	if n := l.Len(); n != 0 {
		t.Errorf("Len = %d after every goroutine dropped its own, want 0", n)
	}
}

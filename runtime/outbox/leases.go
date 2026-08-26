package outbox

import (
	"sync"

	"github.com/google/uuid"
)

// Leases are the claims this process is holding, so a clean shutdown can give
// them back rather than leaving them to expire.
//
// The lease TTL is for crashes. A process that knows it is going has no excuse
// for being slow about saying so, and leaving its claims turns every ordinary
// rollout into a delivery delay — repeatedly, for a rollout that replaces every
// pod.
//
// Its own lock rather than the caller's, because the two things a dispatcher
// guards — whether it is still claiming, and what it is holding — are never read
// together, and sharing a mutex only made that harder to see.
//
// Safe for concurrent use. The zero value is usable.
type Leases struct {
	mu   sync.Mutex
	held map[uuid.UUID]bool
}

// Hold records ids as claimed by this process.
func (l *Leases) Hold(ids ...uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = make(map[uuid.UUID]bool, len(ids))
	}
	for _, id := range ids {
		l.held[id] = true
	}
}

// Drop forgets ids, for rows whose outcome has been written.
func (l *Leases) Drop(ids ...uuid.UUID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		delete(l.held, id)
	}
}

// Clear forgets everything.
//
// [Leases.Drop] is what a pass giving back its own claims should reach for: two
// passes can run at once — an in-process goroutine and a cron task both calling
// the same dispatch — and clearing there would wipe the other one's claims,
// leaving them to expire on a TTL meant for crashes.
func (l *Leases) Clear() {
	l.mu.Lock()
	defer l.mu.Unlock()
	clear(l.held)
}

// IDs is a snapshot of what is held, in no order. The slice is the caller's.
func (l *Leases) IDs() []uuid.UUID {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]uuid.UUID, 0, len(l.held))
	for id := range l.held {
		out = append(out, id)
	}
	return out
}

// Len is how many are held.
func (l *Leases) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.held)
}

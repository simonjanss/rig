package throttle

import (
	"context"
	"sync"
	"time"
)

// Memory is an in-process Counter.
//
// It is here for tests, and for a single-process deployment that has decided it
// does not need what the database gives. It is not the default for a reason:
// two replicas holding separate counters enforce a limit twice as loose as the
// one configured, and neither of them can tell.
type Memory struct {
	mu     sync.Mutex
	events map[memKey][]time.Time
}

type memKey struct {
	event string
	kind  string
	value string
}

// NewMemory builds an empty counter.
func NewMemory() *Memory {
	return &Memory{events: make(map[memKey][]time.Time)}
}

// Record notes that an event happened. It stands in for the row the caller
// would otherwise have written to its auth log.
func (m *Memory) Record(event string, key Key, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	k := memKey{event, key.Kind, key.Value}
	m.events[k] = append(m.events[k], at)
}

// Count implements [Counter].
func (m *Memory) Count(_ context.Context, limit Limit, key Key, since time.Time) (int, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if limit.ClearedBy != "" {
		for _, at := range m.events[memKey{limit.ClearedBy, key.Kind, key.Value}] {
			if at.After(since) {
				since = at
			}
		}
	}

	var (
		n        int
		earliest time.Time
	)
	for _, at := range m.events[memKey{limit.Event, key.Kind, key.Value}] {
		if !at.After(since) {
			continue
		}
		n++
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return n, earliest, nil
}

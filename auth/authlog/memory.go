package authlog

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Memory keeps entries in a slice.
//
// It is for tests, and it is all three interfaces at once — [Log], [Reader] and
// [Pruner] — because the thing worth testing without a database is usually a
// round trip: something recorded an event, and something else showed it.
//
// What it cannot show is the half of this table that lives in SQL. The tenant
// predicate, the count that pagination reports, and the ordering that makes two
// pages of one query disjoint are all statements about a query, and a slice
// filtered in Go agrees with them by construction rather than by test. Those
// belong in the Docker suite, and they are asserted there.
type Memory struct {
	mu      sync.Mutex
	records []Record
	// seq numbers the entries, since nothing here has a clock of its own and an
	// entry carries the instant it was recorded at.
	seq uint64
}

// NewMemory builds an empty log.
func NewMemory() *Memory { return &Memory{} }

var (
	_ Log    = (*Memory)(nil)
	_ Reader = (*Memory)(nil)
	_ Pruner = (*Memory)(nil)
)

// Write implements [Log]. It cannot fail, which is the contract.
func (m *Memory) Write(_ context.Context, e Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.seq++
	// A v7 identifier would need a clock this type does not have; the sequence
	// is enough for a double, and it keeps the order tests write in readable.
	m.records = append(m.records, Record{ID: idOf(m.seq), Entry: e})
}

// Read implements [Reader].
func (m *Memory) Read(_ context.Context, q Query) ([]Record, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	matched := make([]Record, 0, len(m.records))
	for _, r := range m.records {
		if r.matches(q) {
			matched = append(matched, r)
		}
	}

	// Newest first, and the sequence breaks a tie, because a test that records
	// three entries at one instant should still read them back in an order.
	sort.SliceStable(matched, func(i, j int) bool {
		if !matched[i].At.Equal(matched[j].At) {
			return matched[i].At.After(matched[j].At)
		}
		return matched[i].ID.String() > matched[j].ID.String()
	})

	total := int64(len(matched))
	if q.Offset >= len(matched) {
		return nil, total, nil
	}
	matched = matched[q.Offset:]
	if q.Limit > 0 && len(matched) > q.Limit {
		matched = matched[:q.Limit]
	}
	return slices.Clone(matched), total, nil
}

// Prune implements [Pruner].
func (m *Memory) Prune(_ context.Context, olderThan time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	kept := make([]Record, 0, len(m.records))
	for _, r := range m.records {
		if r.At.Before(olderThan) {
			continue
		}
		kept = append(kept, r)
	}

	gone := len(m.records) - len(kept)
	m.records = kept
	return gone, nil
}

// matches is the same narrowing the SQL reader renders as predicates.
//
// The tenant is first and unconditional here too. An entry with no tenant — the
// attempts that resolved to nobody — matches nothing, which is the property this
// double exists to preserve alongside the real one.
func (r Record) matches(q Query) bool {
	if r.TenantID == nil || *r.TenantID != q.TenantID {
		return false
	}
	if q.AccountID != nil && (r.AccountID == nil || *r.AccountID != *q.AccountID) {
		return false
	}
	if q.Event != "" && r.Event != q.Event {
		return false
	}
	if q.Outcome != "" && r.Outcome != q.Outcome {
		return false
	}
	if !q.Since.IsZero() && r.At.Before(q.Since) {
		return false
	}
	if !q.Until.IsZero() && !r.At.Before(q.Until) {
		return false
	}
	return true
}

// idOf turns a sequence number into a stable identifier, so a test can say which
// entry it means.
func idOf(seq uint64) uuid.UUID {
	var id uuid.UUID
	for i := range 8 {
		id[15-i] = byte(seq >> (8 * i))
	}
	return id
}

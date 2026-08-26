package cache

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/simonjanss/rig/runtime/dbx"
)

// Keyed is a [Map] and the [Topic] that withdraws from it, as one thing.
//
// One type rather than a map and a topic side by side because the two are halves
// of one agreement: a bus serving "session" under one name and a revocation
// publishing under another is a cache that never invalidates. Registering the map
// and handing back the publisher in the same call types the name once, and leaves
// nowhere for it to drift.
//
// # Four decisions, spelled once
//
// Every method is safe on a nil receiver, which is what a cache somebody decided
// against looks like: [Keyed.Load] calls through, and the rest are no-ops. So no
// call site that withdraws a value needs a condition around it.
//
// A failure is never held. That is [Map]'s behaviour and this borrows it as a
// seam: a loader that turns "no such row" into a sentinel error produces the
// answer without anything remembering it. It matters most where the key is
// attacker-supplied — caching "no such token" would let anybody fill the map with
// invented identifiers and evict every real entry in it.
//
// A held value is copied on the way out, if [KeyedConfig.Clone] says how. [Map]
// recommends the opposite — clone in the loader, once per miss — and this
// contradicts it deliberately: without a cache every caller got its own row out
// of the store, so with one every caller has to get its own value, or the cache
// would have changed something other than where the read happened.
//
// And a cache that cannot withdraw holds nothing. Not yet attached and attached
// to a channel that has dropped are the same answer for different reasons, and
// both give it: the map reads through and stores nothing.
//
// Safe for concurrent use.
type Keyed[V any] struct {
	cfg    KeyedConfig[V]
	m      *Map[V]
	served atomic.Pointer[servedOn]
}

// servedOn is the bus a [Keyed] was attached to and the publisher it got back.
//
// One pointer swapped at once rather than two fields, so that nothing can observe
// a cache half-attached.
type servedOn struct {
	bus   *Bus
	topic *Topic
}

// KeyedConfig is what a [Keyed] needs.
type KeyedConfig[V any] struct {
	// Bus is the invalidation channel and Topic the name this cache's
	// invalidations travel under.
	//
	// A non-nil Bus is served immediately, and panics if that bus already serves
	// Topic — the wiring mistake of attaching two caches to one name. A nil Bus
	// builds a cache attached to nothing, which holds nothing until
	// [Keyed.Serve]: that is what a cache built before the bus it will run on
	// has, and what a project with no `cache:` block has for good.
	Bus   *Bus
	Topic string

	// TTL, MaxEntries and Now are [MapConfig]'s, unchanged.
	//
	// Zero TTL caches nothing. A caller with a default of its own resolves it
	// before it gets here, because "off" and "the package default" are different
	// answers and only the caller knows which it meant.
	TTL        time.Duration
	MaxEntries int
	Now        func() time.Time

	// Clone copies a value on the way out of [Keyed.Load]. Nil is identity, which
	// is right for a V with nothing a caller could write through.
	Clone func(V) V
}

// NewKeyed builds one, serving it on [KeyedConfig.Bus] if there is one.
func NewKeyed[V any](cfg KeyedConfig[V]) *Keyed[V] {
	k := &Keyed[V]{cfg: cfg}
	k.m = NewMap[V](MapConfig{
		TTL:        cfg.TTL,
		MaxEntries: cfg.MaxEntries,
		Live:       k.live,
		Now:        cfg.Now,
	})
	if cfg.Bus != nil {
		k.Serve(cfg.Bus)
	}
	return k
}

// Serve attaches the cache to a bus, so that what it holds can be withdrawn
// everywhere.
//
// Nothing is held until this runs. It panics if the bus already serves the
// configured topic, which is the wiring mistake of attaching two caches to one
// name.
func (k *Keyed[V]) Serve(bus *Bus) {
	if k == nil || bus == nil {
		return
	}
	k.served.Store(&servedOn{bus: bus, topic: bus.Serve(k.cfg.Topic, k.m)})
}

// ServeLocally attaches the cache to nothing, so it holds values and forgets them
// in this process alone.
//
// One process, no replicas, and staleness is the caller's problem. It exists
// because "attached to no channel" is a real posture and the alternative is a
// test reaching into an unexported field to reach the same state.
func (k *Keyed[V]) ServeLocally() {
	if k == nil {
		return
	}
	k.served.Store(&servedOn{})
}

// live reports whether an entry could be withdrawn if one were held.
func (k *Keyed[V]) live() bool {
	s := k.served.Load()
	switch {
	case s == nil:
		// Never served. There is no channel, so nothing could withdraw an entry
		// and nothing may be kept.
		return false
	case s.bus == nil:
		// Served on no bus at all — see [Keyed.ServeLocally].
		return true
	}
	return s.bus.Live()
}

// Load answers from the cache, or calls fn and keeps what it returns.
//
// A failure is never kept, which is the seam a miss borrows: see the type's own
// documentation. The sentinel is the caller's, because only the caller knows what
// to translate it back into.
//
// On a nil receiver fn's value is returned as it is — nothing was cached, so
// nothing needs copying.
func (k *Keyed[V]) Load(key string, fn func() (V, error)) (V, error) {
	if k == nil {
		return fn()
	}
	v, err := k.m.Load(key, fn)
	if err != nil {
		var zero V
		return zero, err
	}
	if k.cfg.Clone != nil {
		return k.cfg.Clone(v), nil
	}
	return v, nil
}

// Forget publishes an invalidation for each key, on db.
//
// Postgres delivers a notification when the transaction issuing it commits and
// discards it when that transaction rolls back, so an invalidation published on
// the transaction that caused it is atomic with the change. The publisher hears
// its own notification, which is why nothing is dropped locally as well.
//
// A cache served on no channel has no notification to hear, so there it drops
// what it holds directly — see [Keyed.ServeLocally]. Publishing nowhere *and*
// keeping the value would be the one arrangement where a withdrawal silently
// does not happen, and it is the arrangement a single-instance deployment is in.
func (k *Keyed[V]) Forget(ctx context.Context, db dbx.Conn, keys ...string) error {
	if k == nil {
		return nil
	}
	s := k.served.Load()
	if s == nil {
		// Never served, so nothing anywhere is holding an answer to withdraw.
		return nil
	}
	if s.bus == nil {
		k.Drop(keys...)
		return nil
	}
	for _, key := range keys {
		if err := s.topic.Forget(ctx, db, key); err != nil {
			return err
		}
	}
	return nil
}

// Drop forgets in this process only.
//
// The wrong tool for a revocation and the right one for a caller with no
// transaction to publish on — a store that is not Postgres, or a write no other
// replica needs to hear about.
func (k *Keyed[V]) Drop(keys ...string) {
	if k == nil {
		return
	}
	for _, key := range keys {
		k.m.Forget(key)
	}
}

// ForgetOrDrop publishes on the transaction already on ctx, or drops locally when
// there is none.
//
// One caller shape: a write that may or may not be inside a transaction. A
// Postgres store always is, and a store that is not Postgres has no channel to
// publish on and no other replica to reach — so the two answers are the same
// decision made from what the context carries.
func (k *Keyed[V]) ForgetOrDrop(ctx context.Context, keys ...string) error {
	if k == nil {
		return nil
	}
	if tx, ok := dbx.Tx(ctx); ok {
		return k.Forget(ctx, tx, keys...)
	}
	k.Drop(keys...)
	return nil
}

// Clear withdraws every held answer, everywhere.
//
// For a write that changes what an answer *means* rather than which one is
// wrong. Working out which keys those reach is a query, and "clear it" is the
// cheaper answer for a map that refills itself on the next request.
//
// On a cache served on no channel this empties the map directly, for the reason
// [Keyed.Forget] gives.
func (k *Keyed[V]) Clear(ctx context.Context, db dbx.Conn) error {
	if k == nil {
		return nil
	}
	s := k.served.Load()
	if s == nil {
		return nil
	}
	if s.bus == nil {
		k.m.Clear()
		return nil
	}
	return s.topic.Clear(ctx, db)
}

// Len is how many entries are held, expired ones included. For a metric or a
// test.
func (k *Keyed[V]) Len() int {
	if k == nil {
		return 0
	}
	return k.m.Len()
}

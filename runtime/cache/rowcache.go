package cache

import (
	"context"
	"time"

	"github.com/simonjanss/rig/runtime/dbx"
)

// RowCache holds one table's rows between requests.
//
// It is what `persist-go` puts behind a `cache:` block, and it used to be
// emitted: a hundred lines of generic machinery, identical in every generated
// project because it depends on nothing the document says, testable only by
// reading a golden file. Here it is ordinary Go with tests against it.
//
// A [Keyed] underneath, and the two places it departs from one are the whole of
// what makes it a separate type. Both are written out below, against Keyed's
// opposite choice, because a reader who knows one will assume the other.
//
// # A write is dropped locally as well as published
//
// [Keyed.Forget] publishes on the writing transaction and drops nothing here,
// on the grounds that the publisher hears its own notification. That is right
// for a revoked session, where a moment of staleness on the revoking replica is
// nobody's problem.
//
// It is wrong for a row. The notification travels out through Postgres and back
// in on the listener's own connection, which takes some moments — and those
// moments belong to the caller who just wrote, who would spend them reading the
// row as it was. Somebody who saves a change and is shown the old value has been
// told their write did not happen. So [RowCache.Forget] does both: the
// publication for every other replica, on the transaction so that a rollback
// takes it back, and a local drop registered on the same commit so that it lands
// before the write returns to whoever asked for it.
//
// Dropping an entry a rollback made valid again costs one query, and
// [dbx.AfterCommit] does not run on a rollback anyway.
//
// # There is no serving it locally
//
// [Keyed.ServeLocally] is a cache attached to no channel that holds values all
// the same — one process, no replicas, staleness is the caller's problem. A row
// cache has no such posture: [RowCache.Serve] with a nil bus leaves it dead, and
// then it reads through and stores nothing.
//
// The reason is what is being cached. A `cache:` block is over the application's
// own rows, written by handlers that run everywhere, and a plain time-to-live
// over those is the trade this whole mechanism exists to refuse. Reading through
// costs queries; holding a row nothing can withdraw costs correctness, and only
// one of those is worth defaulting to.
//
// Safe for concurrent use. Every method is safe on a nil receiver.
type RowCache[V any] struct {
	k *Keyed[V]
}

// RowCacheConfig is what a [RowCache] needs.
//
// No Clone: the generated store copies a row at the call site, with a clone
// function it wrote for that entity, and threading it through here would be a
// second place the copy could fail to happen.
type RowCacheConfig struct {
	// Topic is the name this table's invalidations travel under, which is the
	// table's own name.
	Topic string

	// TTL and MaxEntries are [MapConfig]'s. A zero TTL holds nothing.
	TTL        time.Duration
	MaxEntries int
}

// NewRowCache builds a cache that holds nothing until it is served.
func NewRowCache[V any](cfg RowCacheConfig) *RowCache[V] {
	return &RowCache[V]{k: NewKeyed[V](KeyedConfig[V]{
		Topic:      cfg.Topic,
		TTL:        cfg.TTL,
		MaxEntries: cfg.MaxEntries,
	})}
}

// Serve attaches this cache to a bus, so its entries can be withdrawn.
//
// A nil bus leaves it unserved and therefore dead, for the reason the type's own
// documentation gives.
func (c *RowCache[V]) Serve(bus *Bus) {
	if c == nil {
		return
	}
	c.k.Serve(bus)
}

// Load answers from the cache, or reads through it.
func (c *RowCache[V]) Load(key string, fn func() (V, error)) (V, error) {
	if c == nil {
		return fn()
	}
	return c.k.Load(key, fn)
}

// Forget withdraws one row: here once the change lands, and everywhere else on
// the transaction that made it.
//
// Both, and each covers what the other cannot — see the type's own
// documentation. With no transaction on ctx the local drop is all there is, and
// there is no other replica it could have reached.
func (c *RowCache[V]) Forget(ctx context.Context, key string) error {
	if c == nil {
		return nil
	}
	dbx.AfterCommit(ctx, func() { c.k.Drop(key) })

	tx, ok := dbx.Tx(ctx)
	if !ok {
		return nil
	}
	return c.k.Forget(ctx, tx, key)
}

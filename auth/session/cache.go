package session

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/cache"
	"github.com/simonjanss/rig/runtime/dbx"
)

// TokenTopic is the name this package's invalidations travel under.
//
// Exported because a deployment reading the channel with psql to see whether
// anything is being published needs to know what it is looking at, and because
// it is half of an agreement — a bus serving "session" under one name and a
// revocation publishing under another is a cache that never invalidates.
const TokenTopic = "session"

// TokenCache holds verified access tokens, and is how a revocation reaches the
// replicas holding them.
//
// A nil *TokenCache is usable and caches nothing, which is what a manager built
// without one has: every [Manager.Verify] reads the row, exactly as it did
// before this existed.
//
// It is one type rather than a map and a topic side by side because the two are
// halves of one agreement. [NewTokenCache] registers the map and returns the
// publisher for the same registration, so the name is typed once and there is no
// second place for it to drift.
type TokenCache struct {
	m     *cache.Map[*Token]
	topic *cache.Topic
}

// TokenCacheConfig is what a [TokenCache] needs beyond its bus.
type TokenCacheConfig struct {
	// TTL is how long a verified token may be reused with no invalidation
	// arriving, and the whole cost of the cache stated as time.
	//
	// It is the backstop rather than the guarantee: a revocation is published on
	// the transaction that performed it, so the ordinary case is that every
	// replica has forgotten before the next request arrives. This is what
	// remains if one of them was not listening.
	//
	// Zero or less caches nothing.
	TTL time.Duration

	// MaxEntries bounds the map. Zero takes the package default.
	MaxEntries int

	// Now is the clock, for tests.
	Now func() time.Time
}

// NewTokenCache registers a cache on a bus and returns it.
//
// The bus must not already serve [TokenTopic]; [cache.Bus.Serve] panics if it
// does, which is the wiring mistake of building two managers over one bus.
func NewTokenCache(bus *cache.Bus, cfg TokenCacheConfig) *TokenCache {
	if bus == nil {
		// A cache with no channel is a time-to-live over authentication, which
		// is the trade this package refuses. Refusing it here rather than
		// building something that looks like a cache and forgets nothing.
		return nil
	}
	m := cache.NewMap[*Token](cache.MapConfig{
		TTL:        cfg.TTL,
		MaxEntries: cfg.MaxEntries,
		// So that a process which lost the channel reads through rather than
		// serving sessions nobody can revoke.
		Live: bus.Live,
		Now:  cfg.Now,
	})
	return &TokenCache{m: m, topic: bus.Serve(TokenTopic, m)}
}

// errNoToken is how a miss reaches [cache.Map.Load] without being stored.
//
// The map keeps what its loader returns and never keeps a failure, and a miss is
// the one answer that must not be kept: the identifier half of a token is
// attacker-supplied, so caching "no such token" would let anybody fill the map
// with invented ids and evict every real entry in it. Returning an error is how
// the answer is produced without being remembered.
var errNoToken = errors.New("session: no such token")

// load answers from the cache, or reads through it.
//
// The copy is made on the way out, once per call, and runtime/cache recommends
// the opposite — clone in the loader, once per miss. The difference is what this
// value is: without a cache every caller gets its own row out of the store, so
// with one every caller has to get its own token or the cache would have changed
// something other than where the read happened. A [Token] carries the hash every
// verification compares against and a payload that reaches a handler, and one
// caller writing through either would be editing what every other request in the
// window is reading. A struct and two short slices is not a price worth arguing
// about on a path that used to be a query.
func (c *TokenCache) load(id uuid.UUID, fn func() (*Token, error)) (*Token, error) {
	// [errNoToken] rather than a nil token on both paths, so that a caller has
	// one shape to handle and a nil cache is not a second contract.
	read := func() (*Token, error) {
		t, err := fn()
		switch {
		case err != nil:
			return nil, err
		case t == nil:
			return nil, errNoToken
		}
		return t, nil
	}
	if c == nil {
		return read()
	}
	t, err := c.m.Load(id.String(), read)
	if err != nil {
		return nil, err
	}
	return t.clone(), nil
}

// forget publishes an invalidation for each identifier, on the transaction that
// revoked them.
//
// Postgres delivers a notification when the transaction issuing it commits and
// discards it when that transaction rolls back, so an invalidation published
// here is atomic with the revocation causing it. The publisher hears its own
// notification, which is why nothing is dropped locally as well.
func (c *TokenCache) forget(ctx context.Context, db dbx.Conn, ids ...uuid.UUID) error {
	if c == nil {
		return nil
	}
	for _, id := range ids {
		if err := c.topic.Forget(ctx, db, id.String()); err != nil {
			return err
		}
	}
	return nil
}

// drop forgets in this process only.
//
// For the one caller that has no transaction to publish on: a store that is not
// Postgres, which in practice is [MemoryStore] in a test. There are no other
// replicas of a memory store to tell.
func (c *TokenCache) drop(ids ...uuid.UUID) {
	if c == nil {
		return
	}
	for _, id := range ids {
		c.m.Forget(id.String())
	}
}

// clone copies a token deeply enough to hand to one caller.
//
// The two slices are copied because a caller could write through either. The
// pointer fields are not: nothing in this package writes through one — a
// rotation or a revocation assigns a new pointer to a fresh value — so copying
// the pointer copies something nobody edits.
func (t *Token) clone() *Token {
	out := *t
	out.SecretHash = slices.Clone(t.SecretHash)
	out.Payload = slices.Clone(t.Payload)
	return &out
}

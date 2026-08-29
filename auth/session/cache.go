package session

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/cache"
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
type TokenCache struct{ k *cache.Keyed[*Token] }

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
	// A process that lost the channel reads through rather than serving sessions
	// nobody can revoke — see [cache.Keyed], where that is spelled once.
	return &TokenCache{k: cache.NewKeyed(cache.KeyedConfig[*Token]{
		Bus:        bus,
		Topic:      TokenTopic,
		TTL:        cfg.TTL,
		MaxEntries: cfg.MaxEntries,
		Now:        cfg.Now,
		Clone:      (*Token).clone,
	})}
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
	return c.keyed().Load(id.String(), func() (*Token, error) {
		t, err := fn()
		switch {
		case err != nil:
			return nil, err
		case t == nil:
			return nil, errNoToken
		}
		return t, nil
	})
}

// keyed is the cache, or nil for a manager built without one. Every method below
// is safe on a nil [cache.Keyed], which is what makes the nil *TokenCache
// contract cost nothing here.
func (c *TokenCache) keyed() *cache.Keyed[*Token] {
	if c == nil {
		return nil
	}
	return c.k
}

// forgetOrDrop publishes on the transaction already on ctx, or drops locally when
// there is none — a Postgres store always has one, a [MemoryStore] never does.
func (c *TokenCache) forgetOrDrop(ctx context.Context, ids ...uuid.UUID) error {
	return c.keyed().ForgetOrDrop(ctx, keysOf(ids)...)
}

// keysOf is what a set of identifiers is held under.
func keysOf(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
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

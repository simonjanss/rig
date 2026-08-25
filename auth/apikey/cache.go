package apikey

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/simonjanss/rig/runtime/cache"
	"github.com/simonjanss/rig/runtime/dbx"
)

// KeyTopic is the name this package's invalidations travel under.
//
// Exported for the reason [session.TokenTopic] is: it is half of an agreement,
// and a channel somebody is reading with psql should be readable.
const KeyTopic = "apikey"

// KeyCache holds verified keys, and is how a revocation reaches the replicas
// holding them.
//
// A machine integration is the traffic this helps most and the traffic a cache
// is most often wrong about: one key, called as fast as a loop can call it, for
// a row that changes when somebody rotates it. Which is why the entry is
// withdrawn on the transaction that rotated it rather than waiting for a
// lifetime to run out.
//
// A nil *KeyCache is usable and caches nothing.
type KeyCache struct {
	m     *cache.Map[*Key]
	topic *cache.Topic
}

// KeyCacheConfig is what a [KeyCache] needs beyond its bus.
type KeyCacheConfig struct {
	// TTL is how long a verified key may be reused with no invalidation
	// arriving. Zero or less caches nothing.
	TTL time.Duration

	// MaxEntries bounds the map. Zero takes the package default.
	MaxEntries int

	// Now is the clock, for tests.
	Now func() time.Time
}

// NewKeyCache registers a cache on a bus and returns it.
func NewKeyCache(bus *cache.Bus, cfg KeyCacheConfig) *KeyCache {
	if bus == nil {
		return nil
	}
	m := cache.NewMap[*Key](cache.MapConfig{
		TTL:        cfg.TTL,
		MaxEntries: cfg.MaxEntries,
		Live:       bus.Live,
		Now:        cfg.Now,
	})
	return &KeyCache{m: m, topic: bus.Serve(KeyTopic, m)}
}

// errNoKey is how a miss reaches [cache.Map.Load] without being stored.
//
// The key identifier is the half a caller supplies, so caching "no such key"
// would let anybody fill the map with invented identifiers and evict every real
// entry. A loader that fails is a loader whose answer is not kept, which is
// exactly the behaviour wanted here.
var errNoKey = errors.New("apikey: no such key")

// load answers from the cache, or reads through it.
//
// Keyed by the public identifier rather than the row's uuid, because that is
// what a request presents and a cache keyed on anything else would need a lookup
// to use — which is the lookup it exists to avoid.
//
// The copy is made on the way out, for the reason [session.TokenCache] makes
// one: without a cache every caller got its own row, and a key carries the
// scopes that become a caller's permissions. One request appending to those
// would be widening what every other request in the window may do.
func (c *KeyCache) load(keyID string, fn func() (*Key, error)) (*Key, error) {
	// [errNoKey] rather than a nil key on both paths, so that a caller has one
	// shape to handle and a nil cache is not a second contract.
	read := func() (*Key, error) {
		k, err := fn()
		switch {
		case err != nil:
			return nil, err
		case k == nil:
			return nil, errNoKey
		}
		return k, nil
	}
	if c == nil {
		return read()
	}
	k, err := c.m.Load(keyID, read)
	if err != nil {
		return nil, err
	}
	return k.clone(), nil
}

// forget publishes an invalidation on the transaction that changed the key.
func (c *KeyCache) forget(ctx context.Context, db dbx.Conn, keyID string) error {
	if c == nil {
		return nil
	}
	return c.topic.Forget(ctx, db, keyID)
}

// drop forgets in this process only.
//
// Two callers. A store that is not Postgres has no transaction to publish on
// and no other replica to tell. And a last-used timestamp that was just written
// is a change no other replica needs to hear about — see [Manager.Verify].
func (c *KeyCache) drop(keyID string) {
	if c == nil {
		return
	}
	c.m.Forget(keyID)
}

// clone copies a key deeply enough to hand to one caller.
//
// Three slices, one of them the hash every verification compares against and one
// of them the scopes that become a caller's permissions. The pointer fields are
// left alone: nothing writes through one, and a revocation replaces the row
// rather than editing the value a pointer names.
func (k *Key) clone() *Key {
	out := *k
	out.SecretHash = slices.Clone(k.SecretHash)
	out.Scopes = slices.Clone(k.Scopes)
	out.CIDRAllowList = slices.Clone(k.CIDRAllowList)
	return &out
}

// FailureTopic is the name invalidations for the failure count travel under.
//
// A second topic rather than a second use of [KeyTopic], though both are keyed
// by the same public identifier. They are withdrawn by different writes — a
// rotation changes the key, a wrong secret changes the count — and one topic
// would mean every failed attempt against a key also threw away the verified row
// for it, which is a grinder deciding how often the honest caller pays for a
// lookup.
const FailureTopic = "apikeyfail"

// FailureCache holds the one answer about a key's failure count worth holding:
// that there are none.
//
// Turning the cache on removes the row read from [Manager.Verify] and leaves
// this one, which is then the whole cost of an API-key request:
// [Manager.checkFailureLimit] runs before the lookup on purpose, so a caller
// grinding secrets stops costing a query long before they stop asking. That
// ordering is right and stays; what it should not do is charge the same query to
// the integration that has never once got its key wrong.
//
// **Only a zero is remembered, and that is what makes this sound rather than
// merely cheap.** Inside a window the count only rises, and every row it counts
// is one rig writes — so a held "no failures" can be wrong only if a failure was
// recorded, and a failure recorded is a notification published on the way past.
// A count that is already above zero is never held: that caller is the one the
// limit exists for, and their next attempt should be counted rather than
// guessed at.
//
// Compare [throttle.Local], which is the same optimisation without the channel
// and is deliberately not used for these limits. Its error is one-sided in the
// permissive direction — a replica sees only its own tally — so it allows an
// interval of traffic per replica past the limit. This has no such window,
// because the writes that move the answer are the writes that withdraw it, and a
// replica that has lost the channel stops answering from memory altogether.
//
// A nil *FailureCache is usable and caches nothing.
type FailureCache struct {
	m     *cache.Map[struct{}]
	topic *cache.Topic
}

// FailureCacheConfig is what a [FailureCache] needs beyond its bus.
type FailureCacheConfig struct {
	// TTL is how long "no failures" may be reused with no invalidation
	// arriving. Zero or less caches nothing.
	TTL time.Duration

	// MaxEntries bounds the map. Zero takes the package default.
	MaxEntries int

	// Now is the clock, for tests.
	Now func() time.Time
}

// NewFailureCache registers a cache on a bus and returns it.
func NewFailureCache(bus *cache.Bus, cfg FailureCacheConfig) *FailureCache {
	if bus == nil {
		return nil
	}
	m := cache.NewMap[struct{}](cache.MapConfig{
		TTL:        cfg.TTL,
		MaxEntries: cfg.MaxEntries,
		// So that a process which lost the channel counts rows again rather
		// than waving through a key somebody is grinding.
		Live: bus.Live,
		Now:  cfg.Now,
	})
	return &FailureCache{m: m, topic: bus.Serve(FailureTopic, m)}
}

// errHasFailures is how a non-zero count reaches [cache.Map.Load] without being
// stored.
//
// The map keeps what its loader returns and never keeps a failure, which is the
// seam this borrows: the answer is produced and handed back, and nothing
// remembers it. [errNoKey] uses the same one for the same reason.
var errHasFailures = errors.New("apikey: failures recorded")

// clean reports whether this key has no recorded failures, counting rows only
// when the answer is not already held.
//
// count reports how many the limiter found. It is not called at all on a hit,
// which is the point of the type.
func (c *FailureCache) clean(keyID string, count func() (int, error)) (bool, error) {
	read := func() (struct{}, error) {
		n, err := count()
		switch {
		case err != nil:
			return struct{}{}, err
		case n > 0:
			return struct{}{}, errHasFailures
		}
		return struct{}{}, nil
	}
	if c == nil {
		_, err := read()
		switch {
		case errors.Is(err, errHasFailures):
			return false, nil
		case err != nil:
			return false, err
		}
		return true, nil
	}

	_, err := c.m.Load(keyID, read)
	switch {
	case errors.Is(err, errHasFailures):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// forget publishes an invalidation for a key whose count has just moved.
//
// Published *after* the row it describes has been written, never before: a
// replica that re-read in between would find the same empty window and hold it
// for a full lifetime, which is the one ordering that turns this from a cache
// into a hole in the limit.
func (c *FailureCache) forget(ctx context.Context, db dbx.Conn, keyID string) error {
	if c == nil {
		return nil
	}
	return c.topic.Forget(ctx, db, keyID)
}

// drop forgets in this process only, for a store that is not Postgres.
func (c *FailureCache) drop(keyID string) {
	if c == nil {
		return
	}
	c.m.Forget(keyID)
}

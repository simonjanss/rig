package auth

import (
	"context"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/runtime/cache"
	"github.com/simonjanss/rig/runtime/dbx"
)

// GrantsTopic is the name grant invalidations travel under.
//
// Exported for the reason [session.TokenTopic] and [apikey.KeyTopic] are: it is
// half of an agreement, and somebody reading the channel with psql to find out
// whether anything is being published needs to know what they are looking at.
const GrantsTopic = "grants"

// NewGrantsCache builds a cache for an application's [authhttp.Grants]
// function, not yet attached to anything.
//
// **rig does not turn this on and cannot.** [CacheOptions] covers the two reads
// rig owns on both sides; this one is over an application's own tables, written
// to by an application's own code, and a cache whose invalidations somebody has
// to remember to publish is only as correct as the write path they forgot. That
// is why this is a call rather than a key in rig.yaml: the decision belongs next
// to the writes it is a promise about.
//
// What it is for is the part that is the same for everybody. Grants runs on
// every authenticated request — that is what makes a role taken away stop
// working on the next call rather than at the next sign-in — and it is usually
// the most expensive read on that path. Caching it is six decisions, all of them
// already made next door in [session.TokenCache], and all of them easy to get
// wrong once:
//
//   - the key is the tenant *and* the account, because permissions are per
//     tenant and one account can be in several;
//   - the two slices are copied on the way out, because they reach a handler as
//     [tenancy.Claims] fields and one caller appending to them would widen what
//     every other request in the window may do;
//   - an error is never held, so a role table that was briefly unreachable does
//     not answer "no permissions" for a lifetime;
//   - an empty answer *is* held, because somebody holding nothing is a real
//     answer and not a miss;
//   - a replica that has lost the channel stops answering from memory
//     altogether, rather than serving permissions it can no longer withdraw;
//   - and an invalidation is published on the caller's transaction, so it is
//     delivered when the change commits and discarded if it rolls back.
//
// Three calls, in this order, because the generated wiring needs the Grants
// function before it can hand back the bus it built:
//
//	grants := auth.NewGrantsCache(auth.GrantsCacheConfig{})
//	front, err := api.New(pool, api.Hooks{Grants: grants.Wrap(authz.Grants(pool))})
//	grants.Serve(front.Parts().Cache)
//
// Every step of that is fail-safe. A cache that was never served holds nothing,
// a bus that is not running reports itself as not live, and a map that is not
// live reads through and stores nothing — so forgetting the third line, or
// leaving the `cache:` block out of rig.yaml entirely, costs latency rather than
// correctness.
func NewGrantsCache(cfg GrantsCacheConfig) *GrantsCache {
	// No Bus: this one is served later, by [GrantsCache.Serve], because the bus is
	// built after the grants function that wraps it. [cache.Keyed] holds nothing
	// until then, which is the fail-safe half of what this doc comment promises.
	return &GrantsCache{k: cache.NewKeyed(cache.KeyedConfig[grants]{
		Topic:      GrantsTopic,
		TTL:        cfg.ttl(),
		MaxEntries: cfg.MaxEntries,
		Now:        cfg.Now,
		Clone:      grants.clone,
	})}
}

// Serve attaches the cache to the invalidation channel, which is [Parts.Cache].
//
// Nothing is held until this runs, and nothing is held at all when bus is nil —
// a project with no `cache:` block. Both are the same rule: an answer that
// cannot be withdrawn is not one this will keep.
//
// It panics if the bus already serves [GrantsTopic], which is the wiring mistake
// of attaching two caches to one bus.
func (c *GrantsCache) Serve(bus *cache.Bus) {
	if c == nil {
		return
	}
	c.k.Serve(bus)
}

// ServeLocally attaches the cache to nothing, so it holds answers and forgets
// them in this process alone.
//
// One process, no replicas, and staleness is the caller's problem — which is a
// real posture for a single-instance deployment, and the state a test wants
// without a database behind it. [GrantsCache.Serve] is what a deployment with
// replicas uses.
func (c *GrantsCache) ServeLocally() {
	if c == nil {
		return
	}
	c.k.ServeLocally()
}

// Wrap returns the function to hand to [Config.Grants].
//
// Safe to call on a nil *GrantsCache, which returns g unchanged — so a project
// that decided against this needs no condition at the call site.
func (c *GrantsCache) Wrap(g authhttp.Grants) authhttp.Grants {
	if c == nil || g == nil {
		return g
	}
	return func(ctx context.Context, tenantID, accountID uuid.UUID) ([]string, []string, error) {
		held, err := c.k.Load(grantsKey(tenantID, accountID), func() (grants, error) {
			roles, permissions, err := g(ctx, tenantID, accountID)
			if err != nil {
				// Never held: see [NewGrantsCache]. The map does not keep what a
				// failing loader returns, so this is the whole of it.
				return grants{}, err
			}
			return grants{roles: roles, permissions: permissions}, nil
		})
		if err != nil {
			return nil, nil, err
		}
		// Already copied on the way out of Load — see [cache.KeyedConfig.Clone].
		return held.roles, held.permissions, nil
	}
}

// GrantsCacheConfig tunes a [GrantsCache].
type GrantsCacheConfig struct {
	// TTL is how long an answer may be reused with no invalidation arriving.
	// Zero takes [DefaultCacheTTL].
	//
	// The backstop and not the guarantee, exactly as [CacheOptions.TTL] is —
	// with one difference worth being honest about. rig publishes every
	// invalidation for the reads it caches itself, so that TTL covers only a
	// replica that was not listening. Here the invalidations are yours, so it
	// also covers every write path you did not publish from. It is the cost of
	// a mistake as much as the cost of an outage.
	TTL time.Duration

	// MaxEntries bounds the map. Zero takes [cache.DefaultMaxEntries].
	MaxEntries int

	// Now is the clock, for tests.
	Now func() time.Time
}

func (c GrantsCacheConfig) ttl() time.Duration {
	if c.TTL <= 0 {
		return DefaultCacheTTL
	}
	return c.TTL
}

// GrantsCache is the map [NewGrantsCache] builds and the publisher that
// withdraws from it.
//
// A nil *GrantsCache is usable and does nothing: [GrantsCache.Wrap] hands back
// the function it was given and the rest are no-ops. So is one that was never
// served. Both mean a project that decided against this, or has no `cache:`
// block, and neither needs a condition at the call sites that withdraw grants —
// the writes look the same either way.
type GrantsCache struct{ k *cache.Keyed[grants] }

// grants is one account's answer, as it is held.
//
// A struct rather than two slices in the map so that the pair is stored and
// withdrawn together. They are one answer and a half-updated one would be a
// caller holding roles from before a change and permissions from after it.
type grants struct {
	roles       []string
	permissions []string
}

// clone copies the pair on the way out, so one caller appending to what it was
// handed cannot widen what every other request in the window may do.
func (g grants) clone() grants {
	return grants{roles: slices.Clone(g.roles), permissions: slices.Clone(g.permissions)}
}

// grantsKey is what one answer is held under.
//
// Both identifiers, because permissions are resolved within a tenant and the
// same account in two of them holds two different sets. Keying on the account
// alone would serve one tenant's permissions to a request for the other.
func grantsKey(tenantID, accountID uuid.UUID) string {
	return tenantID.String() + "/" + accountID.String()
}

// Invalidate withdraws one account's grants in one tenant, everywhere.
//
// Call it from the transaction that changed the roles, passing that
// transaction — `dbx.Tx(ctx)` inside a hook, or the pool for a write that has
// none. Postgres delivers a notification when the transaction issuing it commits
// and discards it when that transaction rolls back, so an invalidation published
// this way is atomic with the change causing it and reaches every replica.
//
// Published rather than dropped locally, and that distinction is the whole
// feature: a role removed on one replica has to stop working on all of them.
func (c *GrantsCache) Invalidate(
	ctx context.Context, db dbx.Conn, tenantID, accountID uuid.UUID,
) error {
	if c == nil {
		return nil
	}
	return c.k.Forget(ctx, db, grantsKey(tenantID, accountID))
}

// InvalidateAll withdraws every held answer, everywhere.
//
// For the writes that change what a role *means* rather than who holds it —
// seeding a tenant's roles, syncing the permission catalogue after a deploy,
// editing the grants on a role somebody else holds. Working out which accounts
// those reach is a query, and the answer is "clear it" for a map that refills
// itself on the next request.
func (c *GrantsCache) InvalidateAll(ctx context.Context, db dbx.Conn) error {
	if c == nil {
		return nil
	}
	return c.k.Clear(ctx, db)
}

// Forget withdraws one answer in this process only.
//
// For a caller with no database handle to publish on. It leaves every other
// replica holding what it had, so it is the wrong tool for a revocation and the
// right one for a test.
func (c *GrantsCache) Forget(tenantID, accountID uuid.UUID) {
	if c == nil {
		return
	}
	c.k.Drop(grantsKey(tenantID, accountID))
}

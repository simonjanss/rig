package auth

import (
	"log/slog"
	"time"

	"github.com/simonjanss/rig/runtime/cache"
)

// CacheOptions is whether rig holds its own per-request reads in memory, and
// what that costs.
//
// The reads are the two this module makes on behalf of every caller: resolving a
// session token, and resolving an API key. Both are rig's end to end — it
// performs the read and it performs every write that invalidates it — which is
// the whole reason they are the ones cached. An application supplies no
// invalidation, registers no hook and has no write path it can forget, because
// there is none belonging to it.
//
// The read this deliberately does not cover is [Config.Grants], which is an
// application's own function over an application's own tables. rig cannot see
// those writes, so caching that answer would mean an application publishing its
// own invalidations — and one forgotten write path there is a withdrawn
// permission that goes on working. [Parts.Cache] is the bus for a project that
// decides to take that on knowingly.
type CacheOptions struct {
	// Enabled turns it on. The zero value reads a row per authenticated
	// request, which is what rig did before this existed and is still the
	// correct choice for a deployment that is not paying for it.
	Enabled bool

	// TTL is how long an answer may be reused when no invalidation arrives.
	//
	// It is the backstop and not the guarantee. The guarantee is the channel: a
	// revocation publishes on the transaction that performed it, so every
	// replica that is listening has forgotten by the time that transaction
	// commits. This is what remains for a replica that was not.
	//
	// Zero takes [DefaultCacheTTL].
	TTL time.Duration

	// Channel is the Postgres channel invalidations travel on. Zero takes
	// [cache.DefaultChannel].
	//
	// It has to be an identifier — it reaches Postgres both quoted in a LISTEN
	// and as a parameter to pg_notify — and [cache.NewBus] panics on one that is
	// not, at startup, rather than failing at the first notification.
	Channel string

	// MaxEntries bounds each cache. Zero takes [cache.DefaultMaxEntries].
	MaxEntries int

	// Logger receives one line when invalidations stop arriving and one when
	// they resume. Zero uses [slog.Default].
	//
	// Worth setting, and worth reading. It is the only way anybody learns that
	// this module has gone back to reading a row per request: the fallback is
	// correct and silent, so nothing else about the server will say so.
	Logger *slog.Logger
}

// DefaultCacheTTL is thirty seconds.
//
// Long enough that somebody clicking through an application is served from
// memory throughout, and short enough that the window it actually covers — a
// replica that lost the channel between a revocation and its reconnect — is
// measured in seconds.
const DefaultCacheTTL = 30 * time.Second

// newCacheBus builds the invalidation channel, or nil when there is to be none.
//
// Nil rather than a bus that carries nothing, because nil is what every
// constructor downstream already answers with a cache that reads through. A
// disabled cache is then one absent object instead of a flag five call sites
// have to agree about.
func newCacheBus(cfg Config) *cache.Bus {
	if !cfg.Cache.Enabled {
		return nil
	}
	return cache.NewBus(cache.BusConfig{
		Pool:    cfg.Pool,
		Channel: cfg.Cache.Channel,
		Logger:  cfg.Cache.Logger,
	})
}

// cacheTTL is the lifetime the maps are built with.
func (o CacheOptions) cacheTTL() time.Duration {
	if o.TTL <= 0 {
		return DefaultCacheTTL
	}
	return o.TTL
}

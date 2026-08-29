package project

import (
	"time"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// Cache is the invalidation channel cached reads are withdrawn over.
//
// It is the one place rig keeps shared state outside Postgres, and the block
// exists because that departure needs a decision rather than a default. What it
// switches on is not a time-to-live — a bare one over authentication would mean
// a revoked session that keeps working, which is a trade rig refuses — but a
// Postgres NOTIFY issued inside the transaction that revoked something, so every
// replica forgets at the moment the revocation commits. The time-to-live below
// is the backstop for a notification nobody received.
//
// Nothing an application writes reads this block. Every cache it turns on is
// held, filled and invalidated by generated code — inside rig/auth for the
// authenticated path, and inside the generated repository for a table that asked
// for `cache: true` — so there is no map to wire and no publish to remember. See
// `runtime/cache` for the argument, and NEXT.md for why the one read this does
// *not* cover is the application's own Grants function.
//
// A table opting in is the one place that stops being airtight, and it is why
// opting in is per table rather than implied by this block: rig publishes the
// withdrawal from the writes it makes, so a write that goes around the generated
// repository serves a stale row until the entry expires.
type Cache struct {
	// Enabled is what turns the caching on. Off by default: a cache is a second
	// answer to a question the database was already answering correctly, and
	// switching that on for somebody is not an upgrade.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether reads are held in memory between requests — session and API key verification, and the Get of any table setting cache: true — behind a Postgres NOTIFY channel that invalidates them. Off by default."`

	// TTL is how long an answer may be reused when no invalidation arrives, and
	// the honest statement of what the cache costs.
	TTL Duration `yaml:"ttl,omitempty" json:"ttl,omitempty" jsonschema_description:"How long a cached answer may be reused when no invalidation arrives. It is the backstop, not the guarantee: a revocation is published on the transaction that made it. Defaults to 30s."`

	// Channel is the Postgres channel invalidations travel on.
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty" jsonschema_description:"The Postgres NOTIFY channel invalidations travel on. Two deployments sharing one database and not wanting to share invalidations is the reason it is configurable. Defaults to rig_cache."`

	// MaxEntries bounds each cache.
	MaxEntries int `yaml:"max_entries,omitempty" json:"max_entries,omitempty" jsonschema_description:"How many entries one cache may hold before it is dropped whole. Defaults to 50000."`
}

const (
	// DefaultCacheTTL is thirty seconds.
	//
	// Long enough that a person clicking through an application is served from
	// memory throughout, and short enough that the case the backstop is for — a
	// replica that lost the channel between the revocation and the reconnect —
	// is measured in seconds. It is not the number that makes revocation
	// immediate; the notification is.
	DefaultCacheTTL = 30 * time.Second

	// DefaultCacheChannel matches cache.DefaultChannel, spelled here because
	// this package configures the runtime rather than importing it.
	DefaultCacheChannel = "rig_cache"

	// DefaultCacheMaxEntries matches cache.DefaultMaxEntries, for the same
	// reason.
	DefaultCacheMaxEntries = 50_000

	// maxCacheChannel is the 63 bytes Postgres allows for a name.
	maxCacheChannel = 63
)

// Configured reports whether the block says anything beyond being switched on.
func (c Cache) Configured() bool {
	return configured(c, Cache{Enabled: c.Enabled})
}

// applyCacheDefaults resolves every value the cache block leaves out.
func (p *Project) applyCacheDefaults() {
	c := &p.Config.Cache
	if !c.Enabled {
		return
	}
	setDuration(&c.TTL, DefaultCacheTTL)
	if c.Channel == "" {
		c.Channel = DefaultCacheChannel
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = DefaultCacheMaxEntries
	}
}

// checkCache validates what the JSON Schema cannot.
func (p *Project) checkCache() diag.List {
	var diags diag.List
	c := p.Config.Cache

	if !c.Enabled {
		// The same failure every other block refuses: numbers somebody set and
		// believed in, which nothing reads.
		if c.Configured() {
			diags.Add(diag.CodeConfigInvalid, p.At("cache", "enabled"),
				"cache is configured but cache.enabled is false, so none of it is read; "+
					"set `enabled: true` or remove the block")
		}
		return diags
	}

	// The dependency this block used to carry — an `auth:` block, because the
	// only reads it covered were authentication's — now has a second way to be
	// satisfied: a table that asked for `cache: true`. Both facts are not visible
	// from here, since table configuration is a separate set of files, so the
	// check lives in `internal/compile` where the two meet. Everything below is
	// about this block's own values, which is all this package can see.

	if d := c.TTL.Duration(); d <= 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("cache", "ttl"),
			"cache.ttl is %s, which caches nothing — every read would call through. "+
				"Remove the key to take the default, or set cache.enabled to false", c.TTL)
	} else if d > time.Hour {
		diags.AddSeverity(diag.CodeConfigInvalid, diag.SeverityWarning, p.At("cache", "ttl"),
			"cache.ttl is %s. It is only the backstop, but it is also how long a replica "+
				"that lost the invalidation channel would go on answering out of memory", c.TTL)
	}

	if c.MaxEntries < 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("cache", "max_entries"),
			"cache.max_entries is %d; it has to be positive", c.MaxEntries)
	}

	// The name reaches Postgres twice — quoted in a LISTEN and as a parameter to
	// pg_notify — and both have to resolve to the same channel. Restricting it to
	// an identifier is how that is true with no escaping rule to get wrong, and
	// checking it here is what turns cache.NewBus's panic at startup into a
	// diagnostic naming the line that caused it.
	if err := checkCacheChannel(c.Channel); err != "" {
		diags.Add(diag.CodeConfigInvalid, p.At("cache", "channel"), "cache.channel %s", err)
	}

	return diags
}

// checkCacheChannel returns what is wrong with a channel name, or "".
func checkCacheChannel(name string) string {
	if len(name) > maxCacheChannel {
		return "is longer than the " + itoa(maxCacheChannel) + " bytes Postgres allows for a name"
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return "is not an identifier: it may hold letters, digits and underscores, " +
				"and may not start with a digit"
		}
	}
	return ""
}

// IR is the resolved block, as a document carries it.
//
// Nil for a project that caches nothing, so a generator asks the document one
// question rather than reading a flag and deciding what it implies.
func (c Cache) IR() *ir.Cache {
	if !c.Enabled {
		return nil
	}
	return &ir.Cache{
		Enabled:    true,
		TTLSeconds: c.TTL.Duration().Seconds(),
		Channel:    c.Channel,
		MaxEntries: c.MaxEntries,
	}
}

package project

import (
	"reflect"
	"time"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// Cache is the invalidation channel rig's own per-request reads run over.
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
// held, filled and invalidated inside rig/auth, so there is no map to wire, no
// publish to remember and no way to leave one write path out. See
// `runtime/cache` for the argument, and NEXT.md for why the one read this does
// *not* cover is the application's own Grants function.
type Cache struct {
	// Enabled is what turns rig's own caching on. Off by default: a cache is a
	// second answer to a question the database was already answering correctly,
	// and switching that on for somebody is not an upgrade.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether rig caches its own per-request reads — session and API key verification — behind a Postgres NOTIFY channel that invalidates them. Off by default."`

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
	return !reflect.DeepEqual(c, Cache{Enabled: c.Enabled})
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

	// The dependency, for the reason monitoring has one on tracing: the two reads
	// this caches are session verification and API key verification, both of them
	// inside rig/auth, and `server-go` writes the wiring from the same emitter it
	// writes the rest of the authentication from. A project with no `auth:` block
	// gets no emitter and so no cache — numbers somebody set and believed in,
	// which is the failure the branch above refuses in its other form.
	if !p.Config.Auth.Enabled {
		diags.Add(diag.CodeConfigInvalid, p.At("cache", "enabled"),
			"cache.enabled is true but this project has no `auth:` block, and the only "+
				"reads rig caches are the ones authentication makes; nothing would read it")
	}

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

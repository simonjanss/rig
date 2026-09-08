// Package cache holds an answer for a short while, and tells the other replicas
// when to stop holding it.
//
// Everything else rig ships puts shared state in Postgres. Rate-limit tallies
// are rows, idempotency keys are rows, notification claims are leases, presence
// is a table — and the reason is written down in five generators as one
// sentence: a cron job rather than something racing itself in every replica.
// This package is the departure, so it owes an argument.
//
// The argument is that some reads are pure functions of data that changes a few
// times a month and are paid for on every single request. Resolving what a
// caller may do is the example rig cares about: a join over role tables, twice a
// second per active person, for an answer that last changed when somebody was
// hired. The usual objection is not cost but staleness — a permission taken away
// has to stop working now, not when a timer expires — and that objection is
// correct, which is why a bare time-to-live cache over authorization is a bad
// trade and rig has never shipped one.
//
// So the time-to-live is not the mechanism here. [Bus] is. Postgres NOTIFY
// issued inside a transaction is delivered exactly when that transaction
// commits, and discarded if it rolls back, to every session listening. That is
// an invalidation that is atomic with the write causing it, with no outbox, no
// trigger, no table and no second piece of infrastructure. A role change
// publishes; every replica forgets; the next request reads the new answer. The
// time-to-live stops being the guarantee and becomes the backstop.
//
// # Using it
//
// A [Map] per kind of thing, a [Bus] to carry the invalidations, and a [Topic]
// from [Bus.Serve] that joins the two:
//
//	bus := cache.NewBus(cache.BusConfig{Pool: pool, Logger: logger})
//	grants := cache.NewMap[grant](cache.MapConfig{TTL: 30 * time.Second, Live: bus.Live})
//	topic := bus.Serve("grants", grants)
//
//	bus.Start()
//	app.CloseWithin("cache", 5*time.Second, bus.Close)
//
// Reads go through [Map.Load]. Writes publish on the topic, inside the
// transaction that did the writing:
//
//	roles, err := grants.Load(tenantID.String()+"/"+accountID.String(), func() (grant, error) {
//		return readGrants(ctx, db, tenantID, accountID)
//	})
//
//	// in a service's After hook, where dbx.Tx(ctx) is the write's transaction
//	if tx, ok := dbx.Tx(ctx); ok {
//		err = topic.Forget(ctx, tx, tenantID.String()+"/"+accountID.String())
//	}
//
// # What it does not do
//
// It does not deduplicate concurrent misses. Ten requests for one cold key make
// ten calls, which is what the same ten requests cost today with no cache at
// all — so this is a bound that was never improved rather than one made worse.
// [Map.Load] does keep a register of the loads running right now, and that is
// not a first step towards collapsing them: one shared answer would make one
// caller's failure and one caller's deadline into everybody's, and a slow leader
// into a queue. What the register is for is knowing which load an invalidation
// was about.
//
// It is not a store. Nothing here is durable, nothing is shared, and a value put
// in one replica is never read from another. The only thing that crosses the
// wire is the word "forget".
package cache

import (
	"sync"
	"time"
)

// DefaultMaxEntries bounds a [Map] that did not say.
const DefaultMaxEntries = 50_000

// Forgetter is the part of a [Map] a [Bus] needs, so that one bus can route
// invalidations to maps holding different types.
//
// Both methods are what a notification means rather than what a caller wants:
// Forget for one key that changed, Clear for "this process has been out of
// touch and cannot know what it missed".
type Forgetter interface {
	// Forget drops one key.
	Forget(key string)
	// Clear drops everything.
	Clear()
}

// Map keeps values against string keys until their time-to-live runs out or
// something forgets them.
//
// Keys are strings, and that is a decision rather than a shortcut. The key a
// notification carries and the key a lookup uses have to be the same key, and
// the cheapest way to guarantee it is to have only one of them. A typed key with
// a formatting function beside it is two keys that agree until somebody edits
// one, and the failure that follows is a cache that silently never invalidates —
// which is indistinguishable from a cache that works, right up until it matters.
//
// Safe for concurrent use. Zero value is not usable; call [NewMap].
type Map[V any] struct {
	cfg MapConfig

	mu sync.RWMutex
	m  map[string]entry[V]
	// gen counts clears, which are the invalidation that cannot name a key. It
	// is half of what stops [Map.Load] storing an answer that was already stale
	// when it arrived — see the comment there.
	gen uint64
	// flights is the loads running right now: one entry per key that has one and
	// none otherwise, so this is bounded by concurrent misses and needs no
	// eviction of its own.
	//
	// Exactly one flight exists per key at a time and only its last reader
	// removes it. A second one for a key that already had a live flight would be
	// a forget bumping a counter nobody is watching, which is the stale answer
	// this whole mechanism is here to refuse — so nothing else may delete from
	// this map. Clear in particular must not: it has gen for what it means.
	flights map[string]*flight
}

// flight is the loads of one key that are running, and the forgets of that key
// since each of them started.
//
// A counter rather than a flag, and the difference is a load that starts after a
// forget while an earlier one is still running. That one read the new data and
// may be kept, which a flag cannot express — it would go on discarding every
// load of the key until the last of them had drained.
type flight struct {
	readers int
	gen     uint64
}

// MapConfig is what a [Map] needs to know.
type MapConfig struct {
	// TTL is how long a value may be reused, and the honest statement of what
	// the cache costs: with no invalidation reaching this process, a change
	// takes effect this long after it was made.
	//
	// Zero or less caches nothing and calls through every time. That is so a
	// project can turn the cache off by changing a number rather than by
	// unpicking its wiring, and so a value read from configuration needs no
	// condition around it.
	TTL time.Duration

	// Live reports whether invalidations are currently arriving. [Bus.Live] is
	// what belongs here.
	//
	// When it returns false the map calls through and stores nothing, which is
	// the opposite choice from the rate limiter next door: that one fails open,
	// because refusing traffic is worse than allowing a little too much, and
	// this one fails closed, because serving permissions nobody can withdraw is
	// worse than serving them slowly. The cost is real and worth saying out
	// loud — a database that has gone away puts the full read back on every
	// request at the moment it can least afford it.
	//
	// Nil means always live, which is a cache with no invalidation channel: a
	// time-to-live and nothing else, correct only in a single process or for
	// somebody who has decided the staleness is acceptable.
	Live func() bool

	// MaxEntries bounds the map. Zero means [DefaultMaxEntries].
	//
	// Past it the whole map is dropped rather than swept. That sounds worse than
	// it is: no entry outlives TTL, so reaching the bound costs one window
	// behaving as though nobody had configured a cache. Refusing to store
	// anything new instead would leave a process answering forever out of
	// whichever keys happened to arrive first, which is a cache that gets colder
	// the longer it runs.
	MaxEntries int

	// Now is the clock. Zero is [time.Now]. It is here because what is worth
	// testing about a cache is the edge of its window, and a test that sleeps to
	// reach that edge is a test that fails on a loaded machine.
	Now func() time.Time
}

type entry[V any] struct {
	v     V
	until time.Time
}

// NewMap builds a map from a configuration.
func NewMap[V any](cfg MapConfig) *Map[V] {
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultMaxEntries
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Map[V]{
		cfg:     cfg,
		m:       make(map[string]entry[V]),
		flights: make(map[string]*flight),
	}
}

// Load answers from the map, or calls fn and keeps what it returns.
//
// A failure is never kept. A database that was unreachable for one request does
// not get to decide the answer for the rest of the window.
//
// # The value is shared, not copied
//
// What fn returned is stored as it is and handed back as it is. If V holds a
// slice, a map or a pointer — and the read this package exists for returns a
// grant of roles and permissions, so it does — then every caller inside the
// window has the same backing array, and one of them appending to it or editing
// it in place is editing what the others are reading. Under `-race` that is a
// race; without it, it is worse.
//
// Nothing here can prevent it: copying an arbitrary V is not something a library
// can do. So a V with a reference type in it has to be cloned by somebody, and
// the cheap place is fn, once per miss, rather than at every call site that
// receives one.
//
// # Invalidation during a load
//
// The interesting case is neither the hit nor the miss but the invalidation that
// lands in between. A caller reads, a role changes, the notification arrives and
// is applied, and only then does the read come back — with the old answer, about
// to be written into a map that has just been told to forget it. Nothing later
// corrects that: the entry looks fresh and survives its full time-to-live, so the
// one request that mattered is the one the cache gets wrong. Load closes it by
// registering the read before making it, and declining to store an answer an
// invalidation has landed on since.
//
// It is the key's own invalidations that count, and not the map's. A [Bus]
// delivers every replica the same notification, and most of them are for keys
// this process has never held — so discarding on somebody else's key would be a
// cache that stops filling in proportion to how busy the table is. Worst exactly
// where it was wanted, and silent, because a cache that holds nothing and a
// cache that works differ only in a number nobody is looking at. The register is
// what makes per-key affordable: an entry in it exists only while a load of that
// key is running, so it is bounded by concurrent misses rather than by anything
// needing an eviction policy of its own.
//
// [Map.Clear] is the exception and stays map-wide, because it is what a bus does
// on reconnecting and the one thing it knows then is that it cannot name what it
// missed.
//
// The register is taken before fn is called, which is what makes the window
// that remains safe rather than merely small: an invalidation applied before fn
// starts is one fn's own read will see, so the answer it comes back with is the
// new one. That holds for a loader that goes and reads, and not for one reading
// inside a transaction opened earlier — which is why a generated Get consults
// the cache only when there is no transaction in the context.
func (m *Map[V]) Load(key string, fn func() (V, error)) (V, error) {
	if m.cfg.TTL <= 0 || (m.cfg.Live != nil && !m.cfg.Live()) {
		return fn()
	}

	now := m.cfg.Now()

	m.mu.RLock()
	e, held := m.m[key]
	m.mu.RUnlock()

	if held && now.Before(e.until) {
		return e.v, nil
	}

	// Released with a defer, because a loader that panics still has to leave the
	// register as it found it: a registration that was never dropped is a load
	// that stays in flight for the life of the process, and the bound this map
	// claims is the reason it has no eviction of its own.
	gen, f, keyGen := m.begin(key)
	defer m.release(key, f)

	v, err := fn()
	if err != nil {
		var zero V
		return zero, err
	}

	// Answer the caller either way. What is in doubt is whether this may be
	// reused, not whether it was true when it was read.
	//
	// Locked and unlocked by hand rather than deferred, because release above
	// takes the same lock and a mutex that is not reentrant tolerates that only
	// as long as this section has finished before it runs.
	m.mu.Lock()
	if m.gen == gen && f.gen == keyGen {
		if len(m.m) >= m.cfg.MaxEntries {
			clear(m.m)
		}
		m.m[key] = entry[V]{v: v, until: now.Add(m.cfg.TTL)}
	}
	m.mu.Unlock()
	return v, nil
}

// begin registers a load, and reports what has to still be true at the end of it
// for the answer to be worth keeping.
func (m *Map[V]) begin(key string) (gen uint64, f *flight, keyGen uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	f, running := m.flights[key]
	if !running {
		f = &flight{}
		m.flights[key] = f
	}
	f.readers++
	return m.gen, f, f.gen
}

// release drops one load's registration, and the flight with the last of them.
func (m *Map[V]) release(key string, f *flight) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if f.readers--; f.readers == 0 {
		delete(m.flights, key)
	}
}

// Forget drops one key, so that the next [Map.Load] calls through.
//
// Forgetting something that was never held is not an error — a [Bus] delivers
// every replica the same notification, and most of them will not have the key.
// A load of this key that is in flight is told, and will not keep what it comes
// back with. Loads of every other key are left alone — the method takes one key
// and that is the whole of what it invalidates.
func (m *Map[V]) Forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.m, key)
	if f, running := m.flights[key]; running {
		f.gen++
	}
}

// Clear drops everything, the loads in flight included: what a caller is part
// way through reading is as likely to be stale as what the map is holding.
//
// This is what a [Bus] calls when it has just connected, because whatever was
// published while it was away is unrecoverable — see [Bus.Start]. It is the one
// invalidation that cannot name a key, and the only reason [Map.Load] still
// counts anything for the map as a whole.
func (m *Map[V]) Clear() {
	m.mu.Lock()
	defer m.mu.Unlock()
	clear(m.m)
	m.gen++
}

// Len is how many entries are held, expired ones included.
//
// For a metric or a test. It is not a measure of anything a caller should
// branch on: an entry counted here may be past its time-to-live, and the next
// [Map.Load] of it will call through.
func (m *Map[V]) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.m)
}

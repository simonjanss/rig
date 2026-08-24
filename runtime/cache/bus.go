package cache

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/runtime/dbx"
)

const (
	// DefaultChannel is the Postgres channel a [Bus] listens on when nothing
	// else was asked for.
	DefaultChannel = "rig_cache"

	// DefaultBackoff is how long a [Bus] waits before reconnecting.
	DefaultBackoff = time.Second

	// MaxPayload is the most a notification may carry. Postgres refuses more,
	// and it refuses at the moment of the write rather than at the moment of the
	// configuration — so this is checked here, where the error can name the key.
	MaxPayload = 7999

	// separator splits a payload into its topic and its key. Topic names may not
	// contain it; keys may, because the split takes the first one.
	separator = ":"
)

// Bus carries "forget this" between replicas, over one Postgres channel.
//
// One channel for every topic, rather than a channel each. A channel is a
// subscription that has to be established on a connection, so one of them means
// one LISTEN and means a topic can be added without reconnecting; the topic
// travels in the payload instead, where it costs nothing.
//
// Zero value is not usable; call [NewBus]. Safe for concurrent use.
type Bus struct {
	cfg BusConfig

	// live is atomic rather than guarded, because [Bus.Live] is consulted on
	// every cached read and a mutex there would put a process-global
	// serialization point on the hot path this package exists to shorten.
	live atomic.Bool

	// said is what the logger has already been told, so that a state anybody
	// cares about is logged when it changes rather than when it is retried.
	//
	// Outside the mutex on purpose: [Bus.run] is the only caller of [Bus.listen]
	// and there is only ever one of it, so both of these are touched by one
	// goroutine and a lock would only suggest otherwise.
	said struct {
		arriving bool
		missing  bool
	}

	mu      sync.Mutex
	topics  map[string]Forgetter
	running bool
	closed  bool
	stop    chan struct{}
	done    chan struct{}
}

// BusConfig is what a [Bus] needs.
type BusConfig struct {
	// Pool is the application's pool. The bus does not run queries on it — it
	// takes the pool's connection configuration and opens one connection of its
	// own, because a session that is listening is a session that is blocked, and
	// a pooled connection held for the life of the process is one the
	// application never gets back.
	//
	// Taking the pool rather than a DSN is what keeps the listener on the same
	// database, credentials and TLS settings as everything else, with no second
	// piece of configuration to drift.
	Pool *pgxpool.Pool

	// Logger receives a warning when invalidations stop arriving, and an info
	// line when they resume. Once each, on the change rather than on the
	// reconnect attempt. Zero means [slog.Default].
	//
	// The two loops rig already runs in-process — the notification dispatcher
	// and the presence sweeper — deliberately log nothing, because a pass that
	// failed will be retried and nobody needs to hear about it. This one logs,
	// for the same reason the generated rate limiter does when its counters are
	// unavailable: the fallback is silent and changes what the process is
	// serving, so the only way anybody learns is if it says so.
	Logger *slog.Logger

	// Channel is the Postgres channel to use. Zero means [DefaultChannel].
	//
	// Two deployments sharing one database and not wanting to share
	// invalidations is the reason it is configurable. Both halves have to agree,
	// which is why [Bus.Serve] hands out the publisher rather than letting a
	// caller name the channel at the write.
	Channel string

	// Backoff is how long to wait after a failed or dropped connection before
	// trying again. Zero means [DefaultBackoff].
	Backoff time.Duration
}

// NewBus builds a bus. It does nothing until [Bus.Start].
//
// It panics on a configuration that cannot work — no pool, or a channel name
// that is not an identifier — rather than failing later at the first
// notification. Both are decided when the process starts and neither can be
// fixed at the point it would otherwise be noticed.
func NewBus(cfg BusConfig) *Bus {
	if cfg.Pool == nil {
		panic("cache: BusConfig.Pool is required")
	}
	if cfg.Channel == "" {
		cfg.Channel = DefaultChannel
	}
	if err := checkChannel(cfg.Channel); err != nil {
		panic("cache: " + err.Error())
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = DefaultBackoff
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Bus{
		cfg:    cfg,
		topics: make(map[string]Forgetter),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
}

// checkChannel refuses a name that would have to be escaped.
//
// The name reaches Postgres twice — quoted in a LISTEN, and as a parameter to
// pg_notify — and those two have to resolve to the same channel. Restricting it
// to an identifier is how both are true without an escaping rule to get wrong.
func checkChannel(name string) error {
	if len(name) > 63 {
		return fmt.Errorf("channel %q is longer than the 63 bytes Postgres allows for a name", name)
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return fmt.Errorf("channel %q is not an identifier: it may hold letters, digits and underscores, and may not start with a digit", name)
		}
	}
	return nil
}

// Serve registers a cache under a topic and returns the handle a writer
// publishes on.
//
// The registration and the publisher are one call because they are two halves of
// one agreement. A topic that is listened to under one name and published under
// another is a cache that never invalidates and never says so, and the way to
// make that unrepresentable is to never let anybody type the name twice.
//
// Returning a [Topic] is also what lets a project with no cache configured pass
// a nil one around: every method on it is a no-op, so the call sites that
// publish do not need a condition.
//
// It panics on a duplicate topic, an empty one, or one holding a colon — all
// three are wiring mistakes that a process should not start with.
func (b *Bus) Serve(topic string, f Forgetter) *Topic {
	if topic == "" {
		panic("cache: Serve needs a topic")
	}
	if strings.Contains(topic, separator) {
		panic(fmt.Sprintf("cache: topic %q may not hold %q, which is what separates a topic from a key on the wire", topic, separator))
	}
	if f == nil {
		panic(fmt.Sprintf("cache: topic %q has nothing to forget", topic))
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if _, held := b.topics[topic]; held {
		panic(fmt.Sprintf("cache: topic %q is already served", topic))
	}
	b.topics[topic] = f
	return &Topic{bus: b, name: topic}
}

// Live reports whether invalidations are currently arriving.
//
// False before [Bus.Start], false whenever the listener is disconnected, and
// false after [Bus.Close]. It is what belongs in [MapConfig.Live], and a cache
// that consults it stops caching rather than serving what it cannot withdraw.
func (b *Bus) Live() bool { return b.live.Load() }

// Start connects the listener and keeps it connected.
//
// Idempotent, and safe on a bus that has already been closed.
//
// Every connection begins by clearing every registered cache, and that is the
// property the whole design rests on. LISTEN has no backlog: a process that was
// disconnected for a second cannot ask what it missed, and nothing on the
// server remembers. So a bus that has just connected treats everything it holds
// as wrong, which turns "we lost the channel" from a correctness problem into a
// cold cache.
//
// Register the shutdown before the pool closes:
//
//	bus.Start()
//	app.CloseWithin("cache", 5*time.Second, bus.Close)
func (b *Bus) Start() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running || b.closed {
		return
	}
	b.running = true
	go b.run()
}

// run reconnects until Close.
func (b *Bus) run() {
	defer close(b.done)

	// A cancellable context is the only way out of WaitForNotification, which is
	// otherwise blocked on a connection nobody is going to write to.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-b.stop:
			cancel()
		case <-ctx.Done():
		}
	}()

	for {
		err := b.listen(ctx)
		if err != nil && ctx.Err() == nil {
			b.sayMissing(ctx, err)
		}
		select {
		case <-b.stop:
			return
		case <-time.After(b.cfg.Backoff):
		}
	}
}

// listen holds one connection for as long as it lasts.
func (b *Bus) listen(ctx context.Context) error {
	conn, err := pgx.ConnectConfig(ctx, b.cfg.Pool.Config().ConnConfig)
	if err != nil {
		return fmt.Errorf("cache: open the listener: %w", err)
	}
	defer func() {
		// Its own context, and a short one. The context above is cancelled by
		// Close, and a connection abandoned rather than closed leaves a backend
		// on the server until the server notices.
		closing, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		_ = conn.Close(closing)
	}()

	if _, err := conn.Exec(ctx, `LISTEN "`+b.cfg.Channel+`"`); err != nil {
		return fmt.Errorf("cache: listen on %s: %w", b.cfg.Channel, err)
	}

	// Before going live, not after. A read served in between would come out of a
	// map holding whatever this process believed before it lost touch.
	b.clearAll()
	b.live.Store(true)
	defer b.live.Store(false)

	b.sayArriving(ctx)

	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return fmt.Errorf("cache: wait for a notification: %w", err)
		}
		b.deliver(n.Payload)
	}
}

// sayArriving and sayMissing log the change rather than the attempt.
//
// The reconnect loop runs every [BusConfig.Backoff], so a database that is away
// for an hour would otherwise be thirty-six hundred copies of the same warning —
// which is a warning nobody reads, and this package's whole reason for logging
// is that the fallback is silent and somebody has to learn about it. So each of
// these says its piece once and then holds its peace until the answer changes.
func (b *Bus) sayArriving(ctx context.Context) {
	if b.said.arriving {
		return
	}
	b.said.arriving, b.said.missing = true, false
	b.cfg.Logger.InfoContext(ctx, "cache invalidations are arriving", slog.String("channel", b.cfg.Channel))
}

func (b *Bus) sayMissing(ctx context.Context, err error) {
	if b.said.missing {
		return
	}
	b.said.arriving, b.said.missing = false, true
	b.cfg.Logger.WarnContext(ctx, "cache invalidations are not arriving; cached reads are being bypassed",
		slog.String("channel", b.cfg.Channel),
		slog.Any("error", err))
}

// deliver applies one notification.
func (b *Bus) deliver(payload string) {
	topic, key, ok := strings.Cut(payload, separator)
	if !ok {
		// Something else is using this channel, or a newer rig is. Dropping it
		// beats guessing which cache a message we cannot read was meant for.
		return
	}

	b.mu.Lock()
	f, held := b.topics[topic]
	b.mu.Unlock()

	if !held {
		// Ordinary: replicas of different services can share a channel, and a
		// topic this process does not cache is not this process's business.
		return
	}
	if key == "" {
		f.Clear()
		return
	}
	f.Forget(key)
}

// clearAll drops every registered cache.
func (b *Bus) clearAll() {
	// Collected under the lock and called outside it. Each cache takes a lock of
	// its own, and holding two in one order here while a reader takes them in the
	// other is how a deadlock gets built.
	b.mu.Lock()
	all := slices.Collect(maps.Values(b.topics))
	b.mu.Unlock()

	for _, f := range all {
		f.Clear()
	}
}

// Close stops the listener and waits for it.
//
// Register it with the shutdown, within a timeout: there is nothing in flight
// worth finishing, so this only needs long enough to close a connection.
//
//	app.CloseWithin("cache", 5*time.Second, bus.Close)
//
// A bus that was never started has nothing to wait for and returns at once,
// which is what a project that registered the shutdown and left the cache
// disabled has.
func (b *Bus) Close(ctx context.Context) error {
	b.mu.Lock()
	running := b.running
	if !b.closed {
		b.closed = true
		close(b.stop)
	}
	b.mu.Unlock()

	if !running {
		return nil
	}
	select {
	case <-b.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Topic is one kind of cached thing, and the handle a writer publishes on.
//
// A nil Topic is usable and does nothing, so a project with no cache configured
// can hand nil to the services that would publish and leave their call sites
// alone.
type Topic struct {
	bus  *Bus
	name string
}

// Forget tells every replica to drop one key, including this one.
//
// **Pass the transaction that made the change.** Postgres delivers a
// notification when the transaction that issued it commits, and discards it if
// that transaction rolls back — so an invalidation published on the writing
// transaction is atomic with the write, and one published on the pool is a
// message about a change that may not have happened. In a service hook that is
// [dbx.Tx]:
//
//	if tx, ok := dbx.Tx(ctx); ok {
//		err = topic.Forget(ctx, tx, key)
//	}
//
// The publisher hears its own notification, which is why there is no local
// eviction here. One path for every replica, including the one that wrote.
func (t *Topic) Forget(ctx context.Context, db dbx.Conn, key string) error {
	if t == nil {
		return nil
	}
	if key == "" {
		return fmt.Errorf("cache: forgetting %q needs a key; to drop the whole topic call Clear", t.name)
	}
	return t.publish(ctx, db, t.name+separator+key)
}

// Clear tells every replica to drop everything under this topic.
//
// For the change [Topic.Forget] cannot express. Editing what a role grants moves
// every account that holds it, and an application that would have to run a query
// to find them is better served by dropping the topic and letting the next
// requests read.
func (t *Topic) Clear(ctx context.Context, db dbx.Conn) error {
	if t == nil {
		return nil
	}
	return t.publish(ctx, db, t.name+separator)
}

func (t *Topic) publish(ctx context.Context, db dbx.Conn, payload string) error {
	if len(payload) > MaxPayload {
		return fmt.Errorf("cache: %q is %d bytes, and a notification may carry %d", payload, len(payload), MaxPayload)
	}
	// pg_notify rather than NOTIFY, because NOTIFY takes a literal and this
	// takes a parameter — so a key never becomes part of the statement.
	if _, err := db.Exec(ctx, "SELECT pg_notify($1, $2)", t.bus.cfg.Channel, payload); err != nil {
		return fmt.Errorf("cache: publish on %s: %w", t.name, err)
	}
	return nil
}

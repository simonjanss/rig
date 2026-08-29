//go:build docker

// The invalidation channel, against a real Postgres.
//
//	go test -tags docker ./internal/cachetest/
//
// It is here rather than under runtime/ because everything worth proving is a
// claim about Postgres rather than about Go. The design rests on three of them:
// that a notification issued inside a transaction is delivered when that
// transaction commits, that it is discarded when the transaction rolls back, and
// that a listener which lost its connection has no way to learn what it missed.
// A fake would answer whatever it was written to answer, and the second and third
// are precisely the ones a fake gets wrong.
//
// Every test takes a channel of its own, so they share one container without
// sharing any messages.
package cachetest

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/runtime/cache"
)

const containerName = "rigCache-db"

var (
	once   sync.Once
	shared *pgxpool.Pool
	setErr error
)

func database(t *testing.T) *pgxpool.Pool {
	t.Helper()

	once.Do(func() { shared, setErr = startDatabase() })
	if setErr != nil {
		t.Fatal(setErr)
	}
	return shared
}

// startDatabase brings up Postgres and runs no migrations, because this package
// needs no schema. That is not an omission worth working around — it is the
// point. An invalidation channel that needed a table would be a table every
// project has to have before it can cache anything.
func startDatabase() (*pgxpool.Pool, error) {
	ctx := context.Background()

	_ = exec.Command("docker", "rm", "-f", "-v", dockerdb.Qualify(containerName)).Run()

	db, err := dockerdb.Start(ctx, dockerdb.Config{
		Image:    "postgres:17-alpine",
		Name:     dockerdb.Qualify(containerName),
		Port:     dockerdb.HostPort(dockerdb.PortCache),
		Database: "rig", User: "rig", Password: "rig",
	})
	if err != nil {
		return nil, err
	}
	return pgxpool.New(ctx, db.URL())
}

// channels hands out one per test. Test names are too long and too varied to be
// Postgres identifiers, and what a test needs is only that nobody else is using
// its channel.
var channels atomic.Int64

func channel() string { return fmt.Sprintf("rig_cache_t%d", channels.Add(1)) }

// quiet keeps a disconnect out of the test output. The warning is deliberate in
// production — see BusConfig.Logger — and noise here.
func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

// recorder is a Forgetter that says what it was told.
type recorder struct {
	mu        sync.Mutex
	forgotten []string
	cleared   int
}

func (r *recorder) Forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forgotten = append(r.forgotten, key)
}

func (r *recorder) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cleared++
}

func (r *recorder) seen() ([]string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.forgotten...), r.cleared
}

// live starts a bus and waits for it to connect, so a test that goes on to
// publish is not racing the LISTEN.
func live(t *testing.T, pool *pgxpool.Pool, name string) *cache.Bus {
	t.Helper()

	bus := cache.NewBus(cache.BusConfig{Pool: pool, Logger: quiet(), Channel: name})
	bus.Start()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := bus.Close(ctx); err != nil {
			t.Errorf("close the bus: %v", err)
		}
	})

	waitFor(t, "the bus to connect", bus.Live)
	return bus
}

// waitFor polls until cond holds, because a notification crosses a process
// boundary and arrives when it arrives.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// stayFalse gives something that should not happen a chance to happen. Asserting
// the absence of a message has no edge to wait for, so it waits a fixed while.
func stayFalse(t *testing.T, what string, cond func() bool) {
	t.Helper()

	for range 40 {
		if cond() {
			t.Fatalf("%s, and it should not have", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The claim the whole design rests on: an invalidation published on the
// transaction that made the change is delivered when that change lands, so the
// two cannot be seen out of order by anybody.
func TestANotifyInsideATransactionArrivesOnCommit(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()

	rec := &recorder{}
	bus := live(t, pool, channel())
	topic := bus.Serve("grants", rec)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := topic.Forget(ctx, tx, "tenant-1/account-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	// Nothing yet. The notification is part of the transaction, and the
	// transaction has not happened.
	stayFalse(t, "an uncommitted notification arrived", func() bool {
		got, _ := rec.seen()
		return len(got) > 0
	})

	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	waitFor(t, "the notification", func() bool {
		got, _ := rec.seen()
		return len(got) == 1
	})
	got, _ := rec.seen()
	if got[0] != "tenant-1/account-1" {
		t.Errorf("forgot %q, want %q", got[0], "tenant-1/account-1")
	}
}

// The other half, and the reason publishing on the transaction is the rule rather
// than a suggestion: a write that did not happen invalidates nothing, so a failed
// request cannot cost every replica its cache.
func TestANotifyInsideARolledBackTransactionNeverArrives(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()

	rec := &recorder{}
	bus := live(t, pool, channel())
	topic := bus.Serve("grants", rec)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := topic.Forget(ctx, tx, "tenant-1/account-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	stayFalse(t, "a rolled-back notification arrived", func() bool {
		got, _ := rec.seen()
		return len(got) > 0
	})
}

// Two buses on one channel are two replicas. This is the feature: a single
// process could keep its cache correct with a method call.
func TestTwoBusesBothHearOnePublish(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()
	name := channel()

	one, two := &recorder{}, &recorder{}
	busOne := live(t, pool, name)
	topic := busOne.Serve("grants", one)
	busTwo := live(t, pool, name)
	busTwo.Serve("grants", two)

	if err := topic.Forget(ctx, pool, "tenant-1/account-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	// Including the one that published. There is one path for every replica, so
	// a writer needs no local eviction of its own.
	for label, rec := range map[string]*recorder{"the publisher": one, "the other replica": two} {
		waitFor(t, "the notification at "+label, func() bool {
			got, _ := rec.seen()
			return len(got) == 1
		})
	}
}

// A notification is not a queue. Postgres keeps no backlog for a session that
// was not listening, and nothing on the server can be asked what was missed — so
// the only safe thing a reconnecting listener can believe is nothing.
func TestTheBusDropsEverythingWhenItReconnects(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()
	name := channel()

	rec := &recorder{}
	bus := cache.NewBus(cache.BusConfig{
		Pool:    pool,
		Logger:  quiet(),
		Channel: name,
		Backoff: 50 * time.Millisecond,
	})
	topic := bus.Serve("grants", rec)
	bus.Start()
	t.Cleanup(func() {
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := bus.Close(closing); err != nil {
			t.Errorf("close the bus: %v", err)
		}
	})

	waitFor(t, "the bus to connect", bus.Live)

	// Connecting is itself a clear, for exactly this reason.
	_, cleared := rec.seen()
	if cleared != 1 {
		t.Fatalf("cleared %d times on the first connection, want 1", cleared)
	}

	// Kill the listener's backend out from under it. Matched exactly rather than
	// with a LIKE: every other test in this file has a listener of its own on
	// this database, and `rig_cache_t1` is a substring of `rig_cache_t10` — so a
	// tenth test in this file would make this one terminate somebody else's
	// connection, and fail over there.
	var pid int
	if err := pool.QueryRow(ctx, `
		SELECT pid FROM pg_stat_activity
		WHERE datname = current_database()
		  AND pid <> pg_backend_pid()
		  AND query = 'LISTEN "' || $1 || '"'
	`, name).Scan(&pid); err != nil {
		t.Fatalf("find the listener: %v", err)
	}
	if _, err := pool.Exec(ctx, "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatalf("terminate the listener: %v", err)
	}

	waitFor(t, "the bus to notice it is disconnected", func() bool { return !bus.Live() })
	waitFor(t, "the bus to reconnect", bus.Live)

	// And the reconnection cleared again, which is what makes a lost connection
	// a cold cache rather than a wrong one.
	waitFor(t, "the second clear", func() bool {
		_, cleared := rec.seen()
		return cleared >= 2
	})

	// It is really listening again, not merely reporting that it is.
	if err := topic.Forget(ctx, pool, "tenant-1/account-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}
	waitFor(t, "a notification after reconnecting", func() bool {
		got, _ := rec.seen()
		return len(got) == 1
	})
}

// Clearing a topic is for the change one key cannot express: editing what a role
// grants moves everybody holding it, and no application can cheaply list them.
func TestClearingATopicDropsEveryKeyInIt(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()

	rec := &recorder{}
	bus := live(t, pool, channel())
	topic := bus.Serve("grants", rec)

	_, before := rec.seen()
	if err := topic.Clear(ctx, pool); err != nil {
		t.Fatalf("clear: %v", err)
	}

	waitFor(t, "the clear", func() bool {
		_, cleared := rec.seen()
		return cleared > before
	})
	if got, _ := rec.seen(); len(got) != 0 {
		t.Errorf("forgot %v on a clear, want nothing forgotten by key", got)
	}
}

// Replicas of different services may share a channel, and a topic this process
// does not cache is not this process's business.
func TestATopicThisProcessDoesNotHoldIsIgnored(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()
	name := channel()

	// One bus publishes on a topic the other does not serve.
	mine := &recorder{}
	busOne := live(t, pool, name)
	elsewhere := busOne.Serve("somebody-elses-cache", &recorder{})

	busTwo := live(t, pool, name)
	busTwo.Serve("grants", mine)

	if err := elsewhere.Forget(ctx, pool, "tenant-1/account-1"); err != nil {
		t.Fatalf("forget: %v", err)
	}

	stayFalse(t, "a notification for another topic was applied", func() bool {
		got, _ := mine.seen()
		return len(got) > 0
	})
}

// The whole thing together: a cache that stops asking, and starts again the
// moment somebody else changes the answer.
func TestACachedReadStopsAskingUntilAnotherReplicaChangesIt(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()
	name := channel()

	// The replica doing the reading.
	reader := live(t, pool, name)
	grants := cache.NewMap[string](cache.MapConfig{TTL: time.Hour, Live: reader.Live})
	reader.Serve("grants", grants)

	// The replica doing the writing, which shares nothing with it but a database.
	writer := live(t, pool, name)
	topic := writer.Serve("grants", cache.NewMap[string](cache.MapConfig{TTL: time.Hour, Live: writer.Live}))

	asked := 0
	answer := "reader"
	load := func() (string, error) {
		asked++
		return answer, nil
	}
	const key = "tenant-1/account-1"

	for range 5 {
		got, err := grants.Load(key, load)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if got != "reader" {
			t.Fatalf("got %q, want %q", got, "reader")
		}
	}
	if asked != 1 {
		t.Fatalf("asked %d times, want 1", asked)
	}

	// A role changes, in the other replica, in a transaction.
	answer = "admin"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := topic.Forget(ctx, tx, key); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	waitFor(t, "the reader to forget", func() bool { return grants.Len() == 0 })

	got, err := grants.Load(key, load)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got != "admin" {
		t.Errorf("got %q after the change, want %q", got, "admin")
	}
	if asked != 2 {
		t.Errorf("asked %d times, want 2", asked)
	}
}

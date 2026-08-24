//go:build docker

// The rate-limit counters, against a real Postgres.
//
//	go test -tags docker ./internal/throttletest/
//
// It is here rather than under runtime/ because what wants proving is a claim
// about Postgres, not about Go. The whole design rests on one statement that
// both spends a slot and reports the total, and on that statement being safe
// when several replicas run it at once against a single row — a fake would
// answer whatever it was written to answer.
//
// Every test uses a key of its own, so they share one container and one
// migration run without sharing any rows.
package throttletest

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/runtime/foundation"
	"github.com/simonjanss/rig/runtime/throttle"
)

const containerName = "rigThrottle-db"

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

func startDatabase() (*pgxpool.Pool, error) {
	ctx := context.Background()

	// A schema left behind by an earlier run would make these pass for the wrong
	// reason.
	_ = exec.Command("docker", "rm", "-f", "-v", dockerdb.Qualify(containerName)).Run()

	db, err := dockerdb.Start(ctx, dockerdb.Config{
		Image:    "postgres:17-alpine",
		Name:     dockerdb.Qualify(containerName),
		Port:     dockerdb.HostPort(dockerdb.PortThrottle),
		Database: "rig", User: "rig", Password: "rig",
	})
	if err != nil {
		return nil, err
	}

	// The shipped set, not a copy of its DDL. A test against a copy is a test of
	// the copy.
	set := foundation.Set()
	if _, err := dockerdb.Migrate(ctx, dockerdb.MigrateOptions{
		Dir:   "testdata/migrations",
		Table: "rig_project_migrations",
		URL:   db.URL(),
		Foundation: []migrate.Source{
			{Name: set.Module, FS: set.FS, Dir: set.Dir, Table: foundation.Table},
		},
	}); err != nil {
		return nil, err
	}
	return pgxpool.New(ctx, db.URL())
}

// at is a fixed instant on a bucket boundary, so the arithmetic in these tests
// is about the statement rather than about when they happened to run.
var at = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func limitFor(t *testing.T, maxN int) throttle.Limit {
	t.Helper()
	// One limit name per test, so tests that run in parallel cannot see each
	// other's rows even when they use the same key value.
	return throttle.Limit{Name: t.Name(), Max: maxN, Window: time.Minute}
}

func TestItCountsAndReportsInOneStatement(t *testing.T) {
	t.Parallel()

	tally := throttle.NewTally(database(t), throttle.TallyConfig{})
	limit := limitFor(t, 100)
	key := throttle.Account("acct-1")

	for i := 1; i <= 5; i++ {
		n, resetAt, err := tally.Incr(context.Background(), limit, key, at, 1)
		if err != nil {
			t.Fatal(err)
		}
		if n != i {
			t.Fatalf("call %d reported a total of %d", i, n)
		}
		if want := at.Add(time.Minute); !resetAt.Equal(want) {
			t.Errorf("reset at %s, want the end of the bucket %s", resetAt, want)
		}
	}
}

// The reason the increment and the read are one statement. Two would straddle
// another replica's write and answer with a count that never existed.
func TestConcurrentIncrementsOnOneRowAreAllCounted(t *testing.T) {
	t.Parallel()

	pool := database(t)
	limit := limitFor(t, 1<<30)
	key := throttle.Account("acct-1")

	const goroutines, each = 8, 50

	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A recorder each, the way separate replicas would have.
			tally := throttle.NewTally(pool, throttle.TallyConfig{})
			for range each {
				if _, _, err := tally.Incr(context.Background(), limit, key, at, 1); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}

	tally := throttle.NewTally(pool, throttle.TallyConfig{})
	total, _, err := tally.Incr(context.Background(), limit, key, at, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := goroutines * each; total != want {
		t.Fatalf("counted %d of %d concurrent increments — the statement is losing writes", total, want)
	}
}

// A delta rather than an implied one, which is what lets a replica hold
// requests back and publish them together.
func TestADeltaCountsAsThatMany(t *testing.T) {
	t.Parallel()

	tally := throttle.NewTally(database(t), throttle.TallyConfig{})
	limit := limitFor(t, 1000)
	key := throttle.Account("acct-1")

	if _, _, err := tally.Incr(context.Background(), limit, key, at, 17); err != nil {
		t.Fatal(err)
	}
	total, _, err := tally.Incr(context.Background(), limit, key, at, 3)
	if err != nil {
		t.Fatal(err)
	}
	if total != 20 {
		t.Fatalf("17 then 3 came to %d", total)
	}
}

// Zero asks the question without spending anything, which is how a caller
// drains what it was holding and then reads the truth.
func TestZeroSpendsNothing(t *testing.T) {
	t.Parallel()

	tally := throttle.NewTally(database(t), throttle.TallyConfig{})
	limit := limitFor(t, 1000)
	key := throttle.Account("acct-1")
	ctx := context.Background()

	if _, _, err := tally.Incr(ctx, limit, key, at, 4); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		total, _, err := tally.Incr(ctx, limit, key, at, 0)
		if err != nil {
			t.Fatal(err)
		}
		if total != 4 {
			t.Fatalf("a zero increment moved the count to %d", total)
		}
	}
}

// The weighting is the whole reason this is not plain fixed buckets, and it is
// the part that is only really true if Postgres agrees with the Go arithmetic.
func TestThePreviousBucketDecaysAcrossTheBoundary(t *testing.T) {
	t.Parallel()

	tally := throttle.NewTally(database(t), throttle.TallyConfig{})
	limit := limitFor(t, 1000)
	key := throttle.Account("acct-1")
	ctx := context.Background()

	// Fill one bucket, then read from successive points in the next one.
	if _, _, err := tally.Incr(ctx, limit, key, at, 100); err != nil {
		t.Fatal(err)
	}

	next := at.Add(time.Minute)
	for _, tc := range []struct {
		into time.Duration
		want int
	}{
		{0, 100},               // the whole previous bucket still counts
		{15 * time.Second, 75}, // three quarters of it
		{30 * time.Second, 50},
		{45 * time.Second, 25},
	} {
		total, _, err := tally.Incr(ctx, limit, key, next.Add(tc.into), 0)
		if err != nil {
			t.Fatal(err)
		}
		if total != tc.want {
			t.Errorf("%s into the next bucket the count is %d, want %d", tc.into, total, tc.want)
		}
	}

	// A full window later it is gone: nothing carries over two buckets.
	total, _, err := tally.Incr(ctx, limit, key, at.Add(2*time.Minute), 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("two windows on, the count is still %d", total)
	}
}

func TestKeysAndLimitsAreSeparateBudgets(t *testing.T) {
	t.Parallel()

	tally := throttle.NewTally(database(t), throttle.TallyConfig{})
	ctx := context.Background()
	limit := limitFor(t, 1000)
	other := throttle.Limit{Name: t.Name() + "-other", Max: 1000, Window: time.Minute}

	if _, _, err := tally.Incr(ctx, limit, throttle.Account("acct-1"), at, 9); err != nil {
		t.Fatal(err)
	}

	// Same value, different kind. An account id and an address that happened to
	// read the same must not share a budget.
	for _, k := range []throttle.Key{
		throttle.IP("acct-1"),
		throttle.Tenant("acct-1"),
		throttle.Account("acct-2"),
	} {
		total, _, err := tally.Incr(ctx, limit, k, at, 0)
		if err != nil {
			t.Fatal(err)
		}
		if total != 0 {
			t.Errorf("key %s/%s already counts %d", k.Kind, k.Value, total)
		}
	}

	total, _, err := tally.Incr(ctx, other, throttle.Account("acct-1"), at, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("a second limit over the same key already counts %d", total)
	}
}

// End to end through the limiter, with the local tally in front of it — the way
// a server actually holds this.
func TestALimiterOverTheRealTableRefusesAtMax(t *testing.T) {
	t.Parallel()

	local := throttle.NewLocal(
		throttle.NewTally(database(t), throttle.TallyConfig{}),
		throttle.LocalConfig{Interval: 0},
	)
	limit := limitFor(t, 5)
	key := throttle.IP("203.0.113.9")

	limiter := throttle.NewRecording(local).WithClock(func() time.Time { return at })
	ctx := context.Background()

	for i := 1; i <= limit.Max; i++ {
		d, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key})
		if err != nil {
			t.Fatal(err)
		}
		if !d.Allowed {
			t.Fatalf("call %d of %d was refused", i, limit.Max)
		}
	}

	d, err := limiter.Take(ctx, throttle.Check{Limit: limit, Key: key})
	if err != nil {
		t.Fatal(err)
	}
	if d.Allowed {
		t.Fatal("the call past the limit was allowed")
	}
	if err := d.Err(); err == nil {
		t.Fatal("a refused decision produced no error")
	}
}

func TestSweepDeletesOnlyDeadBuckets(t *testing.T) {
	t.Parallel()

	pool := database(t)
	tally := throttle.NewTally(pool, throttle.TallyConfig{})
	limit := limitFor(t, 1000)
	ctx := context.Background()

	old := at.Add(-2 * time.Hour)
	for i, when := range []time.Time{old, at} {
		key := throttle.Account(fmt.Sprintf("acct-%d", i))
		if _, _, err := tally.Incr(ctx, limit, key, when, 1); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := tally.Sweep(ctx, time.Hour, at); err != nil {
		t.Fatal(err)
	}

	var live int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM rig_throttle WHERE limit_name = $1`, limit.Name).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != 1 {
		t.Fatalf("%d rows survived a sweep that should have left exactly the live one", live)
	}
}

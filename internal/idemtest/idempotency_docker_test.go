//go:build docker

// Idempotency keys, against a real Postgres.
//
//	go test -tags docker ./internal/idemtest/
//
// It is here rather than under runtime/ because the interesting half of this
// package is what two transactions do to each other, and that is a claim about
// Postgres rather than about Go. A fake would answer whatever it was written to
// answer; what wants proving is that the second request blocks on the unique
// index, that the record and the write roll back together, and that a refused
// write leaves the key free.
//
// Every test takes a tenant of its own, so they share one container and one
// migration run without sharing any rows.
package idemtest

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/foundation"
	"github.com/simonjanss/rig/runtime/idempotency"
	"github.com/simonjanss/rig/runtime/rigerr"
)

const containerName = "rigIdem-db"

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
		Port:     dockerdb.HostPort(dockerdb.PortIdempotency),
		Database: "rig", User: "rig", Password: "rig",
	})
	if err != nil {
		return nil, err
	}

	// The shipped set, not a copy of its DDL. A test against a copy is a test of
	// the copy.
	set := foundation.Set()
	if _, err := dockerdb.Migrate(ctx, dockerdb.MigrateOptions{
		Dir:   t0dir(),
		Table: "rig_project_migrations",
		URL:   db.URL(),
		Foundation: []migrate.Source{
			{Name: set.Module, FS: set.FS, Dir: set.Dir, Table: foundation.Table},
		},
	}); err != nil {
		return nil, err
	}

	pool, err := pgxpool.New(ctx, db.URL())
	if err != nil {
		return nil, err
	}

	// Something for a write to actually do, so that "the record and the effect
	// commit together" is a claim with two halves to check.
	_, err = pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS idem_effect (
		id uuid PRIMARY KEY, tenant_id uuid NOT NULL, note text NOT NULL)`)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// t0dir is an empty migrations directory: this suite has no project schema of
// its own, and dockerdb.Migrate wants somewhere to look.
func t0dir() string { return "testdata/migrations" }

// world is one test's tenant and the helpers that read what happened in it.
type world struct {
	pool   *pgxpool.Pool
	tenant uuid.UUID
	// ran counts how many times a write body actually executed, which is the
	// number every test here is really about. Atomic because two of these tests
	// run writes from two goroutines on purpose.
	ran atomic.Int32
}

// conn is the transaction the write was handed, or the pool when there is none.
//
// It is the same three lines every store in the repository opens with, and it
// is load-bearing here rather than incidental: an effect written through the
// pool would commit whatever the surrounding transaction did, and then the test
// that a failed write leaves nothing behind would pass without the property
// holding.
func (w *world) conn(ctx context.Context) dbx.Conn {
	if tx, ok := dbx.Tx(ctx); ok {
		return tx
	}
	return w.pool
}

func setup(t *testing.T) *world {
	t.Helper()
	return &world{pool: database(t), tenant: uuid.New()}
}

// req is a request for this tenant, with a fingerprint over body.
func (w *world) req(key, endpoint string, body any) idempotency.Request {
	return idempotency.Request{
		TenantID:    w.tenant,
		Key:         key,
		Endpoint:    endpoint,
		Fingerprint: idempotency.Fingerprint(body),
		RequestID:   "req-" + key,
	}
}

// write returns a Write that records an effect and answers 201 with note.
func (w *world) write(note string) idempotency.Write {
	return func(ctx context.Context) (int, any, error) {
		w.ran.Add(1)
		_, err := w.conn(ctx).Exec(ctx,
			`INSERT INTO idem_effect (id, tenant_id, note) VALUES ($1, $2, $3)`,
			uuid.New(), w.tenant, note)
		if err != nil {
			return 0, nil, err
		}
		return 201, map[string]string{"note": note}, nil
	}
}

// effects is how many rows this tenant's writes left behind.
func (w *world) effects(t *testing.T) int {
	t.Helper()

	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM idem_effect WHERE tenant_id = $1`, w.tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// records is how many idempotency rows this tenant has.
func (w *world) records(t *testing.T) int {
	t.Helper()

	var n int
	if err := w.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_idempotency WHERE tenant_id = $1`, w.tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// A write with no key is the common path, and the point of it is that it costs
// nothing: no transaction, no row, no second round trip.
func TestAWriteWithNoKeyRecordsNothing(t *testing.T) {
	w := setup(t)

	got, err := idempotency.Run(t.Context(), w.pool, w.req("", "POST /todos", nil), w.write("first"))
	if err != nil {
		t.Fatal(err)
	}

	if got.Status != 201 || got.Replayed {
		t.Errorf("got %+v, want a fresh 201", got)
	}
	if n := w.records(t); n != 0 {
		t.Errorf("%d records, want none: nobody asked for one", n)
	}
	if n := w.effects(t); n != 1 {
		t.Errorf("%d effects, want 1", n)
	}
}

// The whole point, in one test: two requests, one write.
func TestTheSameKeyTwiceRunsTheWriteOnce(t *testing.T) {
	w := setup(t)
	req := w.req("k1", "POST /todos", map[string]string{"title": "x"})

	first, err := idempotency.Run(t.Context(), w.pool, req, w.write("first"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := idempotency.Run(t.Context(), w.pool, req, w.write("second"))
	if err != nil {
		t.Fatal(err)
	}

	if first.Replayed {
		t.Error("the first call was a replay of something")
	}
	if !second.Replayed {
		t.Error("the second call ran the write again")
	}
	if string(first.Body) != string(second.Body) {
		t.Errorf("bodies differ: %s and %s", first.Body, second.Body)
	}
	if second.Status != 201 {
		t.Errorf("status = %d, want the 201 the first one answered", second.Status)
	}
	if w.ran.Load() != 1 {
		t.Errorf("the write ran %d times, want 1", w.ran.Load())
	}
	if n := w.effects(t); n != 1 {
		t.Errorf("%d effects, want 1: the second write was not supposed to happen", n)
	}
}

// The invariant everything else rests on. A write that fails rolls back, and it
// takes the record with it — so the key is free, and a caller who fixes their
// body and reuses it gets the write they wanted rather than a cached complaint
// about the old one.
func TestAFailedWriteLeavesNoRecordAndFreesTheKey(t *testing.T) {
	w := setup(t)
	req := w.req("k2", "POST /todos", map[string]string{"title": "x"})
	refused := rigerr.Invalid("title is required")

	_, err := idempotency.Run(t.Context(), w.pool, req, func(ctx context.Context) (int, any, error) {
		// An effect first, so this also proves the rollback covers both halves.
		if _, err := w.conn(ctx).Exec(ctx,
			`INSERT INTO idem_effect (id, tenant_id, note) VALUES ($1, $2, $3)`,
			uuid.New(), w.tenant, "doomed"); err != nil {
			return 0, nil, err
		}
		return 0, nil, refused
	})
	if !errors.Is(err, refused) {
		t.Fatalf("err = %v, want the write's own refusal, unwrapped", err)
	}

	if n := w.records(t); n != 0 {
		t.Errorf("%d records, want none: a write that wrote nothing has nothing to replay", n)
	}
	if n := w.effects(t); n != 0 {
		t.Errorf("%d effects, want none: the transaction was supposed to take them too", n)
	}

	// And the key still works.
	got, err := idempotency.Run(t.Context(), w.pool, req, w.write("corrected"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Replayed {
		t.Error("the retry replayed a failure that never committed")
	}
	if n := w.effects(t); n != 1 {
		t.Errorf("%d effects, want 1", n)
	}
}

// Replaying the wrong response is worse than any error: the caller would get a
// success describing something it never asked for, with no way to tell.
func TestTheSameKeyForADifferentRequestIsRefused(t *testing.T) {
	w := setup(t)

	if _, err := idempotency.Run(t.Context(), w.pool,
		w.req("k3", "POST /todos", map[string]string{"title": "first"}),
		w.write("first")); err != nil {
		t.Fatal(err)
	}

	_, err := idempotency.Run(t.Context(), w.pool,
		w.req("k3", "POST /todos", map[string]string{"title": "different"}),
		w.write("second"))

	if rigerr.CodeOf(err) != rigerr.CodeUnprocessableEntity {
		t.Fatalf("err = %v (%s), want UnprocessableEntity", err, rigerr.CodeOf(err))
	}
	if w.ran.Load() != 1 {
		t.Errorf("the write ran %d times, want 1: the refusal should come before the work", w.ran.Load())
	}
}

// The endpoint is part of the identity, so one client-side identifier reused
// across two calls is two records rather than a replay of the wrong one.
func TestTheSameKeyOnADifferentEndpointIsADifferentRecord(t *testing.T) {
	w := setup(t)
	body := map[string]string{"title": "x"}

	if _, err := idempotency.Run(t.Context(), w.pool,
		w.req("k4", "POST /todos", body), w.write("todo")); err != nil {
		t.Fatal(err)
	}
	got, err := idempotency.Run(t.Context(), w.pool,
		w.req("k4", "POST /notes", body), w.write("note"))
	if err != nil {
		t.Fatal(err)
	}

	if got.Replayed {
		t.Error("a different endpoint replayed the first one's answer")
	}
	if w.ran.Load() != 2 {
		t.Errorf("the write ran %d times, want 2", w.ran.Load())
	}
}

// Keys are per tenant, so two tenants choosing the same string is not a
// collision and one can never replay the other's response.
func TestTheSameKeyInAnotherTenantIsADifferentRecord(t *testing.T) {
	w := setup(t)
	other := &world{pool: w.pool, tenant: uuid.New()}
	body := map[string]string{"title": "x"}

	if _, err := idempotency.Run(t.Context(), w.pool,
		w.req("k5", "POST /todos", body), w.write("mine")); err != nil {
		t.Fatal(err)
	}
	got, err := idempotency.Run(t.Context(), other.pool,
		other.req("k5", "POST /todos", body), other.write("theirs"))
	if err != nil {
		t.Fatal(err)
	}

	if got.Replayed {
		t.Error("one tenant replayed another's response")
	}
	if other.effects(t) != 1 {
		t.Error("the other tenant's write did not happen")
	}
}

// The design, tested where it actually lives. The second request blocks on the
// unique index until the first transaction ends, and then reads what it
// committed — no lease, no in-flight status, no TTL, because Postgres is
// already keeping this bookkeeping for its own reasons.
func TestASecondRequestWaitsForTheFirstAndThenReplaysIt(t *testing.T) {
	w := setup(t)
	req := w.req("k6", "POST /todos", map[string]string{"title": "x"})

	release := make(chan struct{})
	holding := make(chan struct{})

	var (
		first, second idempotency.Result
		firstErr      error
		secondErr     error
		wg            sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		first, firstErr = idempotency.Run(t.Context(), w.pool, req,
			func(ctx context.Context) (int, any, error) {
				close(holding)
				<-release
				return w.write("first")(ctx)
			})
	}()

	<-holding
	// The first transaction is open and holds the claim. Give the second one
	// long enough to get as far as blocking on it — if it were going to run the
	// write instead, this is when it would.
	time.Sleep(250 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		second, secondErr = idempotency.Run(t.Context(), w.pool, req, w.write("second"))
	}()

	time.Sleep(250 * time.Millisecond)
	close(release)
	wg.Wait()

	if firstErr != nil || secondErr != nil {
		t.Fatalf("first: %v, second: %v", firstErr, secondErr)
	}
	if first.Replayed {
		t.Error("the first request replayed something")
	}
	if !second.Replayed {
		t.Error("the second request did not wait: it ran the write itself")
	}
	if string(first.Body) != string(second.Body) {
		t.Errorf("bodies differ: %s and %s", first.Body, second.Body)
	}
	if n := w.effects(t); n != 1 {
		t.Errorf("%d effects, want 1", n)
	}
}

// The other half of the same story: a first transaction that rolls back leaves
// the key free, and the request waiting on it does the work rather than
// replaying a write that never happened.
func TestASecondRequestDoesTheWorkWhenTheFirstRollsBack(t *testing.T) {
	w := setup(t)
	req := w.req("k7", "POST /todos", map[string]string{"title": "x"})

	release := make(chan struct{})
	holding := make(chan struct{})
	doomed := rigerr.Invalid("no")

	var (
		second    idempotency.Result
		secondErr error
		wg        sync.WaitGroup
	)

	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = idempotency.Run(t.Context(), w.pool, req,
			func(context.Context) (int, any, error) {
				close(holding)
				<-release
				return 0, nil, doomed
			})
	}()

	<-holding
	time.Sleep(250 * time.Millisecond)

	wg.Add(1)
	go func() {
		defer wg.Done()
		second, secondErr = idempotency.Run(t.Context(), w.pool, req, w.write("second"))
	}()

	time.Sleep(250 * time.Millisecond)
	close(release)
	wg.Wait()

	if secondErr != nil {
		t.Fatalf("second: %v", secondErr)
	}
	if second.Replayed {
		t.Error("the second request replayed a write that rolled back")
	}
	if n := w.effects(t); n != 1 {
		t.Errorf("%d effects, want 1: the second request was the one that wrote", n)
	}
}

// Waiting is the right default, and waiting forever is not: a retry storm is
// exactly when there are fewest connections to spare. This one costs the whole
// of idempotency.LockTimeout, which is why it is the only test here that does.
func TestARequestGivesUpWaitingForAKeyThatIsHeldTooLong(t *testing.T) {
	w := setup(t)
	req := w.req("k8", "POST /todos", map[string]string{"title": "x"})

	release := make(chan struct{})
	holding := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = idempotency.Run(t.Context(), w.pool, req,
			func(ctx context.Context) (int, any, error) {
				close(holding)
				<-release
				return w.write("first")(ctx)
			})
	}()

	<-holding
	start := time.Now()
	_, err := idempotency.Run(t.Context(), w.pool, req, w.write("second"))
	waited := time.Since(start)

	close(release)
	wg.Wait()

	if rigerr.CodeOf(err) != rigerr.CodeConflict {
		t.Fatalf("err = %v (%s), want Conflict", err, rigerr.CodeOf(err))
	}
	if waited < idempotency.LockTimeout {
		t.Errorf("gave up after %s, want it to have waited the whole %s first",
			waited, idempotency.LockTimeout)
	}
}

// The five seconds above bound the claim and nothing else.
//
// SET LOCAL lasts the rest of the transaction, so setting it and walking away
// would put a five-second ceiling on every lock the write goes on to take — and
// a write waiting on a contended row has not gone wrong, it is waiting. The
// shape of the bug is a 500 where there used to be a slow 201, which is the kind
// of thing that only shows up under the load that produces the contention. So
// the write is asked what it sees.
func TestTheLockTimeoutIsPutBackBeforeTheWriteRuns(t *testing.T) {
	w := setup(t)

	var seen string
	_, err := idempotency.Run(t.Context(), w.pool, w.req("k8b", "POST /todos", nil),
		func(ctx context.Context) (int, any, error) {
			if err := w.conn(ctx).QueryRow(ctx, "SHOW lock_timeout").Scan(&seen); err != nil {
				return 0, nil, err
			}
			return 201, nil, nil
		})
	if err != nil {
		t.Fatal(err)
	}

	// Whatever the server's own setting is — "0" out of the box — but not the
	// claim's.
	if want := "5s"; seen == want {
		t.Errorf("the write ran with lock_timeout = %s, want the claim's ceiling lifted", seen)
	}
}

// A 204 has no body, and it has to read back as no body rather than as the four
// bytes that spell null.
func TestAWriteWithNoBodyReplaysWithNoBody(t *testing.T) {
	w := setup(t)
	req := w.req("k9", "DELETE /todos/1", nil)

	empty := func(context.Context) (int, any, error) { return 204, nil, nil }
	if _, err := idempotency.Run(t.Context(), w.pool, req, empty); err != nil {
		t.Fatal(err)
	}
	got, err := idempotency.Run(t.Context(), w.pool, req, empty)
	if err != nil {
		t.Fatal(err)
	}

	if !got.Replayed || got.Status != 204 {
		t.Errorf("got %+v, want a replayed 204", got)
	}
	if len(got.Body) != 0 {
		t.Errorf("body = %q, want nothing at all", got.Body)
	}
}

// A stored response is replayed verbatim, so what the caller reads the second
// time is byte-for-byte what it read the first.
func TestTheStoredResponseIsReplayedVerbatim(t *testing.T) {
	w := setup(t)
	req := w.req("k10", "POST /todos", map[string]string{"title": "x"})

	body := map[string]any{"id": "abc", "count": 3, "nested": map[string]string{"a": "b"}}
	once := func(context.Context) (int, any, error) { return 201, body, nil }

	first, err := idempotency.Run(t.Context(), w.pool, req, once)
	if err != nil {
		t.Fatal(err)
	}
	second, err := idempotency.Run(t.Context(), w.pool, req, once)
	if err != nil {
		t.Fatal(err)
	}

	// Byte-for-byte, which is the reason the column is text. jsonb would answer
	// this with the same object rendered its own way — keys reordered,
	// whitespace its own — and a client that hashes or signs what it received
	// would see two different responses to one request.
	if string(first.Body) != string(second.Body) {
		t.Errorf("replayed %s, want %s", second.Body, first.Body)
	}
	var parsed map[string]any
	if err := json.Unmarshal(second.Body, &parsed); err != nil {
		t.Fatalf("the replayed body did not parse: %v (%s)", err, second.Body)
	}
	if parsed["id"] != "abc" {
		t.Errorf("replayed %v, want the stored response", parsed)
	}
}

// A retry that arrives a day later is not a retry — it is a second request that
// happens to reuse an identifier. Prune is what stops a stale key from becoming
// a write that silently does nothing.
func TestPruneDropsRecordsPastTheirRetentionAndKeepsTheRest(t *testing.T) {
	w := setup(t)
	ctx := t.Context()

	if _, err := idempotency.Run(ctx, w.pool,
		w.req("old", "POST /todos", nil), w.write("old")); err != nil {
		t.Fatal(err)
	}
	if _, err := idempotency.Run(ctx, w.pool,
		w.req("new", "POST /todos", nil), w.write("new")); err != nil {
		t.Fatal(err)
	}

	// Backdate one of them rather than wait a day for it.
	if _, err := w.pool.Exec(ctx,
		`UPDATE rig_idempotency SET created_at = now() - interval '48 hours'
		 WHERE tenant_id = $1 AND key = 'old'`, w.tenant); err != nil {
		t.Fatal(err)
	}

	n, err := idempotency.Prune(ctx, w.pool, idempotency.DefaultRetention)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 {
		t.Errorf("pruned %d, want at least the backdated one", n)
	}
	if got := w.records(t); got != 1 {
		t.Errorf("%d records left, want 1", got)
	}

	// And the pruned key is free again, which is the consequence worth stating:
	// past its retention, a key is a string nobody remembers.
	got, err := idempotency.Run(ctx, w.pool,
		w.req("old", "POST /todos", nil), w.write("again"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Replayed {
		t.Error("a pruned key still replayed")
	}
}

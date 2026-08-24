//go:build docker

// Presence, against a real database.
//
//	go test -tags docker ./internal/presencetest/
//
// Everything else about this package is pure and proves the rules. This applies
// the DDL, wires the service over it, and drives the heartbeat, the leave and
// the sweep — so a column that does not exist, an upsert whose conflict target
// is wrong, and a check constraint the validator disagrees with all fail here.
package presencetest

import (
	"context"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	authfnd "github.com/simonjanss/rig/auth/foundation"
	"github.com/simonjanss/rig/internal/dockerdb"
	"github.com/simonjanss/rig/presence"
	presencefnd "github.com/simonjanss/rig/presence/foundation"
	"github.com/simonjanss/rig/runtime/tenancy"
)

const containerName = "rigPresence-db"

// TestTheDDLApplies is first because everything below depends on it, and because
// dbschema.Set.Validate proves the set is coherent and nothing about the SQL
// running.
func TestTheDDLApplies(t *testing.T) {
	pool := database(t)

	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.columns WHERE table_name = $1`,
		presence.Table).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("%s has no columns, so the migration did not create it", presence.Table)
	}

	// The storage settings are load-bearing rather than decorative: without the
	// absolute autovacuum thresholds this table bloats forever, because the
	// default scale factor is a fraction of a table that never grows.
	var opts []string
	if err := pool.QueryRow(context.Background(),
		`SELECT reloptions FROM pg_class WHERE relname = $1`, presence.Table).Scan(&opts); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fillfactor=70", "autovacuum_vacuum_threshold=200"} {
		if !contains(opts, want) {
			t.Errorf("%s is missing %s; it has %v", presence.Table, want, opts)
		}
	}
}

// TestABeatIsAnUpsert is the claim the whole write path rests on: a client does
// not know whether its row exists, and with an upsert it does not have to.
func TestABeatIsAnUpsert(t *testing.T) {
	w := world(t)

	first, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "tab-a", Scope: "board"})
	if err != nil {
		t.Fatal(err)
	}

	w.clock = w.clock.Add(20 * time.Second)
	second, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "tab-a", Scope: "board"})
	if err != nil {
		t.Fatal(err)
	}

	if first.ID != second.ID {
		t.Errorf("a second beat wrote a new row (%s then %s): a tab open all day would be "+
			"four thousand rows", first.ID, second.ID)
	}
	if !second.SeenAt.After(first.SeenAt) {
		t.Errorf("seen_at did not move: %s then %s", first.SeenAt, second.SeenAt)
	}
	// created_at deliberately does not move, so "joined four minutes ago" stays
	// answerable.
	if !second.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("created_at moved on a heartbeat: %s then %s", first.CreatedAt, second.CreatedAt)
	}
	if n := w.count(t); n != 1 {
		t.Errorf("two beats left %d rows, want 1", n)
	}
}

// TestATabIsTheIdentity: two tabs are two presences. A row keyed by account
// alone would have them overwrite each other every beat, and the person would
// appear to teleport between the two things they are doing.
func TestATabIsTheIdentity(t *testing.T) {
	w := world(t)

	a, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "tab-a", Scope: "board"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "tab-b", Scope: "board"})
	if err != nil {
		t.Fatal(err)
	}

	if a.ID == b.ID {
		t.Fatal("two tabs share one row")
	}
	if n := w.count(t); n != 2 {
		t.Errorf("two tabs left %d rows, want 2", n)
	}
}

// TestTheTargetNarrows walks the three levels and asserts the row keeps up.
func TestTheTargetNarrows(t *testing.T) {
	w := world(t)
	row := uuid.New()

	for _, tc := range []struct {
		name   string
		target presence.Target
	}{
		{"in the scope", presence.Target{}},
		{"on a list", presence.Target{Table: "todo"}},
		{"on a row", presence.Target{Table: "todo", ID: row}},
		{"in a field", presence.Target{Table: "todo", ID: row, Field: "title"}},
		// And back out again, which is what a blur is. It has to clear the
		// column rather than leave the last field behind.
		{"and out of it", presence.Target{Table: "todo", ID: row}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{
				SessionKey: "tab-a", Scope: "board", Target: tc.target,
			})
			if err != nil {
				t.Fatal(err)
			}
			if got.Target != tc.target {
				t.Errorf("target is %+v, want %+v", got.Target, tc.target)
			}
		})
	}
}

// TestTheCheckConstraintsHold is the backstop under the validator. The service
// answers a bad target with a 422 that names the field; these are what make the
// rule true for a writer that is not the service.
func TestTheCheckConstraintsHold(t *testing.T) {
	w := world(t)

	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{
			"a field with no row",
			`INSERT INTO ` + presence.Table + `
			 (id, tenant_id, account_id, session_key, scope, target_field, seen_at)
			 VALUES ($1, $2, $3, 'raw', 'board', 'title', now())`,
			nil,
		},
		{
			"a row with no table",
			`INSERT INTO ` + presence.Table + `
			 (id, tenant_id, account_id, session_key, scope, target_id, seen_at)
			 VALUES ($1, $2, $3, 'raw', 'board', gen_random_uuid(), now())`,
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := w.pool.Exec(w.ctx, tc.sql, uuid.New(), w.tenant, w.accountDemo)
			if err == nil {
				t.Error("the database accepted a target no reader could use")
			}
		})
	}
}

// TestLeaveIsIdempotent: the caller is a page being torn down, which has nowhere
// to put an answer, and a retry of a request whose response was lost must not
// look like a failure.
func TestLeaveIsIdempotent(t *testing.T) {
	w := world(t)

	if _, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "tab-a", Scope: "board"}); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := w.svc.Leave(w.ctx, w.demo, "tab-a"); err != nil {
			t.Fatalf("leave %d: %v", i+1, err)
		}
	}
	if n := w.count(t); n != 0 {
		t.Errorf("%d rows survived the leave", n)
	}
}

// TestALeaveCannotReachAnotherAccount. Not the only thing in the way — no route
// takes an account — and the one that survives somebody adding one.
func TestALeaveCannotReachAnotherAccount(t *testing.T) {
	w := world(t)

	if _, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "shared", Scope: "board"}); err != nil {
		t.Fatal(err)
	}
	// Alex leaves a session key that is Demo's. It is Alex's own row that is
	// addressed, and there is none, so nothing happens.
	if err := w.svc.Leave(w.ctx, w.alex, "shared"); err != nil {
		t.Fatal(err)
	}
	if n := w.count(t); n != 1 {
		t.Errorf("Demo's row is gone: %d rows left, want 1", n)
	}
}

// TestOneAccountCannotWriteAnother's row: the same session key from two accounts
// is two rows, because the key is scoped by the account and the account comes
// from the credential.
func TestASharedKeyIsStillTwoRows(t *testing.T) {
	w := world(t)

	a, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "same", Scope: "board"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := w.svc.Beat(w.ctx, w.alex, presence.Beat{SessionKey: "same", Scope: "board"})
	if err != nil {
		t.Fatal(err)
	}

	if a.ID == b.ID {
		t.Fatal("one account's beat overwrote another's row")
	}
	if a.AccountID == b.AccountID {
		t.Fatal("both rows belong to one account")
	}
}

// TestHereIsFreshOnly is the exception that proves the package's rule: a plain
// read is a moment and can afford a predicate that moves, which the stream
// cannot.
func TestHereIsFreshOnly(t *testing.T) {
	w := world(t)

	if _, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "old", Scope: "board"}); err != nil {
		t.Fatal(err)
	}
	// Two TTLs later, that beat means nothing.
	w.clock = w.clock.Add(2 * presence.DefaultTTL)
	if _, err := w.svc.Beat(w.ctx, w.alex, presence.Beat{SessionKey: "new", Scope: "board"}); err != nil {
		t.Fatal(err)
	}

	here, err := w.svc.Here(w.ctx, w.demo, presence.Query{Scope: "board"})
	if err != nil {
		t.Fatal(err)
	}
	if len(here) != 1 {
		t.Fatalf("Here answered with %d rows, want 1 — the stale beat is still counted", len(here))
	}
	if here[0].SessionKey != "new" {
		t.Errorf("Here answered with %q, want the fresh one", here[0].SessionKey)
	}
	// Still two rows in the table. Nothing has swept yet, and Here filtering
	// rather than deleting is the whole point.
	if n := w.count(t); n != 2 {
		t.Errorf("Here removed rows: %d left, want 2", n)
	}
}

// TestHereNarrowsByTarget, so a card can ask about itself.
func TestHereNarrowsByTarget(t *testing.T) {
	w := world(t)
	card, other := uuid.New(), uuid.New()

	beats := []struct {
		key, table string
		id         uuid.UUID
	}{
		{"a", "todo", card},
		{"b", "todo", other},
	}
	for _, b := range beats {
		if _, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{
			SessionKey: b.key, Scope: "board",
			Target: presence.Target{Table: b.table, ID: b.id},
		}); err != nil {
			t.Fatal(err)
		}
	}

	here, err := w.svc.Here(w.ctx, w.demo, presence.Query{
		Target: presence.Target{Table: "todo", ID: card},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(here) != 1 || here[0].SessionKey != "a" {
		t.Fatalf("narrowing to one row answered %d rows: %+v", len(here), here)
	}
}

// TestAnotherTenantIsInvisible. Every read here is filtered on the tenant, and a
// suite that never checked would not notice the day one stopped being.
func TestAnotherTenantIsInvisible(t *testing.T) {
	w := world(t)

	if _, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "mine", Scope: "board"}); err != nil {
		t.Fatal(err)
	}

	other := w.newTenant(t)
	here, err := w.svc.Here(w.ctx, other, presence.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(here) != 0 {
		t.Errorf("another tenant sees %d of these rows", len(here))
	}
}

// TestACredentialWithNoAccountIsRefused. An API key and a system credential both
// have a nil account, and comparing against one would match nothing silently —
// which is the wrong kind of correct, because every machine caller in the tenant
// would land in one row they all fight over.
func TestACredentialWithNoAccountIsRefused(t *testing.T) {
	w := world(t)

	_, err := w.svc.Beat(w.ctx, tenancy.Claims{
		TenantID: w.tenant, Subject: tenancy.SubjectAPIKey,
	}, presence.Beat{SessionKey: "machine", Scope: "board"})
	if err == nil {
		t.Fatal("an API key was recorded as present")
	}
	if n := w.count(t); n != 0 {
		t.Errorf("%d rows were written for a credential with nobody behind it", n)
	}
}

// TestTheSweepIsWhatADepartureIs — the load-bearing assertion of the whole
// design. A shape's filter is re-evaluated when a row changes, so expiry has to
// be a row that goes.
func TestTheSweepIsWhatADepartureIs(t *testing.T) {
	w := world(t)
	w.ownTheTable(t)

	if _, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{SessionKey: "stale", Scope: "board"}); err != nil {
		t.Fatal(err)
	}
	// Past the TTL but inside the grace: still there, and deliberately. A row
	// has to be invisible before it is gone, never the other way round.
	w.clock = w.clock.Add(presence.DefaultTTL + time.Second)
	if _, err := w.svc.Beat(w.ctx, w.alex, presence.Beat{SessionKey: "live", Scope: "board"}); err != nil {
		t.Fatal(err)
	}

	sweeper := presence.NewSweeper(presence.SweeperConfig{Service: w.svc, Interval: -1})
	report, err := sweeper.Sweep(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Expired != 0 {
		t.Errorf("swept %d rows inside the grace period, want 0", report.Expired)
	}

	// Past the grace as well.
	w.clock = w.clock.Add(presence.DefaultGrace + time.Second)
	report, err = sweeper.Sweep(w.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Expired != 1 {
		t.Errorf("swept %d rows, want 1", report.Expired)
	}
	if n := w.count(t); n != 1 {
		t.Errorf("%d rows left, want 1 — the fresh one", n)
	}
}

// TestTwoSweepersAgree is the assertion that stands in for the claim lease the
// notification dispatcher needs and this deliberately does not: deleting rows
// that are already expired is idempotent, so two replicas at once agree and the
// loser deletes nothing.
func TestTwoSweepersAgree(t *testing.T) {
	w := world(t)
	w.ownTheTable(t)

	for i := range 20 {
		if _, err := w.svc.Beat(w.ctx, w.demo, presence.Beat{
			SessionKey: "tab-" + string(rune('a'+i)), Scope: "board",
		}); err != nil {
			t.Fatal(err)
		}
	}
	w.clock = w.clock.Add(presence.DefaultTTL + presence.DefaultGrace + time.Minute)

	a := presence.NewSweeper(presence.SweeperConfig{Service: w.svc, Interval: -1})
	b := presence.NewSweeper(presence.SweeperConfig{Service: w.svc, Interval: -1})

	var wg sync.WaitGroup
	reports := make([]presence.SweepReport, 2)
	errs := make([]error, 2)
	for i, s := range []*presence.Sweeper{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reports[i], errs[i] = s.Sweep(w.ctx)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("sweeper %d: %v", i, err)
		}
	}
	if total := reports[0].Expired + reports[1].Expired; total != 20 {
		t.Errorf("two sweepers removed %d rows between them, want 20 — a row was "+
			"double-counted or missed", total)
	}
	if n := w.count(t); n != 0 {
		t.Errorf("%d rows survived two sweeps", n)
	}
}

// --- the world ---------------------------------------------------------------

type harness struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	svc    *presence.Service
	clock  time.Time
	tenant uuid.UUID

	accountDemo, accountAlex uuid.UUID
	demo, alex               tenancy.Claims
}

// world gives one test a tenant, two accounts and a clock it can move.
//
// A tenant per test rather than a database per test: the container is shared and
// every query here is tenant-filtered, so isolation is the thing under test
// doing its job.
func world(t *testing.T) *harness {
	t.Helper()

	pool := database(t)
	w := &harness{
		ctx:   context.Background(),
		pool:  pool,
		clock: time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC),
	}
	w.tenant = w.insertTenant(t)
	w.accountDemo = w.insertAccount(t, w.tenant, "demo@example.test")
	w.accountAlex = w.insertAccount(t, w.tenant, "alex@example.test")
	w.demo = claims(w.tenant, w.accountDemo)
	w.alex = claims(w.tenant, w.accountAlex)

	w.svc = presence.NewService(presence.Config{
		DB:      pool,
		Targets: []string{"todo"},
		// Read through a pointer to the harness so a test can move the clock
		// between beats, which is the only way to exercise a TTL without waiting
		// one out.
		Now: func() time.Time { return w.clock },
	})
	return w
}

// newTenant is a second tenant with an account in it, for the isolation check.
func (w *harness) newTenant(t *testing.T) tenancy.Claims {
	t.Helper()
	tenant := w.insertTenant(t)
	return claims(tenant, w.insertAccount(t, tenant, "stranger@example.test"))
}

func (w *harness) insertTenant(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := w.pool.Exec(w.ctx,
		`INSERT INTO rig_tenant (id, name, slug) VALUES ($1, $2, $3)`,
		id, "t-"+id.String()[:8], "t-"+id.String()[:8])
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (w *harness) insertAccount(t *testing.T, tenant uuid.UUID, email string) uuid.UUID {
	t.Helper()

	// Unique per identity, because rig_identity_email_key is global: two tenants
	// in one test would otherwise collide on the same address.
	address := uuid.New().String()[:8] + "-" + email

	identity := uuid.New()
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO rig_identity (id, email_address, display_name) VALUES ($1, $2, $3)`,
		identity, address, email); err != nil {
		t.Fatal(err)
	}

	id := uuid.New()
	if _, err := w.pool.Exec(w.ctx,
		`INSERT INTO rig_account (id, tenant_id, identity_id, kind, email_address, display_name)
		 VALUES ($1, $2, $3, 'Person', $4, $5)`,
		id, tenant, identity, address, email); err != nil {
		t.Fatal(err)
	}
	return id
}

// ownTheTable clears every other tenant's rows, so a test may assert on what a
// sweep counted.
//
// It is here because the sweeper is deliberately *not* tenant-scoped: expiry is a
// property of the clock rather than of a tenant, so one pass is one statement for
// the whole table. That is the right design and it means a SweepReport from a
// shared container counts whatever the tests before it left behind. Every other
// test in this file is isolated by its tenant; these two need the table.
func (w *harness) ownTheTable(t *testing.T) {
	t.Helper()
	if _, err := w.pool.Exec(w.ctx,
		`DELETE FROM `+presence.Table+` WHERE tenant_id <> $1`, w.tenant); err != nil {
		t.Fatal(err)
	}
}

// count is how many presence rows this tenant has, fresh or not.
func (w *harness) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := w.pool.QueryRow(w.ctx,
		`SELECT count(*) FROM `+presence.Table+` WHERE tenant_id = $1`, w.tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func claims(tenant, account uuid.UUID) tenancy.Claims {
	return tenancy.Claims{TenantID: tenant, AccountID: account, Subject: tenancy.SubjectAccount}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// --- the container -----------------------------------------------------------

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

	// A schema left behind by an earlier run would make every one of these tests
	// pass for the wrong reason.
	_ = exec.Command("docker", "rm", "-f", "-v", dockerdb.Qualify(containerName)).Run()

	db, err := dockerdb.Start(ctx, dockerdb.Config{
		Image:    "postgres:17-alpine",
		Name:     dockerdb.Qualify(containerName),
		Port:     dockerdb.HostPort(dockerdb.PortPresence),
		Database: "rig", User: "rig", Password: "rig",
	})
	if err != nil {
		return nil, err
	}

	pool, err := pgxpool.New(ctx, db.URL())
	if err != nil {
		return nil, err
	}

	// The tenancy migration first, because a presence row names an account and a
	// tenant. Applied from the set rather than from a copy, so this tests the SQL
	// that ships.
	for _, set := range []struct {
		name string
		sql  func() ([]byte, error)
	}{
		{"tenancy", func() ([]byte, error) { return authfnd.Set().Read("tenancy") }},
		{"presence", func() ([]byte, error) { return presencefnd.Set().Read("presence") }},
	} {
		raw, err := set.sql()
		if err != nil {
			return nil, err
		}
		if _, err := pool.Exec(ctx, upOf(string(raw))); err != nil {
			return nil, err
		}
	}
	return pool, nil
}

// upOf is the Up half of a goose migration, executed directly.
//
// Directly rather than through goose because there is no project here and no
// bookkeeping to record: these two files are applied once, to a container that is
// thrown away, and running them as one statement each is what the single
// StatementBegin block in both of them already assumes.
func upOf(sql string) string {
	const (
		begin = "-- +goose StatementBegin"
		end   = "-- +goose StatementEnd"
	)
	i := indexOf(sql, begin)
	j := indexOf(sql, end)
	if i < 0 || j < 0 {
		return sql
	}
	return sql[i+len(begin) : j]
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

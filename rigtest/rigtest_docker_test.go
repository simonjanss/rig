//go:build docker

package rigtest_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth/foundation"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/rigtest"
)

// sources is rig's own foundation, which is what the reads in this package are
// reads of. A suite here cannot invent a schema to run against: the whole claim
// of the package is that these queries match the DDL rig ships, so they are
// checked against that DDL and nothing else.
func sources() []migrate.Source {
	set := foundation.Set()
	return []migrate.Source{{Name: set.Module, FS: set.FS, Dir: set.Dir, Table: set.Table}}
}

func start(t *testing.T) *rigtest.Rig {
	t.Helper()
	return rigtest.New(t, rigtest.Config{DatabaseURL: database(t), Migrations: sources()})
}

func TestATenantIsUsable(t *testing.T) {
	t.Parallel()

	rt := start(t)
	one := rt.Tenant(t)
	two := rt.Tenant(t, rigtest.Named("Acme"))

	if one.ID == two.ID || one.Slug == two.Slug {
		t.Fatalf("two tenants came out the same: %+v and %+v", one, two)
	}
	if two.Name != "Acme" {
		t.Errorf("Named did not take: %q", two.Name)
	}
}

// TestTwoTenantsInTheSameMillisecondDoNotCollide is the trap this fixture exists
// to close. A slug built from the leading characters of a UUIDv7 is the high bits
// of a millisecond timestamp, so every identifier minted inside about a minute
// shares them — and under parallel tests that is a unique-constraint violation
// on lower(slug) rather than a flake.
func TestTwoTenantsInTheSameMillisecondDoNotCollide(t *testing.T) {
	t.Parallel()

	rt := start(t)
	seen := map[string]bool{}
	for range 50 {
		slug := rt.Tenant(t).Slug
		if seen[slug] {
			t.Fatalf("two tenants got the slug %q", slug)
		}
		seen[slug] = true
	}
}

func TestReadingIdentitiesAndAccounts(t *testing.T) {
	t.Parallel()

	rt := start(t)
	tenant := rt.Tenant(t)

	if got := rt.Identity(t, "nobody@example.com"); got != nil {
		t.Fatalf("an address nobody has resolved to %+v", got)
	}

	identity := seedIdentity(t, rt, "ada@example.com", "Ada")
	found := rt.Identity(t, "ADA@example.com")
	if found == nil {
		t.Fatal("an address is matched case-insensitively, and this one was not")
	}
	if found.ID != identity || found.DisplayName != "Ada" || found.Verified() {
		t.Errorf("read back %+v", found)
	}

	// Invited and not yet arrived: rig writes the identity and a pending
	// membership, and accepting is what creates the account. The pair of reads
	// answering (row, nil) is what that state looks like from here.
	if got := rt.Account(t, tenant.ID, identity); got != nil {
		t.Fatalf("an identity with no account resolved to %+v", got)
	}

	seedAccount(t, rt, tenant.ID, identity, "ada@example.com")
	accounts := rt.Accounts(t, tenant.ID)
	if len(accounts) != 1 {
		t.Fatalf("the tenant has %d accounts, want 1", len(accounts))
	}
	if accounts[0].Role != "Basic" || accounts[0].Kind != "Person" {
		t.Errorf("the defaults did not take: %+v", accounts[0])
	}
	if rt.Account(t, tenant.ID, identity) == nil {
		t.Error("the account exists and Account did not find it")
	}
}

// TestTheAuthLogReadsItsOwnEnums is what the outcome constants are for. Both
// columns are Postgres enums, so a wrong label is an error from the driver
// rather than an assertion that fails.
func TestTheAuthLogReadsItsOwnEnums(t *testing.T) {
	t.Parallel()

	rt := start(t)
	tenant := rt.Tenant(t)
	address := "log-" + uuid.NewString()[:8] + "@example.com"

	seedLog(t, rt, tenant.ID, address, "EmailCodeRequested", rigtest.Succeeded, nil)
	seedLog(t, rt, tenant.ID, address, "EmailCodeRequested", rigtest.Failed,
		map[string]any{"reason": "no such address"})

	all := rt.AuthLog(t, rigtest.LogQuery{EmailAddress: address})
	if len(all) != 2 {
		t.Fatalf("read %d entries, want 2", len(all))
	}
	if n := rt.Events(t, rigtest.LogQuery{
		EmailAddress: address, Outcome: rigtest.Failed,
	}); n != 1 {
		t.Errorf("counted %d failures, want 1", n)
	}

	failed := rt.AuthLog(t, rigtest.LogQuery{EmailAddress: address, Outcome: rigtest.Failed})
	if got := failed[0].Reason(); got != "no such address" {
		t.Errorf("the detail said %q", got)
	}
	if got := all[0].Reason(); got != "" {
		t.Errorf("an entry with no detail said %q", got)
	}

	rt.ForgetAuthLog(t, "EmailCodeRequested", address)
	if n := rt.Events(t, rigtest.LogQuery{EmailAddress: address}); n != 0 {
		t.Errorf("%d entries survived being forgotten", n)
	}
}

// TestWhereSomebodyWasLast covers the read with no column behind it: the tenant
// of the newest session root, which is not the newest token and not a count.
func TestWhereSomebodyWasLast(t *testing.T) {
	t.Parallel()

	rt := start(t)
	first, second := rt.Tenant(t), rt.Tenant(t)
	identity := seedIdentity(t, rt, "sam-"+uuid.NewString()[:8]+"@example.com", "Sam")

	if got := rt.LastTenant(t, identity); got != uuid.Nil {
		t.Fatalf("somebody who never signed in was last in %s", got)
	}

	firstAccount := seedAccount(t, rt, first.ID, identity, "sam@example.com")
	root := seedSession(t, rt, first.ID, firstAccount)
	if got := rt.LastTenant(t, identity); got != first.ID {
		t.Fatalf("last tenant is %s, want %s", got, first.ID)
	}

	// A refresh in the first tenant, which shares the first root's family. If
	// this read counted tokens rather than roots, it would now say the first
	// tenant forever.
	seedRefresh(t, rt, first.ID, firstAccount, root)

	rt.AgeSessions(t, identity, time.Hour)
	secondAccount := seedAccount(t, rt, second.ID, identity, "sam@example.com")
	seedSession(t, rt, second.ID, secondAccount)

	if got := rt.LastTenant(t, identity); got != second.ID {
		t.Errorf("last tenant is %s, want %s — the newest root, not the busiest family", got, second.ID)
	}
}

// TestAnIncompleteConfigFailsRatherThanSkips is the promise the package makes
// about itself: a suite with nowhere to run must stop, because one that skipped
// would report the same green as one that ran and nothing would say which it was.
func TestAnIncompleteConfigFailsRatherThanSkips(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		cfg  rigtest.Config
	}{
		{"neither a DatabaseURL nor a Pool", rigtest.Config{Migrations: sources()}},
		{"no Migrations", rigtest.Config{DatabaseURL: database(t)}},
		{"nothing answering", rigtest.Config{
			DatabaseURL: "postgres://rig:rig@127.0.0.1:1/rig?sslmode=disable",
			Migrations:  sources(),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if said := refusal(func(tb testing.TB) { rigtest.New(tb, tc.cfg) }); said == "" {
				t.Error("it was accepted without a word")
			}
		})
	}
}

// refusal runs something against a stand-in TB and answers with what it said,
// or "" if it said nothing. A real testing.TB cannot be used because Fatal is
// the behaviour under test.
func refusal(f func(testing.TB)) (said string) {
	var r recorder
	defer func() {
		_ = recover()
		said = r.said
	}()
	f(&r)
	return r.said
}

// recorder is a testing.TB that records a refusal and unwinds, the way Fatal
// ends a test. Everything it does not implement is embedded and never reached.
type recorder struct {
	testing.TB
	said string
}

func (r *recorder) Helper()        {}
func (r *recorder) Cleanup(func()) {}

func (r *recorder) Fatal(args ...any) {
	r.said = fmt.Sprint(args...)
	panic(r.said)
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.said = fmt.Sprintf(format, args...)
	panic(r.said)
}

func seedIdentity(t *testing.T, rt *rigtest.Rig, address, name string) uuid.UUID {
	t.Helper()

	id := uuid.New()
	write(t, rt, `INSERT INTO rig_identity (id, email_address, display_name) VALUES ($1, $2, $3)`,
		id, address, name)
	return id
}

func seedAccount(t *testing.T, rt *rigtest.Rig, tenantID, identityID uuid.UUID, address string) uuid.UUID {
	t.Helper()

	id := uuid.New()
	write(t, rt, `
		INSERT INTO rig_account (id, tenant_id, identity_id, email_address, display_name)
		VALUES ($1, $2, $3, $4, $4)`, id, tenantID, identityID, address)
	return id
}

func seedSession(t *testing.T, rt *rigtest.Rig, tenantID, accountID uuid.UUID) uuid.UUID {
	t.Helper()

	id := uuid.New()
	write(t, rt, `
		INSERT INTO rig_account_token
			(id, tenant_id, account_id, kind, root_token_id, secret_hash, expires_at)
		VALUES ($1, $2, $3, 'Refresh', $1, '\x00', now() + interval '1 hour')`,
		id, tenantID, accountID)
	return id
}

func seedRefresh(t *testing.T, rt *rigtest.Rig, tenantID, accountID, root uuid.UUID) {
	t.Helper()

	write(t, rt, `
		INSERT INTO rig_account_token
			(id, tenant_id, account_id, kind, root_token_id, parent_token_id,
			 secret_hash, expires_at)
		VALUES ($1, $2, $3, 'Refresh', $4, $4, '\x00', now() + interval '1 hour')`,
		uuid.New(), tenantID, accountID, root)
}

func seedLog(
	t *testing.T, rt *rigtest.Rig, tenantID uuid.UUID, address, event, outcome string,
	detail map[string]any,
) {
	t.Helper()

	write(t, rt, `
		INSERT INTO rig_auth_log (id, tenant_id, event, outcome, email_address, detail)
		VALUES ($1, $2, $3::rig_auth_event, $4::rig_auth_outcome, $5, $6)`,
		uuid.New(), tenantID, event, outcome, address, detail)
}

func write(t *testing.T, rt *rigtest.Rig, sql string, args ...any) {
	t.Helper()
	if _, err := rt.Pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatal(err)
	}
}

// TestABorrowedPoolIsNotClosed covers the shape a suite adopting this package
// one read at a time uses: it already has a pool, and closing it here would end
// somebody else's test rather than this one.
func TestABorrowedPoolIsNotClosed(t *testing.T) {
	t.Parallel()

	pool, err := pgxpool.New(context.Background(), database(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	rt := rigtest.New(t, rigtest.Config{Pool: pool, Migrations: sources()})
	if rt.Pool != pool {
		t.Fatal("a borrowed pool was replaced")
	}
	// Inside a subtest, so that its cleanups have run by the time the parent
	// asks whether the pool still works.
	t.Run("used", func(t *testing.T) {
		rigtest.New(t, rigtest.Config{Pool: pool, Migrations: sources()}).Tenant(t)
	})
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("the borrowed pool was closed underneath its owner: %v", err)
	}
}

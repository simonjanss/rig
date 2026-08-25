//go:build docker

// The invalidation channel, over the real stores.
//
//	go test -tags docker ./internal/authtest/
//
// internal/cachetest proves that Postgres delivers a notification when the
// transaction issuing it commits and discards it when that transaction rolls
// back. This proves the other half — that rig's own revocations issue one, on
// the transaction that revoked something, for every token or key that stopped
// being usable. It needs the real SQL, because what is under test is a
// `RETURNING` clause and a transaction boundary.
//
// Two managers over one database is the shape every test here takes, because it
// is the shape the design is for: one process revokes, another was holding the
// answer, and nothing but the channel connects them.
package authtest

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/auth/authpg"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/cache"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// errRolledBack fails a transaction on purpose.
var errRolledBack = errors.New("authtest: rolled back on purpose")

// replicas builds two of something over one database, each with a bus of its own
// on a channel this test does not share.
//
// A channel per test, so that a package running them in parallel does not have
// one test's revocations clearing another's maps — which would make every
// assertion here pass for the wrong reason.
func replicas[T any](t *testing.T, build func(*cache.Bus, *authpg.Stores) T) (T, T) {
	t.Helper()

	pool := database(t)
	// An identifier, because the name reaches Postgres quoted in a LISTEN and as
	// a parameter to pg_notify, and NewBus refuses anything else.
	channel := "rig_cache_test_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "_")

	var made []T
	for range 2 {
		bus := cache.NewBus(cache.BusConfig{Pool: pool, Channel: channel})
		bus.Start()
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = bus.Close(ctx)
		})
		waitLive(t, bus)
		made = append(made, build(bus, authpg.New(pool)))
	}
	return made[0], made[1]
}

// waitLive blocks until the listener has connected.
//
// Start is asynchronous, and a cache whose bus is not live reads through and
// stores nothing — so a test that raced it would fill no map and then prove
// nothing when the entry it meant to see withdrawn had never been there.
func waitLive(t *testing.T, bus *cache.Bus) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !bus.Live() {
		if time.Now().After(deadline) {
			t.Fatal("the cache bus never connected")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settle waits for a notification to be delivered and applied.
//
// Delivery is asynchronous by nature: the publishing transaction commits and the
// listening connection is woken after it. Polling is the honest way to wait —
// the alternative is a sleep long enough to be flaky in the other direction.
func settle(t *testing.T, what string, done func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: the invalidation never arrived", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sessionManagers is the pair of session managers every session test here wants.
func sessionManagers(t *testing.T) (*session.Manager, *session.Manager) {
	t.Helper()

	return replicas(t, func(bus *cache.Bus, stores *authpg.Stores) *session.Manager {
		m, err := session.New(session.Config{
			Store: stores.Sessions, Log: stores.Log,
			// An hour, so that nothing here can pass because a lifetime ran out.
			AccessTTL: time.Hour, RefreshTTL: time.Hour,
			Cache: session.NewTokenCache(bus, session.TokenCacheConfig{TTL: time.Hour}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	})
}

// A session revoked on one replica stops verifying on the other, with no clock
// advance and a lifetime far longer than the test.
//
// This is the whole feature in one assertion. If the lifetime were the guarantee
// rather than the backstop, the reader would go on letting the token through for
// an hour.
func TestARevocationReachesAnotherReplica(t *testing.T) {
	t.Parallel()

	h := setup(t)
	ctx := context.Background()
	writer, reader := sessionManagers(t)

	pair, err := writer.Issue(ctx, session.IssueInput{
		TenantID: h.tenant, AccountID: h.account, Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The reader holds the answer now, which is what makes the next step mean
	// something: without it the verification below would simply read the row.
	if _, err := reader.Verify(ctx, pair.Access.Token); err != nil {
		t.Fatal(err)
	}

	if err := writer.Revoke(ctx, pair.RootTokenID); err != nil {
		t.Fatal(err)
	}

	settle(t, "a revoked session", func() bool {
		_, err := reader.Verify(ctx, pair.Access.Token)
		return err != nil
	})
}

// A revocation that rolls back withdraws nothing, and this is what makes
// publishing inside the writing transaction the right place for it rather than
// merely a convenient one.
//
// The revocation and its notification are issued, and the transaction they share
// is then failed. Postgres throws the notification away with the UPDATE, so the
// reader is still holding a session that is still valid — the correct answer,
// and the one a publish on the pool would have got wrong.
func TestAnInvalidationRolledBackIsNotDelivered(t *testing.T) {
	t.Parallel()

	h := setup(t)
	ctx := context.Background()
	writer, reader := sessionManagers(t)

	pair, err := writer.Issue(ctx, session.IssueInput{
		TenantID: h.tenant, AccountID: h.account, Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Verify(ctx, pair.Access.Token); err != nil {
		t.Fatal(err)
	}

	// Revoke joins this transaction rather than opening one of its own — that is
	// what dbx.InTx does with a transaction already on the context — so failing
	// here takes the UPDATE and the pg_notify with it.
	err = h.stores.Sessions.InTx(ctx, func(ctx context.Context) error {
		if err := writer.Revoke(ctx, pair.RootTokenID); err != nil {
			return err
		}
		return errRolledBack
	})
	if !errors.Is(err, errRolledBack) {
		t.Fatalf("the transaction should have failed with the sentinel: %v", err)
	}

	// Long enough that a notification would have arrived if one had been sent.
	time.Sleep(500 * time.Millisecond)
	if _, err := reader.Verify(ctx, pair.Access.Token); err != nil {
		t.Errorf("a rolled-back revocation withdrew a session that is still valid: %v", err)
	}

	// And the session really is still there, so the check above was about the
	// notification rather than about a revoke that never ran.
	if _, err := writer.Verify(ctx, pair.Access.Token); err != nil {
		t.Errorf("the rollback should have left the session usable: %v", err)
	}
}

// Every token the revocation killed is withdrawn, not only the root.
//
// The cache is keyed by the access token a request presents, and a session is a
// family — so a revoke that named only the root would leave every access token
// under it answering out of memory until its lifetime ran out. This is why
// RevokeFamily returns identifiers instead of a count.
func TestEveryTokenInTheFamilyIsWithdrawn(t *testing.T) {
	t.Parallel()

	h := setup(t)
	ctx := context.Background()
	writer, reader := sessionManagers(t)

	first, err := writer.Issue(ctx, session.IssueInput{
		TenantID: h.tenant, AccountID: h.account, Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	// A refresh, so the family holds a second access token that is not the root
	// and not the one issued with it.
	second, err := writer.Rotate(ctx, first.Refresh.Token)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reader.Verify(ctx, second.Access.Token); err != nil {
		t.Fatal(err)
	}

	if err := writer.Revoke(ctx, first.RootTokenID); err != nil {
		t.Fatal(err)
	}

	settle(t, "a refreshed session's newest access token", func() bool {
		_, err := reader.Verify(ctx, second.Access.Token)
		return err != nil
	})
}

// An API key revoked on one replica stops verifying on the other, over a
// different topic on the same channel — which is also what shows two topics
// sharing one LISTEN without sharing entries.
func TestAKeyRevocationReachesAnotherReplica(t *testing.T) {
	t.Parallel()

	h := setup(t)
	ctx := context.Background()

	writer, reader := replicas(t, func(bus *cache.Bus, stores *authpg.Stores) *apikey.Manager {
		m, err := apikey.New(apikey.Config{
			Store: stores.APIKeys, Log: stores.Log,
			Cache: apikey.NewKeyCache(bus, apikey.KeyCacheConfig{TTL: time.Hour}),
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	})

	minted, err := writer.Mint(ctx, apikey.MintInput{
		TenantID: h.tenant, AccountID: h.account, Name: "ci", Scopes: []string{"note.read"},
	})
	if err != nil {
		t.Fatal(err)
	}

	from := netip.MustParseAddr("198.51.100.7")
	if _, _, err := reader.Verify(ctx, minted.Secret, from); err != nil {
		t.Fatal(err)
	}

	if err := writer.Revoke(ctx, h.tenant, minted.Key.ID); err != nil {
		t.Fatal(err)
	}

	settle(t, "a revoked API key", func() bool {
		_, _, err := reader.Verify(ctx, minted.Secret, from)
		return err != nil
	})
}

// keyFailureReplicas is a pair of managers with the failure limit cached, over
// the real rig_auth_log the limit counts.
//
// The limiter is the Postgres one rather than a fake, because what is under test
// is the count against that table and the notification that withdraws it — a
// counter held in memory would prove neither.
func keyFailureReplicas(t *testing.T, maxN int) (*apikey.Manager, *apikey.Manager) {
	t.Helper()

	pool := database(t)
	return replicas(t, func(bus *cache.Bus, stores *authpg.Stores) *apikey.Manager {
		m, err := apikey.New(apikey.Config{
			Store: stores.APIKeys, Log: stores.Log,
			Cache: apikey.NewKeyCache(bus, apikey.KeyCacheConfig{TTL: time.Hour}),
			FailureCache: apikey.NewFailureCache(bus, apikey.FailureCacheConfig{
				// An hour, so that nothing here can pass because a lifetime ran
				// out. Only the channel can make these assertions hold.
				TTL: time.Hour,
			}),
			Limiter: throttle.New(throttle.NewPostgres(pool, throttle.PostgresConfig{})),
			FailureLimit: throttle.Limit{
				Name:      "apikey.failed",
				Event:     throttle.EventAPIKeyAuthFailed,
				ClearedBy: throttle.EventAPIKeyAuthSucceeded,
				Max:       maxN,
				Window:    time.Hour,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return m
	})
}

// wrongSecret is a well-formed key that is not the minted one.
func wrongSecret(minted apikey.Minted) string {
	return apikey.Prefix + minted.Key.KeyID + "_" + strings.Repeat("A", 52)
}

// Failures counted on one replica lock the key on the other, even though the
// other was holding "no failures for this key".
//
// This is the assertion that makes the failure cache something other than a hole
// in the limit. The reader is deliberately warmed first — a successful
// verification, which is what puts the zero in its map — and the lifetime is an
// hour, so nothing here can pass by expiring. If the invalidation did not cross,
// the reader would go on waving the key through for the rest of that hour while
// somebody ground secrets against it next door.
func TestKeyFailuresOnOneReplicaLockTheOther(t *testing.T) {
	t.Parallel()

	h := setup(t)
	ctx := context.Background()
	writer, reader := keyFailureReplicas(t, 3)

	minted, err := writer.Mint(ctx, apikey.MintInput{
		TenantID: h.tenant, AccountID: h.account, Name: "ci", Scopes: []string{"note.read"},
	})
	if err != nil {
		t.Fatal(err)
	}

	from := netip.MustParseAddr("198.51.100.7")
	if _, _, err := reader.Verify(ctx, minted.Secret, from); err != nil {
		t.Fatal(err)
	}

	// Every one of these is counted on the writer and has to reach the reader,
	// or the reader's held zero survives all four.
	wrong := wrongSecret(minted)
	for range 4 {
		_, _, _ = writer.Verify(ctx, wrong, from)
	}

	settle(t, "a key locked by another replica's failures", func() bool {
		_, _, err := reader.Verify(ctx, minted.Secret, from)
		return rigerr.Is(err, rigerr.CodeRateLimited)
	})
}

// And the honest key is answered from memory rather than by counting rows.
//
// Asserted by moving the count behind rig's back: the rows go in with plain SQL,
// so nothing publishes and nothing is withdrawn. A reader that still lets the key
// through is one that never went back to the table — and a reader that refuses it
// was counting on every request, which is the cost this exists to remove.
//
// It is also the honest statement of the boundary. A write nobody publishes from
// is invisible for a lifetime, which is what `ttl` is the backstop for, and why
// rig caches only the counts whose writes it makes itself.
func TestACleanKeyIsAnsweredWithoutCountingRows(t *testing.T) {
	t.Parallel()

	h := setup(t)
	ctx := context.Background()
	writer, reader := keyFailureReplicas(t, 3)

	minted, err := writer.Mint(ctx, apikey.MintInput{
		TenantID: h.tenant, AccountID: h.account, Name: "ci", Scopes: []string{"note.read"},
	})
	if err != nil {
		t.Fatal(err)
	}

	from := netip.MustParseAddr("198.51.100.7")
	if _, _, err := reader.Verify(ctx, minted.Secret, from); err != nil {
		t.Fatal(err)
	}

	for range 10 {
		if _, err := h.pool.Exec(ctx, `
			INSERT INTO rig_auth_log (id, tenant_id, created_at, event, outcome, api_key_ref)
			VALUES ($1, $2, now(), $3, 'Failed', $4)`,
			uuid.New(), h.tenant, throttle.EventAPIKeyAuthFailed, minted.Key.KeyID); err != nil {
			t.Fatal(err)
		}
	}

	if _, _, err := reader.Verify(ctx, minted.Secret, from); err != nil {
		t.Fatalf("the reader answered %v; it counted rows it should have had held in memory", err)
	}
}

// grantsPair is a cache and the function wrapped in it, which these tests need
// both halves of: one to read through and one to publish on.
type grantsPair struct {
	cache  *auth.GrantsCache
	grants authhttp.Grants
}

// grantsReplicas builds a pair, each over a bus of its own, answering whatever
// held says at the moment they are asked.
//
// A function rather than a table because what is under test is the channel, not
// anybody's authorization model — and the model is exactly the part rig has no
// opinion about.
func grantsReplicas(t *testing.T, held *string) (*grantsPair, *grantsPair) {
	t.Helper()

	return replicas(t, func(bus *cache.Bus, _ *authpg.Stores) *grantsPair {
		// An hour, so that nothing here can pass because a lifetime ran out.
		c := auth.NewGrantsCache(auth.GrantsCacheConfig{TTL: time.Hour})
		g := c.Wrap(func(context.Context, uuid.UUID, uuid.UUID) ([]string, []string, error) {
			return []string{*held}, nil, nil
		})
		c.Serve(bus)
		return &grantsPair{cache: c, grants: g}
	})
}

// A role change published on one replica's transaction reaches another replica's
// cache.
//
// This is the half rig cannot do on an application's behalf, proved once here so
// that nobody has to discover it in their own project. The reader is warmed with
// the old answer, the writer publishes inside a transaction, and the reader has
// to have forgotten by the time that transaction commits.
func TestAGrantInvalidationReachesAnotherReplica(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()
	tenant, account := uuid.New(), uuid.New()

	held := "basic"
	writer, reader := grantsReplicas(t, &held)

	roles, _, err := reader.grants(ctx, tenant, account)
	if err != nil {
		t.Fatal(err)
	}
	if roles[0] != "basic" {
		t.Fatalf("warmed with %v, want basic", roles)
	}

	held = "owner"

	// In a transaction, which is what makes the invalidation atomic with the
	// change that caused it. The publisher hears its own notification, exactly
	// like every other topic on this bus.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.cache.Invalidate(ctx, tx, tenant, account); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	settle(t, "a withdrawn grant", func() bool {
		roles, _, err := reader.grants(ctx, tenant, account)
		return err == nil && roles[0] == "owner"
	})
}

// And one rolled back is not delivered, so a role change that failed does not
// leave every replica having thrown away what was still true.
func TestAGrantInvalidationRolledBackIsNotDelivered(t *testing.T) {
	t.Parallel()

	pool := database(t)
	ctx := context.Background()
	tenant, account := uuid.New(), uuid.New()

	held := "basic"
	writer, reader := grantsReplicas(t, &held)

	if _, _, err := reader.grants(ctx, tenant, account); err != nil {
		t.Fatal(err)
	}
	held = "owner"

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.cache.Invalidate(ctx, tx, tenant, account); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	// Long enough that a notification which was going to arrive would have.
	time.Sleep(250 * time.Millisecond)
	roles, _, err := reader.grants(ctx, tenant, account)
	if err != nil {
		t.Fatal(err)
	}
	if roles[0] != "basic" {
		t.Error("a rolled-back invalidation was delivered; the reader forgot an answer " +
			"that was never withdrawn")
	}
}

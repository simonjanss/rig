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

	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authpg"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/cache"
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

package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authhttp"
)

// An internal test, for the reason auth/session's and auth/apikey's are: the
// cache is built from a [cache.Bus] and a bus needs a pool, but what is worth
// checking here is what the wrapper does with the answers — held, copied,
// keyed and withdrawn. testGrants assembles the map directly, with no Live
// function and no topic, so every path but the publish runs.
func testGrants(t *testing.T, g authhttp.Grants) (authhttp.Grants, *GrantsCache) {
	t.Helper()
	c := NewGrantsCache(GrantsCacheConfig{TTL: time.Hour})
	// Served on no bus, which is live with nothing to publish on: a nil
	// *cache.Topic is a working no-op, so every path but the publish runs.
	c.served.Store(&servedOn{})
	return c.Wrap(g), c
}

// counted wraps a Grants function and says how often it was actually asked.
type counted struct {
	calls       int
	roles       []string
	permissions []string
	err         error
}

func (c *counted) grants() authhttp.Grants {
	return func(context.Context, uuid.UUID, uuid.UUID) ([]string, []string, error) {
		c.calls++
		if c.err != nil {
			return nil, nil, c.err
		}
		return append([]string(nil), c.roles...), append([]string(nil), c.permissions...), nil
	}
}

// A cache that was never attached to a bus holds nothing, which is what a
// project with no cache: block has. Every call still reaches the role tables,
// and the withdrawal methods are still safe to call — so the role writes look
// the same whether or not the block is there.
func TestAnUnservedCacheHoldsNothing(t *testing.T) {
	t.Parallel()

	c := &counted{roles: []string{"owner"}}
	cached := NewGrantsCache(GrantsCacheConfig{})
	g := cached.Wrap(c.grants())
	cached.Serve(nil) // a project with no cache: block

	tenant, account := uuid.New(), uuid.New()
	for range 3 {
		if _, _, err := g(context.Background(), tenant, account); err != nil {
			t.Fatal(err)
		}
	}
	if c.calls != 3 {
		t.Errorf("the wrapped function was called %d times, want 3: nothing should be held", c.calls)
	}

	// And the withdrawal is a no-op rather than a panic, which is what lets a
	// call site drop it in without a condition.
	if err := cached.Invalidate(context.Background(), nil, tenant, account); err != nil {
		t.Errorf("Invalidate on a nil cache answered %v, want nil", err)
	}
	if err := cached.InvalidateAll(context.Background(), nil); err != nil {
		t.Errorf("InvalidateAll on a nil cache answered %v, want nil", err)
	}
	cached.Forget(tenant, account)
}

// The same account in two tenants holds two different sets, so the key has to
// carry both. Keying on the account alone would serve one tenant's permissions
// to a request for the other, which is the worst bug this file could have.
func TestGrantsAreHeldPerTenant(t *testing.T) {
	t.Parallel()

	account := uuid.New()
	one, two := uuid.New(), uuid.New()

	g, _ := testGrants(t, func(
		_ context.Context, tenantID, _ uuid.UUID,
	) ([]string, []string, error) {
		if tenantID == one {
			return []string{"owner"}, []string{"note.delete"}, nil
		}
		return []string{"basic"}, []string{"note.read"}, nil
	})

	for range 2 {
		roles, _, err := g(context.Background(), one, account)
		if err != nil {
			t.Fatal(err)
		}
		if len(roles) != 1 || roles[0] != "owner" {
			t.Fatalf("tenant one answered %v, want owner", roles)
		}
		roles, _, err = g(context.Background(), two, account)
		if err != nil {
			t.Fatal(err)
		}
		if len(roles) != 1 || roles[0] != "basic" {
			t.Fatalf("tenant two answered %v; one tenant's grants reached the other", roles)
		}
	}
}

// The slices reach a handler as claims, so what is handed out is a copy. One
// request appending to them would be widening what every other request in the
// window may do.
func TestTheHeldGrantsAreCopied(t *testing.T) {
	t.Parallel()

	c := &counted{roles: []string{"basic"}, permissions: []string{"note.read"}}
	g, _ := testGrants(t, c.grants())

	tenant, account := uuid.New(), uuid.New()
	roles, permissions, err := g(context.Background(), tenant, account)
	if err != nil {
		t.Fatal(err)
	}
	roles[0] = "owner"
	permissions[0] = "note.delete"

	roles, permissions, err = g(context.Background(), tenant, account)
	if err != nil {
		t.Fatal(err)
	}
	if roles[0] != "basic" || permissions[0] != "note.read" {
		t.Errorf("the next caller got %v / %v; one request's edit reached another's claims",
			roles, permissions)
	}
	if c.calls != 1 {
		t.Errorf("the wrapped function was called %d times, want 1", c.calls)
	}
}

// A role table that was briefly unreachable must not answer "no permissions"
// for a lifetime. The map never keeps what a failing loader returned.
func TestAFailedLookupIsNotHeld(t *testing.T) {
	t.Parallel()

	c := &counted{err: errors.New("role table unreachable")}
	g, _ := testGrants(t, c.grants())

	tenant, account := uuid.New(), uuid.New()
	for range 3 {
		if _, _, err := g(context.Background(), tenant, account); err == nil {
			t.Fatal("the error should have reached the caller")
		}
	}
	if c.calls != 3 {
		t.Errorf("the wrapped function was called %d times, want 3: a failure must not be held", c.calls)
	}

	// And once it answers, that answer is held.
	c.err, c.roles = nil, []string{"basic"}
	for range 2 {
		if _, _, err := g(context.Background(), tenant, account); err != nil {
			t.Fatal(err)
		}
	}
	if c.calls != 4 {
		t.Errorf("the wrapped function was called %d times, want 4", c.calls)
	}
}

// Somebody with no roles is a real answer, unlike a session that does not
// exist: the identifier was not supplied by whoever is asking, so there is no
// map to fill with invented ones.
func TestAnEmptyAnswerIsHeld(t *testing.T) {
	t.Parallel()

	c := &counted{}
	g, _ := testGrants(t, c.grants())

	tenant, account := uuid.New(), uuid.New()
	for range 3 {
		roles, permissions, err := g(context.Background(), tenant, account)
		if err != nil {
			t.Fatal(err)
		}
		if len(roles) != 0 || len(permissions) != 0 {
			t.Fatalf("answered %v / %v, want nothing", roles, permissions)
		}
	}
	if c.calls != 1 {
		t.Errorf("the wrapped function was called %d times, want 1", c.calls)
	}
}

// Withdrawing locally is enough to prove the wiring; that it reaches another
// replica is what internal/authtest checks against a real Postgres.
func TestForgetSendsTheNextRequestBackToTheTables(t *testing.T) {
	t.Parallel()

	c := &counted{roles: []string{"basic"}}
	g, cached := testGrants(t, c.grants())

	tenant, account := uuid.New(), uuid.New()
	if _, _, err := g(context.Background(), tenant, account); err != nil {
		t.Fatal(err)
	}
	cached.Forget(tenant, account)

	c.roles = []string{"owner"}
	roles, _, err := g(context.Background(), tenant, account)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 1 || roles[0] != "owner" {
		t.Errorf("answered %v after a withdrawal, want the new roles", roles)
	}
}

package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/cache"
)

// An internal test, because a [TokenCache] is built from a [cache.Bus] and a bus
// needs a pool. What is worth proving here is not the channel — internal/cachetest
// settles that against a real Postgres — but that verification behaves the same
// whether the answer came from the map or from the row, and that is a claim
// about this package which should not need a container to check.
//
// So the cache is assembled directly, with the map a bus would have registered
// and no topic to publish on. A nil *cache.Topic is a working no-op, so the
// invalidation path still runs and still drops what it is told to.
func testCache(ttl time.Duration, now func() time.Time) *TokenCache {
	return &TokenCache{m: cache.NewMap[*Token](cache.MapConfig{TTL: ttl, Now: now})}
}

type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time          { return c.at }
func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func cachedManager(t *testing.T, ttl time.Duration, c *fakeClock, cfg Config) *Manager {
	t.Helper()
	cfg.Store = NewMemoryStore()
	cfg.Now = c.now
	cfg.Cache = testCache(ttl, c.now)
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func issue(t *testing.T, m *Manager) Pair {
	t.Helper()
	pair, err := m.Issue(context.Background(), IssueInput{
		TenantID: uuid.New(), AccountID: uuid.New(), Client: ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

// A revocation this process performed takes effect on the next request whether
// or not the answer was cached, and that is the whole design in one assertion:
// the invalidation travels with the revocation instead of waiting for a
// lifetime to run out.
//
// Here there is no channel, so the manager drops the entry locally — which is
// the same code path a notification takes when it arrives, one line further on.
// That a *notification* is delivered on commit and discarded on rollback is
// Postgres's claim to make and internal/cachetest's to check.
func TestARevocationBeatsTheCache(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m := cachedManager(t, time.Hour, c, Config{})
	pair := issue(t, m)

	// Read once, so the answer is in the map.
	if _, err := m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(context.Background(), pair.RootTokenID); err != nil {
		t.Fatal(err)
	}

	// No clock advance at all, and a lifetime an hour long: if the cache were
	// the guarantee this would still succeed.
	if _, err := m.Verify(context.Background(), pair.Access.Token); err == nil {
		t.Error("a revoked token should stop working on the next request, cached or not")
	}
}

// Every check runs on the answer whichever side of the cache it came from. A
// short access lifetime is only short if it is enforced, and a cache that
// answered before the expiry check would quietly extend every one of them.
func TestTheCacheNeverOutlivesTheToken(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m := cachedManager(t, time.Hour, c, Config{AccessTTL: time.Minute})
	pair := issue(t, m)

	if _, err := m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Fatal(err)
	}

	c.advance(2 * time.Minute)
	if _, err := m.Verify(context.Background(), pair.Access.Token); err == nil {
		t.Error("an expired token should not be answered from a longer-lived cache")
	}
}

// The identifier is the key and the secret is not part of it, so a warm entry
// must not become a way to authenticate by knowing an identifier — which
// travels in logs and in the audit trail and was never a secret.
func TestAWrongSecretIsNeverAnsweredFromTheCache(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m := cachedManager(t, time.Hour, c, Config{})
	pair := issue(t, m)

	if _, err := m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Fatal(err)
	}

	id, _, _ := strings.Cut(strings.TrimPrefix(pair.Access.Token, PrefixAccess), ".")
	forged := PrefixAccess + id + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := m.Verify(context.Background(), forged); err == nil {
		t.Error("knowing an identifier is not knowing the token")
	}
}

// A miss is answered and never stored. The identifier half of a token is
// supplied by whoever is asking, so an unknown one that stuck would let anybody
// fill the map with invented ids and evict every real entry in it.
func TestAMissIsNotCached(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m := cachedManager(t, time.Hour, c, Config{})

	invented := format(KindAccess, uuid.New(), make(secret, secretBytes))
	for range 3 {
		if _, err := m.Verify(context.Background(), invented); err == nil {
			t.Fatal("a token that does not exist should not verify")
		}
	}
	if n := m.cache.m.Len(); n != 0 {
		t.Errorf("the map holds %d entries after three misses; a miss must not be stored", n)
	}
}

// What the map hands back is shared by every request inside the window, so what
// goes into it is a copy. Two slices, one of them the hash every verification
// compares against.
func TestTheCachedTokenIsACopy(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m := cachedManager(t, time.Hour, c, Config{})
	pair := issue(t, m)

	first, err := m.Verify(context.Background(), pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	// What a careless caller does to what it was handed.
	for i := range first.SecretHash {
		first.SecretHash[i] = 0
	}

	if _, err := m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Errorf("one caller editing what it was handed broke the next verification: %v", err)
	}
}

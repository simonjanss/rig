package apikey

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/cache"
)

// An internal test, for the reason auth/session's is: a [KeyCache] is built from
// a [cache.Bus] and a bus needs a pool, but what is worth checking here is that
// verification behaves the same whether the key came from the map or the row.
// The map is assembled directly, with no topic to publish on — a nil
// *cache.Topic is a working no-op, so the invalidation path still runs.
func testCache(ttl time.Duration, now func() time.Time) *KeyCache {
	return &KeyCache{m: cache.NewMap[*Key](cache.MapConfig{TTL: ttl, Now: now})}
}

type fakeClock struct{ at time.Time }

func (c *fakeClock) now() time.Time          { return c.at }
func (c *fakeClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func cachedManager(t *testing.T, c *fakeClock) (*Manager, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	m, err := New(Config{
		Store: store,
		Now:   c.now,
		Cache: testCache(time.Hour, c.now),
	})
	if err != nil {
		t.Fatal(err)
	}
	return m, store
}

func mintOne(t *testing.T, m *Manager, tenant uuid.UUID) (Minted, uuid.UUID) {
	t.Helper()
	acct := uuid.New()
	minted, err := m.Mint(context.Background(), MintInput{
		TenantID: tenant, AccountID: acct, Name: "ci", Scopes: []string{"note.read"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return minted, acct
}

// A revocation this process performed takes effect on the next request whether
// or not the key was cached. The lifetime is an hour and the clock does not
// move: if the cache were the guarantee, this would still let the key through.
func TestARevocationBeatsTheCache(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, _ := cachedManager(t, c)
	tenant := uuid.New()
	minted, _ := mintOne(t, m, tenant)

	from := netip.MustParseAddr("198.51.100.7")
	if _, _, err := m.Verify(context.Background(), minted.Secret, from); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(context.Background(), tenant, minted.Key.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.Verify(context.Background(), minted.Secret, from); err == nil {
		t.Error("a revoked key should stop working on the next request, cached or not")
	}
}

// The last-used timestamp is written at most once per interval, and a cached
// copy carrying the old one would turn that into a write per request. What the
// cache must not do is make an interval-bounded write unbounded.
func TestTouchingDoesNotBecomeAWritePerRequest(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, store := cachedManager(t, c)
	tenant := uuid.New()
	minted, _ := mintOne(t, m, tenant)

	from := netip.MustParseAddr("198.51.100.7")
	for range 5 {
		if _, _, err := m.Verify(context.Background(), minted.Secret, from); err != nil {
			t.Fatal(err)
		}
	}

	k, err := store.Find(context.Background(), tenant, minted.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k.LastUsedAt == nil {
		t.Fatal("the first use should have been recorded")
	}
	first := *k.LastUsedAt

	// Inside the interval, so nothing more should be written — which is only
	// true if the cached copy learned about the first touch.
	c.advance(time.Minute)
	if _, _, err := m.Verify(context.Background(), minted.Secret, from); err != nil {
		t.Fatal(err)
	}
	k, err = store.Find(context.Background(), tenant, minted.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !k.LastUsedAt.Equal(first) {
		t.Errorf("last used moved to %s inside the touch interval; a cached copy is "+
			"reporting a stale timestamp and every request is writing", k.LastUsedAt)
	}

	// Past it, one more write.
	c.advance(DefaultTouchInterval)
	if _, _, err := m.Verify(context.Background(), minted.Secret, from); err != nil {
		t.Fatal(err)
	}
	k, err = store.Find(context.Background(), tenant, minted.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if k.LastUsedAt.Equal(first) {
		t.Error("past the touch interval the last-used time should have moved")
	}
}

// A miss is answered and never stored: the identifier is supplied by whoever is
// asking, and an unknown one that stuck would let anybody evict every real
// entry by inventing identifiers.
func TestAMissIsNotCached(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, _ := cachedManager(t, c)

	from := netip.MustParseAddr("198.51.100.7")
	for range 3 {
		if _, _, err := m.Verify(context.Background(), Prefix+"AAAAAAAAAAAAAAAA_nonsense", from); err == nil {
			t.Fatal("a key that does not exist should not verify")
		}
	}
	if n := m.cache.m.Len(); n != 0 {
		t.Errorf("the map holds %d entries after three misses; a miss must not be stored", n)
	}
}

// The scopes a cached key carries become a caller's permissions, so what is
// handed out is a copy — one request appending to them would be widening what
// every other request in the window may do.
func TestTheCachedKeyIsACopy(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, _ := cachedManager(t, c)
	minted, _ := mintOne(t, m, uuid.New())

	from := netip.MustParseAddr("198.51.100.7")
	_, first, err := m.Verify(context.Background(), minted.Secret, from)
	if err != nil {
		t.Fatal(err)
	}
	first.Scopes[0] = "note.delete"
	for i := range first.SecretHash {
		first.SecretHash[i] = 0
	}

	claims, _, err := m.Verify(context.Background(), minted.Secret, from)
	if err != nil {
		t.Fatalf("one caller editing what it was handed broke the next verification: %v", err)
	}
	if len(claims.Permissions) != 1 || claims.Permissions[0] != "note.read" {
		t.Errorf("the next caller got %v; one request's edit reached another's permissions",
			claims.Permissions)
	}
}

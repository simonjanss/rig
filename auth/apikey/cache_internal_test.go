package apikey

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/runtime/cache"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// An internal test, for the reason auth/session's is: a [KeyCache] is built from
// a [cache.Bus] and a bus needs a pool, but what is worth checking here is that
// verification behaves the same whether the key came from the map or the row.
//
// Assembled with no bus and then [cache.Keyed.ServeLocally], which is the posture
// of a cache attached to no channel: it holds values and forgets them in this
// process alone, so the invalidation path still runs with nothing to publish on.
func testCache(ttl time.Duration, now func() time.Time) *KeyCache {
	k := cache.NewKeyed(cache.KeyedConfig[*Key]{
		Topic: KeyTopic, TTL: ttl, Now: now, Clone: (*Key).clone,
	})
	k.ServeLocally()
	return &KeyCache{k: k}
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
	if n := m.cache.k.Len(); n != 0 {
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

// testFailureCache is the failure count's half of [testCache], assembled the same
// way and for the same reason: served locally, so it holds without a channel.
func testFailureCache(ttl time.Duration, now func() time.Time) *FailureCache {
	k := cache.NewKeyed(cache.KeyedConfig[struct{}]{Topic: FailureTopic, TTL: ttl, Now: now})
	k.ServeLocally()
	return &FailureCache{k: k}
}

// countingCounter is a [throttle.Counter] that says how often it was asked.
//
// The number is the whole point of the cache, so it is the thing the tests
// assert on rather than a timing.
type countingCounter struct {
	next  *throttle.Memory
	calls int
}

func (c *countingCounter) Count(
	ctx context.Context, limit throttle.Limit, key throttle.Key, since time.Time,
) (int, time.Time, error) {
	c.calls++
	return c.next.Count(ctx, limit, key, since)
}

// limitedCachedManager wires a manager with both caches and a failure limit,
// with the counter behind the limit fed from the log the manager writes — which
// is how the real one is fed, and the reason the two cannot drift.
func limitedCachedManager(t *testing.T, c *fakeClock, maxN int) (*Manager, *countingCounter) {
	t.Helper()

	counter := &countingCounter{next: throttle.NewMemory()}
	log := &feedingLog{}
	m, err := New(Config{
		Store:        NewMemoryStore(),
		Log:          log,
		Now:          c.now,
		Cache:        testCache(time.Hour, c.now),
		FailureCache: testFailureCache(time.Hour, c.now),
		Limiter:      throttle.New(counter).WithClock(c.now),
		FailureLimit: throttle.Limit{
			Name:      "apikey.failed",
			Event:     throttle.EventAPIKeyAuthFailed,
			ClearedBy: throttle.EventAPIKeyAuthSucceeded,
			Max:       maxN,
			Window:    time.Minute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	log.into = counter.next
	return m, counter
}

// feedingLog records the two events the limit counts, and nothing else.
type feedingLog struct{ into *throttle.Memory }

func (l *feedingLog) Write(_ context.Context, e authlog.Entry) {
	if l.into == nil || e.APIKeyRef == "" {
		return
	}
	switch e.Event {
	case throttle.EventAPIKeyAuthFailed, throttle.EventAPIKeyAuthSucceeded:
		l.into.Record(e.Event, throttle.APIKey(e.APIKeyRef), e.At)
	}
}

// The failure limit is checked before the key is looked up, so with only the
// key cached an API-key request still costs a query — which is the one the
// `cache:` block is supposed to have removed.
func TestACleanKeyIsCountedOnce(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, counter := limitedCachedManager(t, c, 3)
	minted, _ := mintOne(t, m, uuid.New())

	from := netip.MustParseAddr("198.51.100.7")
	for i := range 5 {
		if _, _, err := m.Verify(context.Background(), minted.Secret, from); err != nil {
			t.Fatalf("verification %d failed: %v", i+1, err)
		}
	}
	if counter.calls != 1 {
		t.Errorf("five requests with a correct secret counted rows %d times, want 1", counter.calls)
	}
}

// A count above zero is answered and never held: that caller is the one the
// limit exists for, and their next attempt has to be counted rather than
// guessed at.
func TestAKeyWithFailuresIsNotHeld(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, _ := limitedCachedManager(t, c, 5)
	minted, _ := mintOne(t, m, uuid.New())

	from := netip.MustParseAddr("198.51.100.7")
	wrong := Prefix + minted.Key.KeyID + "_" + strings.Repeat("A", 52)
	if _, _, err := m.Verify(context.Background(), wrong, from); err == nil {
		t.Fatal("a wrong secret should not verify")
	}
	if _, _, err := m.Verify(context.Background(), wrong, from); err == nil {
		t.Fatal("a wrong secret should not verify")
	}
	if n := m.failures.k.Len(); n != 0 {
		t.Errorf("the map holds %d entries for a key with failures against it, want 0", n)
	}
}

// The one this is all for. A held "no failures" that outlived the failure that
// made it wrong is not a cache, it is a hole in the limit — so the refusal has
// to land on the same attempt it would have without one, and the clock does not
// move.
func TestTheLimitStillBitesWithTheCountCached(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, _ := limitedCachedManager(t, c, 3)
	minted, _ := mintOne(t, m, uuid.New())

	from := netip.MustParseAddr("198.51.100.7")
	wrong := Prefix + minted.Key.KeyID + "_" + strings.Repeat("A", 52)

	// A successful request first, so the zero is genuinely held before anything
	// goes wrong. Without the invalidation this is what would let the next four
	// through.
	if _, _, err := m.Verify(context.Background(), minted.Secret, from); err != nil {
		t.Fatal(err)
	}
	// Past the success, because a success clears the window and one recorded at
	// the same instant as the failures would wipe them.
	c.advance(time.Second)

	for i := range 3 {
		if _, _, err := m.Verify(context.Background(), wrong, from); !rigerr.Is(err, rigerr.CodeUnauthorized) {
			t.Fatalf("attempt %d answered %v, want 401", i+1, err)
		}
	}
	if _, _, err := m.Verify(context.Background(), wrong, from); rigerr.CodeOf(err) != rigerr.CodeRateLimited {
		t.Fatal("the fourth wrong secret was not rate limited; the held count outlived the failures")
	}
}

// And a success still clears the window with the cache on, which is the
// property that keeps an integration misconfigured for a minute from being
// locked out for the rest of it.
func TestASuccessStillClearsTheWindowWithTheCountCached(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, _ := limitedCachedManager(t, c, 3)
	minted, _ := mintOne(t, m, uuid.New())

	from := netip.MustParseAddr("198.51.100.7")
	wrong := Prefix + minted.Key.KeyID + "_" + strings.Repeat("A", 52)

	for range 2 {
		_, _, _ = m.Verify(context.Background(), wrong, from)
	}
	c.advance(time.Second)
	if _, _, err := m.Verify(context.Background(), minted.Secret, from); err != nil {
		t.Fatalf("the correct secret was refused before the limit: %v", err)
	}

	c.advance(time.Second)
	for i := range 3 {
		if _, _, err := m.Verify(context.Background(), wrong, from); rigerr.CodeOf(err) == rigerr.CodeRateLimited {
			t.Fatalf("attempt %d after a success hit the limit; the window did not clear", i+1)
		}
	}
}

// A key id nobody minted is the one failure an unauthenticated caller can
// produce at will, so its withdrawal never leaves the process. What has to stay
// true is that the limit still bites exactly when it would have with no cache:
// the local drop is synchronous, so the count is fresh on every attempt.
//
// There is no cross-replica assertion to write here, and that is the argument
// rather than a gap: a replica only ever holds a zero for an identifier
// presented to it, stored by the limit check and dropped inside the same
// request, so no other replica is holding an answer a publish could withdraw.
func TestAnUnknownKeyIDIsLimitedWithoutPublishing(t *testing.T) {
	t.Parallel()

	c := &fakeClock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, counter := limitedCachedManager(t, c, 3)

	from := netip.MustParseAddr("198.51.100.7")
	invented := Prefix + strings.Repeat("A", 16) + "_" + strings.Repeat("A", 52)

	for i := range 3 {
		if _, _, err := m.Verify(context.Background(), invented, from); !rigerr.Is(err, rigerr.CodeUnauthorized) {
			t.Fatalf("attempt %d answered %v, want 401", i+1, err)
		}
	}
	if _, _, err := m.Verify(context.Background(), invented, from); rigerr.CodeOf(err) != rigerr.CodeRateLimited {
		t.Fatal("the fourth invented key id was not rate limited; a held zero outlived the failures")
	}

	// Counted on every attempt, because nothing was left held to answer from —
	// which is also what keeps invented identifiers from filling the map.
	if counter.calls != 4 {
		t.Errorf("four attempts counted rows %d times, want 4", counter.calls)
	}
	if n := m.failures.k.Len(); n != 0 {
		t.Errorf("the map holds %d entries for an identifier nobody minted, want 0", n)
	}
}

package apikey_test

import (
	"context"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
	"github.com/simonjanss/rig/runtime/throttle"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

type recorder struct {
	entries []authlog.Entry
	// onWrite mirrors an entry as it is written, standing in for what
	// rig_auth_log does for free: the limiter counts the rows the trail keeps,
	// so in production there is nothing to keep in step and here there is.
	onWrite func(authlog.Entry)
}

func (r *recorder) Write(_ context.Context, e authlog.Entry) {
	r.entries = append(r.entries, e)
	if r.onWrite != nil {
		r.onWrite(e)
	}
}

func (r *recorder) last(event string) (authlog.Entry, bool) {
	for i := len(r.entries) - 1; i >= 0; i-- {
		if r.entries[i].Event == event {
			return r.entries[i], true
		}
	}
	return authlog.Entry{}, false
}

type fixture struct {
	m     *apikey.Manager
	store *apikey.MemoryStore
	log   *recorder
	clock *clock

	tenant  uuid.UUID
	service uuid.UUID
}

func setup(t *testing.T) *fixture {
	t.Helper()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	store := apikey.NewMemoryStore()
	log := &recorder{}

	m, err := apikey.New(apikey.Config{Store: store, Log: log, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{m: m, store: store, log: log, clock: c, tenant: uuid.New(), service: uuid.New()}
}

func (f *fixture) mint(t *testing.T, in apikey.MintInput) apikey.Minted {
	t.Helper()

	if in.TenantID == uuid.Nil {
		in.TenantID = f.tenant
	}
	if in.AccountID == uuid.Nil {
		in.AccountID = f.service
	}
	if in.Name == "" {
		in.Name = "nightly export"
	}

	minted, err := f.m.Mint(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return minted
}

var anywhere = netip.MustParseAddr("203.0.113.10")

func TestMintAndVerify(t *testing.T) {
	t.Parallel()

	f := setup(t)
	minted := f.mint(t, apikey.MintInput{Scopes: []string{"export.run"}})

	// A leaked key should be identifiable on sight by a scanner watching a
	// public repository.
	if !strings.HasPrefix(minted.Secret, apikey.Prefix) {
		t.Errorf("secret = %q", minted.Secret)
	}
	// The public half is in the value, so a log line can name the key without
	// the row being fetched.
	if !strings.Contains(minted.Secret, minted.Key.KeyID) {
		t.Error("the presented value should carry the public identifier")
	}

	claims, k, err := f.m.Verify(context.Background(), minted.Secret, anywhere)
	if err != nil {
		t.Fatal(err)
	}
	if k.ID != minted.Key.ID {
		t.Error("the wrong key was resolved")
	}
	if claims.Subject != tenancy.SubjectAPIKey {
		t.Errorf("subject = %q, want ApiKey", claims.Subject)
	}
	if claims.TenantID != f.tenant || claims.AccountID != f.service {
		t.Error("the key should act as its service account, inside its tenant")
	}
	// A key's scopes are its permissions. There is no role lookup, or a
	// machine credential would gain powers whenever somebody edited a role.
	if len(claims.Permissions) != 1 || claims.Permissions[0] != "export.run" {
		t.Errorf("permissions = %v", claims.Permissions)
	}
}

// Nothing stored can produce the secret again. That is the whole reason it is
// safe to store.
func TestTheSecretIsNotRecoverable(t *testing.T) {
	t.Parallel()

	f := setup(t)
	minted := f.mint(t, apikey.MintInput{})

	stored, err := f.store.Find(context.Background(), f.tenant, minted.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(stored.SecretHash), minted.Secret) {
		t.Fatal("the secret is in the row")
	}
	for _, k := range mustList(t, f) {
		if strings.Contains(k.Name+k.KeyID, strings.TrimPrefix(minted.Secret, apikey.Prefix+k.KeyID+"_")) {
			t.Error("the secret leaked into a listable field")
		}
	}
}

func TestBadKeysAreRefused(t *testing.T) {
	t.Parallel()

	f := setup(t)
	real := f.mint(t, apikey.MintInput{})

	// Same identifier, different secret. Knowing the public half is not
	// knowing the key.
	forged := apikey.Prefix + real.Key.KeyID + "_" + strings.Repeat("A", 52)

	for _, tc := range []struct{ name, presented string }{
		{"empty", ""},
		{"no prefix", "hunter2"},
		{"no separator", apikey.Prefix + "abc"},
		{"unknown identifier", apikey.Prefix + "ZZZZZZZZZZZZZZZZ_" + strings.Repeat("A", 52)},
		{"right identifier wrong secret", forged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := f.m.Verify(context.Background(), tc.presented, anywhere)
			if err == nil {
				t.Fatal("this should not have verified")
			}
			if !rigerr.Is(err, rigerr.CodeUnauthorized) {
				t.Errorf("err = %v, want 401", err)
			}
		})
	}
}

// A limit on failed key authentication is useless if it can only count keys
// that exist.
func TestAFailureAgainstAnUnknownKeyIsStillCountable(t *testing.T) {
	t.Parallel()

	f := setup(t)
	presented := apikey.Prefix + "ZZZZZZZZZZZZZZZZ_" + strings.Repeat("A", 52)

	if _, _, err := f.m.Verify(context.Background(), presented, anywhere); err == nil {
		t.Fatal("an unknown key should not verify")
	}

	e, ok := f.log.last(authlog.EventAPIKeyAuthFailed)
	if !ok {
		t.Fatal("the failure should be recorded")
	}
	if e.APIKeyRef != "ZZZZZZZZZZZZZZZZ" {
		t.Errorf("the entry should name the key as presented, got %q", e.APIKeyRef)
	}
	if e.IPAddress != anywhere.String() {
		t.Errorf("the entry should say where it came from, got %q", e.IPAddress)
	}
}

func TestRevocationTakesEffectImmediately(t *testing.T) {
	t.Parallel()

	f := setup(t)
	minted := f.mint(t, apikey.MintInput{})

	if err := f.m.Revoke(context.Background(), f.tenant, minted.Key.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Verify(context.Background(), minted.Secret, anywhere); err == nil {
		t.Error("a revoked key should stop working on the next request")
	}
}

func TestExpiry(t *testing.T) {
	t.Parallel()

	f := setup(t)
	at := f.clock.at.Add(time.Hour)
	minted := f.mint(t, apikey.MintInput{ExpiresAt: &at})

	f.clock.advance(59 * time.Minute)
	if _, _, err := f.m.Verify(context.Background(), minted.Secret, anywhere); err != nil {
		t.Fatalf("the key has not expired yet: %v", err)
	}

	f.clock.advance(2 * time.Minute)
	if _, _, err := f.m.Verify(context.Background(), minted.Secret, anywhere); err == nil {
		t.Error("an expired key should be refused")
	}
}

func TestTheAllowListIsEnforced(t *testing.T) {
	t.Parallel()

	f := setup(t)
	minted := f.mint(t, apikey.MintInput{
		CIDRAllowList: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")},
	})

	if _, _, err := f.m.Verify(context.Background(), minted.Secret,
		netip.MustParseAddr("10.4.1.9")); err != nil {
		t.Errorf("an address inside the range should be allowed: %v", err)
	}
	if _, _, err := f.m.Verify(context.Background(), minted.Secret, anywhere); err == nil {
		t.Error("an address outside the range should be refused")
	}

	// A restriction that fails open when the address is unknown is advisory,
	// not a restriction.
	if _, _, err := f.m.Verify(context.Background(), minted.Secret, netip.Addr{}); err == nil {
		t.Error("an unknown address should not satisfy an allow list")
	}
}

// Recording the last use on every request turns a read into a write on the
// hottest path a machine integration has.
func TestLastUsedIsWrittenSparingly(t *testing.T) {
	t.Parallel()

	f := setup(t)
	minted := f.mint(t, apikey.MintInput{})

	verify := func() {
		if _, _, err := f.m.Verify(context.Background(), minted.Secret, anywhere); err != nil {
			t.Fatal(err)
		}
	}

	verify()
	first := mustFind(t, f, minted.Key.ID).LastUsedAt
	if first == nil {
		t.Fatal("the first use should be recorded")
	}

	f.clock.advance(time.Minute)
	verify()
	if got := mustFind(t, f, minted.Key.ID).LastUsedAt; !got.Equal(*first) {
		t.Error("a use a minute later should not have been written")
	}

	f.clock.advance(apikey.DefaultTouchInterval)
	verify()
	if got := mustFind(t, f, minted.Key.ID).LastUsedAt; got.Equal(*first) {
		t.Error("a use after the interval should have been written")
	}
}

// Revoking immediately on rotation breaks whatever is deployed with the old
// value, which is why nobody rotates keys.
func TestRotationGivesTheOldKeyAnOverlap(t *testing.T) {
	t.Parallel()

	f := setup(t)
	old := f.mint(t, apikey.MintInput{Scopes: []string{"export.run"}})

	fresh, err := f.m.Rotate(context.Background(), f.tenant, old.Key.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if fresh.Secret == old.Secret {
		t.Fatal("rotation should mint a new secret")
	}
	// The replacement carries the same authority, or a rotation would be a
	// silent downgrade.
	if _, k, err := f.m.Verify(context.Background(), fresh.Secret, anywhere); err != nil {
		t.Fatal(err)
	} else if len(k.Scopes) != 1 || k.Scopes[0] != "export.run" {
		t.Errorf("the new key has scopes %v", k.Scopes)
	}

	f.clock.advance(30 * time.Minute)
	if _, _, err := f.m.Verify(context.Background(), old.Secret, anywhere); err != nil {
		t.Errorf("the old key should still work inside the overlap: %v", err)
	}

	f.clock.advance(time.Hour)
	if _, _, err := f.m.Verify(context.Background(), old.Secret, anywhere); err == nil {
		t.Error("the old key should be dead once the overlap has passed")
	}
}

// A key known to have leaked should be replaceable without an overlap.
func TestRotationWithNoOverlapKillsTheOldKeyAtOnce(t *testing.T) {
	t.Parallel()

	f := setup(t)
	old := f.mint(t, apikey.MintInput{})

	if _, err := f.m.Rotate(context.Background(), f.tenant, old.Key.ID, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Verify(context.Background(), old.Secret, anywhere); err == nil {
		t.Error("the old key should be dead immediately")
	}
}

func TestAKeyNeedsAName(t *testing.T) {
	t.Parallel()

	f := setup(t)

	// A list of keys called "" is a list nobody can safely revoke from.
	_, err := f.m.Mint(context.Background(), apikey.MintInput{
		TenantID: f.tenant, AccountID: f.service, Name: "   ",
	})
	if err == nil {
		t.Error("an unnamed key should be refused")
	}
}

func TestListIsScopedToTheTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.mint(t, apikey.MintInput{Name: "ours"})
	f.mint(t, apikey.MintInput{TenantID: uuid.New(), Name: "theirs"})

	keys := mustList(t, f)
	if len(keys) != 1 || keys[0].Name != "ours" {
		t.Errorf("got %d keys, want only this tenant's", len(keys))
	}
}

func mustFind(t *testing.T, f *fixture, id uuid.UUID) *apikey.Key {
	t.Helper()

	k, err := f.store.Find(context.Background(), f.tenant, id)
	if err != nil || k == nil {
		t.Fatalf("find %s: %v", id, err)
	}
	return k
}

func mustList(t *testing.T, f *fixture) []*apikey.Key {
	t.Helper()

	keys, err := f.m.List(context.Background(), f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// Both identifiers are required. A key with no tenant is a key the scoping
// cannot be applied to, and a key with no service account writes rows
// attributable to nobody.
func TestMintNeedsATenantAndAServiceAccount(t *testing.T) {
	t.Parallel()

	f := setup(t)

	for name, in := range map[string]apikey.MintInput{
		"no tenant":  {AccountID: f.service, Name: "ci"},
		"no account": {TenantID: f.tenant, Name: "ci"},
		"neither":    {Name: "ci"},
	} {
		if _, err := f.m.Mint(context.Background(), in); err == nil {
			t.Errorf("%s: should have been refused", name)
		}
	}
}

// A default TTL is how a deployment says "no key lives forever" once, rather
// than trusting whoever mints one to remember.
func TestADefaultTTLExpiresAKeyNobodyGaveAnExpiryTo(t *testing.T) {
	t.Parallel()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, err := apikey.New(apikey.Config{
		Store: apikey.NewMemoryStore(), Now: c.now, DefaultTTL: 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	tenant, service := uuid.New(), uuid.New()
	minted, err := m.Mint(context.Background(), apikey.MintInput{
		TenantID: tenant, AccountID: service, Name: "ci",
	})
	if err != nil {
		t.Fatal(err)
	}
	if minted.Key.ExpiresAt == nil || !minted.Key.ExpiresAt.Equal(c.at.Add(24*time.Hour)) {
		t.Errorf("ExpiresAt = %v, want the default TTL applied", minted.Key.ExpiresAt)
	}

	// An expiry the caller asked for wins: the default is a floor for people
	// who did not think about it, not a ceiling on people who did.
	asked := c.at.Add(time.Hour)
	explicit, err := m.Mint(context.Background(), apikey.MintInput{
		TenantID: tenant, AccountID: service, Name: "short", ExpiresAt: &asked,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !explicit.Key.ExpiresAt.Equal(asked) {
		t.Errorf("ExpiresAt = %v, want %v", explicit.Key.ExpiresAt, asked)
	}
}

// The overlap is what makes rotation a two-step an operator can complete, and
// extending a key that was already about to expire would undo an expiry
// somebody chose on purpose.
func TestRotationNeverExtendsAKeyPastItsOwnExpiry(t *testing.T) {
	t.Parallel()

	f := setup(t)

	soon := f.clock.at.Add(time.Hour)
	minted := f.mint(t, apikey.MintInput{Name: "ci", ExpiresAt: &soon})

	if _, err := f.m.Rotate(context.Background(), f.tenant, minted.Key.ID, 30*24*time.Hour); err != nil {
		t.Fatal(err)
	}

	found, err := f.store.Find(context.Background(), f.tenant, minted.Key.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found.ExpiresAt.Equal(soon) {
		t.Errorf("ExpiresAt = %v, want the expiry it already had", found.ExpiresAt)
	}
}

// Rotating something that is not there is a 404, not a new key with the old
// one's name — the caller has the wrong identifier and needs to know.
func TestRotatingAKeyThatIsNotThere(t *testing.T) {
	t.Parallel()

	f := setup(t)
	minted := f.mint(t, apikey.MintInput{})

	if _, err := f.m.Rotate(context.Background(), f.tenant, uuid.New(), time.Hour); rigerr.CodeOf(err) != rigerr.CodeNotFound {
		t.Errorf("code = %q, want NotFound", rigerr.CodeOf(err))
	}
	// And another tenant's key is not there either, whatever the identifier.
	if _, err := f.m.Rotate(context.Background(), uuid.New(), minted.Key.ID, time.Hour); rigerr.CodeOf(err) != rigerr.CodeNotFound {
		t.Errorf("cross-tenant: code = %q, want NotFound", rigerr.CodeOf(err))
	}
}

// The rotated key inherits everything that made the old one useful. A
// replacement with no scopes is a replacement that cannot be deployed.
func TestARotatedKeyInheritsWhatTheOldOneCouldDo(t *testing.T) {
	t.Parallel()

	f := setup(t)

	only := netip.MustParsePrefix("203.0.113.0/24")
	admin := uuid.New()
	minted := f.mint(t, apikey.MintInput{
		Name: "nightly export", Scopes: []string{"lesson.read"},
		CIDRAllowList: []netip.Prefix{only}, CreatedByAccountID: &admin,
	})

	rotated, err := f.m.Rotate(context.Background(), f.tenant, minted.Key.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	switch {
	case rotated.Key.Name != minted.Key.Name:
		t.Errorf("name = %q", rotated.Key.Name)
	case len(rotated.Key.Scopes) != 1 || rotated.Key.Scopes[0] != "lesson.read":
		t.Errorf("scopes = %v", rotated.Key.Scopes)
	case len(rotated.Key.CIDRAllowList) != 1 || rotated.Key.CIDRAllowList[0] != only:
		t.Errorf("allow list = %v", rotated.Key.CIDRAllowList)
	case rotated.Key.AccountID != minted.Key.AccountID:
		t.Errorf("service account = %s", rotated.Key.AccountID)
	}
	// A new secret, or it is not a rotation.
	if rotated.Secret == minted.Secret {
		t.Error("the replacement should be a different value")
	}

	// Both work during the overlap, which is the whole point.
	for name, secret := range map[string]string{"old": minted.Secret, "new": rotated.Secret} {
		if _, _, err := f.m.Verify(context.Background(), secret, netip.MustParseAddr("203.0.113.10")); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	f.clock.advance(time.Hour + time.Minute)
	if _, _, err := f.m.Verify(context.Background(), minted.Secret, netip.MustParseAddr("203.0.113.10")); err == nil {
		t.Error("past the overlap the old key should be dead")
	}
}

// Every mutation is scoped to the tenant, or an identifier leaked from one
// customer's logs is a lever on another's.
func TestRevokeIsScopedToTheTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	minted := f.mint(t, apikey.MintInput{})

	if err := f.m.Revoke(context.Background(), uuid.New(), minted.Key.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Verify(context.Background(), minted.Secret, anywhere); err != nil {
		t.Errorf("another tenant's revoke should not have touched it: %v", err)
	}

	if err := f.m.Revoke(context.Background(), f.tenant, minted.Key.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.m.Verify(context.Background(), minted.Secret, anywhere); err == nil {
		t.Error("its own tenant's revoke should have")
	}
}

// A key identifier is the lookup key, and the pieces around it are what tell a
// malformed value from a wrong one. Every failure has to look the same.
func TestEveryShapeOfMalformedKeyLooksTheSame(t *testing.T) {
	t.Parallel()

	f := setup(t)
	minted := f.mint(t, apikey.MintInput{})
	keyID := minted.Key.KeyID

	for name, presented := range map[string]string{
		"empty":             "",
		"no prefix":         keyID + "_AAAA",
		"no separator":      apikey.Prefix + keyID,
		"no identifier":     apikey.Prefix + "_AAAA",
		"secret not base32": apikey.Prefix + keyID + "_!!!!",
		"secret too short":  apikey.Prefix + keyID + "_AAAA",
		"a session token":   "rig_at_AAAA.AAAA",
	} {
		_, _, err := f.m.Verify(context.Background(), presented, anywhere)
		if err == nil {
			t.Errorf("%s: should not have verified", name)
			continue
		}
		if !strings.Contains(err.Error(), "not valid") {
			t.Errorf("%s: err = %v, want the one message every failure gives", name, err)
		}
	}
}

// limitedKeys is setup with a failure limit, and the counter behind it fed from
// the log the manager writes.
func limitedKeys(t *testing.T, maxN int) *fixture {
	t.Helper()

	f := setup(t)
	counter := throttle.NewMemory()
	limit := throttle.Limit{
		Name:      "apikey.failed",
		Event:     throttle.EventAPIKeyAuthFailed,
		ClearedBy: throttle.EventAPIKeyAuthSucceeded,
		Max:       maxN,
		Window:    time.Minute,
	}

	m, err := apikey.New(apikey.Config{
		Store: f.store, Log: f.log, Now: f.clock.now,
		Limiter:      throttle.New(counter).WithClock(f.clock.now),
		FailureLimit: limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.m = m

	f.log.onWrite = func(e authlog.Entry) {
		if e.APIKeyRef == "" {
			return
		}
		switch e.Event {
		case throttle.EventAPIKeyAuthFailed, throttle.EventAPIKeyAuthSucceeded:
			counter.Record(e.Event, throttle.APIKey(e.APIKeyRef), e.At)
		}
	}
	return f
}

// A key id is the public half — it turns up in logs and in configuration — so
// without this the only thing between one and its secret is how fast somebody
// can ask.
func TestGrindingSecretsAgainstOneKeyIDIsLimited(t *testing.T) {
	t.Parallel()

	f := limitedKeys(t, 3)
	ctx := context.Background()
	minted := f.mint(t, apikey.MintInput{})
	wrong := wrongSecret(minted)

	for i := range 3 {
		if _, _, err := f.m.Verify(ctx, wrong, anywhere); !rigerr.Is(err, rigerr.CodeUnauthorized) {
			t.Fatalf("attempt %d answered %v, want 401", i+1, err)
		}
	}

	_, _, err := f.m.Verify(ctx, wrong, anywhere)
	if rigerr.CodeOf(err) != rigerr.CodeRateLimited {
		t.Fatalf("the fourth wrong secret was refused with %v, want a rate limit", rigerr.CodeOf(err))
	}

	// And the real key is refused too, which is the point of locking the id
	// rather than the attempt: the limit has to bite whoever is holding it.
	if _, _, err := f.m.Verify(ctx, minted.Secret, anywhere); rigerr.CodeOf(err) != rigerr.CodeRateLimited {
		t.Fatalf("the correct secret answered %v while the id was locked", rigerr.CodeOf(err))
	}
}

// Cleared by a success, so an integration misconfigured for a minute is not
// locked out for the rest of the window once somebody fixes it.
func TestASuccessClearsTheKeyFailureWindow(t *testing.T) {
	t.Parallel()

	f := limitedKeys(t, 3)
	ctx := context.Background()
	minted := f.mint(t, apikey.MintInput{})
	wrong := wrongSecret(minted)

	for range 2 {
		_, _, _ = f.m.Verify(ctx, wrong, anywhere)
	}

	f.clock.advance(time.Second)
	if _, _, err := f.m.Verify(ctx, minted.Secret, anywhere); err != nil {
		t.Fatalf("the correct secret was refused before the limit: %v", err)
	}

	// The earlier failures are wiped, so there is a full budget again.
	f.clock.advance(time.Second)
	for i := range 3 {
		if _, _, err := f.m.Verify(ctx, wrong, anywhere); rigerr.CodeOf(err) == rigerr.CodeRateLimited {
			t.Fatalf("attempt %d after a success hit the limit; the window did not clear", i+1)
		}
	}
}

func TestWithNoLimiterKeyFailuresAreUnbounded(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()
	minted := f.mint(t, apikey.MintInput{})
	wrong := wrongSecret(minted)

	for i := range 30 {
		if _, _, err := f.m.Verify(ctx, wrong, anywhere); rigerr.CodeOf(err) == rigerr.CodeRateLimited {
			t.Fatalf("attempt %d was rate limited by a manager with no limiter", i+1)
		}
	}
}

// wrongSecret keeps the key id and replaces the secret, which is the shape of
// the attack this limit exists for: the id is the half that leaks.
func wrongSecret(minted apikey.Minted) string {
	return apikey.Prefix + minted.Key.KeyID + "_" + strings.Repeat("A", 52)
}

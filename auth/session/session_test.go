package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
)

type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

// recorder collects entries so a test can assert on what was written. The log
// is the audit trail and the rate-limit substrate, so an event that never lands
// is two failures, not one.
type recorder struct{ entries []authlog.Entry }

func (r *recorder) Write(_ context.Context, e authlog.Entry) { r.entries = append(r.entries, e) }

func (r *recorder) count(event string) int {
	n := 0
	for _, e := range r.entries {
		if e.Event == event {
			n++
		}
	}
	return n
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
	m     *session.Manager
	store *session.MemoryStore
	log   *recorder
	clock *clock

	tenant  uuid.UUID
	account uuid.UUID
}

func setup(t *testing.T) *fixture {
	t.Helper()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	store := session.NewMemoryStore()
	log := &recorder{}

	m, err := session.New(session.Config{Store: store, Log: log, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{m: m, store: store, log: log, clock: c, tenant: uuid.New(), account: uuid.New()}
}

func (f *fixture) issue(t *testing.T) session.Pair {
	t.Helper()

	pair, err := f.m.Issue(context.Background(), session.IssueInput{
		TenantID:  f.tenant,
		AccountID: f.account,
		Client:    session.ClientWeb,
		IPAddress: "203.0.113.10",
		UserAgent: "Mozilla/5.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func TestIssueReturnsAUsablePair(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair := f.issue(t)

	// A leaked token should be identifiable on sight by whoever finds it.
	if !strings.HasPrefix(pair.Access.Token, session.PrefixAccess) {
		t.Errorf("access token = %q", pair.Access.Token)
	}
	if !strings.HasPrefix(pair.Refresh.Token, session.PrefixRefresh) {
		t.Errorf("refresh token = %q", pair.Refresh.Token)
	}

	tok, err := f.m.Verify(context.Background(), pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != f.account || tok.TenantID != f.tenant {
		t.Error("the verified token describes somebody else")
	}

	// The first refresh token is the session's identity.
	if pair.RootTokenID != pair.Refresh.TokenID {
		t.Error("the first refresh token should be its own family root")
	}
}

// The client is an enum column, so leaving it empty would reach Postgres as an
// invalid value rather than as a default — an error about input syntax, for a
// caller who simply did not care.
func TestAnUnstatedClientIsAWeb(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair, err := f.m.Issue(context.Background(), session.IssueInput{
		TenantID: f.tenant, AccountID: f.account,
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := f.m.Verify(context.Background(), pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.Client != session.ClientWeb {
		t.Errorf("client = %q, want %q", tok.Client, session.ClientWeb)
	}
}

func TestTheTwoKindsAreNotInterchangeable(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair := f.issue(t)

	// A refresh token is not a credential for the API. It lives in a keychain
	// and is worth far more than the access token it mints.
	if _, err := f.m.Verify(context.Background(), pair.Refresh.Token); err == nil {
		t.Error("a refresh token must not authenticate a request")
	}
	if _, err := f.m.Rotate(context.Background(), pair.Access.Token); err == nil {
		t.Error("an access token must not be exchangeable for a session")
	}
}

func TestGarbageIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t)
	real := f.issue(t)

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"no prefix", "hello"},
		{"no separator", session.PrefixAccess + "AAAA"},
		{"unknown id", session.PrefixAccess + "AAAAAAAAAAAAAAAAAAAAAAAAAA.AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"},
		{"right id wrong secret", swapSecret(real.Access.Token)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.m.Verify(context.Background(), tc.token); err == nil {
				t.Error("this should not have verified")
			} else if !rigerr.Is(err, rigerr.CodeUnauthorized) {
				t.Errorf("err = %v, want 401", err)
			}
		})
	}
}

// Knowing a token's identifier is not knowing the token. The identifier travels
// in the same string, so a store lookup that trusts it is a store lookup an
// attacker can steer.
func swapSecret(token string) string {
	id, _, _ := strings.Cut(token, ".")
	return id + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
}

func TestRotationConsumesTheOldPair(t *testing.T) {
	t.Parallel()

	f := setup(t)
	first := f.issue(t)

	f.clock.advance(time.Minute)
	second, err := f.m.Rotate(context.Background(), first.Refresh.Token)
	if err != nil {
		t.Fatal(err)
	}

	if second.Refresh.Token == first.Refresh.Token {
		t.Error("rotation must mint a new refresh token, not return the old one")
	}
	if second.RootTokenID != first.RootTokenID {
		t.Error("a rotation stays inside the same session")
	}

	// The new access token works.
	if _, err := f.m.Verify(context.Background(), second.Access.Token); err != nil {
		t.Errorf("the new access token should verify: %v", err)
	}
	// The old one is still inside its ten minutes, and deliberately still
	// valid: revoking it on rotation would break every request already in
	// flight when the refresh happened.
	if _, err := f.m.Verify(context.Background(), first.Access.Token); err != nil {
		t.Errorf("the previous access token should live out its lifetime: %v", err)
	}

	if f.log.count(authlog.EventTokenRefreshed) != 1 {
		t.Error("a refresh should be recorded")
	}
	// Issuing writes nothing: a login, an OAuth callback, and an impersonation
	// all reach Issue, and only the caller knows which one happened.
	if f.log.count(authlog.EventLoginSucceeded) != 0 {
		t.Error("Issue should not decide what kind of sign-in this was")
	}
}

// A dropped response is not an attack. Revoking a family because a client
// retried would make every flaky connection a logout.
func TestAReplayInsideTheLeewayIsARetry(t *testing.T) {
	t.Parallel()

	f := setup(t)
	first := f.issue(t)

	if _, err := f.m.Rotate(context.Background(), first.Refresh.Token); err != nil {
		t.Fatal(err)
	}

	f.clock.advance(5 * time.Second)
	retry, err := f.m.Rotate(context.Background(), first.Refresh.Token)
	if err != nil {
		t.Fatalf("a retry inside the leeway should succeed: %v", err)
	}

	// Both tabs end up holding a working pair in the same session. Neither is
	// signed out, which is the outcome that matters.
	if _, err := f.m.Verify(context.Background(), retry.Access.Token); err != nil {
		t.Errorf("the retry's access token should work: %v", err)
	}
	if retry.RootTokenID != first.RootTokenID {
		t.Error("the retry should stay inside the same session")
	}
	if f.log.count(authlog.EventTokenReuseDetected) != 0 {
		t.Error("a retry is not a reuse")
	}
}

// The leeway must not slide. An attacker replaying every twenty seconds would
// otherwise hold a thirty-second window open forever.
func TestTheLeewayIsMeasuredFromTheFirstUse(t *testing.T) {
	t.Parallel()

	f := setup(t)
	first := f.issue(t)

	if _, err := f.m.Rotate(context.Background(), first.Refresh.Token); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(20 * time.Second)
	if _, err := f.m.Rotate(context.Background(), first.Refresh.Token); err != nil {
		t.Fatalf("still inside the leeway: %v", err)
	}

	// Twenty more seconds: forty from the first use, so outside a
	// thirty-second leeway even though the last replay was twenty ago.
	f.clock.advance(20 * time.Second)
	if _, err := f.m.Rotate(context.Background(), first.Refresh.Token); err == nil {
		t.Error("the leeway should be measured from the first use, not the last")
	}
}

// The signal that matters. Somebody is using a token that was consumed a while
// ago, and there is no way to tell from here whether it is the thief or the
// victim — so the session ends for both.
func TestAReplayOutsideTheLeewayRevokesTheFamily(t *testing.T) {
	t.Parallel()

	f := setup(t)
	first := f.issue(t)

	second, err := f.m.Rotate(context.Background(), first.Refresh.Token)
	if err != nil {
		t.Fatal(err)
	}

	f.clock.advance(time.Hour)
	if _, err := f.m.Rotate(context.Background(), first.Refresh.Token); err == nil {
		t.Fatal("replaying a long-consumed token should be refused")
	}

	// Everything in the family, including the pair the legitimate client is
	// holding right now.
	if _, err := f.m.Verify(context.Background(), second.Access.Token); err == nil {
		t.Error("the live access token should have been revoked with the family")
	}
	if _, err := f.m.Rotate(context.Background(), second.Refresh.Token); err == nil {
		t.Error("the live refresh token should have been revoked with the family")
	}

	// "A token was replayed" is not actionable. Where it came from is.
	e, ok := f.log.last(authlog.EventTokenReuseDetected)
	if !ok {
		t.Fatal("reuse should be recorded")
	}
	if e.Outcome != authlog.Failed {
		t.Errorf("outcome = %q", e.Outcome)
	}
	if e.TokenRootID == nil || *e.TokenRootID != first.RootTokenID {
		t.Error("the entry should name the family that was revoked")
	}
	if e.Detail["original_ip"] != "203.0.113.10" {
		t.Errorf("the entry should carry where the token was issued: %v", e.Detail)
	}
}

func TestAnExpiredAccessTokenStopsWorking(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair := f.issue(t)

	f.clock.advance(session.DefaultAccessTTL + time.Second)
	if _, err := f.m.Verify(context.Background(), pair.Access.Token); err == nil {
		t.Error("an expired access token should be refused")
	}
	// The refresh token outlives it, which is the whole arrangement.
	if _, err := f.m.Rotate(context.Background(), pair.Refresh.Token); err != nil {
		t.Errorf("the refresh token should still work: %v", err)
	}
}

// Refreshing must not extend the session. Otherwise a tab left open makes a
// twelve-hour session immortal.
func TestRotationDoesNotExtendTheSession(t *testing.T) {
	t.Parallel()

	f := setup(t)
	first := f.issue(t)

	f.clock.advance(11 * time.Hour)
	second, err := f.m.Rotate(context.Background(), first.Refresh.Token)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Refresh.ExpiresAt.Equal(first.Refresh.ExpiresAt) {
		t.Errorf("expiry moved from %s to %s", first.Refresh.ExpiresAt, second.Refresh.ExpiresAt)
	}

	f.clock.advance(2 * time.Hour)
	if _, err := f.m.Rotate(context.Background(), second.Refresh.Token); err == nil {
		t.Error("the session should have ended at twelve hours regardless of refreshing")
	}
}

func TestRememberMeLastsLonger(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair, err := f.m.Issue(context.Background(), session.IssueInput{
		TenantID: f.tenant, AccountID: f.account, Client: session.ClientWeb, Remember: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	f.clock.advance(20 * 24 * time.Hour)
	if _, err := f.m.Rotate(context.Background(), pair.Refresh.Token); err != nil {
		t.Errorf("a remembered session should survive twenty days: %v", err)
	}
}

func TestRevokeEndsOneSession(t *testing.T) {
	t.Parallel()

	f := setup(t)
	phone := f.issue(t)
	f.clock.advance(time.Second)
	laptop := f.issue(t)

	if err := f.m.Revoke(context.Background(), phone.RootTokenID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.m.Verify(context.Background(), phone.Access.Token); err == nil {
		t.Error("the revoked session should be dead")
	}
	if _, err := f.m.Verify(context.Background(), laptop.Access.Token); err != nil {
		t.Errorf("the other session should be untouched: %v", err)
	}
}

// Changing a password while whoever stole it stays signed in elsewhere is not
// a recovery.
func TestRevokeAllEndsEverySession(t *testing.T) {
	t.Parallel()

	f := setup(t)
	phone := f.issue(t)
	f.clock.advance(time.Second)
	laptop := f.issue(t)

	if err := f.m.RevokeAll(context.Background(), f.tenant, f.account); err != nil {
		t.Fatal(err)
	}

	for name, pair := range map[string]session.Pair{"phone": phone, "laptop": laptop} {
		if _, err := f.m.Verify(context.Background(), pair.Access.Token); err == nil {
			t.Errorf("the %s session survived", name)
		}
	}
}

func TestListReportsTheSessions(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.issue(t)
	f.clock.advance(time.Minute)
	newest := f.issue(t)

	families, err := f.m.List(context.Background(), f.tenant, f.account)
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 2 {
		t.Fatalf("got %d sessions, want 2", len(families))
	}
	if families[0].Root.ID != newest.RootTokenID {
		t.Error("sessions should be newest first")
	}
	if families[0].Root.IPAddress != "203.0.113.10" {
		t.Error("a session should say where it was opened from")
	}

	// Last used is derived from the newest token in the family rather than
	// written on every request.
	f.clock.advance(time.Minute)
	if _, err := f.m.Rotate(context.Background(), newest.Refresh.Token); err != nil {
		t.Fatal(err)
	}
	families, _ = f.m.List(context.Background(), f.tenant, f.account)
	if !families[0].LastUsedAt.Equal(f.clock.at) {
		t.Errorf("last used = %s, want the rotation just made", families[0].LastUsedAt)
	}
}

// A session that began as an administrator acting as somebody else must not be
// able to shed that fact by refreshing.
func TestImpersonationSurvivesRotation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	admin := uuid.New()

	pair, err := f.m.Issue(context.Background(), session.IssueInput{
		TenantID:                f.tenant,
		AccountID:               f.account,
		Client:                  session.ClientWeb,
		ImpersonatedByAccountID: &admin,
	})
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		f.clock.advance(time.Minute)
		pair, err = f.m.Rotate(context.Background(), pair.Refresh.Token)
		if err != nil {
			t.Fatal(err)
		}
	}

	tok, err := f.m.Verify(context.Background(), pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.ImpersonatedByAccountID == nil || *tok.ImpersonatedByAccountID != admin {
		t.Error("three rotations later, the session forgot it was an impersonation")
	}
}

// Revocation is immediate because verification is a row read. That is the whole
// reason these are not JWTs.
func TestRevocationTakesEffectImmediately(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair := f.issue(t)

	if _, err := f.m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Fatal(err)
	}
	if err := f.m.Revoke(context.Background(), pair.RootTokenID); err != nil {
		t.Fatal(err)
	}
	// No clock advance at all: the very next request is refused.
	if _, err := f.m.Verify(context.Background(), pair.Access.Token); err == nil {
		t.Error("a revoked token should stop working on the next request")
	}
}

func TestAStoreIsRequired(t *testing.T) {
	t.Parallel()

	if _, err := session.New(session.Config{}); err == nil {
		t.Error("a manager with nowhere to put tokens should refuse to exist")
	}
}

// Verification is a row read, which is what makes revocation immediate. The
// cache trades exactly that, and the trade has to be the one the doc comment
// describes rather than a longer one.
func TestTheCacheTradesImmediateRevocationForThroughput(t *testing.T) {
	t.Parallel()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	store := session.NewMemoryStore()
	m, err := session.New(session.Config{
		Store: store, Now: c.now, CacheTTL: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	tenant, acct := uuid.New(), uuid.New()
	pair, err := m.Issue(context.Background(), session.IssueInput{
		TenantID: tenant, AccountID: acct, Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Read once to fill the cache.
	if _, err := m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Fatal(err)
	}
	if err := m.Revoke(context.Background(), pair.RootTokenID); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Errorf("inside the window the cached row is still answered: %v", err)
	}

	c.advance(6 * time.Second)
	if _, err := m.Verify(context.Background(), pair.Access.Token); err == nil {
		t.Error("past the window the revocation should take effect")
	}

	// A wrong secret is never answered from the cache: the identifier is the
	// key, and the secret is what proves the caller has the token.
	id, _, _ := strings.Cut(strings.TrimPrefix(pair.Access.Token, session.PrefixAccess), ".")
	forged := session.PrefixAccess + id + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := m.Verify(context.Background(), forged); err == nil {
		t.Error("knowing an identifier is not knowing the token")
	}
}

// Never past the token's own expiry: a short access lifetime is only short if
// it is enforced, and a long cache would quietly extend every one of them.
func TestTheCacheNeverOutlivesTheToken(t *testing.T) {
	t.Parallel()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, err := session.New(session.Config{
		Store: session.NewMemoryStore(), Now: c.now,
		AccessTTL: time.Minute, CacheTTL: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}

	pair, err := m.Issue(context.Background(), session.IssueInput{
		TenantID: uuid.New(), AccountID: uuid.New(), Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Fatal(err)
	}

	c.advance(2 * time.Minute)
	if _, err := m.Verify(context.Background(), pair.Access.Token); err == nil {
		t.Error("an expired token should not be answered from a longer-lived cache")
	}
}

// The identifier half of a token is not a secret — it travels in every log
// line the token appears in — so a real identifier with an invented secret is
// the attack the hash comparison exists to stop.
func TestARealIdentifierWithAWrongSecretGetsNowhere(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair := f.issue(t)

	forge := func(token, prefix string) string {
		id, _, _ := strings.Cut(strings.TrimPrefix(token, prefix), ".")
		return prefix + id + ".AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	}

	if _, err := f.m.Verify(context.Background(), forge(pair.Access.Token, session.PrefixAccess)); err == nil {
		t.Error("Verify should refuse a secret that does not hash to the stored one")
	}
	if _, err := f.m.Rotate(context.Background(), forge(pair.Refresh.Token, session.PrefixRefresh)); err == nil {
		t.Error("Rotate should too")
	}

	// And the family survives it. A wrong secret is a guess, not a replay of a
	// consumed token, and revoking on a guess would hand anybody a way to sign
	// somebody else out by naming their token identifier.
	if _, err := f.m.Verify(context.Background(), pair.Access.Token); err != nil {
		t.Errorf("the real token should still work: %v", err)
	}
	if f.log.count(authlog.EventTokenReuseDetected) != 0 {
		t.Error("a wrong guess is not a reuse")
	}
}

// The defaults are the numbers the documentation quotes, and nothing else in
// the package restates them.
func TestTheDefaultLifetimes(t *testing.T) {
	t.Parallel()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	m, err := session.New(session.Config{Store: session.NewMemoryStore(), Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	ordinary, err := m.Issue(context.Background(), session.IssueInput{
		TenantID: uuid.New(), AccountID: uuid.New(), Client: session.ClientWeb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := ordinary.Access.ExpiresAt.Sub(c.at); got != session.DefaultAccessTTL {
		t.Errorf("access = %s, want %s", got, session.DefaultAccessTTL)
	}
	if got := ordinary.Refresh.ExpiresAt.Sub(c.at); got != session.DefaultRefreshTTL {
		t.Errorf("refresh = %s, want %s", got, session.DefaultRefreshTTL)
	}

	remembered, err := m.Issue(context.Background(), session.IssueInput{
		TenantID: uuid.New(), AccountID: uuid.New(), Client: session.ClientWeb, Remember: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := remembered.Refresh.ExpiresAt.Sub(c.at); got != session.DefaultRememberTTL {
		t.Errorf("remembered = %s, want %s", got, session.DefaultRememberTTL)
	}
}

// Revoking a session that is not there is what a client retrying a logout
// does, and answering it with an error would turn a duplicate click into an
// error page.
func TestRevokingNothingIsNotAFailure(t *testing.T) {
	t.Parallel()

	f := setup(t)

	if err := f.m.Revoke(context.Background(), uuid.New()); err != nil {
		t.Errorf("Revoke: %v", err)
	}
	if err := f.m.RevokeAll(context.Background(), f.tenant, uuid.New()); err != nil {
		t.Errorf("RevokeAll: %v", err)
	}
}

// Sessions are listed per tenant as well as per account, because an identifier
// that resolves across tenants is one worth probing.
func TestListIsScopedToTheTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.issue(t)

	elsewhere, err := f.m.List(context.Background(), uuid.New(), f.account)
	if err != nil {
		t.Fatal(err)
	}
	if len(elsewhere) != 0 {
		t.Errorf("got %d sessions from another tenant, want none", len(elsewhere))
	}

	mine, err := f.m.List(context.Background(), f.tenant, f.account)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 {
		t.Errorf("got %d sessions, want 1", len(mine))
	}
}

// A payload is the application's own context for one session. These four tests
// are the whole contract: it is stored, it survives rotation, a hook can replace
// it, and a hook that fails fails the refresh rather than quietly emptying it.

func TestAPayloadIsStoredOnTheSession(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair, err := f.m.Issue(context.Background(), session.IssueInput{
		TenantID: f.tenant, AccountID: f.account,
		Payload: json.RawMessage(`{"device":"laptop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := f.m.Verify(context.Background(), pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if string(tok.Payload) != `{"device":"laptop"}` {
		t.Errorf("payload = %s", tok.Payload)
	}
}

// The important one. A fact recorded at sign-in has to still be there after the
// access token expires, or an application would have to re-derive it every ten
// minutes and the payload would be useless.
func TestAPayloadSurvivesRotation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair, err := f.m.Issue(context.Background(), session.IssueInput{
		TenantID: f.tenant, AccountID: f.account,
		Payload: json.RawMessage(`{"device":"laptop"}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Twice, because carrying it one generation and losing it on the next is the
	// bug a single rotation would not catch.
	for i := range 2 {
		f.clock.advance(time.Minute)
		pair, err = f.m.Rotate(context.Background(), pair.Refresh.Token)
		if err != nil {
			t.Fatalf("rotation %d: %v", i+1, err)
		}

		tok, err := f.m.Verify(context.Background(), pair.Access.Token)
		if err != nil {
			t.Fatal(err)
		}
		if string(tok.Payload) != `{"device":"laptop"}` {
			t.Errorf("after rotation %d the payload is %s", i+1, tok.Payload)
		}
	}
}

func TestOnRotateReplacesThePayload(t *testing.T) {
	t.Parallel()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	store := session.NewMemoryStore()

	var saw json.RawMessage
	m, err := session.New(session.Config{
		Store: store, Now: c.now,
		OnRotate: func(_ context.Context, prev *session.Token) (json.RawMessage, error) {
			// The hook is handed the token being exchanged, so the payload it is
			// replacing is available to build the next one from.
			saw = prev.Payload
			return json.RawMessage(`{"refreshed":true}`), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tenant, account := uuid.New(), uuid.New()
	pair, err := m.Issue(context.Background(), session.IssueInput{
		TenantID: tenant, AccountID: account,
		Payload: json.RawMessage(`{"refreshed":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	c.advance(time.Minute)
	pair, err = m.Rotate(context.Background(), pair.Refresh.Token)
	if err != nil {
		t.Fatal(err)
	}

	if string(saw) != `{"refreshed":false}` {
		t.Errorf("the hook saw %s, want the previous payload", saw)
	}

	// Both halves of the pair, because a client that reads one and sends the
	// other would otherwise see the session change under it.
	for what, token := range map[string]string{
		"access":  pair.Access.Token,
		"refresh": pair.Refresh.Token,
	} {
		tok, err := m.Verify(context.Background(), token)
		if err != nil {
			// A refresh token is not verifiable as an access token; read it from
			// the store instead.
			stored, findErr := store.Find(context.Background(), pair.Refresh.TokenID)
			if findErr != nil {
				t.Fatal(findErr)
			}
			tok = stored
		}
		if string(tok.Payload) != `{"refreshed":true}` {
			t.Errorf("the %s token carries %s", what, tok.Payload)
		}
	}
}

// A hook that cannot produce a payload fails the refresh. Serving the previous
// one would mean answering with context the application explicitly declined to
// vouch for, and an empty one would silently drop whatever depended on it.
func TestOnRotateFailingFailsTheRefresh(t *testing.T) {
	t.Parallel()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	store := session.NewMemoryStore()
	m, err := session.New(session.Config{
		Store: store, Now: c.now,
		OnRotate: func(context.Context, *session.Token) (json.RawMessage, error) {
			return nil, errors.New("the directory is down")
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	pair, err := m.Issue(context.Background(), session.IssueInput{
		TenantID: uuid.New(), AccountID: uuid.New(),
	})
	if err != nil {
		t.Fatal(err)
	}

	c.advance(time.Minute)
	if _, err := m.Rotate(context.Background(), pair.Refresh.Token); err == nil {
		t.Fatal("a failing OnRotate should fail the refresh")
	}

	// And the transaction rolled back, so the token it was given is still usable
	// once the hook recovers — a refresh that half-happened would log somebody out
	// for an outage somewhere else.
	prev, err := store.Find(context.Background(), pair.Refresh.TokenID)
	if err != nil {
		t.Fatal(err)
	}
	if prev.RotatedAt != nil {
		t.Error("the refresh token was consumed by a rotation that failed")
	}
}

package account_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/password"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

const goodPassword = "correct horse battery staple"

type clock struct{ at time.Time }

func (c *clock) now() time.Time          { return c.at }
func (c *clock) advance(d time.Duration) { c.at = c.at.Add(d) }

// recorder is both the audit trail and the rate-limit substrate, which is
// exactly what it is in production: the limiter counts the rows the log writes.
type recorder struct {
	entries []authlog.Entry
	counter *throttle.Memory
}

func (r *recorder) Write(_ context.Context, e authlog.Entry) {
	r.entries = append(r.entries, e)

	// Mirror the entry into whatever the limits count it against, the way the
	// Postgres counter's WHERE clause does.
	if e.EmailAddress != "" {
		r.counter.Record(e.Event, throttle.Email(e.EmailAddress), e.At)
	}
	if e.IPAddress != "" {
		r.counter.Record(e.Event, throttle.IP(e.IPAddress), e.At)
	}
	if e.AccountID != nil {
		r.counter.Record(e.Event, throttle.Account(e.AccountID.String()), e.At)
	}
}

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

type notifier struct {
	reset    string
	verify   string
	invite   string
	resetTo  *account.Identity
	verifyTo *account.Identity
	inviteTo *account.Account
}

func (n *notifier) SendPasswordReset(_ context.Context, i *account.Identity, token string) error {
	n.reset, n.resetTo = token, i
	return nil
}

func (n *notifier) SendEmailVerification(_ context.Context, i *account.Identity, token string) error {
	n.verify, n.verifyTo = token, i
	return nil
}

func (n *notifier) SendInvitation(_ context.Context, _ *account.Identity, a *account.Account, token string) error {
	n.invite, n.inviteTo = token, a
	return nil
}

type fixture struct {
	svc        *account.Service
	store      *account.MemoryStore
	sessions   *session.Manager
	identities *session.IdentityManager
	log        *recorder
	notify     *notifier
	clock      *clock

	tenant uuid.UUID
	ident  *account.Identity
	acct   *account.Account
}

func setup(t *testing.T) *fixture {
	t.Helper()

	c := &clock{at: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	log := &recorder{counter: throttle.NewMemory()}
	store := account.NewMemoryStore()

	// One store for both credentials, the way the Postgres one is.
	tokens := session.NewMemoryStore()
	sessions, err := session.New(session.Config{Store: tokens, Log: log, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := session.NewIdentity(session.IdentityConfig{Store: tokens, Now: c.now})
	if err != nil {
		t.Fatal(err)
	}

	notify := &notifier{}
	// Cheap argon2 parameters and no padding: the suite is about the rules,
	// not about how long they take.
	svc, err := account.New(account.Config{
		Store:      store,
		Sessions:   sessions,
		Identities: identities,
		Hasher:     password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}),
		Log:        log,
		Notifier:   notify,
		Limiter:    throttle.New(log.counter).WithClock(c.now),
		Now:        c.now,
		Sleep:      func(context.Context, time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{
		svc: svc, store: store, sessions: sessions, identities: identities,
		log: log, notify: notify, clock: c, tenant: uuid.New(),
	}
	f.ident = &account.Identity{
		ID:           uuid.New(),
		EmailAddress: "Sam@Example.com",
		DisplayName:  "Sam",
		IsActive:     true,
	}
	f.acct = &account.Account{
		ID:          uuid.New(),
		TenantID:    f.tenant,
		DisplayName: "Sam",
		IsActive:    true,
	}
	store.PutPerson(f.ident, f.acct)

	if err := svc.SetPassword(context.Background(), f.ident.ID, goodPassword); err != nil {
		t.Fatal(err)
	}
	return f
}

// login signs the fixture's person in and hands back the tenant session.
//
// The pair rather than the whole result, because that is what almost every test
// here is about. The tests that care about the tenant-less half call signIn.
func (f *fixture) login(password string) (session.Pair, error) {
	res, err := f.signIn(password)
	if err != nil {
		return session.Pair{}, err
	}
	if res.Session == nil {
		return session.Pair{}, errors.New("signed in with no tenant")
	}
	return *res.Session, nil
}

func (f *fixture) signIn(password string) (account.SignInResult, error) {
	return f.svc.Login(context.Background(), account.LoginInput{
		TenantID:     f.tenant,
		EmailAddress: "sam@example.com",
		Password:     password,
		Client:       session.ClientWeb,
		IPAddress:    "203.0.113.10",
		UserAgent:    "Mozilla/5.0",
	})
}

func TestLogin(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair, err := f.login(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	tok, err := f.sessions.Verify(context.Background(), pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != f.acct.ID {
		t.Error("the session belongs to somebody else")
	}

	e, ok := f.log.last(authlog.EventLoginSucceeded)
	if !ok {
		t.Fatal("a successful login should be recorded")
	}
	if e.TokenRootID == nil || *e.TokenRootID != pair.RootTokenID {
		t.Error("the entry should name the session it started")
	}
}

// Nobody believes Sam@example.com and sam@example.com are two people.
func TestTheAddressIsMatchedCaseInsensitively(t *testing.T) {
	t.Parallel()

	f := setup(t)
	for _, typed := range []string{"SAM@EXAMPLE.COM", "  sam@example.com  ", "Sam@Example.Com"} {
		_, err := f.svc.Login(context.Background(), account.LoginInput{
			TenantID: f.tenant, EmailAddress: typed, Password: goodPassword,
			IPAddress: "203.0.113.10",
		})
		if err != nil {
			t.Errorf("%q should sign in: %v", typed, err)
		}
	}
}

// A wrong password and an address nobody has registered must be indistinguishable.
func TestAWrongPasswordAndAnUnknownAddressLookTheSame(t *testing.T) {
	t.Parallel()

	f := setup(t)

	_, wrong := f.login("not the password")
	_, unknown := f.svc.Login(context.Background(), account.LoginInput{
		TenantID: f.tenant, EmailAddress: "nobody@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})

	if wrong == nil || unknown == nil {
		t.Fatal("both should fail")
	}
	if wrong.Error() != unknown.Error() {
		t.Errorf("the two answers differ:\n  wrong password: %v\n  unknown address: %v", wrong, unknown)
	}
	if !rigerr.Is(wrong, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want 401", wrong)
	}
	// And the message must not mention the address, or the answer is the
	// oracle by another route.
	if strings.Contains(wrong.Error(), "sam@example.com") {
		t.Errorf("the message names the address: %v", wrong)
	}
}

// Refusing a disabled account before the password is checked would answer
// "disabled" to anybody who guessed the address.
func TestADisabledAccountIsOnlyRevealedToSomebodyWithThePassword(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.acct.IsActive = false
	f.store.Put(f.acct)

	// Right password: 403, and the reason is safe to give.
	if _, err := f.login(goodPassword); !rigerr.Is(err, rigerr.CodeForbidden) {
		t.Errorf("err = %v, want 403", err)
	}

	// Wrong password: 401, exactly as for an enabled account.
	if _, err := f.login("not the password"); !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want 401 — the account's state must not leak", err)
	}
}

func TestRequireVerifiedEmail(t *testing.T) {
	t.Parallel()

	f := setup(t)
	strict, err := account.New(account.Config{
		Store:                f.store,
		Sessions:             f.sessions,
		Identities:           f.identities,
		Hasher:               password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}),
		Log:                  f.log,
		Limiter:              throttle.New(f.log.counter).WithClock(f.clock.now),
		RequireVerifiedEmail: true,
		Now:                  f.clock.now,
		Sleep:                func(context.Context, time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = strict.Login(context.Background(), account.LoginInput{
		TenantID: f.tenant, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if !rigerr.Is(err, rigerr.CodeForbidden) {
		t.Errorf("err = %v, want 403 until the address is confirmed", err)
	}
}

// Six wrong passwords and the door closes — with a Retry-After, so a client can
// do something other than hammer.
func TestTheLockout(t *testing.T) {
	t.Parallel()

	f := setup(t)

	for i := range 5 {
		if _, err := f.login("wrong"); !rigerr.Is(err, rigerr.CodeUnauthorized) {
			t.Fatalf("attempt %d: err = %v, want 401", i+1, err)
		}
	}

	_, err := f.login("wrong")
	if !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Fatalf("the sixth attempt should be refused: %v", err)
	}
	// Even the right password. That is the point of a lockout.
	if _, err := f.login(goodPassword); !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Errorf("err = %v, want the lockout to hold", err)
	}

	if _, ok := f.log.last(authlog.EventAccountLocked); !ok {
		t.Error("the lockout should be recorded")
	}

	// A locked attempt must not record a failure of its own, or the lockout
	// would extend itself for as long as somebody kept knocking.
	before := f.log.count(authlog.EventLoginFailed)
	_, _ = f.login("wrong")
	if f.log.count(authlog.EventLoginFailed) != before {
		t.Error("a refused attempt should not extend its own window")
	}

	// It ends on its own.
	f.clock.advance(16 * time.Minute)
	if _, err := f.login(goodPassword); err != nil {
		t.Errorf("the window should have passed: %v", err)
	}
}

// Four typos and then the right password is a person having a bad morning.
func TestASuccessClearsTheLockout(t *testing.T) {
	t.Parallel()

	f := setup(t)
	for range 4 {
		_, _ = f.login("wrong")
	}
	if _, err := f.login(goodPassword); err != nil {
		t.Fatal(err)
	}

	f.clock.advance(time.Second)
	for range 4 {
		if _, err := f.login("wrong"); !rigerr.Is(err, rigerr.CodeUnauthorized) {
			t.Fatalf("err = %v, want 401 — the earlier failures were cleared", err)
		}
	}
}

// A cost you can raise but never apply to existing accounts is a cost you have
// not raised. Login is the only moment the plaintext exists to rehash from.
func TestLoginUpgradesAnOldHash(t *testing.T) {
	t.Parallel()

	f := setup(t)
	before, err := f.store.Credential(context.Background(), f.ident.ID)
	if err != nil {
		t.Fatal(err)
	}

	raised, err := account.New(account.Config{
		Store:      f.store,
		Sessions:   f.sessions,
		Identities: f.identities,
		Hasher:     password.New(password.Params{Memory: 16 * 1024, Iterations: 1, Parallelism: 1}),
		Log:        f.log,
		Limiter:    throttle.New(f.log.counter).WithClock(f.clock.now),
		Now:        f.clock.now,
		Sleep:      func(context.Context, time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := raised.Login(context.Background(), account.LoginInput{
		TenantID: f.tenant, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	}); err != nil {
		t.Fatal(err)
	}

	after, _ := f.store.Credential(context.Background(), f.ident.ID)
	if after.Params.Memory != 16*1024 {
		t.Errorf("memory = %d, want the raised cost", after.Params.Memory)
	}
	if after.PasswordHash == before.PasswordHash {
		t.Error("the stored hash should have been replaced")
	}
	// And the person can still sign in with the same password.
	if _, err := f.login(goodPassword); err != nil {
		t.Errorf("the upgraded hash should still verify: %v", err)
	}
}

func TestPasswordReset(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	original, err := f.login(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.svc.RequestPasswordReset(ctx, f.tenant, "sam@example.com", "203.0.113.10"); err != nil {
		t.Fatal(err)
	}
	if f.notify.reset == "" {
		t.Fatal("a link should have been handed to the notifier")
	}
	// The service must not have the token anywhere a handler could return it:
	// only the notifier sees it. And it goes to the person, not to one of their
	// accounts — the password it resets is the same one everywhere.
	if f.notify.resetTo.ID != f.ident.ID {
		t.Error("the link went to the wrong person")
	}

	const newPassword = "a completely different passphrase"
	if err := f.svc.ConfirmPasswordReset(ctx, f.notify.reset, newPassword, "203.0.113.10"); err != nil {
		t.Fatal(err)
	}

	// A reset happens because somebody lost control of the account. Leaving
	// the old sessions alive leaves whoever took it signed in.
	if _, err := f.sessions.Verify(ctx, original.Access.Token); err == nil {
		t.Error("the sessions from before the reset should be gone")
	}

	if _, err := f.login(goodPassword); err == nil {
		t.Error("the old password should no longer work")
	}
	if _, err := f.login(newPassword); err != nil {
		t.Errorf("the new password should work: %v", err)
	}
	if _, ok := f.log.last(authlog.EventPasswordResetCompleted); !ok {
		t.Error("the reset should be recorded")
	}
}

func TestAResetLinkIsSingleUse(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if err := f.svc.RequestPasswordReset(ctx, f.tenant, "sam@example.com", ""); err != nil {
		t.Fatal(err)
	}
	token := f.notify.reset

	if err := f.svc.ConfirmPasswordReset(ctx, token, "the first new passphrase", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ConfirmPasswordReset(ctx, token, "the second new passphrase", ""); err == nil {
		t.Error("a consumed link should not work again")
	}
}

func TestAResetLinkExpires(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if err := f.svc.RequestPasswordReset(ctx, f.tenant, "sam@example.com", ""); err != nil {
		t.Fatal(err)
	}

	f.clock.advance(account.DefaultResetTTL + time.Minute)
	if err := f.svc.ConfirmPasswordReset(ctx, f.notify.reset, "a new passphrase entirely", ""); err == nil {
		t.Error("an expired link should not work")
	}
}

// Asking for a reset must answer the same way whether or not the address is
// registered, or the endpoint is a way to enumerate customers.
func TestAResetForAnUnknownAddressLooksIdentical(t *testing.T) {
	t.Parallel()

	f := setup(t)

	if err := f.svc.RequestPasswordReset(context.Background(), f.tenant, "nobody@example.com", ""); err != nil {
		t.Errorf("an unknown address should answer the same way: %v", err)
	}
	if f.notify.reset != "" {
		t.Error("no link should have been sent")
	}
	// It is still recorded, which is what makes the rate limit bite: hammering
	// addresses costs budget whether or not any of them exist.
	e, ok := f.log.last(authlog.EventPasswordResetRequested)
	if !ok {
		t.Fatal("the attempt should be recorded")
	}
	if e.Outcome != authlog.Failed {
		t.Errorf("outcome = %q, want the log to say no account matched", e.Outcome)
	}
}

func TestResetRequestsAreRateLimited(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	for i := range 5 {
		if err := f.svc.RequestPasswordReset(ctx, f.tenant, "sam@example.com", ""); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}
	if err := f.svc.RequestPasswordReset(ctx, f.tenant, "sam@example.com", ""); !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Errorf("err = %v, want 429 — a reset endpoint is a mail cannon otherwise", err)
	}
}

func TestChangePassword(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	old, err := f.login(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	const newPassword = "an entirely different passphrase"
	fresh, err := f.svc.ChangePassword(ctx, account.ChangePasswordInput{
		TenantID: f.tenant, AccountID: f.acct.ID,
		CurrentPassword: goodPassword, NewPassword: newPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Every old session dies, and the one making the request is handed a
	// replacement so the person is not signed out of the tab they are in.
	if _, err := f.sessions.Verify(ctx, old.Access.Token); err == nil {
		t.Error("the sessions from before the change should be gone")
	}
	if _, err := f.sessions.Verify(ctx, fresh.Access.Token); err != nil {
		t.Errorf("the caller should be handed a working session: %v", err)
	}

	if _, err := f.login(newPassword); err != nil {
		t.Errorf("the new password should work: %v", err)
	}
	if _, ok := f.log.last(authlog.EventPasswordChanged); !ok {
		t.Error("the change should be recorded")
	}
}

func TestChangePasswordNeedsTheCurrentOne(t *testing.T) {
	t.Parallel()

	f := setup(t)

	_, err := f.svc.ChangePassword(context.Background(), account.ChangePasswordInput{
		TenantID: f.tenant, AccountID: f.acct.ID,
		CurrentPassword: "not the password", NewPassword: "a new passphrase entirely",
	})
	if !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want 401", err)
	}
}

func TestANewPasswordMustSatisfyThePolicy(t *testing.T) {
	t.Parallel()

	f := setup(t)

	_, err := f.svc.ChangePassword(context.Background(), account.ChangePasswordInput{
		TenantID: f.tenant, AccountID: f.acct.ID,
		CurrentPassword: goodPassword, NewPassword: "short",
	})
	if !rigerr.Is(err, rigerr.CodeUnprocessableEntity) {
		t.Errorf("err = %v, want the policy to refuse it", err)
	}
}

func TestEmailVerification(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if err := f.svc.SendEmailVerification(ctx, f.tenant, f.acct.ID); err != nil {
		t.Fatal(err)
	}
	if f.notify.verify == "" {
		t.Fatal("a link should have been sent")
	}

	if err := f.svc.VerifyEmail(ctx, f.notify.verify); err != nil {
		t.Fatal(err)
	}

	// The address belongs to the person, so it is the identity that is now
	// confirmed — in every tenant they belong to, not just this one.
	got, _ := f.store.FindIdentityByID(ctx, f.ident.ID)
	if !got.Verified() {
		t.Error("the address should be confirmed")
	}
	if _, ok := f.log.last(authlog.EventEmailVerified); !ok {
		t.Error("the confirmation should be recorded")
	}

	// A confirmation link for an already-confirmed address is not an error:
	// somebody clicking "resend" has got what they wanted.
	if err := f.svc.SendEmailVerification(ctx, f.tenant, f.acct.ID); err != nil {
		t.Errorf("resending for a verified address should be a no-op: %v", err)
	}
}

// An expired link, a used link, and one somebody invented all answer the same
// way, because knowing which would confirm a guess.
func TestBadLinksAllLookAlike(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if err := f.svc.RequestPasswordReset(ctx, f.tenant, "sam@example.com", ""); err != nil {
		t.Fatal(err)
	}
	real := f.notify.reset

	// A reset link is not a verification link, even though both are tokens.
	kindMismatch := f.svc.VerifyEmail(ctx, real)
	invented := f.svc.VerifyEmail(ctx, strings.Repeat("A", 52))
	garbage := f.svc.VerifyEmail(ctx, "not base32 at all !!!")

	for name, err := range map[string]error{
		"wrong kind": kindMismatch, "invented": invented, "garbage": garbage,
	} {
		if err == nil {
			t.Fatalf("%s should have failed", name)
		}
	}
	if kindMismatch.Error() != invented.Error() || invented.Error() != garbage.Error() {
		t.Errorf("the answers differ:\n  %v\n  %v\n  %v", kindMismatch, invented, garbage)
	}
}

// A session that began as an administrator acting as somebody else must say so,
// at both ends.
func TestImpersonation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()
	admin := uuid.New()

	pair, err := f.svc.Impersonate(ctx, account.ImpersonateInput{
		TenantID: f.tenant, AdministratorID: admin, AccountID: f.acct.ID,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := f.sessions.Verify(ctx, pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.ImpersonatedByAccountID == nil || *tok.ImpersonatedByAccountID != admin {
		t.Fatal("the session should carry who is really behind it")
	}

	started, ok := f.log.last(authlog.EventImpersonationStarted)
	if !ok {
		t.Fatal("starting should be recorded")
	}
	if started.Detail["administrator_account_id"] != admin.String() {
		t.Errorf("the entry should name the administrator: %v", started.Detail)
	}

	if err := f.svc.EndImpersonation(ctx, tok); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.Verify(ctx, pair.Access.Token); err == nil {
		t.Error("ending it should revoke the session")
	}
	if _, ok := f.log.last(authlog.EventImpersonationEnded); !ok {
		t.Error("ending should be recorded too")
	}
}

func TestEndingAnOrdinarySessionIsNotImpersonation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair, err := f.login(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := f.sessions.Verify(context.Background(), pair.Access.Token)

	if err := f.svc.EndImpersonation(context.Background(), tok); err == nil {
		t.Error("an ordinary session has no impersonation to end")
	}
}

// A login endpoint with no lockout is a password oracle with a queue.
func TestALimiterIsRequired(t *testing.T) {
	t.Parallel()

	tokens := session.NewMemoryStore()
	sessions, err := session.New(session.Config{Store: tokens})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := session.NewIdentity(session.IdentityConfig{Store: tokens})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := account.New(account.Config{
		Store: account.NewMemoryStore(), Sessions: sessions, Identities: identities,
	}); err == nil {
		t.Error("a service with no rate limiter should refuse to exist")
	}
}

// Logout and Refresh are one line each, and both are the line an application
// reaches for rather than touching the session manager itself.
func TestLogoutEndsTheSessionAndRefreshContinuesIt(t *testing.T) {
	t.Parallel()

	f := setup(t)

	pair, err := f.login(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	fresh, err := f.svc.Refresh(context.Background(), pair.Refresh.Token)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if fresh.Access.Token == pair.Access.Token {
		t.Error("a refresh should hand back a new access token")
	}
	// The same session throughout: a refresh continues a session, it does not
	// start one, and a session list that grew a row on every refresh would be
	// unreadable within a day.
	if fresh.RootTokenID != pair.RootTokenID {
		t.Errorf("root = %s, want %s", fresh.RootTokenID, pair.RootTokenID)
	}

	if err := f.svc.Logout(context.Background(), pair.RootTokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sessions.Verify(context.Background(), fresh.Access.Token); err == nil {
		t.Error("logging out should kill the whole family, not just the token presented")
	}
	if _, err := f.svc.Refresh(context.Background(), fresh.Refresh.Token); err == nil {
		t.Error("a logged-out session should not refresh")
	}
}

// SetPassword is what provisioning and an administrator's rescue both go
// through, so the guards on it are the guards on both.
func TestSetPassword(t *testing.T) {
	t.Parallel()

	f := setup(t)

	if err := f.svc.SetPassword(context.Background(), uuid.New(), goodPassword); err == nil {
		t.Error("an account that is not there should be a 404, not a credential nobody owns")
	} else if rigerr.CodeOf(err) != rigerr.CodeNotFound {
		t.Errorf("code = %q, want NotFound", rigerr.CodeOf(err))
	}

	// The policy applies here too. An administrator setting "12345" is the
	// same weak password as anybody else setting it.
	if err := f.svc.SetPassword(context.Background(), f.ident.ID, "short"); err == nil {
		t.Error("the policy should apply to an administrative reset")
	}

	pair, err := f.login(goodPassword)
	if err != nil {
		t.Fatal(err)
	}

	const replacement = "a different long enough password"
	if err := f.svc.SetPassword(context.Background(), f.ident.ID, replacement); err != nil {
		t.Fatal(err)
	}

	// Sessions go either way: the reason to set a password over somebody's head
	// is usually that the account is compromised.
	if _, err := f.sessions.Verify(context.Background(), pair.Access.Token); err == nil {
		t.Error("the old sessions should be gone")
	}
	if _, err := f.login(replacement); err != nil {
		t.Errorf("the new password should work: %v", err)
	}
	if _, err := f.login(goodPassword); err == nil {
		t.Error("the old one should not")
	}
}

// A tenant identifier that does not match is the cross-tenant case, and it has
// to answer the same way as an identifier that does not exist — anything else
// makes account identifiers probeable across customers.
func TestAnotherTenantsAccountIsNotFound(t *testing.T) {
	t.Parallel()

	f := setup(t)
	elsewhere := uuid.New()

	if err := f.svc.SendEmailVerification(context.Background(), elsewhere, f.acct.ID); rigerr.CodeOf(err) != rigerr.CodeNotFound {
		t.Errorf("SendEmailVerification: code = %q, want NotFound", rigerr.CodeOf(err))
	}

	_, err := f.svc.ChangePassword(context.Background(), account.ChangePasswordInput{
		TenantID: elsewhere, AccountID: f.acct.ID,
		CurrentPassword: goodPassword, NewPassword: "a different long enough password",
	})
	if rigerr.CodeOf(err) != rigerr.CodeNotFound {
		t.Errorf("ChangePassword: code = %q, want NotFound", rigerr.CodeOf(err))
	}

	_, err = f.svc.Impersonate(context.Background(), account.ImpersonateInput{
		TenantID: elsewhere, AdministratorID: uuid.New(), AccountID: f.acct.ID,
	})
	if rigerr.CodeOf(err) != rigerr.CodeNotFound {
		t.Errorf("Impersonate: code = %q, want NotFound", rigerr.CodeOf(err))
	}
}

// An account provisioned through OAuth has no password at all, and telling
// somebody the current one is wrong when there is no current one sends them to
// a reset link they will not be able to explain.
func TestChangingAPasswordThatWasNeverSetSaysSo(t *testing.T) {
	t.Parallel()

	f := setup(t)

	fresh := &account.Account{
		ID: uuid.New(), TenantID: f.tenant, DisplayName: "Robin", IsActive: true,
	}
	f.store.PutPerson(&account.Identity{
		ID: uuid.New(), EmailAddress: "robin@example.com",
		DisplayName: "Robin", IsActive: true,
	}, fresh)

	_, err := f.svc.ChangePassword(context.Background(), account.ChangePasswordInput{
		TenantID: f.tenant, AccountID: fresh.ID,
		CurrentPassword: "anything", NewPassword: "a long enough new password",
	})
	if err == nil {
		t.Fatal("changing a password that does not exist should fail")
	}
	if !strings.Contains(err.Error(), "reset") {
		t.Errorf("the message should point at the way out: %v", err)
	}
}

// The resend limit is keyed on the account rather than the address, because
// resending is authenticated: the person is already known, and the limit exists
// to stop their inbox being used as a mail cannon.
func TestVerificationResendsAreRateLimited(t *testing.T) {
	t.Parallel()

	f := setup(t)

	for i := range 5 {
		if err := f.svc.SendEmailVerification(context.Background(), f.tenant, f.acct.ID); err != nil {
			t.Fatalf("resend %d: %v", i+1, err)
		}
	}

	err := f.svc.SendEmailVerification(context.Background(), f.tenant, f.acct.ID)
	if err == nil {
		t.Fatal("the sixth resend should be refused")
	}
	if rigerr.StatusOf(err) != 429 {
		t.Errorf("status = %d, want 429", rigerr.StatusOf(err))
	}

	// And the window ages out on its own: no cleanup job, no stuck accounts.
	f.clock.advance(time.Hour + time.Minute)
	if err := f.svc.SendEmailVerification(context.Background(), f.tenant, f.acct.ID); err != nil {
		t.Errorf("the window should have passed: %v", err)
	}
}

// A reset link is not a way around the password policy.
func TestAResetCannotSetAPasswordThePolicyRefuses(t *testing.T) {
	t.Parallel()

	f := setup(t)

	if err := f.svc.RequestPasswordReset(context.Background(), f.tenant, "sam@example.com", "203.0.113.10"); err != nil {
		t.Fatal(err)
	}

	if err := f.svc.ConfirmPasswordReset(context.Background(), f.notify.reset, "short", "203.0.113.10"); err == nil {
		t.Fatal("the policy should apply to a reset")
	}

	// And the link survives the refusal: making somebody request a new link
	// because they typed a short password once is a bad afternoon.
	if err := f.svc.ConfirmPasswordReset(context.Background(), f.notify.reset,
		"a long enough new password", "203.0.113.10"); err != nil {
		t.Errorf("the link should still be good: %v", err)
	}
}

// Login is padded so that response time does not reveal whether an account
// exists — an unknown address skips the hash entirely and would otherwise
// answer in a fraction of the time a real one takes.
func TestLoginTakesTheSameFloorWhoeverAsks(t *testing.T) {
	t.Parallel()

	f := setup(t)

	var waited []time.Duration
	svc, err := account.New(account.Config{
		Store:      f.store,
		Sessions:   f.sessions,
		Identities: f.identities,
		Hasher:     password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}),
		Log:        f.log,
		Notifier:   f.notify,
		Limiter:    throttle.New(f.log.counter).WithClock(f.clock.now),
		Now:        f.clock.now,
		// The real floor, and a recording of what it asked for. Actually
		// sleeping would make the suite pay for the property being tested.
		MinDuration: 750 * time.Millisecond,
		Sleep:       func(_ context.Context, d time.Duration) { waited = append(waited, d) },
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, address := range []string{"sam@example.com", "nobody@example.com"} {
		if _, err := svc.Login(context.Background(), account.LoginInput{
			TenantID: f.tenant, EmailAddress: address, Password: "nope",
			Client: session.ClientWeb, IPAddress: "203.0.113.10",
		}); err == nil {
			t.Fatalf("%s: the login should have failed", address)
		}
	}

	if len(waited) != 2 {
		t.Fatalf("padded %d logins, want both", len(waited))
	}
	// The unknown address does no work at all, so it is the one with something
	// left to wait for.
	if waited[1] <= 0 {
		t.Errorf("an address that does no hashing should still take the floor: %v", waited)
	}
	for _, d := range waited {
		if d > 750*time.Millisecond {
			t.Errorf("waited %s, longer than the floor itself", d)
		}
	}
}

// The default padding is a real sleep, and one that ignores a client that has
// already given up is a goroutine held open by anybody who can disconnect.
func TestTheDefaultPaddingStopsWhenTheRequestDoes(t *testing.T) {
	t.Parallel()

	f := setup(t)

	svc, err := account.New(account.Config{
		Store:       f.store,
		Sessions:    f.sessions,
		Identities:  f.identities,
		Hasher:      password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}),
		Log:         f.log,
		Notifier:    f.notify,
		Limiter:     throttle.New(f.log.counter).WithClock(f.clock.now),
		Now:         f.clock.now,
		MinDuration: time.Hour,
		// No Sleep: the package's own.
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.Login(ctx, account.LoginInput{
			TenantID: f.tenant, EmailAddress: "nobody@example.com", Password: "nope",
			Client: session.ClientWeb, IPAddress: "203.0.113.10",
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the padding outlived the request")
	}
}

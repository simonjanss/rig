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
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// The address every fixture's person has, in the case they typed it.
const samAddress = "Sam@Example.com"

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
	code     string
	verify   string
	invite   string
	codeTo   *account.Identity
	verifyTo *account.Identity
	inviteTo *account.Invitation
	// codes is every code this notifier has been handed, so that a test about
	// superseding can see that the old one and the new one are different.
	codes []string
}

func (n *notifier) SendEmailCode(_ context.Context, i *account.Identity, code string) error {
	n.code, n.codeTo = code, i
	n.codes = append(n.codes, code)
	return nil
}

func (n *notifier) SendEmailVerification(_ context.Context, i *account.Identity, token string) error {
	n.verify, n.verifyTo = token, i
	return nil
}

func (n *notifier) SendInvitation(_ context.Context, _ *account.Identity, inv *account.Invitation, token string) error {
	n.invite, n.inviteTo = token, inv
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
	return setupWith(t, nil)
}

// setupWith builds the fixture with one chance to edit the configuration
// before the service exists — which is the only time a hook like OnRegistered
// can be attached.
func setupWith(t *testing.T, edit func(*account.Config)) *fixture {
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
	// No padding: the suite is about the rules, not about how long they take.
	cfg := account.Config{
		Store:      store,
		Sessions:   sessions,
		Identities: identities,
		Log:        log,
		Notifier:   notify,
		// Provisioning off, which is the default and the invite-only reading. A
		// test that wants the open one turns it on in its own edit closure.
		EmailCode: account.EmailCodeOptions{Enabled: true},
		Limiter:   throttle.New(log.counter).WithClock(c.now),
		Now:       c.now,
		Sleep:     func(context.Context, time.Duration) {},
	}
	if edit != nil {
		edit(&cfg)
	}
	svc, err := account.New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	// The store's clock is the fixture's, so that an invitation expires for the
	// double when it would expire for Postgres.
	store.Now = c.now

	f := &fixture{
		svc: svc, store: store, sessions: sessions, identities: identities,
		log: log, notify: notify, clock: c, tenant: uuid.New(),
	}
	f.ident = &account.Identity{
		ID:           uuid.New(),
		EmailAddress: samAddress,
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
	return f
}

// login signs the fixture's person in with a mailed code and hands back the
// tenant session.
//
// The pair rather than the whole result, because that is what almost every test
// here is about. The tests that care about the tenant-less half call signIn.
func (f *fixture) login() (session.Pair, error) {
	res, err := f.signIn()
	if err != nil {
		return session.Pair{}, err
	}
	if res.Session == nil {
		return session.Pair{}, errors.New("signed in with no tenant")
	}
	return *res.Session, nil
}

// signIn asks for a code and types it back, which is the whole flow.
func (f *fixture) signIn() (account.SignInResult, error) {
	code, err := f.askForCode()
	if err != nil {
		return account.SignInResult{}, err
	}
	return f.verify(code)
}

// askForCode requests one and reads it out of the notifier, which is the only
// place the plaintext ever exists.
func (f *fixture) askForCode() (string, error) {
	if err := f.svc.RequestEmailCode(context.Background(), account.RequestEmailCodeInput{
		TenantID:     f.tenant,
		EmailAddress: "sam@example.com",
		IPAddress:    "203.0.113.10",
		UserAgent:    "Mozilla/5.0",
	}); err != nil {
		return "", err
	}
	if f.notify.code == "" {
		return "", errors.New("no code was mailed")
	}
	return f.notify.code, nil
}

func (f *fixture) verify(code string) (account.SignInResult, error) {
	return f.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
		TenantID:     f.tenant,
		EmailAddress: "sam@example.com",
		Code:         code,
		Client:       session.ClientWeb,
		IPAddress:    "203.0.113.10",
		UserAgent:    "Mozilla/5.0",
	})
}

// askForCodeAs and verifyAs are the two halves for an address that is not the
// fixture's own.
func (f *fixture) askForCodeAs(email string) (string, error) {
	f.notify.code = ""
	if err := f.svc.RequestEmailCode(context.Background(), account.RequestEmailCodeInput{
		TenantID:     f.tenant,
		EmailAddress: email,
		IPAddress:    "203.0.113.10",
	}); err != nil {
		return "", err
	}
	if f.notify.code == "" {
		return "", errors.New("no code was mailed")
	}
	return f.notify.code, nil
}

func (f *fixture) verifyAs(email, code string) (account.SignInResult, error) {
	return f.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
		TenantID:     f.tenant,
		EmailAddress: email,
		Code:         code,
		Client:       session.ClientWeb,
		IPAddress:    "203.0.113.10",
		UserAgent:    "Mozilla/5.0",
	})
}

// wrongCode is a code of the right shape that is not this one, so that a test
// about a wrong guess is not accidentally a test about a malformed request —
// the two are charged differently on purpose.
func wrongCode(code string) string {
	out := []byte(code)
	if out[len(out)-1] == '0' {
		out[len(out)-1] = '1'
	} else {
		out[len(out)-1] = '0'
	}
	return string(out)
}

func TestLogin(t *testing.T) {
	t.Parallel()

	f := setup(t)
	pair, err := f.login()
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
		code, err := f.askForCodeAs(typed)
		if err != nil {
			t.Fatalf("%q should be sent a code: %v", typed, err)
		}
		if _, err := f.verifyAs(typed, code); err != nil {
			t.Errorf("%q should sign in: %v", typed, err)
		}
	}
}

// A wrong code and an address nobody has registered must be indistinguishable.
// The pad in the exported half is what makes that true of the timing too.
func TestAWrongCodeAndAnUnknownAddressLookTheSame(t *testing.T) {
	t.Parallel()

	f := setup(t)
	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}

	_, wrong := f.verify(wrongCode(code))
	_, unknown := f.verifyAs("nobody@example.com", code)

	if wrong == nil || unknown == nil {
		t.Fatal("both should fail")
	}
	if wrong.Error() != unknown.Error() {
		t.Errorf("the two answers differ:\n  wrong code: %v\n  unknown address: %v", wrong, unknown)
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

// Refusing a disabled account before the code is compared would answer
// "disabled" to anybody who guessed the address.
func TestADisabledAccountIsOnlyRevealedToSomebodyWithTheCode(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.acct.IsActive = false
	f.store.Put(f.acct)

	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}

	// Wrong code: 401, exactly as for an enabled account. Checked first,
	// because it is the one that must not leak and the right code consumes.
	if _, err := f.verify(wrongCode(code)); !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want 401 — the account's state must not leak", err)
	}

	code, err = f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	// Right code: 403, and the reason is safe to give.
	if _, err := f.verify(code); !rigerr.Is(err, rigerr.CodeForbidden) {
		t.Errorf("err = %v, want 403", err)
	}
}

// A code is guessable in a way a token is not, so it dies of guessing rather
// than only being slowed down.
func TestACodeDiesAfterTooManyWrongGuesses(t *testing.T) {
	t.Parallel()

	f := setup(t)
	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}

	for i := range account.DefaultEmailCodeMaxAttempts {
		if _, err := f.verify(wrongCode(code)); !rigerr.Is(err, rigerr.CodeUnauthorized) {
			t.Fatalf("guess %d: err = %v, want 401", i+1, err)
		}
	}

	// The right code, and it is dead. That is the difference between a ceiling
	// on one secret and a rate limit on an address: the limit would still have
	// let this through.
	if _, err := f.verify(code); !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want the burned code to stay dead", err)
	}

	// And asking for another works, which is the answer to somebody burning
	// your code on purpose.
	next, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	if next == code {
		t.Fatal("the new code should not be the old one")
	}
	if _, err := f.verify(next); err != nil {
		t.Errorf("a fresh code should work: %v", err)
	}
}

// One live code per person, so that "the newest code is the one that works" is
// true rather than probable.
func TestAskingAgainSupersedesTheLastCode(t *testing.T) {
	t.Parallel()

	f := setup(t)
	first, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("two requests should not draw the same code")
	}

	if _, err := f.verify(first); !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("the superseded code should be refused: %v", err)
	}
	if _, err := f.verify(second); err != nil {
		t.Errorf("the newest code should work: %v", err)
	}
}

func TestACodeIsSingleUse(t *testing.T) {
	t.Parallel()

	f := setup(t)
	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(code); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(code); !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want a consumed code to be refused", err)
	}
}

func TestACodeExpires(t *testing.T) {
	t.Parallel()

	f := setup(t)
	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}

	f.clock.advance(account.DefaultEmailCodeTTL + time.Minute)
	if _, err := f.verify(code); !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want an expired code to be refused", err)
	}
}

// The code is what confirms the address: it went there and came back. So a
// deployment that demands a verified address is satisfied by the first sign-in
// rather than deadlocked by it.
func TestSigningInWithACodeConfirmsTheAddress(t *testing.T) {
	t.Parallel()

	f := setup(t)
	if f.ident.Verified() {
		t.Fatal("the fixture's person should start unverified")
	}
	if _, err := f.login(); err != nil {
		t.Fatal(err)
	}

	after, err := f.store.FindIdentityByID(context.Background(), f.ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.Verified() {
		t.Error("typing the code back should have confirmed the address")
	}
}

// Asking for a code must answer the same way for an address nobody has, or the
// endpoint is a list of your customers.
func TestAskingForACodeLooksIdenticalForAnUnknownAddress(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.notify.code = ""

	if err := f.svc.RequestEmailCode(context.Background(), account.RequestEmailCodeInput{
		TenantID: f.tenant, EmailAddress: "nobody@example.com", IPAddress: "203.0.113.10",
	}); err != nil {
		t.Errorf("an unknown address should be accepted silently: %v", err)
	}
	if f.notify.code != "" {
		t.Error("nothing should have been mailed")
	}
	// Recorded as a failure even so: the row is what an operator reads and what
	// the limit counts, and only the response is the same.
	e, ok := f.log.last(authlog.EventEmailCodeRequested)
	if !ok {
		t.Fatal("the request should be recorded")
	}
	if e.Outcome != authlog.Failed {
		t.Errorf("outcome = %s, want the row to say it went nowhere", e.Outcome)
	}
}

// Off by default, so a deployment is invite-only until it says otherwise.
func TestACodeReachesANewAddressOnlyWithProvisioning(t *testing.T) {
	t.Parallel()

	closed := setup(t)
	if err := closed.svc.RequestEmailCode(context.Background(), account.RequestEmailCodeInput{
		TenantID: closed.tenant, EmailAddress: "newcomer@example.com", IPAddress: "203.0.113.11",
	}); err != nil {
		t.Fatal(err)
	}
	if got, _ := closed.store.FindIdentityByEmail(context.Background(), "newcomer@example.com"); got != nil {
		t.Error("no identity should have been created")
	}

	open := setupWith(t, func(c *account.Config) {
		c.EmailCode.AllowProvisioning = true
	})
	if err := open.svc.RequestEmailCode(context.Background(), account.RequestEmailCodeInput{
		TenantID: open.tenant, EmailAddress: "Newcomer@Example.com", IPAddress: "203.0.113.11",
	}); err != nil {
		t.Fatal(err)
	}
	made, err := open.store.FindIdentityByEmail(context.Background(), "newcomer@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if made == nil {
		t.Fatal("provisioning should have created the person")
	}
	if made.EmailAddress != "Newcomer@Example.com" {
		t.Errorf("address = %q, want the cased form they typed", made.EmailAddress)
	}
	if made.DisplayName != "Newcomer" {
		t.Errorf("display name = %q, want the local part", made.DisplayName)
	}
	// And they can finish: a person with no tenant signs in and lands in the
	// picker, which is the state the whole invitation flow runs through.
	res, err := open.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
		EmailAddress: "newcomer@example.com", Code: open.notify.code,
		IPAddress: "203.0.113.11",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != nil || res.Identity.Token == "" {
		t.Error("somebody who belongs nowhere should get an identity token and no session")
	}
}

// Five wrong codes and the door closes — with a Retry-After, so a client can do
// something other than hammer.
//
// It takes a fresh code each time, so that the ceiling on one code never fires
// and what is under test is the limit on the address. That means asking for more
// codes than the standard hourly limit allows, which the fixture widens here and
// TestTheRequestLimitBindsFirst is about.
func TestTheLockout(t *testing.T) {
	t.Parallel()

	f := setupWith(t, func(c *account.Config) {
		c.Limits = throttle.Standard()
		c.Limits.EmailCodeRequest.Max = 50
		c.Limits.EmailCodeByIP.Max = 50
	})

	// Each request draws a new code, so the ceiling on one code never fires and
	// what is being tested is the limit on the address.
	for i := range 5 {
		code, err := f.askForCode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.verify(wrongCode(code)); !rigerr.Is(err, rigerr.CodeUnauthorized) {
			t.Fatalf("attempt %d: err = %v, want 401", i+1, err)
		}
	}

	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.verify(wrongCode(code)); !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Fatalf("the sixth attempt should be refused: %v", err)
	}
	// Even the right code. That is the point of a lockout.
	if _, err := f.verify(code); !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Errorf("err = %v, want the lockout to hold", err)
	}

	if _, ok := f.log.last(authlog.EventAccountLocked); !ok {
		t.Error("the lockout should be recorded")
	}

	// A locked attempt must not record a failure of its own, or the lockout
	// would extend itself for as long as somebody kept knocking.
	before := f.log.count(authlog.EventLoginFailed)
	_, _ = f.verify(code)
	if f.log.count(authlog.EventLoginFailed) != before {
		t.Error("a refused attempt should not extend its own window")
	}

	// It ends on its own.
	f.clock.advance(16 * time.Minute)
	if _, err := f.login(); err != nil {
		t.Errorf("the window should have passed: %v", err)
	}
}

// With the standard numbers the request limit is what somebody actually hits,
// and it is worth pinning because it means the address lockout is defence in
// depth here rather than the thing doing the work.
//
// Five code requests an hour per address. Reaching the five failed sign-ins that
// lock an address takes five codes, because the ceiling kills each one after
// three guesses — so the sixth request is refused before the sixth sign-in can
// be tried.
func TestTheRequestLimitBindsFirst(t *testing.T) {
	t.Parallel()

	f := setup(t)
	for i := range 5 {
		if _, err := f.askForCode(); err != nil {
			t.Fatalf("request %d: %v", i+1, err)
		}
	}

	_, err := f.askForCode()
	if !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Errorf("err = %v, want the sixth request refused", err)
	}
}

// Four fat-fingered codes and then the right one is a person having a bad
// morning.
func TestASuccessClearsTheLockout(t *testing.T) {
	t.Parallel()

	f := setupWith(t, func(c *account.Config) {
		c.Limits = throttle.Standard()
		c.Limits.EmailCodeRequest.Max = 50
		c.Limits.EmailCodeByIP.Max = 50
	})
	for range 4 {
		code, err := f.askForCode()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.verify(wrongCode(code))
	}
	if _, err := f.login(); err != nil {
		t.Fatal(err)
	}

	f.clock.advance(time.Second)
	for range 4 {
		code, err := f.askForCode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.verify(wrongCode(code)); !rigerr.Is(err, rigerr.CodeUnauthorized) {
			t.Fatalf("err = %v, want 401 — the earlier failures were cleared", err)
		}
	}
}

// RequireVerifiedEmail governs the provider door. The code door satisfies it by
// construction, which is what stops it deadlocking the only way in — and the
// reason VerifyEmailCode calls the unexported tail.
func TestRequireVerifiedEmail(t *testing.T) {
	t.Parallel()

	f := setupWith(t, func(c *account.Config) { c.RequireVerifiedEmail = true })

	// A provider sign-in for an unconfirmed address is refused.
	_, err := f.svc.SignInIdentity(context.Background(), account.SignInIdentityInput{
		IdentityID: f.ident.ID,
		TenantID:   f.tenant,
		IPAddress:  "203.0.113.10",
		Method:     "Google",
	})
	if !rigerr.Is(err, rigerr.CodeForbidden) {
		t.Errorf("err = %v, want 403 until the address is confirmed", err)
	}

	// The code flow is not, because typing the code is the confirmation.
	if _, err := f.login(); err != nil {
		t.Errorf("a mailed code should satisfy the gate it fills in: %v", err)
	}

	// And now the provider door works too.
	if _, err := f.svc.SignInIdentity(context.Background(), account.SignInIdentityInput{
		IdentityID: f.ident.ID,
		TenantID:   f.tenant,
		IPAddress:  "203.0.113.10",
		Method:     "Google",
	}); err != nil {
		t.Errorf("the address is confirmed now: %v", err)
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

	other := uuid.New()
	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: other, EmailAddress: "sam@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	// An invitation is not a verification link, even though both are tokens.
	kindMismatch := f.svc.VerifyEmail(ctx, f.notify.invite)
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
	pair, err := f.login()
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

	pair, err := f.login()
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

	_, err := f.svc.Impersonate(context.Background(), account.ImpersonateInput{
		TenantID: elsewhere, AdministratorID: uuid.New(), AccountID: f.acct.ID,
	})
	if rigerr.CodeOf(err) != rigerr.CodeNotFound {
		t.Errorf("Impersonate: code = %q, want NotFound", rigerr.CodeOf(err))
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

// The verify is padded so that response time does not reveal whether an address
// has an account. With no password to verify there is no expensive work making
// the two paths cost the same, so the floor is the whole of it — which is why
// there is a test about the floor rather than only about the answer.
func TestVerifyTakesTheSameFloorWhoeverAsks(t *testing.T) {
	t.Parallel()

	f := setup(t)

	var waited []time.Duration
	svc, err := account.New(account.Config{
		Store:      f.store,
		Sessions:   f.sessions,
		Identities: f.identities,
		Log:        f.log,
		Notifier:   f.notify,
		EmailCode:  account.EmailCodeOptions{Enabled: true},
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
		if _, err := svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
			TenantID: f.tenant, EmailAddress: address, Code: "000000",
			Client: session.ClientWeb, IPAddress: "203.0.113.10",
		}); err == nil {
			t.Fatalf("%s: the sign-in should have failed", address)
		}
	}

	if len(waited) != 2 {
		t.Fatalf("padded %d sign-ins, want both", len(waited))
	}
	for _, d := range waited {
		if d <= 0 {
			t.Errorf("both paths should still owe the floor: %v", waited)
		}
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
		Log:         f.log,
		Notifier:    f.notify,
		EmailCode:   account.EmailCodeOptions{Enabled: true},
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
		_, _ = svc.VerifyEmailCode(ctx, account.VerifyEmailCodeInput{
			TenantID: f.tenant, EmailAddress: "nobody@example.com", Code: "000000",
			Client: session.ClientWeb, IPAddress: "203.0.113.10",
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the padding outlived the request")
	}
}

// A code minted and never sent has no secret, so nothing can match it — and a
// deployment that enables the flow without a way to send is refused rather than
// left minting into a void.
func TestEmailCodeRefusesToExistWithNoNotifier(t *testing.T) {
	t.Parallel()

	f := setup(t)
	_, err := account.New(account.Config{
		Store:      f.store,
		Sessions:   f.sessions,
		Identities: f.identities,
		Log:        f.log,
		EmailCode:  account.EmailCodeOptions{Enabled: true},
		Limiter:    throttle.New(f.log.counter).WithClock(f.clock.now),
		Now:        f.clock.now,
	})
	if err == nil {
		t.Fatal("a code flow with no Notifier should be refused")
	}
	if !strings.Contains(err.Error(), "Notifier") {
		t.Errorf("err = %v, want it to name what is missing", err)
	}
}

// Six to ten digits, and the reason is the ceiling: four digits with any
// workable number of attempts is not a credential.
func TestEmailCodeRefusesALengthThatCannotBeSafe(t *testing.T) {
	t.Parallel()

	f := setup(t)
	for _, length := range []int{4, 5, 11} {
		_, err := account.New(account.Config{
			Store:      f.store,
			Sessions:   f.sessions,
			Identities: f.identities,
			Log:        f.log,
			Notifier:   f.notify,
			EmailCode:  account.EmailCodeOptions{Enabled: true, Length: length},
			Limiter:    throttle.New(f.log.counter).WithClock(f.clock.now),
			Now:        f.clock.now,
		})
		if err == nil {
			t.Errorf("a length of %d should be refused", length)
		}
	}
}

// Leading zeros are part of the secret, and stripping them is the bug that
// ships in half the implementations of this flow.
func TestACodeKeepsItsLeadingZeros(t *testing.T) {
	t.Parallel()

	f := setup(t)
	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != account.DefaultEmailCodeLength {
		t.Fatalf("code = %q, want %d digits", code, account.DefaultEmailCodeLength)
	}

	// Typed with the spacing somebody pastes, which has to be tolerated, and
	// with the zeros intact, which has to matter.
	spaced := code[:3] + " " + code[3:]
	if _, err := f.verify(spaced); err != nil {
		t.Errorf("a pasted code should be read: %v", err)
	}
}

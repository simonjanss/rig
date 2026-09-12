package account_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/throttle"
)

// signInIdentity is the shared last step from the entry point a provider uses:
// no password, no rate limit, an identity that has already been established.
func (f *fixture) signInIdentity(in account.SignInIdentityInput) (account.SignInResult, error) {
	if in.IdentityID == uuid.Nil {
		in.IdentityID = f.ident.ID
	}
	if in.IPAddress == "" {
		in.IPAddress = "203.0.113.10"
	}
	if in.Client == "" {
		in.Client = session.ClientWeb
	}
	return f.svc.SignInIdentity(context.Background(), in)
}

// markVerified confirms the fixture person's address, the way clicking the link
// in a verification mail would.
func (f *fixture) markVerified(t *testing.T) {
	t.Helper()
	if err := f.store.MarkIdentityVerified(t.Context(), f.ident.ID, f.clock.now()); err != nil {
		t.Fatal(err)
	}
}

// second gives the fixture's person an account in another tenant, and hands
// back the tenant and the account.
func (f *fixture) second(t *testing.T, name string) (uuid.UUID, *account.Account) {
	t.Helper()

	tenantID := uuid.New()
	acct := &account.Account{
		ID: uuid.New(), TenantID: tenantID, IdentityID: &f.ident.ID,
		DisplayName: "Sam", IsActive: true,
	}
	f.store.Put(acct)
	if f.store.TenantNames == nil {
		f.store.TenantNames = map[uuid.UUID]string{}
	}
	f.store.TenantNames[tenantID] = name
	return tenantID, acct
}

// The requirement the whole shared step exists for: land back where you were.
//
// The fixture's own tenant is the oldest, so the other one being chosen is the
// assertion — under the old rule this landed in the first and there was nothing
// a returning person could do about it.
func TestSignInIdentityLandsWhereSomebodyWasLast(t *testing.T) {
	t.Parallel()

	f := setup(t)
	elsewhere, acct := f.second(t, "Beta")
	f.store.SetLastAccount(f.ident.ID, acct.ID)

	res, err := f.signInIdentity(account.SignInIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TenantID != elsewhere {
		t.Errorf("landed in %s, want the tenant they were last in, %s", res.TenantID, elsewhere)
	}
	if res.Session == nil {
		t.Fatal("there was somewhere to be, so there should be a session")
	}
	if res.Identity.Token == "" {
		t.Error("the identity token is issued alongside a session, not instead of one")
	}
	// Both of them, so the picker needs no second call.
	if len(res.Tenants) != 2 {
		t.Fatalf("%d tenants, want 2", len(res.Tenants))
	}
}

// The fallback, which is a first sign-in: nowhere to have been last.
func TestSignInIdentityWithNoHistoryLandsInTheOldest(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.second(t, "Beta")

	res, err := f.signInIdentity(account.SignInIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TenantID != f.tenant {
		t.Errorf("landed in %s, want the tenant they joined first, %s", res.TenantID, f.tenant)
	}
}

// A landing nobody can use is worse than no landing, so a remembered account
// that has since been disabled falls through rather than being honoured.
func TestSignInIdentitySkipsARememberedAccountThatIsGone(t *testing.T) {
	t.Parallel()

	f := setup(t)
	_, acct := f.second(t, "Beta")
	acct.IsActive = false
	f.store.Put(acct)
	f.store.SetLastAccount(f.ident.ID, acct.ID)

	res, err := f.signInIdentity(account.SignInIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.TenantID != f.tenant {
		t.Errorf("landed in %s, want the fallback %s", res.TenantID, f.tenant)
	}
}

// Signed in and nowhere to be, which is a success rather than a 403. It is what
// somebody with an invitation waiting has, and what a provider sign-in produces
// for a person who has not joined anything yet.
func TestSignInIdentityWithNowhereToBe(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.acct.IsActive = false
	f.store.Put(f.acct)

	res, err := f.signInIdentity(account.SignInIdentityInput{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != nil || res.TenantID != uuid.Nil {
		t.Error("there was nowhere to land, so there should be no session")
	}
	if res.Identity.Token == "" {
		t.Fatal("the picker needs a credential, and this is the only one it gets")
	}
	e, ok := f.log.last(authlog.EventLoginSucceeded)
	if !ok {
		t.Fatal("it is a success: they proved who they are")
	}
	if e.TenantID != nil || e.TokenRootID != nil {
		t.Error("no tenant and no session to record")
	}
}

// Named rather than inferred, and they are not in it. The same refusal a
// password login gives, from the entry point a provider uses — which used to
// answer by joining them to it instead.
func TestSignInIdentityRefusesATenantYouAreNotIn(t *testing.T) {
	t.Parallel()

	f := setup(t)
	res, err := f.signInIdentity(account.SignInIdentityInput{TenantID: uuid.New()})
	if rigerr.CodeOf(err) != rigerr.CodeForbidden {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if res.Session != nil {
		t.Error("nothing should have been issued")
	}
	if e, ok := f.log.last(authlog.EventLoginFailed); !ok || e.Detail["reason"] != "no account in this tenant" {
		t.Errorf("entry = %v, want the reason in the detail", e.Detail)
	}
}

// The gates that used to live only on the password path. A provider link carries
// no deleted_at and no is_active, so without these two a soft-deleted or
// disabled person keeps a way in for as long as their Google account exists.
func TestSignInIdentityRefusesAnIdentityThatIsGone(t *testing.T) {
	t.Parallel()

	f := setup(t)
	_, err := f.signInIdentity(account.SignInIdentityInput{IdentityID: uuid.New()})
	if rigerr.CodeOf(err) != rigerr.CodeForbidden {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if e, ok := f.log.last(authlog.EventLoginFailed); !ok || e.Detail["reason"] != "no such identity" {
		t.Errorf("entry = %v, want the reason in the detail", e.Detail)
	}
}

func TestSignInIdentityRefusesADisabledIdentity(t *testing.T) {
	t.Parallel()

	f := setup(t)
	f.store.DeactivateIdentity(f.ident.ID)

	if _, err := f.signInIdentity(account.SignInIdentityInput{}); rigerr.CodeOf(err) != rigerr.CodeForbidden {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if e, ok := f.log.last(authlog.EventLoginFailed); !ok || e.Detail["reason"] != "identity disabled" {
		t.Errorf("entry = %v, want the reason in the detail", e.Detail)
	}
}

func TestSignInIdentityRefusesADisabledAccountAndAServiceAccount(t *testing.T) {
	t.Parallel()

	for name, reason := range map[string]string{
		"disabled":        "account disabled",
		"service account": "service account",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			f := setup(t)
			if name == "disabled" {
				f.acct.IsActive = false
			} else {
				f.acct.Kind = account.KindService
			}
			f.store.Put(f.acct)

			// Named, because a tenant that was inferred would have skipped a
			// disabled account rather than refusing it.
			_, err := f.signInIdentity(account.SignInIdentityInput{TenantID: f.tenant})
			if rigerr.CodeOf(err) != rigerr.CodeForbidden {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if e, ok := f.log.last(authlog.EventLoginFailed); !ok || e.Detail["reason"] != reason {
				t.Errorf("entry = %v, want %q", e.Detail, reason)
			}
		})
	}
}

// RequireVerifiedEmail is what this door is held to, and the code door is not —
// because typing the code *is* the confirmation, so gating it would refuse
// somebody on a column their own request had just filled in.
func TestSignInIdentityAppliesTheVerifiedEmailGate(t *testing.T) {
	t.Parallel()

	f := setupWith(t, func(c *account.Config) { c.RequireVerifiedEmail = true })

	_, err := f.signInIdentity(account.SignInIdentityInput{Method: "Google"})
	if rigerr.CodeOf(err) != rigerr.CodeForbidden {
		t.Fatalf("err = %v, want the verified-email refusal", err)
	}
	if e, ok := f.log.last(authlog.EventLoginFailed); !ok || e.Detail["reason"] != "email not verified" {
		t.Errorf("entry = %v, want the reason recorded", e.Detail)
	}

	// And the refusal is about the column rather than about the method: verify
	// the address and the same provider sign-in goes through.
	f.markVerified(t)
	if _, err := f.signInIdentity(account.SignInIdentityInput{Method: "Google"}); err != nil {
		t.Fatalf("a verified address should sign in: %v", err)
	}
}

// What the audit trail gains, and it is the gap that made a provider sign-in
// untraceable: OAuthSignIn carries no TokenRootID, so nothing linked a sign-in
// to the session family it created.
func TestSignInIdentityRecordsTheMethodAndTheSession(t *testing.T) {
	t.Parallel()

	f := setup(t)
	res, err := f.signInIdentity(account.SignInIdentityInput{Method: "Google"})
	if err != nil {
		t.Fatal(err)
	}

	e, ok := f.log.last(authlog.EventLoginSucceeded)
	if !ok {
		t.Fatal("no success recorded")
	}
	if e.Detail["method"] != "Google" {
		t.Errorf("detail = %v, want the method", e.Detail)
	}
	if e.TokenRootID == nil || *e.TokenRootID != res.Session.RootTokenID {
		t.Error("the entry should name the session family it created")
	}
	if e.EmailAddress != "sam@example.com" {
		t.Errorf("address = %q, want the identity's, lowercased", e.EmailAddress)
	}
}

// A code sign-in names its method too, so that a trail with both doors in it
// says which one each row came through.
func TestACodeSignInRecordsItsMethod(t *testing.T) {
	t.Parallel()

	f := setup(t)
	if _, err := f.login(); err != nil {
		t.Fatal(err)
	}
	e, _ := f.log.last(authlog.EventLoginSucceeded)
	if e.Detail["method"] != "EmailCode" {
		t.Errorf("detail = %v, want the method", e.Detail)
	}
}

// The two consequences of writing LoginSucceeded and LoginFailed rather than an
// event of its own, both deliberate and both surprising enough to pin.
//
// Clearing is safe: it takes control of the provider account, which is not
// something somebody guessing a code has. And having no limit of its own is
// right, because there is no secret being guessed — but it means the bound on
// calling this comes from the caller and nowhere else.
func TestAProviderSignInAndTheLockout(t *testing.T) {
	t.Parallel()

	f := setupWith(t, func(c *account.Config) {
		c.Limits = throttle.Standard()
		c.Limits.EmailCodeRequest.Max = 50
		c.Limits.EmailCodeByIP.Max = 50
	})
	for range 6 {
		code, err := f.askForCode()
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.verify(wrongCode(code))
	}
	if _, err := f.login(); err == nil {
		t.Fatal("the address should be locked out")
	}

	// Not bounded by it.
	if _, err := f.signInIdentity(account.SignInIdentityInput{Method: "Google"}); err != nil {
		t.Fatalf("a provider sign-in has no lockout of its own: %v", err)
	}
	// And it cleared it, because it is a LoginSucceeded.
	if _, err := f.login(); err != nil {
		t.Errorf("a provider sign-in should lift the code lockout: %v", err)
	}
}

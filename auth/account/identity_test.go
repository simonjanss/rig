package account_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// join gives the fixture's person a second account, in another tenant, the way
// an invitation would.
func (f *fixture) join(t *testing.T, tenantID uuid.UUID) *account.Account {
	t.Helper()

	a := &account.Account{
		ID: uuid.New(), TenantID: tenantID, DisplayName: "Sam", IsActive: true,
		EmailAddress: f.ident.EmailAddress, IdentityID: &f.ident.ID,
	}
	f.store.Put(a)
	return a
}

// The whole reason for the split: one address, one password, two tenants.
func TestOnePasswordSignsInToEveryTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	other := uuid.New()
	elsewhere := f.join(t, other)

	here, err := f.login(goodPassword)
	if err != nil {
		t.Fatal(err)
	}
	thereRes, err := f.svc.Login(context.Background(), account.LoginInput{
		TenantID: other, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatalf("the same password should sign in to the other tenant: %v", err)
	}
	there := thereRes.Session

	// Two different sessions, each belonging to the account in its own tenant.
	// The person is one person; the claims are not.
	mine, err := f.sessions.Verify(context.Background(), here.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := f.sessions.Verify(context.Background(), there.Access.Token)
	if err != nil {
		t.Fatal(err)
	}

	if mine.AccountID != f.acct.ID || mine.TenantID != f.tenant {
		t.Error("the first session should belong to the account in the first tenant")
	}
	if theirs.AccountID != elsewhere.ID || theirs.TenantID != other {
		t.Error("the second session should belong to the account in the second tenant")
	}
}

// Somebody real, in the wrong place. It has to be told apart from a wrong
// password — they proved who they are, so 403 gives nothing away — and it must
// not be told apart before the password is checked.
func TestSigningInToATenantYouDoNotBelongToIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t)
	stranger := uuid.New()

	_, err := f.svc.Login(context.Background(), account.LoginInput{
		TenantID: stranger, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if !rigerr.Is(err, rigerr.CodeForbidden) {
		t.Errorf("err = %v, want 403", err)
	}

	// The wrong password against the same tenant is still 401, so the response
	// cannot be used to find out which tenants somebody belongs to without
	// already knowing their password.
	_, err = f.svc.Login(context.Background(), account.LoginInput{
		TenantID: stranger, EmailAddress: "sam@example.com", Password: "not the password",
		IPAddress: "203.0.113.10",
	})
	if !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want 401", err)
	}
}

// One password covers every tenant, so changing it has to end the sessions in
// tenants the request was not made from. Anything less leaves a thief signed in
// to the tenant the person was not looking at.
func TestChangingAPasswordEndsSessionsInEveryTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	other := uuid.New()
	f.join(t, other)

	elsewhereRes, err := f.svc.Login(context.Background(), account.LoginInput{
		TenantID: other, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	elsewhere := elsewhereRes.Session
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.ChangePassword(context.Background(), account.ChangePasswordInput{
		TenantID:        f.tenant,
		AccountID:       f.acct.ID,
		CurrentPassword: goodPassword,
		NewPassword:     "an entirely different passphrase",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.sessions.Verify(context.Background(), elsewhere.Access.Token); err == nil {
		t.Error("the session in the other tenant should have been revoked too")
	}
}

// A reset is the same rule, reached the other way: the link is about the person,
// so it cannot leave one of their tenants signed in.
func TestAResetEndsSessionsInEveryTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	other := uuid.New()
	f.join(t, other)

	elsewhereRes, err := f.svc.Login(context.Background(), account.LoginInput{
		TenantID: other, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	elsewhere := elsewhereRes.Session
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	if err := f.svc.RequestPasswordReset(ctx, f.tenant, "sam@example.com", ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.ConfirmPasswordReset(ctx, f.notify.reset, "a brand new passphrase", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := f.sessions.Verify(ctx, elsewhere.Access.Token); err == nil {
		t.Error("the session in the other tenant should have been revoked too")
	}
}

// Somebody who works at two of your customers is one person. Provisioning them
// into a second tenant must reuse the identity, or they end up with two
// passwords and no idea which is which.
func TestProvisioningReusesAnExistingPerson(t *testing.T) {
	t.Parallel()

	f := setup(t)
	other := uuid.New()

	acct, err := f.svc.Provision(context.Background(), account.ProvisionInput{
		TenantID: other, EmailAddress: "Sam@Example.com", DisplayName: "Sam",
	})
	if err != nil {
		t.Fatal(err)
	}
	if acct.IdentityID == nil || *acct.IdentityID != f.ident.ID {
		t.Fatal("the new account should belong to the person who already had that address")
	}

	// And the password they already have works in the new tenant immediately,
	// without anything being set for it.
	if _, err := f.svc.Login(context.Background(), account.LoginInput{
		TenantID: other, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	}); err != nil {
		t.Errorf("their existing password should work in the new tenant: %v", err)
	}
}

// Twice into the same tenant is a conflict, and it is the identity that decides
// so rather than the copied address.
func TestProvisioningTwiceIntoOneTenantConflicts(t *testing.T) {
	t.Parallel()

	f := setup(t)

	_, err := f.svc.Provision(context.Background(), account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "sam@example.com",
	})
	if !rigerr.Is(err, rigerr.CodeConflict) {
		t.Errorf("err = %v, want 409", err)
	}
}

// A service account is nobody: no identity, so no address in the global space
// and nothing to sign in with.
func TestAServiceAccountHasNoIdentity(t *testing.T) {
	t.Parallel()

	f := setup(t)

	acct, err := f.svc.Provision(context.Background(), account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "reports@service.example.com",
		DisplayName: "Reports", Kind: account.KindService,
	})
	if err != nil {
		t.Fatal(err)
	}
	if acct.IdentityID != nil {
		t.Error("a service account should have no identity")
	}
	if acct.Person() {
		t.Error("a service account is not a person")
	}

	// Nothing was added to the global address space, so the same integration
	// name is free in every other tenant.
	ident, err := f.store.FindIdentityByEmail(context.Background(), "reports@service.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ident != nil {
		t.Error("provisioning a service account should not create a person")
	}
}

// Disabling somebody globally is not the same as removing them from one tenant,
// and the identity's flag is the one that stops them everywhere.
func TestADisabledIdentityCannotSignInAnywhere(t *testing.T) {
	t.Parallel()

	f := setup(t)
	other := uuid.New()
	f.join(t, other)

	f.ident.IsActive = false
	f.store.PutIdentity(f.ident)

	for _, tenantID := range []uuid.UUID{f.tenant, other} {
		_, err := f.svc.Login(context.Background(), account.LoginInput{
			TenantID: tenantID, EmailAddress: "sam@example.com", Password: goodPassword,
			IPAddress: "203.0.113.10",
		})
		if !rigerr.Is(err, rigerr.CodeForbidden) {
			t.Errorf("tenant %s: err = %v, want 403", tenantID, err)
		}
	}
}

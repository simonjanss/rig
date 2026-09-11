package account_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/session"
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

// signInTo signs the fixture's person in to a named tenant, with a code.
func (f *fixture) signInTo(tenantID uuid.UUID) (*session.Pair, error) {
	code, err := f.askForCode()
	if err != nil {
		return nil, err
	}
	res, err := f.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
		TenantID: tenantID, EmailAddress: "sam@example.com", Code: code,
		Client: session.ClientWeb, IPAddress: "203.0.113.10",
	})
	if err != nil {
		return nil, err
	}
	if res.Session == nil {
		return nil, errors.New("no session for the tenant that was named")
	}
	return res.Session, nil
}

// The whole reason for the split: one address, one person, two tenants.
func TestOneAddressSignsInToEveryTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	other := uuid.New()
	elsewhere := f.join(t, other)

	here, err := f.login()
	if err != nil {
		t.Fatal(err)
	}
	there, err := f.signInTo(other)
	if err != nil {
		t.Fatalf("the same address should sign in to the other tenant: %v", err)
	}

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

// Somebody real, in the wrong place. It has to be told apart from a wrong code
// — they proved who they are, so 403 gives nothing away — and it must not be
// told apart before the code is compared.
func TestSigningInToATenantYouDoNotBelongToIsRefused(t *testing.T) {
	t.Parallel()

	f := setup(t)
	stranger := uuid.New()

	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}

	// The wrong code against that tenant is 401, so the response cannot be used
	// to find out which tenants somebody belongs to without already holding
	// their code. Checked first, because the right one consumes.
	_, err = f.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
		TenantID: stranger, EmailAddress: "sam@example.com", Code: wrongCode(code),
		IPAddress: "203.0.113.10",
	})
	if !rigerr.Is(err, rigerr.CodeUnauthorized) {
		t.Errorf("err = %v, want 401", err)
	}

	code, err = f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
		TenantID: stranger, EmailAddress: "sam@example.com", Code: code,
		IPAddress: "203.0.113.10",
	})
	if !rigerr.Is(err, rigerr.CodeForbidden) {
		t.Errorf("err = %v, want 403", err)
	}
}

// A person is global and their sessions are not, so "sign me out everywhere"
// has to reach the tenants the request was not made from.
//
// It used to be reached by changing a password, which was the only thing that
// could mean it. There is no credential to change, so the operation is exported
// and the decision to use it is the application's.
func TestRevokeEverySessionReachesEveryTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()
	other := uuid.New()
	f.join(t, other)

	here, err := f.login()
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := f.signInTo(other)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.svc.RevokeEverySession(ctx, f.ident.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := f.sessions.Verify(ctx, here.Access.Token); err == nil {
		t.Error("the session here should have been revoked")
	}
	if _, err := f.sessions.Verify(ctx, elsewhere.Access.Token); err == nil {
		t.Error("the session in the other tenant should have been revoked too")
	}
}

// Somebody who works at two of your customers is one person. Provisioning them
// into a second tenant must reuse the identity, or they end up with two of
// everything and no idea which is which.
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

	// And they can sign in to the new tenant immediately, with nothing set up
	// for it: the address is the person, and the person is already here.
	if _, err := f.signInTo(other); err != nil {
		t.Errorf("they should be able to sign in to the new tenant: %v", err)
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
		// Asking for the code still works — the endpoint answers the same for
		// everybody — and typing it back is where the refusal lands.
		code, err := f.askForCode()
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
			TenantID: tenantID, EmailAddress: "sam@example.com", Code: code,
			IPAddress: "203.0.113.10",
		})
		if !rigerr.Is(err, rigerr.CodeForbidden) {
			t.Errorf("tenant %s: err = %v, want 403", tenantID, err)
		}
	}
}

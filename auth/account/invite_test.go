package account_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// The flow an invitation exists for: somebody with no account here is invited,
// clicks, sets a password, and is signed in — one round trip, not a verification
// link followed by a password reset for a password they never had.
func TestAnInvitationBringsSomebodyIn(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	acct, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
		DisplayName: "Grace", Role: account.RoleAdmin, Invite: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.notify.invite == "" {
		t.Fatal("an invitation should have been handed to the notifier")
	}
	// It goes with the account, because an invitation is about one tenant and a
	// mail that cannot say which tenant is a mail nobody can act on.
	if f.notify.inviteTo == nil || f.notify.inviteTo.ID != acct.ID {
		t.Error("the invitation should name the account it is for")
	}
	if _, ok := f.log.last(authlog.EventInvitationSent); !ok {
		t.Error("sending an invitation should be recorded")
	}

	// A verification link is not what was sent: the difference is the whole
	// point, and redeeming it as one must not work.
	if err := f.svc.VerifyEmail(ctx, f.notify.invite); err == nil {
		t.Error("an invitation should not be redeemable as an email verification")
	}

	const chosen = "a password grace picked herself"
	pair, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite, Password: chosen, IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Signed in, as the account the invitation was for.
	tok, err := f.sessions.Verify(ctx, pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != acct.ID || tok.TenantID != f.tenant {
		t.Error("the session should be for the invited account")
	}

	// The address is confirmed, because the link is the proof: it went there and
	// came back.
	ident, err := f.store.FindIdentityByEmail(ctx, "grace@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ident.Verified() {
		t.Error("redeeming an invitation should confirm the address")
	}

	// And the password works from the front door.
	if _, err := f.svc.Login(ctx, account.LoginInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", Password: chosen,
		IPAddress: "203.0.113.10",
	}); err != nil {
		t.Errorf("the password set by the invitation should sign in: %v", err)
	}

	if _, ok := f.log.last(authlog.EventInvitationAccepted); !ok {
		t.Error("accepting an invitation should be recorded")
	}
}

// Somebody who already signs in here is joining a second tenant. Their
// password is not this invitation's business, and quietly replacing it would let
// an invitation to any tenant change the credential for all of them.
func TestAnInvitationToASecondTenantKeepsTheExistingPassword(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	other := uuid.New()
	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: other, EmailAddress: "sam@example.com", Invite: true,
	}); err != nil {
		t.Fatal(err)
	}

	// A password is offered and must be ignored.
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite, Password: "an attempt to change the password",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.Login(ctx, account.LoginInput{
		TenantID: other, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	}); err != nil {
		t.Errorf("the original password should still be the password: %v", err)
	}
	if _, err := f.svc.Login(ctx, account.LoginInput{
		TenantID: other, EmailAddress: "sam@example.com",
		Password: "an attempt to change the password", IPAddress: "203.0.113.10",
	}); err == nil {
		t.Error("the password offered with the invitation should not have been set")
	}
}

func TestAnInvitationIsSingleUse(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", Invite: true,
	}); err != nil {
		t.Fatal(err)
	}
	token := f.notify.invite

	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: token, Password: "the first password chosen",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: token, Password: "a second attempt entirely",
	}); err == nil {
		t.Error("a link forwarded to somebody else is not a second invitation")
	}
}

// A first password has to satisfy the policy. Somebody joining should not be the
// one account with a four-character password.
func TestAnInvitationEnforcesThePasswordPolicy(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", Invite: true,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite, Password: "short",
	}); err == nil {
		t.Fatal("a short first password should be refused")
	}

	// And the link survives, because nothing was consumed: being told the
	// password is too short and then finding the link dead would be a dead end.
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite, Password: "a long enough password now",
	}); err != nil {
		t.Errorf("the link should still work after a refused password: %v", err)
	}
}

// Switching is for a tenant you already belong to. It needs no password —
// they have proved who they are — and it must refuse everything else.
func TestSwitchingTenants(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	other := uuid.New()
	f.store.TenantNames = map[uuid.UUID]string{f.tenant: "Here", other: "Elsewhere"}
	joined, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: other, EmailAddress: "sam@example.com", Role: account.RoleBasic,
	})
	if err != nil {
		t.Fatal(err)
	}

	spaces, err := f.svc.Tenants(ctx, f.tenant, f.acct.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 2 {
		t.Fatalf("%d tenants, want both", len(spaces))
	}

	pair, err := f.svc.Switch(ctx, account.SwitchInput{
		TenantID: f.tenant, AccountID: f.acct.ID, ToTenantID: other,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := f.sessions.Verify(ctx, pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.TenantID != other || tok.AccountID != joined.ID {
		t.Error("the new session should be for the other tenant's account")
	}
	if _, ok := f.log.last(authlog.EventTenantSwitched); !ok {
		t.Error("switching should be recorded")
	}

	// A tenant they do not belong to is refused, which is the only thing between
	// a switcher and every customer's data.
	if _, err := f.svc.Switch(ctx, account.SwitchInput{
		TenantID: f.tenant, AccountID: f.acct.ID, ToTenantID: uuid.New(),
	}); !rigerr.Is(err, rigerr.CodeForbidden) {
		t.Errorf("switching to a stranger's tenant: err = %v, want 403", err)
	}
}

// Withdrawing an invitation kills the link and removes the account it was for.
//
// Both halves, because either alone leaves something wrong: a live link with no
// account, or an account nobody can use that also blocks a second invitation.
func TestRevokingAnInvitation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
		DisplayName: "Grace", Invite: true,
	}); err != nil {
		t.Fatal(err)
	}
	token := f.notify.invite

	pending, err := f.svc.Invitations(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d pending invitations, want 1", len(pending))
	}
	if pending[0].EmailAddress != "grace@example.com" {
		t.Errorf("the list should name the person: %+v", pending[0])
	}

	by := f.acct.ID
	if err := f.svc.RevokeInvitation(ctx, account.RevokeInput{
		TenantID: f.tenant, InvitationID: pending[0].ID, ByAccountID: &by,
	}); err != nil {
		t.Fatal(err)
	}

	// The link is dead.
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: token, Password: "a password grace chose",
	}); err == nil {
		t.Error("a withdrawn invitation should not be redeemable")
	}

	// It is off the list, and it is recorded as withdrawn rather than used.
	if after, _ := f.svc.Invitations(ctx, f.tenant); len(after) != 0 {
		t.Errorf("%d pending invitations after revoking, want 0", len(after))
	}
	if _, ok := f.log.last(authlog.EventInvitationRevoked); !ok {
		t.Error("withdrawing should be recorded")
	}
	if _, ok := f.log.last(authlog.EventInvitationAccepted); ok {
		t.Error("withdrawn is not accepted; the trail has to tell them apart")
	}

	// And the same person can be invited again, which is the thing that does not
	// work if the account is left behind.
	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", Invite: true,
	}); err != nil {
		t.Errorf("re-inviting after a withdrawal should work: %v", err)
	}
}

// The rule that keeps this from being a way to delete a colleague: once somebody
// has accepted, their account is theirs.
func TestRevokingCannotRemoveSomebodyWhoAccepted(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", Invite: true,
	}); err != nil {
		t.Fatal(err)
	}

	pending, err := f.svc.Invitations(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	invitation := pending[0].ID

	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite, Password: "a password grace chose",
	}); err != nil {
		t.Fatal(err)
	}

	// Somebody withdraws it a moment too late. It is no longer pending, so there
	// is nothing to withdraw — and crucially the account survives.
	err = f.svc.RevokeInvitation(ctx, account.RevokeInput{
		TenantID: f.tenant, InvitationID: invitation,
	})
	if !rigerr.Is(err, rigerr.CodeNotFound) {
		t.Errorf("err = %v, want NotFound", err)
	}

	if _, err := f.svc.Login(ctx, account.LoginInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
		Password: "a password grace chose", IPAddress: "203.0.113.10",
	}); err != nil {
		t.Errorf("she accepted, so she is a member: %v", err)
	}
}

// An invitation belongs to one tenant, and naming another tenant's has to answer
// the same way as naming one that does not exist.
func TestRevokingAnotherTenantsInvitation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", Invite: true,
	}); err != nil {
		t.Fatal(err)
	}
	pending, _ := f.svc.Invitations(ctx, f.tenant)

	err := f.svc.RevokeInvitation(ctx, account.RevokeInput{
		TenantID: uuid.New(), InvitationID: pending[0].ID,
	})
	if !rigerr.Is(err, rigerr.CodeNotFound) {
		t.Errorf("err = %v, want NotFound", err)
	}

	// Untouched.
	if after, _ := f.svc.Invitations(ctx, f.tenant); len(after) != 1 {
		t.Error("another tenant should not be able to withdraw it")
	}
}

// Signing in without naming a tenant, which is what a single sign-in page
// needs: nobody knows which tenants an address belongs to until the password
// has been checked, so asking first asks a question the visitor cannot answer.
func TestSigningInWithoutATenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	pair, err := f.signInAs(ctx, account.LoginInput{
		EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}

	tok, err := f.sessions.Verify(ctx, pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.TenantID != f.tenant || tok.AccountID != f.acct.ID {
		t.Error("the session should be for the one tenant they belong to")
	}

	// With a second tenant it lands on the oldest, and the rest are one switch
	// away — predictable beats clever when the interface shows them all as tabs.
	second := uuid.New()
	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: second, EmailAddress: "sam@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	pair, err = f.signInAs(ctx, account.LoginInput{
		EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := f.sessions.Verify(ctx, pair.Access.Token); tok.TenantID != f.tenant {
		t.Error("it should land on the tenant they joined first")
	}

	// And naming one still works, which is what a subdomain deployment does.
	pair, err = f.signInAs(ctx, account.LoginInput{
		TenantID: second, EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := f.sessions.Verify(ctx, pair.Access.Token); tok.TenantID != second {
		t.Error("a named tenant should still be honoured")
	}
}

// Somebody real who belongs nowhere signs in successfully and lands in the
// picker.
//
// This used to be a 403, and that made the flow it exists for impossible: an
// invitation waiting to be accepted is a perfectly good reason to have an account
// and no tenant, and somebody who cannot sign in cannot accept it. What they
// get instead is the tenant-less credential and an empty list — proof of who they
// are, and nowhere to be yet.
func TestSigningInBelongingToNoTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	// Their only account is removed, the way withdrawing an invitation does it.
	if err := f.store.SoftDeleteAccount(ctx, account.DeleteAccountInput{
		TenantID: f.tenant, AccountID: f.acct.ID,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := f.svc.Login(ctx, account.LoginInput{
		EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatalf("signing in with no tenant should succeed: %v", err)
	}
	if res.Session != nil {
		t.Error("there is no tenant to be in, so there should be no session")
	}
	if len(res.Tenants) != 0 {
		t.Errorf("tenants = %v, want none", res.Tenants)
	}
	if res.Identity.Token == "" {
		t.Fatal("the tenant-less credential is the whole point of this state")
	}

	// And it is a credential: it resolves to the person, and to nothing else.
	who, err := f.identities.Verify(ctx, res.Identity.Token)
	if err != nil {
		t.Fatal(err)
	}
	if who.IdentityID != f.ident.ID {
		t.Errorf("the identity session is for %s, want %s", who.IdentityID, f.ident.ID)
	}

	// Naming a tenant they are not in is still a refusal. They asked for
	// somewhere specific.
	_, err = f.svc.Login(ctx, account.LoginInput{
		TenantID:     f.tenant,
		EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if !rigerr.Is(err, rigerr.CodeForbidden) {
		t.Fatalf("err = %v, want 403", err)
	}
	if !strings.Contains(err.Error(), "this tenant") {
		t.Errorf("the message should be about the tenant they named: %v", err)
	}
}

// A failed sign-in that named no tenant still has to be recorded, because the
// lockout counts these rows: dropping them would leave the attempts that most
// need a rate limit with none.
func TestATenantlessFailureIsStillCounted(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	for range 6 {
		_, _ = f.svc.Login(ctx, account.LoginInput{
			EmailAddress: "sam@example.com", Password: "not the password",
			IPAddress: "203.0.113.10",
		})
	}
	if n := f.log.count(authlog.EventLoginFailed); n < 5 {
		t.Fatalf("%d failures recorded, want them all", n)
	}

	// And the lockout arrives, which is the whole point of recording them.
	_, err := f.svc.Login(ctx, account.LoginInput{
		EmailAddress: "sam@example.com", Password: goodPassword,
		IPAddress: "203.0.113.10",
	})
	if !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Errorf("err = %v, want 429 — the failures should have locked it", err)
	}
}

// signInAs signs in and hands back the tenant session, for the tests that are
// about which tenant a sign-in lands in rather than about the picker.
func (f *fixture) signInAs(ctx context.Context, in account.LoginInput) (session.Pair, error) {
	res, err := f.svc.Login(ctx, in)
	if err != nil {
		return session.Pair{}, err
	}
	if res.Session == nil {
		return session.Pair{}, errors.New("signed in with no tenant")
	}
	return *res.Session, nil
}

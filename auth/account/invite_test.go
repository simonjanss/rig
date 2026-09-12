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
)

// The claim the whole flow turns on: an invitation is a pending membership, so
// until somebody accepts it the tenant has not gained a person.
func TestAnInvitationIsNotAMembership(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()
	f.store.TenantNames = map[uuid.UUID]string{f.tenant: "Skolan i Solna"}

	by := f.acct.ID
	inv, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
		DisplayName: "Grace", Role: account.RoleAdmin, ByAccountID: &by,
	})
	if err != nil {
		t.Fatal(err)
	}
	if f.notify.invite == "" {
		t.Fatal("an invitation should have been handed to the notifier")
	}
	// It goes with the invitation, because a mail that cannot say which tenant
	// is a mail nobody can act on — and there is no account to carry that yet.
	if f.notify.inviteTo == nil || f.notify.inviteTo.TenantName != "Skolan i Solna" {
		t.Errorf("the mail should be able to name the tenant: %+v", f.notify.inviteTo)
	}
	if _, ok := f.log.last(authlog.EventInvitationSent); !ok {
		t.Error("sending an invitation should be recorded")
	}

	// The person exists, because a row has to hang off somebody.
	ident, err := f.store.FindIdentityByEmail(ctx, "grace@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if ident == nil {
		t.Fatal("inviting an unknown address should create the person")
	}

	// And they are in no tenant at all. This is the assertion #164 is about.
	acct, err := f.store.AccountForIdentity(ctx, f.tenant, ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acct != nil {
		t.Error("inviting somebody must not put them in the tenant")
	}
	if spaces, _ := f.svc.MyTenants(ctx, ident.ID); len(spaces) != 0 {
		t.Errorf("MyTenants = %v, want none until she accepts", spaces)
	}

	// What she can see is the invitation, in her own listing and in the
	// administrator's.
	mine, err := f.svc.MyInvitations(ctx, ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].TenantName != "Skolan i Solna" {
		t.Fatalf("her listing = %+v", mine)
	}
	pending, err := f.svc.Invitations(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != inv.ID {
		t.Fatalf("the tenant's listing = %+v", pending)
	}
	if pending[0].InvitedByName != "Sam" {
		t.Errorf("invitedBy = %q, want the inviter's name", pending[0].InvitedByName)
	}

	// A verification link is not what was sent: the difference is the whole
	// point, and redeeming it as one must not work.
	if err := f.svc.VerifyEmail(ctx, f.notify.invite); err == nil {
		t.Error("an invitation should not be redeemable as an email verification")
	}

	// Accepting is what makes her a member.
	pair, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite, IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}

	made, err := f.store.AccountForIdentity(ctx, f.tenant, ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if made == nil {
		t.Fatal("accepting should have created the account")
	}
	if made.Role != account.RoleAdmin {
		t.Errorf("role = %s, want the one the inviter chose", made.Role)
	}
	if made.DisplayName != "Grace" {
		t.Errorf("display name = %q, want the one the inviter chose", made.DisplayName)
	}
	// The provenance survives the invitation being consumed, which is the
	// reason it is copied onto the row rather than read back through the link.
	if made.CreatedBy == nil || *made.CreatedBy != by {
		t.Errorf("created_by = %v, want the inviter", made.CreatedBy)
	}

	tok, err := f.sessions.Verify(ctx, pair.Access.Token)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccountID != made.ID {
		t.Error("the session should be for the account that was just created")
	}
	if _, ok := f.log.last(authlog.EventInvitationAccepted); !ok {
		t.Error("accepting should be recorded")
	}
	// Accepting creates an account, so it writes the event that says one came
	// into existence — the same one Provision writes.
	if _, ok := f.log.last(authlog.EventAccountProvisioned); !ok {
		t.Error("an account came into existence and the trail should say so")
	}

	// It also confirms the address: the link went there and came back.
	after, _ := f.store.FindIdentityByID(ctx, ident.ID)
	if !after.Verified() {
		t.Error("following the link should have confirmed the address")
	}

	// And the invitation is off both listings.
	if pending, _ := f.svc.Invitations(ctx, f.tenant); len(pending) != 0 {
		t.Errorf("%d pending after accepting, want 0", len(pending))
	}
}

// Provision is the other reading, and the two now answer differently — which is
// what the flag on one function could never do.
func TestProvisionAddsAMemberAndInviteDoesNot(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	added, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "added@example.com", DisplayName: "Added",
	})
	if err != nil {
		t.Fatal(err)
	}
	if spaces, _ := f.svc.MyTenants(ctx, *added.IdentityID); len(spaces) != 1 {
		t.Errorf("provisioning should put somebody in the tenant now: %v", spaces)
	}
	if f.notify.invite != "" {
		t.Error("Provision mails nothing; telling them is the application's")
	}

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "asked@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	asked, _ := f.store.FindIdentityByEmail(ctx, "asked@example.com")
	if spaces, _ := f.svc.MyTenants(ctx, asked.ID); len(spaces) != 0 {
		t.Errorf("inviting should put nobody anywhere: %v", spaces)
	}
}

// Empty falls back to the name the person already has rather than to a guess
// made from their address: somebody who signs in here has told you their name.
func TestAnInvitationWithNoNameKeepsTheirOwn(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	other := uuid.New()
	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: other, EmailAddress: "sam@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite,
	}); err != nil {
		t.Fatal(err)
	}

	made, err := f.store.AccountForIdentity(ctx, other, f.ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if made.DisplayName != "Sam" {
		t.Errorf("display name = %q, want the name she already had", made.DisplayName)
	}
	if made.Role != account.RoleBasic {
		t.Errorf("role = %s, want the default", made.Role)
	}
}

// Joining a second tenant adds that tenant and changes nothing about the first.
func TestAnInvitationToASecondTenantAddsOnlyThatTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()
	f.acct.Role = account.RoleOwner
	f.store.Put(f.acct)

	other := uuid.New()
	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: other, EmailAddress: "sam@example.com", Role: account.RoleBasic,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite,
	}); err != nil {
		t.Fatal(err)
	}

	spaces, err := f.svc.MyTenants(ctx, f.ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(spaces) != 2 {
		t.Fatalf("%d tenants, want both", len(spaces))
	}

	// The role is per account, so being Basic in the new tenant leaves her an
	// Owner in the old one.
	here, _ := f.store.AccountForIdentity(ctx, f.tenant, f.ident.ID)
	if here.Role != account.RoleOwner {
		t.Errorf("role here = %s, want it untouched", here.Role)
	}
	there, _ := f.store.AccountForIdentity(ctx, other, f.ident.ID)
	if there.Role != account.RoleBasic {
		t.Errorf("role there = %s", there.Role)
	}
}

// Inviting the same person again supersedes the link rather than conflicting
// with it, which is what makes "send that again" something an administrator can
// just do.
func TestInvitingSomebodyTwiceSupersedesTheFirstLink(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	first := f.notify.invite

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", Role: account.RoleAdmin,
	}); err != nil {
		t.Fatalf("re-inviting should work: %v", err)
	}
	second := f.notify.invite
	if first == second {
		t.Fatal("the second invitation should carry a new link")
	}

	// One row, not two, or the listing grows every time somebody clicks resend.
	if pending, _ := f.svc.Invitations(ctx, f.tenant); len(pending) != 1 {
		t.Errorf("%d pending invitations, want 1", len(pending))
	}

	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: first}); err == nil {
		t.Error("the superseded link should be dead")
	}
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: second}); err != nil {
		t.Errorf("the newest link should work: %v", err)
	}

	ident, _ := f.store.FindIdentityByEmail(ctx, "grace@example.com")
	made, _ := f.store.AccountForIdentity(ctx, f.tenant, ident.ID)
	if made.Role != account.RoleAdmin {
		t.Errorf("role = %s, want the second invitation's", made.Role)
	}
}

// Inviting a colleague who already works here is a mistake worth naming.
func TestInvitingSomebodyWhoIsAlreadyHere(t *testing.T) {
	t.Parallel()

	f := setup(t)
	_, err := f.svc.Invite(context.Background(), account.InviteInput{
		TenantID: f.tenant, EmailAddress: "sam@example.com",
	})
	if !rigerr.Is(err, rigerr.CodeConflict) {
		t.Errorf("err = %v, want 409", err)
	}
}

// Two clicks on the same link, or a link forwarded on.
func TestAnInvitationIsSingleUse(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	token := f.notify.invite

	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: token}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: token}); err == nil {
		t.Error("a second use should be refused")
	}

	// And exactly one account came out of it.
	ident, _ := f.store.FindIdentityByEmail(ctx, "grace@example.com")
	accts, err := f.store.AccountsForIdentity(ctx, ident.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 1 {
		t.Errorf("%d accounts, want 1", len(accts))
	}
}

// Somebody added by another route between the invitation and the click. The
// link said it would put them in this tenant and they are in it, so refusing
// would be refusing on a detail they cannot see.
func TestAcceptingWhenAlreadyAMember(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	other := uuid.New()
	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: other, EmailAddress: "sam@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	joined, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: other, EmailAddress: "sam@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	pair, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: f.notify.invite})
	if err != nil {
		t.Fatalf("accepting should still work: %v", err)
	}
	if tok, _ := f.sessions.Verify(ctx, pair.Access.Token); tok.AccountID != joined.ID {
		t.Error("the session should be for the account they already had")
	}
	e, ok := f.log.last(authlog.EventInvitationAccepted)
	if !ok {
		t.Fatal("accepting should be recorded")
	}
	if e.Detail["already_a_member"] != true {
		t.Errorf("the trail should say no account was created: %+v", e.Detail)
	}
}

// The hook is inside the transaction, so a failure leaves nothing behind — not a
// member with half a setup, and not a spent link.
func TestOnJoinedRollsBackTheWholeAcceptance(t *testing.T) {
	t.Parallel()

	boom := errors.New("the application said no")
	f := setupWith(t, func(c *account.Config) {
		c.OnJoined = func(context.Context, account.Joined) error { return boom }
	})
	ctx := context.Background()

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	token := f.notify.invite

	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: token}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the hook's", err)
	}

	// The memory store has no transactions to roll back — that half is the
	// docker suite's, over real SQL — but the failure has to come back as the
	// hook's own error rather than something laundered, and no session may be
	// issued for a member the application refused.
	open, err := f.sessions.ListTenant(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Errorf("%d sessions, want none for somebody who did not join", len(open))
	}
}

// Everything OnJoined needs to seed a new member, which is the work that used
// to be the caller's next line after Provision.
func TestOnJoinedSeesWhoJustJoined(t *testing.T) {
	t.Parallel()

	var got account.Joined
	f := setupWith(t, func(c *account.Config) {
		c.OnJoined = func(_ context.Context, in account.Joined) error {
			got = in
			return nil
		}
	})
	ctx := context.Background()

	by := f.acct.ID
	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
		DisplayName: "Grace", Role: account.RoleAdmin, ByAccountID: &by,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: f.notify.invite}); err != nil {
		t.Fatal(err)
	}

	switch {
	case got.TenantID != f.tenant:
		t.Errorf("tenant = %s", got.TenantID)
	case got.AccountID == uuid.Nil || got.IdentityID == uuid.Nil:
		t.Errorf("both identifiers should be there: %+v", got)
	case got.Role != account.RoleAdmin:
		t.Errorf("role = %s", got.Role)
	case got.DisplayName != "Grace":
		t.Errorf("display name = %q", got.DisplayName)
	case got.InvitedBy == nil || *got.InvitedBy != by:
		t.Errorf("invitedBy = %v, want the inviter", got.InvitedBy)
	}
}

// The endpoint a landing page calls with the token out of its own URL.
func TestPreviewingAnInvitation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()
	f.store.TenantNames = map[uuid.UUID]string{f.tenant: "Skolan i Solna"}

	by := f.acct.ID
	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "bo@school.example",
		Role: account.RoleAdmin, ByAccountID: &by,
	}); err != nil {
		t.Fatal(err)
	}

	// No credential anywhere in the call, which is the point.
	inv, err := f.svc.PreviewInvitation(ctx, account.PreviewInput{
		Token: f.notify.invite, IPAddress: "198.51.100.7",
	})
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case inv.TenantName != "Skolan i Solna":
		t.Errorf("tenantName = %q, which is the whole reason for the route", inv.TenantName)
	case inv.InvitedByName != "Sam":
		t.Errorf("invitedBy = %q", inv.InvitedByName)
	case inv.Role != account.RoleAdmin:
		t.Errorf("role = %s", inv.Role)
	case inv.EmailAddress != "bo@school.example":
		t.Errorf("the service answers the address unmasked: %q", inv.EmailAddress)
	}
	if _, ok := f.log.last(authlog.EventInvitationPreviewed); !ok {
		t.Error("a preview should be recorded, because it is what the limit counts")
	}
}

// Reading a link is not using it. A preview that consumed would break the link
// it was explaining; one that extended would let anybody who can see the URL
// keep an invitation alive forever.
func TestPreviewingDoesNotConsumeOrExtend(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
	}); err != nil {
		t.Fatal(err)
	}
	token := f.notify.invite

	before, err := f.svc.PreviewInvitation(ctx, account.PreviewInput{Token: token})
	if err != nil {
		t.Fatal(err)
	}

	for range 3 {
		f.clock.advance(time.Hour)
		after, err := f.svc.PreviewInvitation(ctx, account.PreviewInput{Token: token})
		if err != nil {
			t.Fatalf("a preview should not spend the link: %v", err)
		}
		if !after.ExpiresAt.Equal(before.ExpiresAt) {
			t.Errorf("expiry moved from %s to %s", before.ExpiresAt, after.ExpiresAt)
		}
	}

	// And it still works afterwards.
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: token}); err != nil {
		t.Errorf("the link should still be good: %v", err)
	}
}

// One answer for every way a token can be wrong, or the route is a way to probe
// the table one token at a time.
func TestEveryBadTokenPreviewsTheSame(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	// A baseline refusal to compare the rest against.
	got, want := f.svc.PreviewInvitation(ctx, account.PreviewInput{Token: "not a token at all"})
	if got != nil {
		t.Fatal("that is not a token")
	}
	if want == nil {
		t.Fatal("it should have been refused")
	}
	if !rigerr.Is(want, rigerr.CodeNotFound) {
		t.Errorf("err = %v, want 404", want)
	}

	for _, c := range []struct {
		name  string
		token func(*testing.T) string
	}{
		{"empty", func(*testing.T) string { return "" }},
		{"the right shape and invented", func(t *testing.T) string {
			// A shape that decodes, naming a row that does not exist.
			if _, err := f.svc.Invite(ctx, account.InviteInput{
				TenantID: uuid.New(), EmailAddress: "nobody-else@example.com",
			}); err != nil {
				t.Fatal(err)
			}
			return wrongToken(f.notify.invite)
		}},
		{"consumed", func(t *testing.T) string {
			tenant := uuid.New()
			if _, err := f.svc.Invite(ctx, account.InviteInput{
				TenantID: tenant, EmailAddress: "consumed@example.com",
			}); err != nil {
				t.Fatal(err)
			}
			token := f.notify.invite
			if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: token}); err != nil {
				t.Fatal(err)
			}
			return token
		}},
		{"withdrawn", func(t *testing.T) string {
			tenant := uuid.New()
			if _, err := f.svc.Invite(ctx, account.InviteInput{
				TenantID: tenant, EmailAddress: "withdrawn@example.com",
			}); err != nil {
				t.Fatal(err)
			}
			token := f.notify.invite
			pending, _ := f.svc.Invitations(ctx, tenant)
			if err := f.svc.RevokeInvitation(ctx, account.RevokeInput{
				TenantID: tenant, InvitationID: pending[0].ID,
			}); err != nil {
				t.Fatal(err)
			}
			return token
		}},
		{"expired", func(t *testing.T) string {
			tenant := uuid.New()
			if _, err := f.svc.Invite(ctx, account.InviteInput{
				TenantID: tenant, EmailAddress: "expired@example.com",
			}); err != nil {
				t.Fatal(err)
			}
			f.clock.advance(account.DefaultInvitationTTL + time.Hour)
			return f.notify.invite
		}},
		{"the wrong kind of link", func(t *testing.T) string {
			if err := f.svc.SendEmailVerification(ctx, f.tenant, f.acct.ID); err != nil {
				t.Fatal(err)
			}
			return f.notify.verify
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := f.svc.PreviewInvitation(ctx, account.PreviewInput{Token: c.token(t)})
			if err == nil {
				t.Fatal("it should have been refused")
			}
			// The rendered message and the status, not only the code: two
			// refusals that differ in either are two refusals.
			if err.Error() != want.Error() {
				t.Errorf("answer differs:\n  got:  %v\n  want: %v", err, want)
			}
		})
	}
}

// Unauthenticated and keyed by a secret, so one source walking the token space
// has to run out of road.
func TestPreviewingIsThrottledBySource(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	for i := range 60 {
		_, err := f.svc.PreviewInvitation(ctx, account.PreviewInput{
			Token: "not a token", IPAddress: "198.51.100.9",
		})
		if !rigerr.Is(err, rigerr.CodeNotFound) {
			t.Fatalf("attempt %d: err = %v, want the ordinary refusal", i+1, err)
		}
	}

	_, err := f.svc.PreviewInvitation(ctx, account.PreviewInput{
		Token: "not a token", IPAddress: "198.51.100.9",
	})
	if !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Errorf("err = %v, want 429", err)
	}

	// Another source is unaffected: the limit is about where the knocking comes
	// from, not about the endpoint.
	if _, err := f.svc.PreviewInvitation(ctx, account.PreviewInput{
		Token: "not a token", IPAddress: "198.51.100.10",
	}); !rigerr.Is(err, rigerr.CodeNotFound) {
		t.Errorf("err = %v, want another source to be let through", err)
	}
}

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

// Withdrawing an invitation kills the link, and that is now the whole of it.
//
// It used to also soft-delete the account, because one had been created up
// front — so "withdraw an invitation" was "delete a colleague who has not
// answered yet". There is nothing to delete.
func TestRevokingAnInvitation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", DisplayName: "Grace",
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
	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{Token: token}); err == nil {
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

	// Nothing was created, so nothing was removed.
	ident, _ := f.store.FindIdentityByEmail(ctx, "grace@example.com")
	if acct, _ := f.store.AccountForIdentity(ctx, f.tenant, ident.ID); acct != nil {
		t.Error("there was never an account for withdrawing to remove")
	}

	// And the same person can be invited again.
	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
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

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	pending, err := f.svc.Invitations(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	invitation := pending[0].ID

	if _, err := f.svc.AcceptInvitation(ctx, account.AcceptInput{
		Token: f.notify.invite,
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

	ident, _ := f.store.FindIdentityByEmail(ctx, "grace@example.com")
	if acct, _ := f.store.AccountForIdentity(ctx, f.tenant, ident.ID); acct == nil {
		t.Error("she accepted, so she is a member")
	}
}

// An invitation belongs to one tenant, and naming another tenant's has to answer
// the same way as naming one that does not exist.
func TestRevokingAnotherTenantsInvitation(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com",
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
// needs: nobody knows which tenants an address belongs to until the code has
// been typed back, so asking first asks a question the visitor cannot answer.
func TestSigningInWithoutATenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	pair, err := f.signInWithoutATenant()
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

	// With a second tenant it lands on the one they were last in — which is also
	// the one they joined first, so this case cannot tell the two rules apart.
	// The one that can is in signin_test.go; what this asserts is that a second
	// tenant does not turn a sign-in that named none into a refusal.
	second := uuid.New()
	if _, err := f.svc.Provision(ctx, account.ProvisionInput{
		TenantID: second, EmailAddress: "sam@example.com",
	}); err != nil {
		t.Fatal(err)
	}

	pair, err = f.signInWithoutATenant()
	if err != nil {
		t.Fatal(err)
	}
	if tok, _ := f.sessions.Verify(ctx, pair.Access.Token); tok.TenantID != f.tenant {
		t.Error("it should land on the tenant they joined first")
	}

	// And naming one still works, which is what a subdomain deployment does.
	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.VerifyEmailCode(ctx, account.VerifyEmailCodeInput{
		TenantID: second, EmailAddress: "sam@example.com", Code: code,
		Client: session.ClientWeb, IPAddress: "203.0.113.10",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Session == nil {
		t.Fatal("a named tenant should have produced a session")
	}
	if tok, _ := f.sessions.Verify(ctx, res.Session.Access.Token); tok.TenantID != second {
		t.Error("a named tenant should still be honoured")
	}
}

// Somebody real who belongs nowhere signs in successfully and lands in the
// picker.
//
// This used to be a 403, and that made the flow it exists for impossible: an
// invitation waiting to be accepted is a perfectly good reason to have an
// identity and no tenant, and somebody who cannot sign in cannot accept it.
// With invitations pending until accepted it is no longer an edge case — it is
// where every invited person starts.
func TestSigningInBelongingToNoTenant(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()

	// Their only account is removed.
	f.store.Forget(f.acct.ID)

	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.svc.VerifyEmailCode(ctx, account.VerifyEmailCodeInput{
		EmailAddress: "sam@example.com", Code: code, IPAddress: "203.0.113.10",
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
	code, err = f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.VerifyEmailCode(ctx, account.VerifyEmailCodeInput{
		TenantID: f.tenant, EmailAddress: "sam@example.com", Code: code,
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
		_, _ = f.svc.VerifyEmailCode(ctx, account.VerifyEmailCodeInput{
			EmailAddress: "sam@example.com", Code: "000000",
			IPAddress: "203.0.113.10",
		})
	}
	if n := f.log.count(authlog.EventLoginFailed); n < 5 {
		t.Fatalf("%d failures recorded, want them all", n)
	}

	// And the lockout arrives, which is the whole point of recording them.
	code, err := f.askForCode()
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.VerifyEmailCode(ctx, account.VerifyEmailCodeInput{
		EmailAddress: "sam@example.com", Code: code, IPAddress: "203.0.113.10",
	})
	if !rigerr.Is(err, rigerr.CodeRateLimited) {
		t.Errorf("err = %v, want 429 — the failures should have locked it", err)
	}
}

// signInWithoutATenant is the code flow with no tenant named, for the tests
// about which tenant a sign-in lands in.
func (f *fixture) signInWithoutATenant() (session.Pair, error) {
	code, err := f.askForCode()
	if err != nil {
		return session.Pair{}, err
	}
	res, err := f.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
		EmailAddress: "sam@example.com", Code: code,
		Client: session.ClientWeb, IPAddress: "203.0.113.10",
	})
	if err != nil {
		return session.Pair{}, err
	}
	if res.Session == nil {
		return session.Pair{}, errors.New("signed in with no tenant")
	}
	return *res.Session, nil
}

// wrongToken is a token of the right shape that names no row.
func wrongToken(real string) string {
	out := []byte(real)
	for i := range out {
		if out[i] == 'A' {
			out[i] = 'B'
		} else {
			out[i] = 'A'
		}
	}
	return string(out)
}

// An invitation that lapsed still holds the unique slot, so inviting the same
// address again has to clear it rather than leave the index to refuse.
//
// The index is (invited_to_tenant_id, identity_id) where the row is neither
// consumed nor revoked, and expiry is deliberately not in it — now() cannot be
// indexed on. Looking the slot up through a query that hides expired rows
// therefore finds nothing, revokes nothing, and the insert collides: every
// later invitation to that address fails, and there is no endpoint that can
// clear it, because withdrawing also only sees the live ones.
func TestInvitingAgainAfterTheFirstOneExpired(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()
	by := f.acct.ID

	first, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", ByAccountID: &by,
	})
	if err != nil {
		t.Fatal(err)
	}

	f.clock.advance(account.DefaultInvitationTTL + time.Hour)

	// Gone from both listings, which is what makes the slot invisible.
	if pending, err := f.svc.Invitations(ctx, f.tenant); err != nil {
		t.Fatal(err)
	} else if len(pending) != 0 {
		t.Fatalf("an expired invitation should not be listed: %+v", pending)
	}

	second, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", ByAccountID: &by,
	})
	if err != nil {
		t.Fatalf("inviting again after the first lapsed: %v", err)
	}
	if second.ID == first.ID {
		t.Error("the second invitation should be a new row")
	}

	// And the newest is the one that is waiting, rather than two of them.
	pending, err := f.svc.Invitations(ctx, f.tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != second.ID {
		t.Fatalf("got %d invitations, want only the new one: %+v", len(pending), pending)
	}
}

// Mail goes out after the transaction that wrote the row it is about, never
// inside it.
//
// The Notifier is the application's code talking to somebody else's server, and
// there is no timeout on the inline path. Called inside InTx it would hold a
// pool connection and an open transaction for as long as that server takes to
// answer — on the sign-in endpoint, which is the busiest one there is.
func TestMailIsNotSentInsideATransaction(t *testing.T) {
	t.Parallel()

	f := setup(t)
	ctx := context.Background()
	f.notify.store = f.store
	by := f.acct.ID

	if err := f.svc.RequestEmailCode(ctx, account.RequestEmailCodeInput{
		EmailAddress: "sam@example.com", IPAddress: "203.0.113.10",
	}); err != nil {
		t.Fatal(err)
	}
	if f.notify.codeInTx {
		t.Error("the code was mailed from inside a transaction")
	}

	if _, err := f.svc.Invite(ctx, account.InviteInput{
		TenantID: f.tenant, EmailAddress: "grace@example.com", ByAccountID: &by,
	}); err != nil {
		t.Fatal(err)
	}
	if f.notify.inviteInTx {
		t.Error("the invitation was mailed from inside a transaction")
	}
}

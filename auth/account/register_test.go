package account_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/authlog"
)

// signUp is a stranger arriving: they type an address rig has never seen,
// provisioning creates the person, and they type the code back.
//
// There is no registration endpoint. With no password there is nothing for one
// to take, and an endpoint that minted an identity token for anybody who typed
// an address would be a door rather than a form — so the two halves are split
// by the proof, and this is both of them.
func signUp(f *fixture, email string) (account.SignInResult, error) {
	f.notify.code = ""
	if err := f.svc.RequestEmailCode(context.Background(), account.RequestEmailCodeInput{
		EmailAddress: email,
		IPAddress:    "203.0.113.20",
		UserAgent:    "Mozilla/5.0",
	}); err != nil {
		return account.SignInResult{}, err
	}
	if f.notify.code == "" {
		return account.SignInResult{}, errors.New("no code was mailed")
	}
	return f.svc.VerifyEmailCode(context.Background(), account.VerifyEmailCodeInput{
		EmailAddress: email,
		Code:         f.notify.code,
		IPAddress:    "203.0.113.20",
		UserAgent:    "Mozilla/5.0",
	})
}

// open is a fixture where a code may go to an address nothing has ever seen.
func open(t *testing.T, edit func(*account.Config)) *fixture {
	return setupWith(t, func(cfg *account.Config) {
		cfg.EmailCode.AllowProvisioning = true
		if edit != nil {
			edit(cfg)
		}
	})
}

func TestOnRegisteredSeesTheNewcomer(t *testing.T) {
	var got account.Registered
	f := open(t, func(cfg *account.Config) {
		cfg.OnRegistered = func(_ context.Context, _ *account.Service, in account.Registered) error {
			got = in
			return nil
		}
	})

	res, err := signUp(f, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}

	if got.IdentityID != res.IdentityID {
		t.Errorf("hook saw identity %s, the sign-in answered %s", got.IdentityID, res.IdentityID)
	}
	if got.EmailAddress != "new@example.com" {
		t.Errorf("hook saw address %q", got.EmailAddress)
	}
	// The local part, because that is all anybody knows about somebody who has
	// typed nothing but an address.
	if got.DisplayName != "new" {
		t.Errorf("hook saw display name %q", got.DisplayName)
	}
	if got.IPAddress != "203.0.113.20" || got.UserAgent != "Mozilla/5.0" {
		t.Errorf("hook saw %q / %q", got.IPAddress, got.UserAgent)
	}
}

// It runs when the code is asked for rather than when it is typed back, because
// the row holding the code references the person. So a hook that fails fails
// the request that would have sent the mail.
func TestOnRegisteredErrorFailsTheSignUp(t *testing.T) {
	refuse := errors.New("no room")
	f := open(t, func(cfg *account.Config) {
		cfg.OnRegistered = func(context.Context, *account.Service, account.Registered) error {
			return refuse
		}
	})

	if _, err := signUp(f, "new@example.com"); !errors.Is(err, refuse) {
		t.Fatalf("expected the hook's error, got %v", err)
	}
	// The memory store has no transactions to roll back — the Postgres store's
	// rollback is covered by the docker suite — but the failure must at least
	// come back as the hook's own error rather than something laundered.
}

// The invite-only reading of the hook, and the one the flow is built around:
// the newcomer lands in the picker with an invitation waiting rather than in a
// tenant somebody decided for them.
//
// This is now a genuinely different answer from the Provision one below, which
// is what a flag on one function could never express.
func TestOnRegisteredCanLeaveAnInvitationWaiting(t *testing.T) {
	tenant := uuid.New()
	f := open(t, func(cfg *account.Config) {
		cfg.OnRegistered = func(ctx context.Context, accounts *account.Service, in account.Registered) error {
			_, err := accounts.Invite(ctx, account.InviteInput{
				TenantID:     tenant,
				EmailAddress: in.EmailAddress,
				DisplayName:  in.DisplayName,
			})
			return err
		}
	})
	f.store.TenantNames = map[uuid.UUID]string{tenant: "Starter"}

	res, err := signUp(f, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}

	invitations, err := f.svc.MyInvitations(context.Background(), res.IdentityID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invitations) != 1 {
		t.Fatalf("expected the starter invitation, got %d", len(invitations))
	}
	if invitations[0].TenantID != tenant {
		t.Errorf("invitation is for %s, not the starter tenant", invitations[0].TenantID)
	}

	// And they are in no tenant, which is the whole difference. The picker is
	// what they see, and accepting is what puts them somewhere.
	if res.Session != nil || len(res.Tenants) != 0 {
		t.Errorf("session = %v, tenants = %v, want them waiting outside", res.Session, res.Tenants)
	}
	if res.Identity.Token == "" {
		t.Fatal("the identity token is the credential the picker runs on")
	}
}

func TestSigningUpWithoutAHookLandsNowhere(t *testing.T) {
	f := open(t, nil)

	res, err := signUp(f, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != nil || len(res.Tenants) != 0 {
		t.Fatal("a plain sign-up should land nowhere")
	}
	if res.Identity.Token == "" {
		t.Fatal("expected an identity session")
	}
}

// A hook that provisions puts somebody in a real tenant, and the response has
// to say so — rather than telling a newcomer they belong nowhere and making
// them sign in again to find the tenant they had just been put in.
func TestOnRegisteredCanLandSomebodySomewhere(t *testing.T) {
	tenant := uuid.New()
	f := open(t, func(cfg *account.Config) {
		cfg.OnRegistered = func(ctx context.Context, accounts *account.Service, in account.Registered) error {
			_, err := accounts.Provision(ctx, account.ProvisionInput{
				TenantID:     tenant,
				EmailAddress: in.EmailAddress,
				DisplayName:  in.DisplayName,
			})
			return err
		}
	})
	f.store.TenantNames = map[uuid.UUID]string{tenant: "Starter"}

	res, err := signUp(f, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if res.Session == nil {
		t.Fatal("no session, though the hook put them in a tenant")
	}
	if res.TenantID != tenant {
		t.Errorf("landed in %s, want the tenant the hook made an account in", res.TenantID)
	}
	if len(res.Tenants) != 1 || res.Tenants[0].TenantID != tenant {
		t.Fatalf("tenants = %v, want the one they were put in", res.Tenants)
	}
	// The identity token is still issued alongside, because signing in and then
	// switching tenants is one flow.
	if res.Identity.Token == "" {
		t.Error("the identity token should be issued whether or not there is a session")
	}
}

// The state the two entries describe together: a person was created, and a
// session was issued for them. Neither says the whole thing on its own.
func TestSigningUpSomewhereRecordsBothHalves(t *testing.T) {
	tenant := uuid.New()
	f := open(t, func(cfg *account.Config) {
		cfg.OnRegistered = func(ctx context.Context, accounts *account.Service, in account.Registered) error {
			_, err := accounts.Provision(ctx, account.ProvisionInput{
				TenantID: tenant, EmailAddress: in.EmailAddress, DisplayName: in.DisplayName,
			})
			return err
		}
	})
	f.store.TenantNames = map[uuid.UUID]string{tenant: "Starter"}

	if _, err := signUp(f, "new@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.log.last(authlog.EventAccountProvisioned); !ok {
		t.Error("no AccountProvisioned entry, so nothing records the sign-up itself")
	}
	e, ok := f.log.last(authlog.EventLoginSucceeded)
	if !ok {
		t.Fatal("no LoginSucceeded entry, so nothing records the session")
	}
	if e.TokenRootID == nil {
		t.Error("the entry should name the session family it created")
	}
}

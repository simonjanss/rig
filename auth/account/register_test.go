package account_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
)

// register signs a stranger up on whatever fixture it is handed.
func register(f *fixture, email string) (account.SignInResult, error) {
	return f.svc.Register(context.Background(), account.RegisterInput{
		EmailAddress: email,
		DisplayName:  "New Person",
		Password:     goodPassword,
		IPAddress:    "203.0.113.20",
		UserAgent:    "Mozilla/5.0",
	})
}

func TestOnRegisteredSeesTheNewcomer(t *testing.T) {
	var got account.Registered
	f := setupWith(t, func(cfg *account.Config) {
		cfg.OnRegistered = func(_ context.Context, _ *account.Service, in account.Registered) error {
			got = in
			return nil
		}
	})

	res, err := register(f, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}

	if got.IdentityID != res.IdentityID {
		t.Errorf("hook saw identity %s, register answered %s", got.IdentityID, res.IdentityID)
	}
	if got.EmailAddress != "new@example.com" {
		t.Errorf("hook saw address %q", got.EmailAddress)
	}
	if got.DisplayName != "New Person" {
		t.Errorf("hook saw display name %q", got.DisplayName)
	}
	if got.IPAddress != "203.0.113.20" || got.UserAgent != "Mozilla/5.0" {
		t.Errorf("hook saw %q / %q", got.IPAddress, got.UserAgent)
	}
}

func TestOnRegisteredErrorFailsTheSignUp(t *testing.T) {
	refuse := errors.New("no room")
	f := setupWith(t, func(cfg *account.Config) {
		cfg.OnRegistered = func(context.Context, *account.Service, account.Registered) error {
			return refuse
		}
	})

	if _, err := register(f, "new@example.com"); !errors.Is(err, refuse) {
		t.Fatalf("expected the hook's error, got %v", err)
	}
	// The memory store has no transactions to roll back — the Postgres store's
	// rollback is covered by the docker suite — but the failure must at least
	// come back as the hook's own error rather than something laundered.
}

func TestOnRegisteredCanLeaveAnInvitationWaiting(t *testing.T) {
	// The canonical body: provision the newcomer into a starter tenant with an
	// invitation, so the picker they land in has somewhere to go.
	tenant := uuid.New()
	f := setupWith(t, func(cfg *account.Config) {
		cfg.OnRegistered = func(ctx context.Context, accounts *account.Service, in account.Registered) error {
			_, err := accounts.Provision(ctx, account.ProvisionInput{
				TenantID:     tenant,
				EmailAddress: in.EmailAddress,
				DisplayName:  in.DisplayName,
				Invite:       true,
			})
			return err
		}
	})
	f.store.TenantNames = map[uuid.UUID]string{tenant: "Starter"}

	res, err := register(f, "new@example.com")
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
}

func TestRegisterWithoutAHookStillWorks(t *testing.T) {
	f := setup(t)

	res, err := register(f, "new@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if res.Session != nil || len(res.Tenants) != 0 {
		t.Fatal("a plain registration should land nowhere")
	}
	if res.Identity.Token == "" {
		t.Fatal("expected an identity session")
	}
}

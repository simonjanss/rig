package account_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/identity"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// TestTheGateOnTheCodeDoorRefusesWithoutSayingSo is the one place the gate must
// not answer out loud.
//
// POST /auth/email-code answers 204 to every address, so that holding one cannot
// be used to ask whether somebody here has it. A gate refusing with a 403 would
// be exactly that question with a different spelling — so the refusal goes to
// rig_auth_log and the caller is told the same nothing as everybody else.
func TestTheGateOnTheCodeDoorRefusesWithoutSayingSo(t *testing.T) {
	t.Parallel()

	f := setupWith(t, func(c *account.Config) {
		c.EmailCode.AllowProvisioning = true
		c.AllowIdentity = identity.AllowDomains("example.com")
	})

	if _, err := f.askForCodeAs("mallory@other.test"); err == nil {
		t.Fatal("a refused address was mailed a code")
	} else if !strings.Contains(err.Error(), "no code was mailed") {
		t.Fatalf("the caller was told %q, and a stranger is told nothing", err)
	}

	if identityFor(t, f, "mallory@other.test") != nil {
		t.Error("a refused address left a rig_identity behind, which is the row the gate exists to prevent")
	}

	entry, ok := f.log.last(authlog.EventEmailCodeRequested)
	if !ok {
		t.Fatal("nothing was recorded, so an operator has no way to see the refusal at all")
	}
	if entry.Outcome != authlog.Failed {
		t.Errorf("recorded as %s, want %s", entry.Outcome, authlog.Failed)
	}
	if reason, _ := entry.Detail["reason"].(string); !strings.Contains(reason, "AllowIdentity") {
		t.Errorf("the detail said %q, which does not tell an operator which door shut", reason)
	}
}

// TestTheGateOnTheCodeDoorAdmitsTheDomainItAllows is the other branch, so that
// the test above is not passing because everything is refused.
func TestTheGateOnTheCodeDoorAdmitsTheDomainItAllows(t *testing.T) {
	t.Parallel()

	f := setupWith(t, func(c *account.Config) {
		c.EmailCode.AllowProvisioning = true
		c.AllowIdentity = identity.AllowDomains("example.com")
	})

	if _, err := f.askForCodeAs("ada@example.com"); err != nil {
		t.Fatalf("an allowed address was refused: %v", err)
	}
	if identityFor(t, f, "ada@example.com") == nil {
		t.Error("an allowed address was mailed a code and left no identity behind")
	}
}

// TestTheCodeDoorAsksAboutStrangersAndNobodyElse is the contract, on this path.
// The fixture's own person already exists, so asking for a code as them must not
// reach the gate at all.
func TestTheCodeDoorAsksAboutStrangersAndNobodyElse(t *testing.T) {
	t.Parallel()

	var asked []identity.Candidate
	f := setupWith(t, func(c *account.Config) {
		c.EmailCode.AllowProvisioning = true
		c.AllowIdentity = func(_ context.Context, in identity.Candidate) error {
			asked = append(asked, in)
			return nil
		}
	})

	if _, err := f.askForCode(); err != nil {
		t.Fatalf("somebody who already exists was refused: %v", err)
	}
	if len(asked) != 0 {
		t.Fatalf("an existing person was asked about: %+v", asked)
	}

	if _, err := f.askForCodeAs("ada@example.com"); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 {
		t.Fatalf("a stranger was asked about %d times, want 1", len(asked))
	}
	if asked[0].Via != identity.SourceEmailCode {
		t.Errorf("arrived as %s, want %s", asked[0].Via, identity.SourceEmailCode)
	}
	// Unproven on purpose: the identity is created when the code is asked for,
	// not when it is typed back. A gate insisting on a verified address here
	// would shut this door for everybody.
	if asked[0].EmailVerified {
		t.Error("the code door claimed a verified address before anybody had proved one")
	}
}

// TestTheGateOnAnInvitationRefusesOutLoud is the opposite decision from the code
// door, and for the opposite reason: the caller is an administrator who is
// already signed in, so telling them why is the whole point.
func TestTheGateOnAnInvitationRefusesOutLoud(t *testing.T) {
	t.Parallel()

	var asked []identity.Candidate
	f := setupWith(t, func(c *account.Config) {
		c.AllowIdentity = func(_ context.Context, in identity.Candidate) error {
			asked = append(asked, in)
			return rigerr.Forbidden("not this one")
		}
	})

	_, err := f.svc.Invite(context.Background(), account.InviteInput{
		TenantID: f.tenant, EmailAddress: "ada@example.com",
	})
	if err == nil {
		t.Fatal("the gate refused and an invitation went out anyway")
	}
	if rigerr.CodeOf(err) != rigerr.CodeForbidden {
		t.Errorf("refused with %s, want %s", rigerr.CodeOf(err), rigerr.CodeForbidden)
	}
	if len(asked) != 1 {
		t.Fatalf("asked %d times, want 1", len(asked))
	}
	if asked[0].Via != identity.SourceInvitation {
		t.Errorf("arrived as %s, want %s", asked[0].Via, identity.SourceInvitation)
	}
	if asked[0].TenantID == nil || *asked[0].TenantID != f.tenant {
		t.Errorf("the tenant somebody is being invited into did not reach the gate: %+v", asked[0])
	}
	if identityFor(t, f, "ada@example.com") != nil {
		t.Error("a refused invitation left a rig_identity behind")
	}
}

// TestProvisionReachesTheGateToo covers the fourth door, which is the one an
// issue about sign-in methods would have missed.
func TestProvisionReachesTheGateToo(t *testing.T) {
	t.Parallel()

	var asked []identity.Candidate
	f := setupWith(t, func(c *account.Config) {
		c.AllowIdentity = func(_ context.Context, in identity.Candidate) error {
			asked = append(asked, in)
			return rigerr.Forbidden("not this one")
		}
	})

	if _, err := f.svc.Provision(context.Background(), account.ProvisionInput{
		TenantID: f.tenant, EmailAddress: "ada@example.com", DisplayName: "Ada",
	}); err == nil {
		t.Fatal("the gate refused and an account was created anyway")
	}
	if len(asked) != 1 || asked[0].Via != identity.SourceProvision {
		t.Fatalf("asked %+v", asked)
	}
}

// TestProvisioningSomebodyWhoExistsSkipsTheGate is the contract again: the
// second tenant an existing person is added to is not a question about whether
// they may be here.
func TestProvisioningSomebodyWhoExistsSkipsTheGate(t *testing.T) {
	t.Parallel()

	asked := 0
	f := setupWith(t, func(c *account.Config) {
		c.AllowIdentity = func(context.Context, identity.Candidate) error {
			asked++
			return rigerr.Forbidden("no")
		}
	})

	if _, err := f.svc.Provision(context.Background(), account.ProvisionInput{
		TenantID: uuid.New(), EmailAddress: f.ident.EmailAddress, DisplayName: "Sam",
	}); err != nil {
		t.Fatalf("an existing person could not be added to a second tenant: %v", err)
	}
	if asked != 0 {
		t.Errorf("the gate was asked %d times about somebody who already exists", asked)
	}
}

// identityFor reads the store the way the service does, so a test asserting that
// no row was written is asserting about the same lookup the gate guards.
func identityFor(t *testing.T, f *fixture, address string) *account.Identity {
	t.Helper()

	ident, err := f.store.FindIdentityByEmail(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	return ident
}

//go:build docker

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/services/outbox"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/rigtest"
)

// `auth.allowed_identity_domains` in this example's rig.yaml is
// [example.com, acme.test], and this file is what that key means, over the real
// schema rather than over a double.
//
// It is also where this example uses rigtest, which is rig's own harness for
// exactly these reads. The point of the assertions below is not that a request
// was refused — a status code would say that — but that **no row was written**,
// and rig_identity is rig's table rather than this example's.

// TestAStrangerOutsideTheDomainsCannotBecomeAPerson is the mailed-code door, and
// the interesting part is what the caller is told: nothing.
//
// POST /auth/email-code answers 204 to every address, so that holding one cannot
// be used to ask whether somebody here has it. A gate refusing out loud would be
// that question with a different spelling, so the refusal goes to the trail.
func TestAStrangerOutsideTheDomainsCannotBecomeAPerson(t *testing.T) {
	t.Parallel()

	srv := newServer(t)
	rt := gateHarness(t, srv)
	tenant := srv.seed(t)

	outsider := "mallory-" + uuid.NewString()[:8] + "@elsewhere.test"

	res := srv.do(t, request{
		method: "POST", path: "/auth/email-code", tenant: tenant,
		body: map[string]any{"emailAddress": outsider},
	})
	if res.status != 204 {
		t.Fatalf("a refused address was answered %d %s, and every address gets 204",
			res.status, res.body)
	}

	for _, m := range srv.mail.Messages() {
		if m.Kind == outbox.KindEmailCode && strings.EqualFold(m.To, outsider) {
			t.Fatal("a code was mailed to an address the gate refused")
		}
	}
	if got := rt.Identity(t, outsider); got != nil {
		t.Fatalf("a refused address left an identity behind: %+v", got)
	}

	// The operator's side of the same event, which is where the reason lives
	// precisely because the caller's side cannot carry one.
	entries := rt.AuthLog(t, rigtest.LogQuery{
		Event: "EmailCodeRequested", EmailAddress: outsider, Outcome: rigtest.Failed,
	})
	if len(entries) != 1 {
		t.Fatalf("the trail has %d failed requests for %s, want 1", len(entries), outsider)
	}
	if !strings.Contains(entries[0].Reason(), "AllowIdentity") {
		t.Errorf("the trail says %q, which does not name the door that shut",
			entries[0].Reason())
	}
}

// TestAStrangerInsideTheDomainsStillBecomesAPerson is the other branch, so that
// the test above is not passing because nobody can register at all.
func TestAStrangerInsideTheDomainsStillBecomesAPerson(t *testing.T) {
	t.Parallel()

	srv := newServer(t)
	rt := gateHarness(t, srv)
	tenant := srv.seed(t)

	newcomer := "ada-" + uuid.NewString()[:8] + "@acme.test"
	srv.askForCode(t, tenant, newcomer)

	ident := rt.Identity(t, newcomer)
	if ident == nil {
		t.Fatal("an allowed address was mailed a code and left no identity behind")
	}
	// Not yet: the address is unproven until the code comes back, which is why
	// the gate is asked without a verified address on this path.
	if ident.Verified() {
		t.Error("the address was marked verified before anybody typed the code back")
	}
}

// TestAnInvitationIgnoresTheDomains is the rule that earns Via its place.
//
// "Strangers must be at our domain, but an administrator may invite whoever
// they like" is what a deployment actually wants — a school inviting a supply
// teacher with a personal address is the ordinary case rather than an abuse.
// One predicate with no context could not express it.
func TestAnInvitationIgnoresTheDomains(t *testing.T) {
	t.Parallel()

	srv := newServer(t)
	rt := gateHarness(t, srv)
	srv.seed(t)

	// The accounts service out of the generated configuration, so the gate under
	// test is the one rig.yaml wired rather than one this test built.
	accounts := gateAccounts(t, srv)
	guest := "supply-" + uuid.NewString()[:8] + "@elsewhere.test"

	// A tenant of this test's own, and the reason is the other knob. The seeded
	// tenant sets rig_tenant.allowed_email_domains to example.com, which refuses
	// this address before the identity gate is ever reached — so inviting into
	// it would prove nothing about the gate. That the two are separately in play
	// here is the clearest statement of why they have different names.
	own := rt.Tenant(t, rigtest.Named("Supply agency"))

	if _, err := accounts.Invite(context.Background(), account.InviteInput{
		TenantID: own.ID, EmailAddress: guest, DisplayName: "Supply teacher",
	}); err != nil {
		t.Fatalf("inviting somebody outside the identity domains: %v", err)
	}

	ident := rt.Identity(t, guest)
	if ident == nil {
		t.Fatal("an invited address outside the domains was refused")
	}
	// Invited and not yet arrived: accepting is what writes the account, so the
	// tenant's people list does not have them.
	if got := rt.Account(t, own.ID, ident.ID); got != nil {
		t.Errorf("an invitation created an account: %+v", got)
	}
}

// TestTheTenantsOwnDomainsAreADifferentQuestion is the distinction the naming
// exists for, asserted rather than left in a comment.
//
// rig_tenant.allowed_email_domains governs whether somebody may hold an account
// in one tenant, is set per tenant at run time, and refuses an invitation that
// auth.allowed_identity_domains would have allowed. A sign-in naming no tenant
// never reaches it, which is why it could never have answered "who may become a
// person here".
func TestTheTenantsOwnDomainsAreADifferentQuestion(t *testing.T) {
	t.Parallel()

	srv := newServer(t)
	tenant := srv.seed(t)
	accounts := gateAccounts(t, srv)

	_, err := accounts.Invite(context.Background(), account.InviteInput{
		TenantID:     tenant,
		EmailAddress: "supply-" + uuid.NewString()[:8] + "@elsewhere.test",
		DisplayName:  "Supply teacher",
	})
	if err == nil {
		t.Fatal("the seeded tenant allows example.com and took an elsewhere.test address")
	}
	if !strings.Contains(err.Error(), "not in a domain this tenant allows") {
		t.Errorf("refused with %q, which is not the tenant's own list speaking", err)
	}
}

// gateHarness borrows the server's own pool rather than opening a second, which
// is the shape a suite adopting rigtest one read at a time uses.
func gateHarness(t *testing.T, srv *server) *rigtest.Rig {
	t.Helper()

	return rigtest.New(t, rigtest.Config{
		Pool: srv.pool,
		// This example vendors rig's migrations rather than embedding them, so
		// its own directory is the whole set. A project on `migrations.foundation:
		// embedded` passes api.MigrationSources(its own) instead.
		Migrations: []migrate.Source{{
			Name: "auth", FS: migrations, Dir: "migrations", Table: migrate.DefaultTable,
		}},
	})
}

// gateAccounts assembles the account service from the generated configuration.
//
// api.Config is the documented way to reach the parts without api.New doing the
// assembly, and using it here is the point: the gate this exercises is the one
// `allowed_identity_domains` generated, not one the test passed in. A test that
// built its own would prove the hook works and say nothing about the key.
func gateAccounts(t *testing.T, srv *server) *account.Service {
	t.Helper()

	// The real grants and the real notifier, because Config refuses without
	// them — rightly: an application with neither is one where every endpoint
	// answers 403 and no mail is ever sent.
	cfg, err := api.Config(srv.pool, api.Hooks{
		Grants:   srv.grants(t),
		Notifier: srv.mail,
	})
	if err != nil {
		t.Fatal(err)
	}
	front, err := auth.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	closeAuth(t, front)
	return front.Parts().Accounts
}

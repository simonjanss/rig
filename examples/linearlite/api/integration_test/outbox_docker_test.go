//go:build docker

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/examples/linearlite/internal/services/outbox"
)

// The two interfaces rig ships no transport for, over the wire.
//
// account.Notifier delivers the single-use links the auth package mints, and
// notify.Sender delivers a copy of an inbox line to a channel. services/outbox
// implements both with one ring buffer, /_demo/outbox reads it, and everything
// below goes through that route rather than through the box directly — because
// what the front end can walk is the thing worth asserting on.

// A sign-in code, end to end, with the outbox standing in for a mailbox.
func TestASignInCodeThroughTheOutbox(t *testing.T) {
	api := newServer(t)
	api.seed(t)

	// The code is minted for an identity, before anybody knows which tenant it
	// is for — an address can belong to accounts in several — so reading the box
	// needs a session even though the message that comes back has no tenant on
	// it. The demo route beside it is the way in for somebody who has no
	// session yet, which is what this suite uses for the sign-in itself.
	reader := api.login(t, app.SeedEmail2)

	const nobody = "nobody@linearlite.dev"

	// Always 204, and the endpoint answers the same for an address nobody has:
	// anything else would tell a stranger which addresses have accounts.
	for _, address := range []string{app.SeedEmail, nobody} {
		res := api.do(t, request{
			method: http.MethodPost, path: "/auth/email-code",
			body: map[string]any{"emailAddress": address},
		})
		if res.status != http.StatusNoContent {
			t.Fatalf("a code for %s: %d %s, want 204", address, res.status, res.body)
		}
	}

	// Two codes, and that is the point rather than a leak. This example sets
	// allow_provisioning, so an address rig has never seen gets a person and a
	// code of its own — and the caller cannot tell the two cases apart, because
	// both answered 204 and neither mail went to them.
	var code string
	var reached int
	for _, m := range api.outbox(t, reader) {
		if m.Kind != outbox.KindEmailCode {
			continue
		}
		switch m.To {
		case app.SeedEmail:
			code = m.Token
			reached++
		case nobody:
			reached++
		}
	}
	if code == "" {
		t.Fatal("the code should be in the outbox")
	}
	if reached != 2 {
		t.Errorf("%d codes went out, want one per address", reached)
	}

	// The person the second one created, which is what allow_provisioning
	// means: asking for a code is how somebody arrives here.
	var made int
	if err := api.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM rig_identity WHERE lower(email_address) = lower($1)`,
		nobody).Scan(&made); err != nil {
		t.Fatal(err)
	}
	if made != 1 {
		t.Errorf("%d identities for the new address, want 1", made)
	}

	if res := api.do(t, request{
		method: http.MethodPost, path: "/auth/email-code/verify",
		body: map[string]any{"emailAddress": app.SeedEmail, "code": code},
	}); res.status != http.StatusOK {
		t.Fatalf("sign in with the mailed code: %d %s", res.status, res.body)
	}

	// The code is spent. Offering it again is refused rather than ignored,
	// which is what makes it single-use rather than merely short-lived.
	if res := api.do(t, request{
		method: http.MethodPost, path: "/auth/email-code/verify",
		body: map[string]any{"emailAddress": app.SeedEmail, "code": code},
	}); res.status < 400 {
		t.Errorf("a spent code should be refused: %d %s", res.status, res.body)
	}
}

// Inviting somebody mints a link, and needs the permission to.
func TestAnInvitationLandsInTheOutbox(t *testing.T) {
	api := newServer(t)
	api.seed(t)

	owner := api.login(t, app.SeedEmail)
	// A fresh address per run: this database is throwaway but not reset between
	// runs, and inviting somebody who already has an account here is a 409 —
	// correctly, which is another test's subject and not this one's.
	invited := "newcomer-" + uuid.NewString() + "@linearlite.dev"

	res := api.do(t, request{
		method: http.MethodPost, path: "/auth/invitations", token: owner,
		body: map[string]any{
			"emailAddress": invited, "displayName": "Newcomer", "role": "Basic",
		},
	})
	if res.status != http.StatusCreated {
		t.Fatalf("invite: %d %s", res.status, res.body)
	}

	var found bool
	for _, m := range api.outbox(t, owner) {
		if m.Kind == outbox.KindInvitation && m.To == invited && m.Token != "" {
			found = true
		}
	}
	if !found {
		t.Error("the invitation link should be in the outbox")
	}

	// The permission model, not a special case: inviting is administrative, and
	// the Basic role the seed gives alex does not hold it.
	member := api.login(t, app.SeedEmail2)
	if res := api.do(t, request{
		method: http.MethodPost, path: "/auth/invitations", token: member,
		body: map[string]any{
			"emailAddress": "another-" + uuid.NewString() + "@linearlite.dev",
			"displayName":  "Another",
		},
	}); res.status != http.StatusForbidden {
		t.Errorf("a member inviting: %d %s, want 403", res.status, res.body)
	}
}

// A notification's email copy, and the identifier a real transport owes the
// provider.
func TestANotificationReachesTheEmailChannel(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	demoToken := api.login(t, app.SeedEmail)
	alexToken := api.login(t, app.SeedEmail2)
	demo := api.accountID(t, tenant, app.SeedEmail)

	created := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todos", token: demoToken,
		body: map[string]any{"title": "Demo's item, for the email channel", "assigneeAccountId": demo},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.body)
	}
	var item struct {
		ID string `json:"id"`
	}
	created.decode(t, &item)

	if res := api.do(t, request{
		method: http.MethodPatch, path: "/api/v1/todos/" + item.ID, token: alexToken,
		body: map[string]any{"status": "in_progress"},
	}); res.status != http.StatusOK {
		t.Fatalf("alex's change: %d %s", res.status, res.body)
	}

	// The in-process engine sends into its own box; this server's box is the
	// one /_demo/outbox reads, so the pass has to be that engine's. Resolve
	// through the same server rather than through dispatchNotifications, which
	// builds a second API — and a second box — the way a cron entry does.
	api.dispatch(t)

	var sent int
	for _, m := range api.outbox(t, demoToken) {
		if m.Kind != outbox.KindNotification || m.To != app.SeedEmail {
			continue
		}
		sent++
		if len(m.DeliveryIDs) == 0 {
			t.Error("a delivery carries the id a real transport hands the provider as its idempotency key")
		}
	}
	if sent == 0 {
		t.Fatal("the status change should have reached the email channel")
	}

	// The channel is a copy. The inbox line was written either way, which is
	// the difference between a channel and the inbox.
	var lines int
	if err := api.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM rig_notification_recipient
		 WHERE tenant_id = $1 AND account_id = $2`, tenant, demo).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines == 0 {
		t.Error("the inbox line is not a channel and is always written")
	}
}

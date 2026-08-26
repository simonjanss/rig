//go:build docker

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/examples/linearlite/services/outbox"
)

// The two interfaces rig ships no transport for, over the wire.
//
// account.Notifier delivers the single-use links the auth package mints, and
// notify.Sender delivers a copy of an inbox line to a channel. services/outbox
// implements both with one ring buffer, /_demo/outbox reads it, and everything
// below goes through that route rather than through the box directly — because
// what the front end can walk is the thing worth asserting on.

// A password reset, end to end, with the outbox standing in for a mailbox.
func TestAPasswordResetThroughTheOutbox(t *testing.T) {
	api := newServer(t)
	api.seed(t)

	// The token is minted for an identity, before anybody knows which tenant
	// it is for — an address can belong to accounts in several — so reading the
	// box needs a session even though the message that comes back has no tenant
	// on it.
	reader := api.login(t, app.SeedEmail2)

	const nobody = "nobody@linearlite.dev"

	// Always 202, and the endpoint answers the same for an address nobody has:
	// anything else would tell a stranger which addresses have accounts.
	for _, address := range []string{app.SeedEmail, nobody} {
		res := api.do(t, request{
			method: http.MethodPost, path: "/auth/password/reset",
			body: map[string]any{"emailAddress": address},
		})
		if res.status != http.StatusAccepted {
			t.Fatalf("reset for %s: %d %s, want 202", address, res.status, res.body)
		}
	}

	// One link, not two. The answers were identical; what differs is that only
	// one of them had anywhere to send mail — which is the whole shape of the
	// endpoint's refusal to confirm an address.
	var token string
	for _, m := range api.outbox(t, reader) {
		switch {
		case m.Kind == outbox.KindReset && m.To == app.SeedEmail:
			token = m.Token
		case m.To == nobody:
			t.Error("an address with no account must not produce mail")
		}
	}
	if token == "" {
		t.Fatal("the reset link should be in the outbox")
	}

	const newPassword = "a whole new correct horse"

	if res := api.do(t, request{
		method: http.MethodPost, path: "/auth/password/reset/confirm",
		body: map[string]any{"token": token, "newPassword": newPassword},
	}); res.status != http.StatusNoContent && res.status != http.StatusOK {
		t.Fatalf("confirm: %d %s", res.status, res.body)
	}

	// The link is spent. Sending it again is refused rather than ignored,
	// which is what makes it single-use rather than merely short-lived.
	if res := api.do(t, request{
		method: http.MethodPost, path: "/auth/password/reset/confirm",
		body: map[string]any{"token": token, "newPassword": newPassword},
	}); res.status < 400 {
		t.Errorf("a spent link should be refused: %d %s", res.status, res.body)
	}

	if res := api.do(t, request{
		method: http.MethodPost, path: "/auth/login",
		body: map[string]any{"emailAddress": app.SeedEmail, "password": newPassword},
	}); res.status != http.StatusOK {
		t.Fatalf("sign in with the new password: %d %s", res.status, res.body)
	}
	if res := api.do(t, request{
		method: http.MethodPost, path: "/auth/login",
		body: map[string]any{"emailAddress": app.SeedEmail, "password": app.SeedPassword},
	}); res.status == http.StatusOK {
		t.Error("the old password should no longer work")
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
		method: http.MethodPost, path: "/auth/accounts", token: owner,
		body: map[string]any{
			"emailAddress": invited, "displayName": "Newcomer",
			"role": "Basic", "invite": true,
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

	// The permission model, not a special case: provisioning is administrative,
	// and the Basic role the seed gives alex does not hold it.
	member := api.login(t, app.SeedEmail2)
	if res := api.do(t, request{
		method: http.MethodPost, path: "/auth/accounts", token: member,
		body: map[string]any{
			"emailAddress": "another-" + uuid.NewString() + "@linearlite.dev",
			"displayName":  "Another", "invite": true,
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

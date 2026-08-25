//go:build docker

package main

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/linearlite/services/outbox"
	"github.com/simonjanss/rig/notify"
)

// The two notification tables a person owns, as ordinary generated resources.
//
// `notifications: expose: true` and an `operations:` line each is the whole of
// the wiring — there is no hand-written HTTP for either — and both are
// owner-scoped, which is what everything below is really about: a preference and
// a device are rows whose owner is the only person they concern.
func TestNotificationPreferencesAreYourOwn(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	demoToken := api.login(t, SeedEmail)
	alexToken := api.login(t, SeedEmail2)
	demo := api.accountID(t, tenant, SeedEmail)
	alex := api.accountID(t, tenant, SeedEmail2)

	const settings = "/api/v1/rig-notification-settings"

	// One row per account per channel is a unique index, and this database is
	// throwaway without being reset between runs — so the create below is only a
	// create the first time. Clearing first is what makes this re-runnable, and
	// it is the same shape the front end uses: with a row already there, setting
	// a preference is an update.
	clearPreference(t, api, demoToken, "Desktop")

	mine := api.do(t, request{
		method: http.MethodPost, path: settings, token: demoToken,
		body: map[string]any{
			"accountId": demo, "channel": "Desktop",
			"digest": "Immediate", "isEnabled": true, "activeDays": []int{},
		},
	})
	if mine.status != http.StatusCreated {
		t.Fatalf("create a preference: %d %s", mine.status, mine.body)
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	mine.decode(t, &created)

	t.Run("a preference naming somebody else is refused", func(t *testing.T) {
		// Owner scoping narrows reads and updates by reading the row first, so a
		// create is the one write it cannot speak for. The service layer's
		// validator is what closes that, and it answers 422 with the field
		// named rather than quietly writing something else.
		res := api.do(t, request{
			method: http.MethodPost, path: settings, token: alexToken,
			body: map[string]any{
				"accountId": demo, "channel": "Email",
				"digest": "Daily", "isEnabled": true, "activeDays": []int{},
			},
		})
		if res.status != http.StatusUnprocessableEntity {
			t.Fatalf("alex naming demo: %d %s, want 422", res.status, res.body)
		}
	})

	t.Run("somebody else's preferences are not there to read", func(t *testing.T) {
		res := api.do(t, request{method: http.MethodGet, path: settings, token: alexToken})
		if res.status != http.StatusOK {
			t.Fatalf("alex's own list: %d %s", res.status, res.body)
		}
		var page struct {
			Data []struct {
				AccountID uuid.UUID `json:"accountId"`
			} `json:"data"`
		}
		res.decode(t, &page)
		for _, row := range page.Data {
			if row.AccountID != alex {
				t.Errorf("alex's list carries a row belonging to %s", row.AccountID)
			}
		}
	})

	t.Run("widening a preference read is administrative", func(t *testing.T) {
		// rig derived rig_notification_setting.read.all from the table being
		// owner-scoped, and this example grants it to Owner and Admin: "why did
		// they not get the mail" is a real question. Basic is refused.
		if res := api.do(t, request{
			method: http.MethodGet, path: settings + "?scope=all", token: demoToken,
		}); res.status != http.StatusOK {
			t.Errorf("the Owner asking for everybody's: %d %s, want 200", res.status, res.body)
		}
		if res := api.do(t, request{
			method: http.MethodGet, path: settings + "?scope=all", token: alexToken,
		}); res.status != http.StatusForbidden {
			t.Errorf("a member asking for everybody's: %d %s, want 403", res.status, res.body)
		}
	})

	t.Run("nobody may read everybody's devices", func(t *testing.T) {
		// The same widening on the device table, and services/authz grants it to
		// no role at all — so the Owner is refused too. A push token is the
		// address of somebody's machine, and this is what "the product does not
		// answer that question" looks like from outside.
		if res := api.do(t, request{
			method: http.MethodGet, path: "/api/v1/rig-notification-devices?scope=all", token: demoToken,
		}); res.status != http.StatusForbidden {
			t.Errorf("the Owner asking for everybody's devices: %d %s, want 403", res.status, res.body)
		}
	})

	t.Run("clearing a preference is going back to the default", func(t *testing.T) {
		// Basic holds this delete although it holds no other: every table here is
		// owner-scoped, so the only row it can reach is its own, and the absence
		// of a row is what notifications.default_digest answers for.
		if res := api.do(t, request{
			method: http.MethodDelete, path: settings + "/" + created.ID.String(), token: demoToken,
		}); res.status != http.StatusNoContent {
			t.Errorf("clearing it: %d %s, want 204", res.status, res.body)
		}
	})
}

// A push channel, and the half of the notification engine that needs telling
// where.
//
// Email has an address on the account, so a channel for it needs nothing
// registered. A push has to be told, which is what rig_notification_device is
// for — and a channel is handed those rows, one call per account, with every
// live device on it. rig ships no transport, so the example's sender records
// what it was given; what is asserted here is that it was given the right thing.
func TestADesktopDeliveryReachesTheDeviceChannel(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	demoToken := api.login(t, SeedEmail)
	alexToken := api.login(t, SeedEmail2)
	demo := api.accountID(t, tenant, SeedEmail)

	if res := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/rig-notification-devices", token: demoToken,
		body: map[string]any{
			"accountId": demo, "channel": "Desktop",
			"token": "webpush-demo:" + uuid.NewString(), "label": "the test's laptop",
		},
	}); res.status != http.StatusCreated {
		t.Fatalf("register a device: %d %s", res.status, res.body)
	}

	created := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todos", token: demoToken,
		body: map[string]any{"title": "Demo's item, for the push channel", "assigneeAccountId": demo},
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

	api.dispatch(t)

	var desktop, email int
	for _, m := range api.outbox(t, demoToken) {
		if m.Kind != outbox.KindNotification {
			continue
		}
		switch m.Channel {
		case "Desktop":
			desktop++
			if len(m.Devices) == 0 {
				t.Error("a push channel is handed the devices to send to, and this one was handed none")
			}
			if len(m.DeliveryIDs) == 0 {
				t.Error("the delivery ids a real transport owes the provider are missing")
			}
		case "Email":
			email++
		}
	}
	// Both, from one notification: two channels registered, two copies owed, one
	// inbox line underneath them that was written either way.
	if desktop == 0 {
		t.Error("the change should have reached the Desktop channel")
	}
	if email == 0 {
		t.Error("the change should have reached the Email channel too")
	}

	// And the delivery rows that say so, one per channel per line.
	var channels []string
	rows, err := api.pool.Query(context.Background(), `
		SELECT DISTINCT d.channel::text FROM rig_notification_delivery d
		 JOIN rig_notification_recipient r ON r.id = d.recipient_id
		 WHERE r.tenant_id = $1 AND r.account_id = $2
		 ORDER BY 1`, tenant, demo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatal(err)
		}
		channels = append(channels, c)
	}
	if len(channels) < 2 {
		t.Errorf("delivery channels = %v, want both Desktop and Email", channels)
	}
}

// clearPreference removes the caller's channel-wide row for one channel, if
// they have one. Owner-scoped, so this can only ever reach their own.
func clearPreference(t *testing.T, api *server, token, channel string) {
	t.Helper()

	res := api.do(t, request{
		method: http.MethodGet, path: "/api/v1/rig-notification-settings?limit=50", token: token,
	})
	if res.status != http.StatusOK {
		t.Fatalf("list preferences: %d %s", res.status, res.body)
	}
	var page struct {
		Data []struct {
			ID      uuid.UUID `json:"id"`
			Channel string    `json:"channel"`
			Kind    *string   `json:"kind"`
		} `json:"data"`
	}
	res.decode(t, &page)

	for _, row := range page.Data {
		if row.Channel != channel || row.Kind != nil {
			continue
		}
		if res := api.do(t, request{
			method: http.MethodDelete,
			path:   "/api/v1/rig-notification-settings/" + row.ID.String(), token: token,
		}); res.status != http.StatusNoContent {
			t.Fatalf("clear the %s preference: %d %s", channel, res.status, res.body)
		}
	}
}

// What the preferences screen is told about channels.
//
// The list is one key of `/_demo/tour`, and the screen badges every channel
// missing from it as having no sender in this build. So a build that registers
// a sender and does not say so reads as one that cannot deliver at all — a
// screen about channels, wrong about channels, with nothing else asserting on
// it.
func TestTheTourNamesTheChannelsWithASender(t *testing.T) {
	api := newServer(t)
	api.seed(t)
	token := api.login(t, SeedEmail)

	res := api.do(t, request{method: http.MethodGet, path: "/_demo/tour", token: token})
	if res.status != http.StatusOK {
		t.Fatalf("tour: %d %s", res.status, res.body)
	}
	var tour struct {
		Channels []string `json:"channels"`
	}
	res.decode(t, &tour)

	// The two main.go registers, and the one it does not. Mobile is the whole
	// reason the key exists: it is a channel rig knows about that this build
	// cannot deliver on, and the badge is the only place that is said.
	for _, want := range []notify.Channel{notify.ChannelEmail, notify.ChannelDesktop} {
		if !slices.Contains(tour.Channels, string(want)) {
			t.Errorf("the tour names %v, without %s: the screen will say this build has no sender for it", tour.Channels, want)
		}
	}
	if slices.Contains(tour.Channels, string(notify.ChannelMobile)) {
		t.Errorf("the tour names %s, which this build registered no sender for", notify.ChannelMobile)
	}
}

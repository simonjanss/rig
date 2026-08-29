//go:build docker

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/examples/linearlite/services/todo"
)

// The notification rule, over real SQL: a change reaches the item's
// stakeholders and never the person who made it. The dispatch task resolves
// rather than the in-process engine, because the engine belongs to the server
// and a test asserting on timing would be asserting on a race.
func TestAChangeNotifiesTheStakeholders(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	demoToken := api.login(t, app.SeedEmail)
	alexToken := api.login(t, app.SeedEmail2)
	demo := api.accountID(t, tenant, app.SeedEmail)
	alex := api.accountID(t, tenant, app.SeedEmail2)

	// An item demo creates and holds, so alex's change has exactly one
	// stakeholder to reach.
	created := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todos", token: demoToken,
		body: map[string]any{"title": "Demo's item, for the notification test", "assigneeAccountId": demo},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.body)
	}
	var item struct {
		ID uuid.UUID `json:"id"`
	}
	created.decode(t, &item)

	if res := api.do(t, request{
		method: http.MethodPatch, path: "/api/v1/todos/" + item.ID.String(), token: alexToken,
		body: map[string]any{"status": "in_progress"},
	}); res.status != http.StatusOK {
		t.Fatalf("alex's change: %d %s", res.status, res.body)
	}

	// The guarantee path, exactly as cron would run it.
	if err := app.DispatchNotifications(context.Background(), api.pool); err != nil {
		t.Fatal(err)
	}

	var kind string
	err := api.pool.QueryRow(context.Background(), `
		SELECT r.kind FROM rig_notification_recipient r
		 JOIN todo_notification l ON l.notification_id = r.notification_id
		 WHERE r.tenant_id = $1 AND r.account_id = $2 AND l.todo_id = $3`,
		tenant, demo, item.ID).Scan(&kind)
	if err != nil {
		t.Fatalf("demo should have an inbox line about the item: %v", err)
	}
	if kind != todo.KindTodoStatusChanged {
		t.Errorf("kind = %q, want %q", kind, todo.KindTodoStatusChanged)
	}

	var actorLines int
	if err := api.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM rig_notification_recipient r
		 JOIN todo_notification l ON l.notification_id = r.notification_id
		 WHERE r.tenant_id = $1 AND r.account_id = $2 AND l.todo_id = $3`,
		tenant, alex, item.ID).Scan(&actorLines); err != nil {
		t.Fatal(err)
	}
	if actorLines != 0 {
		t.Errorf("alex made the change and must not hear about it, got %d line(s)", actorLines)
	}

	// A plain edit is the other kind — the front end says a different sentence
	// about it.
	if res := api.do(t, request{
		method: http.MethodPatch, path: "/api/v1/todos/" + item.ID.String(), token: alexToken,
		body: map[string]any{"description": "now with more words"},
	}); res.status != http.StatusOK {
		t.Fatalf("alex's edit: %d %s", res.status, res.body)
	}
	if err := app.DispatchNotifications(context.Background(), api.pool); err != nil {
		t.Fatal(err)
	}

	var kinds []string
	rows, err := api.pool.Query(context.Background(), `
		SELECT r.kind FROM rig_notification_recipient r
		 JOIN todo_notification l ON l.notification_id = r.notification_id
		 WHERE r.tenant_id = $1 AND r.account_id = $2 AND l.todo_id = $3
		 ORDER BY r.created_at`,
		tenant, demo, item.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, k)
	}
	// Two lines rather than one grouped line, because the kinds differ — the
	// group key collapses repeats of one kind, not everything about a row.
	if len(kinds) != 2 || kinds[1] != todo.KindTodoUpdated {
		t.Errorf("kinds = %v, want [%s %s]", kinds, todo.KindTodoStatusChanged, todo.KindTodoUpdated)
	}
}

// The change the row cannot name an audience for: a steal.
//
// By the time the notification is sent the item belongs to whoever took it, so
// the person it was taken from is not a stakeholder in it any more — and losing
// your item is not the change to be quiet about. The previous holder rides in
// the notification's payload for exactly this, and NotifyWho reads it back.
func TestAStealNotifiesThePreviousHolder(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)

	demoToken := api.login(t, app.SeedEmail)
	demo := api.accountID(t, tenant, app.SeedEmail)
	alex := api.accountID(t, tenant, app.SeedEmail2)

	// Created by demo and held by alex, which is the shape two of the seeded
	// items have and the one that matters: once demo takes it, the creator and
	// the assignee are both demo, who made the change, so the row names nobody
	// left to tell.
	created := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todos", token: demoToken,
		body: map[string]any{"title": "Alex's item, until demo takes it", "assigneeAccountId": alex},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("create: %d %s", created.status, created.body)
	}
	var item struct {
		ID uuid.UUID `json:"id"`
	}
	created.decode(t, &item)

	if res := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todos/" + item.ID.String() + "/_claim",
		token: demoToken, body: map[string]any{"steal": true},
	}); res.status != http.StatusOK {
		t.Fatalf("steal: %d %s", res.status, res.body)
	}

	if err := app.DispatchNotifications(context.Background(), api.pool); err != nil {
		t.Fatal(err)
	}

	const q = `SELECT count(*) FROM rig_notification_recipient r
		 JOIN todo_notification l ON l.notification_id = r.notification_id
		 WHERE r.tenant_id = $1 AND r.account_id = $2 AND l.todo_id = $3`

	var lines int
	if err := api.pool.QueryRow(context.Background(), q, tenant, alex, item.ID).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if lines == 0 {
		t.Error("the person the item was taken from should hear that it was taken")
	}

	// And the thief still does not hear about their own change, which is the
	// rule the previous holder is an addition to rather than an exception from.
	var actorLines int
	if err := api.pool.QueryRow(context.Background(), q, tenant, demo, item.ID).Scan(&actorLines); err != nil {
		t.Fatal(err)
	}
	if actorLines != 0 {
		t.Errorf("demo took the item and must not hear about it, got %d line(s)", actorLines)
	}
}

//go:build docker

package main

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// The flagship flow, end to end over real SQL: a stranger registers, finds the
// invitation OnRegistered left, accepts it, and can use the board — which
// proves the hook, the provisioning, and the role grant in one pass.
func TestRegisteringLandsOnTheBoard(t *testing.T) {
	api := newServer(t)
	api.seed(t)

	address := "newcomer-" + uuid.New().String()[:8] + "@example.org"

	res := api.do(t, request{
		method: http.MethodPost, path: "/auth/register",
		body: map[string]any{"emailAddress": address, "displayName": "Newcomer", "password": SeedPassword},
	})
	if res.status != http.StatusCreated {
		t.Fatalf("register: %d %s", res.status, res.body)
	}
	var signedUp struct {
		IdentityToken string `json:"identityToken"`
		Tenants       []any  `json:"tenants"`
	}
	res.decode(t, &signedUp)
	if len(signedUp.Tenants) != 0 {
		t.Fatalf("registering must not join anything by itself: %s", res.body)
	}

	listed := api.do(t, request{method: http.MethodGet, path: "/auth/me/invitations", token: signedUp.IdentityToken})
	var page struct {
		Data []struct {
			ID         uuid.UUID `json:"id"`
			TenantID   uuid.UUID `json:"tenantId"`
			TenantName string    `json:"tenantName"`
		} `json:"data"`
	}
	listed.decode(t, &page)
	if len(page.Data) != 1 || page.Data[0].TenantID != uuid.MustParse(SeedTenantID) {
		t.Fatalf("expected the auto-invitation into the seeded tenant, got %s", listed.body)
	}
	if page.Data[0].TenantName != "LinearLite" {
		t.Errorf("the invitation should name the tenant, got %q", page.Data[0].TenantName)
	}

	joined := api.do(t, request{
		method: http.MethodPost, path: "/auth/me/invitations/accept", token: signedUp.IdentityToken,
		body: map[string]any{"invitationId": page.Data[0].ID},
	})
	if joined.status != http.StatusOK {
		t.Fatalf("accept: %d %s", joined.status, joined.body)
	}
	var session struct {
		AccessToken string `json:"accessToken"`
	}
	joined.decode(t, &session)

	// The role grant is what this read proves: an account with no permissions
	// would be refused, not answered.
	todos := api.do(t, request{method: http.MethodGet, path: "/api/v1/todos", token: session.AccessToken})
	if todos.status != http.StatusOK {
		t.Fatalf("a newcomer who accepted should read the board: %d %s", todos.status, todos.body)
	}
	var board struct {
		Data []struct{ Title string } `json:"data"`
	}
	todos.decode(t, &board)
	if len(board.Data) == 0 {
		t.Fatal("the seeded board should not be empty")
	}

	// And write to it: the member role carries todo.write.
	created := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todos", token: session.AccessToken,
		body: map[string]any{"title": "A newcomer's first item"},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("a member should write: %d %s", created.status, created.body)
	}
}

// The other way out of the picker: make a tenant of your own, and be able to
// use it immediately — which is TenantOptions.OnCreated seeding the roles in
// the transaction that made it.
func TestCreatingYourOwnTenant(t *testing.T) {
	api := newServer(t)
	api.seed(t)

	address := "founder-" + uuid.New().String()[:8] + "@example.org"
	res := api.do(t, request{
		method: http.MethodPost, path: "/auth/register",
		body: map[string]any{"emailAddress": address, "displayName": "Founder", "password": SeedPassword},
	})
	if res.status != http.StatusCreated {
		t.Fatalf("register: %d %s", res.status, res.body)
	}
	var signedUp struct {
		IdentityToken string `json:"identityToken"`
	}
	res.decode(t, &signedUp)

	made := api.do(t, request{
		method: http.MethodPost, path: "/auth/tenants", token: signedUp.IdentityToken,
		body: map[string]any{"name": "Founders Inc"},
	})
	if made.status != http.StatusOK {
		t.Fatalf("create a tenant: %d %s", made.status, made.body)
	}
	var out struct {
		AccessToken string `json:"accessToken"`
		Tenants     []struct {
			Role    string `json:"role"`
			Current bool   `json:"current"`
		} `json:"tenants"`
	}
	made.decode(t, &out)
	if len(out.Tenants) == 0 || out.Tenants[0].Role != "Owner" {
		t.Fatalf("making a tenant should make an Owner: %s", made.body)
	}

	// An Owner in a brand-new tenant writes immediately, and sees an empty
	// board rather than the seeded tenant's — the tenancy scoping in one
	// assertion.
	created := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/todos", token: out.AccessToken,
		body: map[string]any{"title": "First item in a fresh tenant"},
	})
	if created.status != http.StatusCreated {
		t.Fatalf("a fresh Owner should write: %d %s", created.status, created.body)
	}
	todos := api.do(t, request{method: http.MethodGet, path: "/api/v1/todos", token: out.AccessToken})
	var board struct {
		Data []struct{ Title string } `json:"data"`
	}
	todos.decode(t, &board)
	if len(board.Data) != 1 {
		t.Fatalf("a fresh tenant sees only its own rows, got %d", len(board.Data))
	}
}

// The exposed account resource: members are listable — the board's assignee
// names come from here — and read-only, because provisioning belongs to the
// auth endpoints.
func TestMembersAreListableAndReadOnly(t *testing.T) {
	api := newServer(t)
	api.seed(t)
	token := api.login(t, SeedEmail)

	res := api.do(t, request{method: http.MethodGet, path: "/api/v1/accounts", token: token})
	if res.status != http.StatusOK {
		t.Fatalf("list accounts: %d %s", res.status, res.body)
	}
	var page struct {
		Data []struct {
			DisplayName string `json:"displayName"`
		} `json:"data"`
	}
	res.decode(t, &page)
	if len(page.Data) < 2 {
		t.Fatalf("both seeded people should be listed, got %s", res.body)
	}

	// operations: [Get, List] — a write is not a 403 but a route that does not
	// exist.
	if res := api.do(t, request{
		method: http.MethodPost, path: "/api/v1/accounts", token: token,
		body: map[string]any{"displayName": "Intruder"},
	}); res.status != http.StatusNotFound && res.status != http.StatusMethodNotAllowed {
		t.Fatalf("creating an account over the resource: %d %s, want no such route", res.status, res.body)
	}
}

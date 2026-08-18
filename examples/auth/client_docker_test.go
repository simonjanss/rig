//go:build docker

// The generated Go client against an API with authentication.
//
//	rig db up && go test -tags docker ./...
//
// auth_docker_test.go drives the endpoints by hand, which is how the server's
// own behaviour is pinned down. This is the other side: what somebody holding
// the SDK writes. Signing in, being handed a session that keeps itself fresh,
// switching tenant, minting a key and calling as it — all of it through methods
// rather than through URLs.
package main

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/auth/client"
	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/authwire"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// newSDK builds a client against the example's own server, in the seeded tenant.
//
// now is the client's clock. A test that has to cross a ten-minute token expiry
// moves it rather than waiting: the token the server issued is still good, and
// what is being checked is that the client renews before it stops being.
func newSDK(t *testing.T, now func() time.Time) (*client.Client, *server, uuid.UUID) {
	t.Helper()

	srv := newServer(t)
	tenant := srv.seed(t)

	c, err := client.New(rigclient.Config{BaseURL: srv.http.URL, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	return c, srv, tenant
}

// signIn is the whole sign-in, as an SDK caller writes it: one call, and from
// here on every request carries the session.
func signIn(t *testing.T, c *client.Client, tenant uuid.UUID) *authwire.SignInResponse {
	t.Helper()

	res, err := c.Auth.SignIn(t.Context(), authwire.LoginRequest{
		EmailAddress: SeedEmail,
		Password:     SeedPassword,
	}, c.Auth.WithTenant(tenant))
	if err != nil {
		t.Fatal(err)
	}
	if res.AccessToken == "" {
		t.Fatal("the sign-in came back without a session")
	}
	return res
}

func TestSDKSignsInAndCalls(t *testing.T) {
	c, _, tenant := newSDK(t, nil)
	ctx := t.Context()

	res := signIn(t, c, tenant)

	// The session is installed, so nothing below mentions a token.
	if _, ok := c.Runtime().Session(); !ok {
		t.Fatal("signing in did not install a session")
	}
	if res.IdentityToken == "" {
		t.Error("no identity token, so there is nothing to draw a tenant picker with")
	}

	note, err := c.Notes.Create(ctx, client.NoteCreateInput{Title: "from the SDK"})
	if err != nil {
		t.Fatal(err)
	}

	got, err := c.Notes.Get(ctx, note.ID, client.NoteGetQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "from the SDK" {
		t.Errorf("title = %q", got.Title)
	}

	// A read defaults to the caller's own rows; asking for the tenant's needs a
	// permission, and the seeded owner holds it.
	all, err := c.Notes.List(ctx, client.NoteListQuery{Scope: rigclient.P(tenancy.ScopeAll)})
	if err != nil {
		t.Fatalf("scope=all was refused for somebody holding %s: %v",
			client.PermissionNoteReadAll, err)
	}
	if all.Pagination.Total == 0 {
		t.Error("the tenant-wide read found nothing")
	}

	if err := c.Auth.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Runtime().Credential() != nil {
		t.Error("logging out left the credential in place")
	}
}

// The point of putting the lifetimes in the document: the client renews before
// the token expires rather than after a request has been refused for it.
func TestSDKRefreshesAheadOfExpiry(t *testing.T) {
	// A clock the test owns, starting now and moved forward by hand.
	base := time.Now()
	offset := time.Duration(0)
	c, _, tenant := newSDK(t, func() time.Time { return base.Add(offset) })
	ctx := t.Context()

	signIn(t, c, tenant)
	session, _ := c.Runtime().Session()
	first := session.Tokens().AccessToken

	// A call while the token is fresh changes nothing.
	if _, err := c.Notes.List(ctx, client.NoteListQuery{}); err != nil {
		t.Fatal(err)
	}
	if session.Tokens().AccessToken != first {
		t.Fatal("the session refreshed while its token was fresh")
	}

	// Move to inside the rotation leeway. The server still considers the token
	// valid, so a refresh here is the client acting early rather than recovering.
	offset = client.AuthProfile.AccessTTL - client.AuthProfile.RotationLeeway/2

	if _, err := c.Notes.List(ctx, client.NoteListQuery{}); err != nil {
		t.Fatal(err)
	}
	if session.Tokens().AccessToken == first {
		t.Error("the session did not renew inside the leeway")
	}
	if session.Tokens().ExpiresAt.Before(base.Add(offset)) {
		t.Error("the renewed token is already expired")
	}
}

// An API key is the other credential: minted through the SDK, presented by it,
// and dead the moment it is revoked.
func TestSDKMintsAndUsesAnAPIKey(t *testing.T) {
	c, srv, tenant := newSDK(t, nil)
	ctx := t.Context()

	signIn(t, c, tenant)

	minted, err := c.Auth.CreateAPIKey(ctx, authwire.CreateKeyRequest{
		Name:   "the nightly import",
		Scopes: []string{client.PermissionNoteRead, client.PermissionNoteWrite},
	})
	if err != nil {
		t.Fatal(err)
	}
	if minted.Secret == "" {
		t.Fatal("a minted key came back without its secret, which is shown only once")
	}

	// A second client, holding only the key. This is what an integration is.
	integration, err := client.New(rigclient.Config{
		BaseURL:    srv.http.URL,
		Credential: rigclient.APIKey(minted.Secret),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := integration.Notes.Create(ctx, client.NoteCreateInput{
		Title: "written by an integration",
	}); err != nil {
		t.Fatal(err)
	}

	// Revoked through the signed-in client, and the key stops working at once.
	if err := c.Auth.RevokeAPIKey(ctx, minted.Key.ID); err != nil {
		t.Fatal(err)
	}
	_, err = integration.Notes.List(ctx, client.NoteListQuery{})
	if !rigclient.IsUnauthorized(err) {
		t.Fatalf("err = %v, want a revoked key to be refused", err)
	}

	// And the key is listed as revoked rather than gone.
	keys, err := c.Auth.APIKeys(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, key := range keys {
		if key.ID == minted.Key.ID {
			found = true
			if key.RevokedAt == nil {
				t.Error("the key is not marked revoked")
			}
		}
	}
	if !found {
		t.Error("the revoked key is not in the list")
	}
}

// Somebody signs in, makes a second tenant, and moves between them. Both halves
// are the SDK's: the picker reads through the identity token, and a switch
// replaces the session in place so the calls after it need no new argument.
func TestSDKSwitchesTenant(t *testing.T) {
	c, _, tenant := newSDK(t, nil)
	ctx := t.Context()

	res := signIn(t, c, tenant)

	if _, err := c.Notes.Create(ctx, client.NoteCreateInput{Title: "in the first tenant"}); err != nil {
		t.Fatal(err)
	}

	// Making a tenant is a thing the holder of an identity token does, and it
	// signs them into what they made.
	made, err := c.Auth.CreateTenant(ctx, res.IdentityToken, authwire.CreateTenantRequest{
		Name: "Second " + uuid.New().String()[:8],
	})
	if err != nil {
		t.Fatal(err)
	}

	// The picker now shows both, read with the identity token rather than with
	// either session.
	tenants, err := c.Auth.MyTenants(ctx, res.IdentityToken)
	if err != nil {
		t.Fatal(err)
	}
	if len(tenants) < 2 {
		t.Fatalf("the picker shows %d tenants, want both", len(tenants))
	}

	// A brand-new tenant holds nothing, which is the only thing worth asserting
	// about having moved.
	fresh, err := c.Notes.List(ctx, client.NoteListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Pagination.Total != 0 {
		t.Errorf("the new tenant already holds %d notes", fresh.Pagination.Total)
	}
	if made.AccessToken == "" {
		t.Error("making a tenant should sign the caller into it")
	}

	// And back, through the endpoint rather than by signing in again.
	if _, err := c.Auth.SwitchTenant(ctx, tenant); err != nil {
		t.Fatal(err)
	}
	back, err := c.Notes.List(ctx, client.NoteListQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if back.Pagination.Total == 0 {
		t.Error("the note written before the switch is not there after switching back")
	}
}

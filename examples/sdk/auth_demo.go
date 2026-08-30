package main

import (
	"context"
	"fmt"
	"time"

	"github.com/simonjanss/rig/examples/auth/client"
	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/authwire"
)

// authDemo signs in and works as the person, then as a key they mint.
//
// The thing to notice is what is missing. After the sign-in nothing mentions a
// token: the session is the client's credential, and it renews itself before the
// access token expires — using the lifetimes in AuthProfile, which the generator
// took from the same rig.yaml the server enforces.
func authDemo(ctx context.Context, args []string) error {
	baseURL := client.DefaultBaseURL
	set := flags("auth", args, &baseURL)
	email := set.String("email", "ada@example.com", "who to sign in as")
	password := set.String("password", "correct horse battery staple", "their password")
	tenantSlug := set.String("tenant", "", "the tenant to sign in to, when the server asks for one")
	if err := set.Parse(args); err != nil {
		return err
	}

	c, err := client.New(rigclient.Config{BaseURL: baseURL, UserAgent: "rig-sdk-demo/1"})
	if err != nil {
		return err
	}
	fmt.Printf("talking to %s\n", baseURL)

	step("what the client knows about this API's authentication")
	// None of this was written here. It is the project's configuration, carried
	// into the client so that it can act on the same numbers the server does.
	profile := client.AuthProfile
	detail("endpoints under %s", profile.BasePath)
	detail("an access token lasts %s, and is renewed %s before it expires",
		profile.AccessTTL, profile.RotationLeeway)
	detail("a session lasts %s, or %s when somebody asked to be remembered",
		profile.RefreshTTL, profile.RememberTTL)
	detail("registration open: %v; anybody may make a tenant: %v",
		profile.HasRegistration, profile.HasTenantCreation)

	step("sign in")
	var opts []rigclient.CallOption
	if *tenantSlug != "" {
		// A sign-in is the one call that cannot read the tenant from a
		// credential, because there is not one yet. How it is named is the
		// project's decision and it is in the profile; this example reads a
		// query parameter or a header.
		opts = append(opts, rigclient.WithHeader(profile.TenantHeader, *tenantSlug))
	}
	res, err := c.Auth.SignIn(ctx, authwire.LoginRequest{
		EmailAddress: *email,
		Password:     *password,
		Client:       "machine",
	}, opts...)
	if err != nil {
		return err
	}

	if res.AccessToken == "" {
		// Not a failure: somebody who belongs to no tenant yet gets an identity
		// token and a list to pick from. That is the tenant picker, and it is
		// the reason a sign-in answers with more than a pair of tokens.
		detail("signed in, but in no tenant yet")
		return pickATenant(ctx, c, res)
	}
	detail("signed in; the access token expires at %s", res.ExpiresAt.Format(time.TimeOnly))

	step("call as the session")
	// No token here, and none below. The credential is installed.
	note, err := c.Notes.Create(ctx, client.NoteCreateInput{
		Title: "written by the SDK demo",
		Body:  ptr("and read back on the next line"),
	})
	if err != nil {
		return err
	}
	got, err := c.Notes.Get(ctx, note.ID, client.NoteGetQuery{})
	if err != nil {
		return err
	}
	detail("%s  %q", got.ID, got.Title)

	step("the tenants this person belongs to")
	tenants, err := c.Auth.Tenants(ctx)
	if err != nil {
		return err
	}
	for _, t := range tenants {
		here := " "
		if t.Current {
			here = "*"
		}
		detail("%s %s  %s (%s)", here, t.TenantID, t.TenantName, t.Role)
	}

	step("mint an API key and call as it")
	// A key is what an integration holds. Its scopes cannot exceed what the
	// person minting it holds, which is why they are named from the generated
	// constants rather than typed as strings.
	minted, err := c.Auth.CreateAPIKey(ctx, authwire.CreateKeyRequest{
		Name:   "sdk demo",
		Scopes: []string{client.PermissionNoteRead, client.PermissionNoteWrite},
	})
	if err != nil {
		return err
	}
	detail("minted %s; the secret is shown exactly once", minted.Key.KeyID)

	integration, err := client.New(rigclient.Config{
		BaseURL:    baseURL,
		Credential: rigclient.APIKey(minted.Secret),
		UserAgent:  "rig-sdk-demo-integration/1",
	})
	if err != nil {
		return err
	}
	asKey, err := integration.Notes.Create(ctx, client.NoteCreateInput{Title: "written by a key"})
	if err != nil {
		return err
	}
	detail("the integration wrote %s", asKey.ID)

	step("revoke it")
	if err := c.Auth.RevokeAPIKey(ctx, minted.Key.ID); err != nil {
		return err
	}
	if _, err := integration.Notes.List(ctx, client.NoteListQuery{}); err != nil {
		if !rigclient.IsUnauthorized(err) {
			return err
		}
		detail("the revoked key is refused at once")
	}

	step("sign out")
	if err := c.Auth.Logout(ctx); err != nil {
		return err
	}
	detail("the session is ended and the client is holding nothing")

	fmt.Println("\ndone.")
	return nil
}

// pickATenant is what a client does for somebody who belongs to none yet: read
// the list with the identity token, and either join one or make one.
//
// It is a separate credential from a session on purpose — it proves who somebody
// is and names no tenant — which is why these calls take it explicitly.
func pickATenant(ctx context.Context, c *client.Client, res *authwire.SignInResponse) error {
	invitations, err := c.Auth.MyInvitations(ctx, res.IdentityToken)
	if err != nil {
		return err
	}
	for _, invite := range invitations {
		detail("invited to %s as %s", invite.TenantName, invite.Role)
	}

	if len(invitations) > 0 {
		step("accept the first invitation")
		joined, err := c.Auth.AcceptMyInvitation(ctx, res.IdentityToken, invitations[0].ID, "machine")
		if err != nil {
			return err
		}
		detail("now in %d tenant(s), and signed into one", len(joined.Tenants))
		return nil
	}

	if !client.AuthProfile.HasTenantCreation {
		detail("no invitations, and this project does not let people make their own tenant")
		return nil
	}

	step("make a tenant instead")
	made, err := c.Auth.CreateTenant(ctx, res.IdentityToken, authwire.CreateTenantRequest{
		Name: "SDK demo", Client: "machine",
	})
	if err != nil {
		return err
	}
	detail("made it, and signed into it; expires at %s", made.ExpiresAt.Format(time.TimeOnly))
	return nil
}

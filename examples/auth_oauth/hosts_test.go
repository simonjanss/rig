package main

import (
	"slices"
	"testing"

	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/examples/auth_oauth/internal/api"
)

// Which tenant a host names, and which origins a provider has to be told about.
//
// No database and no server: this is string arithmetic, and it is the part of the
// example a real deployment copies. It also has the two branches nothing else
// covers — a host with no tenant label in it, which is the only plain-http origin
// Google or Microsoft will register, and an IP address, whose leading label is a
// number rather than a tenant.
func TestWhichTenantAHostNames(t *testing.T) {
	for _, c := range []struct {
		host, defaultTenant, want string
	}{
		{host: "acme.localhost:8083", want: "acme"},
		{host: "beta.localhost:8083", want: "beta"},
		{host: "ACME.localhost", want: "acme"},
		{host: "acme.example.com", want: "acme"},

		// No label to read. Nothing, unless the environment named a tenant for it.
		{host: "localhost:8083", want: ""},
		{host: "localhost:8083", defaultTenant: "acme", want: "acme"},

		// An address is not a name with a subdomain in front of it.
		{host: "127.0.0.1:8083", want: ""},
		{host: "127.0.0.1:8083", defaultTenant: "beta", want: "beta"},
		{host: "[::1]:8083", want: ""},
	} {
		t.Setenv("DEFAULT_TENANT", c.defaultTenant)
		if got := api.TenantSlug(c.host); got != c.want {
			t.Errorf("TenantSlug(%q) with DEFAULT_TENANT=%q = %q, want %q",
				c.host, c.defaultTenant, got, c.want)
		}
	}
}

func TestTheOriginsAProviderHasToRegister(t *testing.T) {
	for _, c := range []struct {
		base string
		want []string
	}{
		// A tenant per host: both, because a callback comes back to the host it
		// started at and a provider will refuse an origin it was not given.
		{
			base: "http://acme.localhost:8083",
			want: []string{"http://acme.localhost:8083", "http://beta.localhost:8083"},
		},
		{
			base: "https://beta.example.com",
			want: []string{"https://beta.example.com", "https://acme.example.com"},
		},

		// One host, one origin. There is no sibling to swap a label with, and
		// inventing one would print an address nothing answers at.
		{base: "http://localhost:8083", want: []string{"http://localhost:8083"}},
		{base: "http://127.0.0.1:8083", want: []string{"http://127.0.0.1:8083"}},
	} {
		if got := tenantOrigins(c.base); !slices.Equal(got, c.want) {
			t.Errorf("tenantOrigins(%q) = %v, want %v", c.base, got, c.want)
		}
	}
}

// The addresses a reader pastes into a provider's console.
//
// Derived from the auth package's own base path rather than written down, so this
// asserts the shape rather than restating it: a README cannot be kept honest, and a
// mismatched redirect URI is refused by the provider with a message that names
// nothing useful.
func TestTheCallbackURL(t *testing.T) {
	got := callbackURL("https://acme.example.com", oauth.Provider{Name: oauth.ProviderGoogle})
	want := "https://acme.example.com/auth/oauth/google/callback"
	if got != want {
		t.Errorf("callbackURL = %q, want %q", got, want)
	}
}

package project_test

import (
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/pkg/ir"
)

// The whole point of the block is that a number written here is the number
// everything downstream reads, so an unset value has to resolve to rig/auth's
// own default rather than to a zero somebody else has to interpret.
func TestAuthDefaultsAreResolved(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal+"auth:\n  enabled: true\n"))
	if diags.HasErrors() {
		t.Fatalf("enabling auth and configuring nothing should be valid:\n%s", diags.String())
	}

	a := p.Config.Auth
	if a.BasePath != "/auth" {
		t.Errorf("base_path = %q, want /auth", a.BasePath)
	}
	if got := a.Tenant.From; len(got) != 1 || got[0] != "header" {
		t.Errorf("tenant.from = %v, want [header]", got)
	}
	if a.Tenant.Header != "X-Tenant-Id" {
		t.Errorf("tenant.header = %q", a.Tenant.Header)
	}
	// The query parameter is not defaulted, because nothing reads it: a name in
	// the document that no source consults reads as though one does.
	if a.Tenant.Query != "" {
		t.Errorf("tenant.query = %q, want empty when the query source is unused", a.Tenant.Query)
	}

	for _, c := range []struct {
		what string
		got  project.Duration
		want time.Duration
	}{
		{"access_ttl", a.Session.AccessTTL, 10 * time.Minute},
		{"refresh_ttl", a.Session.RefreshTTL, 12 * time.Hour},
		{"remember_ttl", a.Session.RememberTTL, 30 * 24 * time.Hour},
		{"rotation_leeway", a.Session.RotationLeeway, 30 * time.Second},
		{"identity_ttl", a.Session.IdentityTTL, 30 * time.Minute},
		// Zero is the documented default: the token row is read on every request,
		// which is what makes revocation immediate.
		{"cache_ttl", a.Session.CacheTTL, 0},
	} {
		if c.got.Duration() != c.want {
			t.Errorf("session.%s = %s, want %s", c.what, c.got, c.want)
		}
	}

	if a.Password.MinLength != 12 || a.Password.MaxLength != 1024 {
		t.Errorf("password policy = %+v", a.Password)
	}
	if a.Limits.LoginByEmail.Max != 5 || a.Limits.LoginByEmail.Window.Duration() != 15*time.Minute {
		t.Errorf("login_by_email = %+v", a.Limits.LoginByEmail)
	}
	if a.Limits.LoginByIP.Max != 50 {
		t.Errorf("login_by_ip.max = %d, want 50", a.Limits.LoginByIP.Max)
	}
}

// A partly configured limit keeps the default for the half nobody wrote, so
// tightening a threshold does not silently reset its window to zero.
func TestAuthLimitsCanBeOverriddenInPart(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal+
		"auth:\n  enabled: true\n  limits:\n    login_by_email: {max: 3}\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}

	got := p.Config.Auth.Limits.LoginByEmail
	if got.Max != 3 {
		t.Errorf("max = %d, want the configured 3", got.Max)
	}
	if got.Window.Duration() != 15*time.Minute {
		t.Errorf("window = %s, want the default 15m", got.Window)
	}
}

// Nothing in the block is read for a project that never turned it on, so a block
// somebody filled in and left off is refused rather than quietly ignored.
func TestAuthConfiguredButNotEnabled(t *testing.T) {
	t.Parallel()

	_, diags := project.Parse("rig.yaml", []byte(minimal+
		"auth:\n  session:\n    access_ttl: 2m\n"))
	if !diags.HasErrors() {
		t.Fatal("a configured but disabled auth block should be an error")
	}
	if !strings.Contains(diags.String(), "enabled: true") {
		t.Errorf("the message should say how to fix it:\n%s", diags.String())
	}
}

// expose and own are about generation and mean something in a project with no
// authentication at all, so they must not trip that check.
func TestAuthTableKeysDoNotCountAsConfiguration(t *testing.T) {
	t.Parallel()

	_, diags := project.Parse("rig.yaml", []byte(minimal+"auth:\n  expose: [account]\n"))
	if diags.HasErrors() {
		t.Errorf("expose without enabled should be fine:\n%s", diags.String())
	}
}

// Each of these would work and behave as nobody intended, which is why they are
// refused rather than adjusted.
func TestAuthSessionCombinationsThatCannotBeMeant(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ name, session, want string }{
		{
			name:    "an access token that outlives its session",
			session: "access_ttl: 20m\n    refresh_ttl: 10m",
			want:    "outlives refresh_ttl",
		},
		{
			name:    "remember me that shortens a session",
			session: "refresh_ttl: 12h\n    remember_ttl: 1h",
			want:    "shorter than refresh_ttl",
		},
		{
			name:    "a leeway a consumed token never leaves",
			session: "refresh_ttl: 30s\n    rotation_leeway: 30s",
			want:    "never stops being usable",
		},
		{
			name:    "a cache longer than the token it caches",
			session: "access_ttl: 5m\n    cache_ttl: 10m",
			want:    "longer than access_ttl",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			_, diags := project.Parse("rig.yaml", []byte(minimal+
				"auth:\n  enabled: true\n  session:\n    "+c.session+"\n"))
			if !diags.HasErrors() {
				t.Fatalf("%s should be refused", c.name)
			}
			if !strings.Contains(diags.String(), c.want) {
				t.Errorf("expected a message about %q:\n%s", c.want, diags.String())
			}
		})
	}
}

func TestAuthOAuthValidation(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ name, oauth, want string }{
		{
			name:  "no origin to build a callback URL from",
			oauth: "providers:\n      - name: google",
			want:  "needs an origin",
		},
		{
			name:  "an origin with a trailing slash",
			oauth: "base_url: https://app.example.com/\n    providers:\n      - name: google",
			want:  "must not end in a slash",
		},
		{
			name: "one provider twice",
			oauth: "base_url: https://app.example.com\n    providers:\n" +
				"      - name: google\n      - name: google",
			want: "already configured",
		},
		{
			name: "Microsoft's directory on somebody else's provider",
			oauth: "base_url: https://app.example.com\n    providers:\n" +
				"      - name: google\n        tenant_env: WORK",
			want: "means nothing to",
		},
		{
			name:  "providers nobody listed",
			oauth: "base_url: https://app.example.com",
			want:  "lists no providers",
		},
		{
			name:  "an origin per host without the host source",
			oauth: "base_url: https://app.example.com\n    origin_from_host: true\n    providers:\n      - name: google",
			want:  "include host",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			_, diags := project.Parse("rig.yaml", []byte(minimal+
				"auth:\n  enabled: true\n  oauth:\n    "+c.oauth+"\n"))
			if !diags.HasErrors() {
				t.Fatalf("%s should be refused", c.name)
			}
			if !strings.Contains(diags.String(), c.want) {
				t.Errorf("expected a message about %q:\n%s", c.want, diags.String())
			}
		})
	}
}

// A client secret is never in the file, so the variable names are what the
// generated code reads — and they have to be derivable from the provider.
func TestAuthProviderEnvironmentDefaults(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal+
		"auth:\n  enabled: true\n  oauth:\n    base_url: https://app.example.com\n"+
		"    providers:\n      - name: microsoft\n      - name: github\n"+
		"        client_id_env: GH_APP_ID\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}

	got := p.Config.Auth.OAuth.Providers
	if got[0].ClientIDEnv != "MICROSOFT_CLIENT_ID" || got[0].ClientSecretEnv != "MICROSOFT_CLIENT_SECRET" {
		t.Errorf("microsoft env defaults wrong: %+v", got[0])
	}
	if got[0].TenantEnv != "MICROSOFT_TENANT" {
		t.Errorf("microsoft tenant_env = %q", got[0].TenantEnv)
	}
	if got[1].ClientIDEnv != "GH_APP_ID" {
		t.Errorf("an explicit client_id_env should win, got %q", got[1].ClientIDEnv)
	}
	if got[1].ClientSecretEnv != "GITHUB_CLIENT_SECRET" {
		t.Errorf("the other half should still default, got %q", got[1].ClientSecretEnv)
	}
	// tenant_env belongs to Microsoft alone, so it is not invented for anybody else.
	if got[1].TenantEnv != "" {
		t.Errorf("github tenant_env = %q, want empty", got[1].TenantEnv)
	}
}

func TestTrustedProxiesMustBeCIDR(t *testing.T) {
	t.Parallel()

	_, diags := project.Parse("rig.yaml", []byte(minimal+
		"auth:\n  enabled: true\n  trusted_proxies: [10.0.0.1]\n"))
	if !diags.HasErrors() {
		t.Fatal("a bare address is not a range and should be refused")
	}
	if !strings.Contains(diags.String(), "not a CIDR range") {
		t.Errorf("unexpected message:\n%s", diags.String())
	}
}

// A project with no authentication carries no auth block in the document, which
// is what tells a generator there is nothing to describe.
func TestAuthIRIsAbsentWhenDisabled(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}
	if p.Config.Auth.IR() != nil {
		t.Error("a project with no authentication should project no auth block")
	}
}

func TestAuthIRCarriesEveryConfiguredValue(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal+
		"auth:\n  enabled: true\n  base_path: /identity\n"+
		"  allow_registration: true\n  require_verified_email: true\n"+
		"  session:\n    access_ttl: 90s\n"+
		"  password:\n    breach_check: true\n"+
		"  trusted_proxies: [10.0.0.0/8]\n"+
		"  tenant:\n    from: [host, hook]\n"+
		"  oauth:\n    base_url: https://app.example.com\n    state_ttl: 4m\n"+
		"    providers:\n      - name: google\n        required: true\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}

	a := p.Config.Auth.IR()
	if a == nil {
		t.Fatal("an enabled auth block should project one")
	}
	if a.BasePath != "/identity" {
		t.Errorf("base_path = %q", a.BasePath)
	}
	if !a.AllowRegistration || !a.RequireVerifiedEmail || a.AllowTenantCreation {
		t.Errorf("flags wrong: %+v", a)
	}
	if a.Session.AccessTTL.Duration() != 90*time.Second {
		t.Errorf("access_ttl = %s", a.Session.AccessTTL)
	}
	if !a.Password.BreachCheck {
		t.Error("breach_check should carry through")
	}
	if !a.Tenant.Uses(ir.TenantFromHost) || !a.Tenant.Uses(ir.TenantFromHook) {
		t.Errorf("tenant sources = %v", a.Tenant.Sources)
	}
	if a.Tenant.Uses(ir.TenantFromHeader) {
		t.Error("the header source was not configured")
	}
	if len(a.TrustedProxies) != 1 || a.TrustedProxies[0] != "10.0.0.0/8" {
		t.Errorf("trusted_proxies = %v", a.TrustedProxies)
	}
	if a.OAuth == nil {
		t.Fatal("a configured provider should project an oauth block")
	}
	if a.OAuth.StateTTL.Duration() != 4*time.Minute {
		t.Errorf("state_ttl = %s", a.OAuth.StateTTL)
	}
	if len(a.OAuth.Providers) != 1 || !a.OAuth.Providers[0].Required {
		t.Errorf("providers = %+v", a.OAuth.Providers)
	}
}

// No providers means no provider routes, so the document says so by carrying no
// oauth block at all rather than an empty one.
func TestAuthIROmitsOAuthWithoutProviders(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal+"auth:\n  enabled: true\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected diagnostics:\n%s", diags.String())
	}
	if a := p.Config.Auth.IR(); a.OAuth != nil {
		t.Errorf("oauth = %+v, want nil", a.OAuth)
	}
}

func TestDurationRejectsNonsense(t *testing.T) {
	t.Parallel()

	// The schema's pattern catches this before the decoder does, which is what
	// puts the cursor on the line rather than on the file.
	_, diags := project.Parse("rig.yaml", []byte(minimal+
		"auth:\n  enabled: true\n  session:\n    access_ttl: soon\n"))
	if !diags.HasErrors() {
		t.Fatal("\"soon\" is not a duration")
	}
}

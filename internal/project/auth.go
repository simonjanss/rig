package project

import (
	"net/netip"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/pkg/ir"
)

// Configured reports whether the authentication block says anything beyond how
// the foundation's tables are treated.
//
// `expose` and `own` are about generation and mean something in a project with
// no authentication at all, so they do not count as having configured one.
func (a Auth) Configured() bool {
	bare := Auth{Enabled: a.Enabled, Expose: a.Expose, Own: a.Own}
	return !reflect.DeepEqual(a, bare)
}

// checkAuth validates what the JSON Schema cannot: values that only make sense
// relative to each other, and a block nothing reads.
func (p *Project) checkAuth() diag.List {
	var diags diag.List
	a := p.Config.Auth

	if !a.Enabled {
		// A block somebody filled in and never turned on is the one failure mode
		// worth refusing outright. Every value in it would be silently unread —
		// including a shortened token lifetime, which is exactly the kind of thing
		// somebody would believe they had configured.
		if a.Configured() {
			diags.Add(diag.CodeConfigInvalid, p.At("auth", "enabled"),
				"auth is configured but auth.enabled is false, so none of it is read; "+
					"set `enabled: true` or remove the block")
		}
		return diags
	}

	diags.Append(p.checkAuthTenant(a.Tenant))
	diags.Append(p.checkAuthSession(a.Session))

	if a.Password.MinLength > a.Password.MaxLength {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "password", "min_length"),
			"auth.password.min_length (%d) is above max_length (%d), so no password is acceptable",
			a.Password.MinLength, a.Password.MaxLength)
	}

	for i, cidr := range a.TrustedProxies {
		if _, err := netip.ParsePrefix(cidr); err != nil {
			diags.Add(diag.CodeConfigInvalid, p.At("auth", "trusted_proxies", itoa(i)),
				"auth.trusted_proxies[%d]: %q is not a CIDR range: %v", i, cidr, err)
		}
	}

	diags.Append(p.checkAuthOAuth(a))
	return diags
}

func (p *Project) checkAuthTenant(t AuthTenant) diag.List {
	var diags diag.List

	seen := make(map[string]bool, len(t.From))
	for i, source := range t.From {
		if seen[source] {
			diags.Add(diag.CodeConfigInvalid, p.At("auth", "tenant", "from", itoa(i)),
				"auth.tenant.from lists %q twice", source)
			continue
		}
		seen[source] = true
	}

	// A slug to fall back to is only reachable through the host source, which is
	// the only one that has a host to fail to read a tenant out of.
	if t.DefaultSlugEnv != "" && !seen[string(ir.TenantFromHost)] {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "tenant", "default_slug_env"),
			"auth.tenant.default_slug_env only applies to the host source; add host to auth.tenant.from")
	}

	return diags
}

func (p *Project) checkAuthSession(s AuthSession) diag.List {
	var diags diag.List

	// Each of these is a configuration that would work and behave as nobody
	// intended, which is why they are refused rather than adjusted.
	if s.AccessTTL > s.RefreshTTL {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "session", "access_ttl"),
			"auth.session.access_ttl (%s) outlives refresh_ttl (%s), so the session ends "+
				"while its access token is still valid", s.AccessTTL, s.RefreshTTL)
	}
	if s.RememberTTL < s.RefreshTTL {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "session", "remember_ttl"),
			"auth.session.remember_ttl (%s) is shorter than refresh_ttl (%s), so asking to "+
				"stay signed in would shorten the session", s.RememberTTL, s.RefreshTTL)
	}
	if s.RotationLeeway >= s.RefreshTTL {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "session", "rotation_leeway"),
			"auth.session.rotation_leeway (%s) is not shorter than refresh_ttl (%s), so a "+
				"consumed refresh token never stops being usable", s.RotationLeeway, s.RefreshTTL)
	}
	if s.CacheTTL > s.AccessTTL {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "session", "cache_ttl"),
			"auth.session.cache_ttl (%s) is longer than access_ttl (%s), which caches a "+
				"token for longer than it is valid", s.CacheTTL, s.AccessTTL)
	}

	return diags
}

func (p *Project) checkAuthOAuth(a Auth) diag.List {
	var diags diag.List
	o := a.OAuth

	if len(o.Providers) == 0 {
		// Nothing is mounted, so nothing here can be wrong — except a block that
		// was configured for providers nobody listed.
		if o.BaseURL != "" || o.AllowProvisioning || len(o.AllowedReturnTo) > 0 {
			diags.Add(diag.CodeConfigInvalid, p.At("auth", "oauth", "providers"),
				"auth.oauth is configured but lists no providers, so no provider routes are mounted")
		}
		return diags
	}

	// A callback URL is absolute and registered with the provider, so an origin
	// has to come from somewhere and there is no default anybody could guess.
	// origin_from_host is not an alternative: it overrides the origin per request,
	// and rig/auth still wants the configured one to build the routes from.
	if o.BaseURL == "" && o.BaseURLEnv == "" {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "oauth", "base_url"),
			"auth.oauth needs an origin the callback URL is built from: set base_url, "+
				"or base_url_env for an origin that differs per deployment")
	}
	if o.BaseURL != "" {
		if !strings.HasPrefix(o.BaseURL, "http://") && !strings.HasPrefix(o.BaseURL, "https://") {
			diags.Add(diag.CodeConfigInvalid, p.At("auth", "oauth", "base_url"),
				"auth.oauth.base_url must be an absolute origin, for example https://app.example.com")
		}
		if strings.HasSuffix(o.BaseURL, "/") {
			diags.Add(diag.CodeConfigInvalid, p.At("auth", "oauth", "base_url"),
				"auth.oauth.base_url must not end in a slash: a provider compares the "+
					"callback URL exactly")
		}
	}

	// origin_from_host reads the tenant out of the host too, or it is deriving an
	// origin for a tenant nothing resolved.
	if o.OriginFromHost && !a.Tenant.Uses(ir.TenantFromHost) {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "oauth", "origin_from_host"),
			"auth.oauth.origin_from_host serves a tenant per host, so auth.tenant.from has "+
				"to include host")
	}

	seen := make(map[string]int, len(o.Providers))
	for i := range o.Providers {
		pr := o.Providers[i]
		at := p.At("auth", "oauth", "providers", itoa(i), "name")

		if prev, dup := seen[pr.Name]; dup {
			diags.Add(diag.CodeConfigInvalid, at,
				"provider %q is already configured at auth.oauth.providers.%d; its routes "+
					"would collide", pr.Name, prev)
			continue
		}
		seen[pr.Name] = i

		if pr.TenantEnv != "" && pr.Name != ir.ProviderMicrosoft {
			diags.Add(diag.CodeConfigInvalid, p.At("auth", "oauth", "providers", itoa(i), "tenant_env"),
				"tenant_env is Microsoft's own directory and means nothing to %q", pr.Name)
		}
		if pr.ClientIDEnv == pr.ClientSecretEnv {
			diags.Add(diag.CodeConfigInvalid, p.At("auth", "oauth", "providers", itoa(i), "client_id_env"),
				"provider %q reads its id and its secret from the same variable %q",
				pr.Name, pr.ClientIDEnv)
		}
	}

	return diags
}

// Uses reports whether a tenant source is configured.
func (t AuthTenant) Uses(source ir.AuthTenantSource) bool {
	return slices.Contains(t.From, string(source))
}

// IR projects the configuration into the document's own shape.
//
// It returns nil for a project with no authentication, which is what tells a
// generator there is nothing to describe. Every value is already resolved, so
// this is a translation and not a second place where defaults are decided.
func (a Auth) IR() *ir.Auth {
	if !a.Enabled {
		return nil
	}

	out := &ir.Auth{
		BasePath: a.BasePath,
		Tenant: ir.AuthTenant{
			Header:         a.Tenant.Header,
			Query:          a.Tenant.Query,
			DefaultSlugEnv: a.Tenant.DefaultSlugEnv,
		},
		Session: ir.AuthSession{
			AccessTTL:      a.Session.AccessTTL.IR(),
			RefreshTTL:     a.Session.RefreshTTL.IR(),
			RememberTTL:    a.Session.RememberTTL.IR(),
			RotationLeeway: a.Session.RotationLeeway.IR(),
			IdentityTTL:    a.Session.IdentityTTL.IR(),
			CacheTTL:       a.Session.CacheTTL.IR(),
		},
		Password: ir.AuthPassword{
			MinLength:   a.Password.MinLength,
			MaxLength:   a.Password.MaxLength,
			BreachCheck: a.Password.BreachCheck,
		},
		Limits: ir.AuthLimits{
			LoginByEmail:       a.Limits.LoginByEmail.IR(),
			LoginByIP:          a.Limits.LoginByIP.IR(),
			PasswordReset:      a.Limits.PasswordReset.IR(),
			VerificationResend: a.Limits.VerificationResend.IR(),
			Refresh:            a.Limits.Refresh.IR(),
			APIKeyFailures:     a.Limits.APIKeyFailures.IR(),
		},
		AllowRegistration:    a.AllowRegistration,
		AllowTenantCreation:  a.AllowTenantCreation,
		RequireVerifiedEmail: a.RequireVerifiedEmail,
		TrustedProxies:       slices.Clone(a.TrustedProxies),
	}

	for _, source := range a.Tenant.From {
		out.Tenant.Sources = append(out.Tenant.Sources, ir.AuthTenantSource(source))
	}

	if len(a.OAuth.Providers) > 0 {
		out.OAuth = &ir.AuthOAuth{
			BaseURL:           a.OAuth.BaseURL,
			BaseURLEnv:        a.OAuth.BaseURLEnv,
			OriginFromHost:    a.OAuth.OriginFromHost,
			SigningKeyEnv:     a.OAuth.SigningKeyEnv,
			StateTTL:          a.OAuth.StateTTL.IR(),
			AllowProvisioning: a.OAuth.AllowProvisioning,
			AllowedReturnTo:   slices.Clone(a.OAuth.AllowedReturnTo),
			Insecure:          a.OAuth.Insecure,
		}
		for _, pr := range a.OAuth.Providers {
			out.OAuth.Providers = append(out.OAuth.Providers, ir.AuthProvider{
				Name:            pr.Name,
				ClientIDEnv:     pr.ClientIDEnv,
				ClientSecretEnv: pr.ClientSecretEnv,
				TenantEnv:       pr.TenantEnv,
				Required:        pr.Required,
			})
		}
	}

	return out
}

// IR projects one limit.
func (l AuthLimit) IR() ir.AuthLimit {
	return ir.AuthLimit{Max: l.Max, Window: l.Window.IR()}
}

// itoa is the index in a diagnostic path, which is a string like every other
// segment.
func itoa(i int) string { return strconv.Itoa(i) }

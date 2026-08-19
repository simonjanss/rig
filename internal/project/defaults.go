package project

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/auth/password"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/pkg/ir"
	"github.com/simonjanss/rig/runtime/throttle"
)

// Defaults every project inherits. They are the shape `rig init` writes, so a
// generated rig.yaml can stay short: anything left out here means the same
// thing as spelling it out.
const (
	DefaultTableDir   = "services/{table}"
	DefaultConfigFile = "{table_dir}/{table}.yaml"

	DefaultAPIVersion   = "v1"
	DefaultSearchMethod = SearchBoth
	DefaultOpenAPI      = "3.1"

	// DefaultRevisionHeader carries the API revision, in both directions.
	//
	// This is the one place the name is decided. Both ends of the conversation
	// are generated from the document that records it, so the server reading it
	// and the client sending it cannot disagree — rigclient carries the same
	// literal only as a fallback for a client somebody built by hand.
	DefaultRevisionHeader = "API-Revision"

	DefaultImage    = "postgres:17-alpine"
	DefaultPort     = 55432
	DefaultDBName   = "rig"
	DefaultDBUser   = "rig"
	DefaultDBPass   = "rig"
	DefaultDBSchema = "public"

	DefaultMigrationsDir = "migrations"

	// DefaultMigrationsTable is rig/migrate's, not a copy of it: `rig db up`
	// and a binary migrating itself have to read the same bookkeeping, and two
	// constants that happen to match today would not.
	DefaultMigrationsTable = migrate.DefaultTable

	DefaultJSONCase = "camel"

	// DefaultTenantQuery is the parameter the query tenant source reads.
	DefaultTenantQuery = "tenant"
	// DefaultSigningKeyEnv is where the key that signs the OAuth state parameter
	// is read from. It has no alternative source: it is a secret, so a variable
	// is the only place it can come from and defaulting the name costs nothing.
	DefaultSigningKeyEnv = "OAUTH_SIGNING_KEY"
)

// The authentication defaults are rig/auth's own rather than copies of them.
// Two constants that happen to match today would not stay matching, and a
// generated wiring that passed 10m while the module's default was something
// else would be documenting a lifetime the server does not have.
var (
	DefaultAuthBasePath = auth.DefaultBasePath
	DefaultTenantHeader = auth.TenantHeader

	DefaultAccessTTL      = session.DefaultAccessTTL
	DefaultRefreshTTL     = session.DefaultRefreshTTL
	DefaultRememberTTL    = session.DefaultRememberTTL
	DefaultRotationLeeway = session.DefaultRotationLeeway
	DefaultIdentityTTL    = session.DefaultIdentityTTL
	DefaultStateTTL       = oauth.DefaultStateTTL
)

func (p *Project) applyDefaults() {
	c := p.Config

	if c.Version == 0 {
		c.Version = 1
	}

	setDefault(&c.Layout.TableDir, DefaultTableDir)
	setDefault(&c.Layout.ConfigFile, DefaultConfigFile)

	setDefault(&c.API.Name, c.Project.Name)
	setDefault(&c.API.Version, DefaultAPIVersion)
	if c.API.BasePath == "" {
		c.API.BasePath = "/api/" + c.API.Version
	}
	c.API.BasePath = "/" + strings.Trim(c.API.BasePath, "/")
	if c.API.SearchMethod == "" {
		c.API.SearchMethod = DefaultSearchMethod
	}
	setDefault(&c.API.RevisionHeader, DefaultRevisionHeader)
	if c.API.Permissions == "" {
		// Derived by default, so an endpoint nobody thought about is refused
		// rather than open. Turning it off is a line in rig.yaml, and being
		// unprotected should be the thing somebody wrote down.
		c.API.Permissions = PermissionsDerived
	}

	setDefault(&c.OpenAPI.Version, DefaultOpenAPI)

	setDefault(&c.Database.Image, DefaultImage)
	setDefault(&c.Database.Name, DefaultDBName)
	setDefault(&c.Database.User, DefaultDBUser)
	setDefault(&c.Database.Password, DefaultDBPass)
	setDefault(&c.Database.Schema, DefaultDBSchema)
	if c.Database.ContainerName == "" {
		name := c.Project.Name
		if name == "" {
			name = "rig"
		}
		c.Database.ContainerName = name + "-db"
	}
	if c.Database.Port == 0 {
		c.Database.Port = DefaultPort
	}

	setDefault(&c.Migrations.Dir, DefaultMigrationsDir)
	setDefault(&c.Migrations.Table, DefaultMigrationsTable)

	setDefault(&c.Naming.JSONCase, DefaultJSONCase)

	p.applyAuthDefaults()
}

// applyAuthDefaults resolves every value the authentication block leaves out.
//
// It resolves them here rather than leaving zeros for rig/auth to fill in at
// startup, because the point of configuring this in a file is that the numbers
// can be read: the generated wiring passes what is written here, the emitted
// specification documents it, and a client library reads the same lifetime the
// server is enforcing. A zero that means "something else decides" would leave
// three places to ask.
//
// Nothing is defaulted for a project with no authentication. An `auth:` block
// that was never enabled should read as unfinished rather than as a full
// configuration nobody mounted.
func (p *Project) applyAuthDefaults() {
	a := &p.Config.Auth
	if !a.Enabled {
		return
	}

	setDefault(&a.BasePath, DefaultAuthBasePath)
	a.BasePath = "/" + strings.Trim(a.BasePath, "/")

	if len(a.Tenant.From) == 0 {
		a.Tenant.From = []string{string(ir.TenantFromHeader)}
	}
	// Only for the sources that are actually configured: a header name in the
	// document that nothing reads reads as though something does.
	if a.Tenant.Uses(ir.TenantFromHeader) {
		setDefault(&a.Tenant.Header, DefaultTenantHeader)
	}
	if a.Tenant.Uses(ir.TenantFromQuery) {
		setDefault(&a.Tenant.Query, DefaultTenantQuery)
	}

	setDuration(&a.Session.AccessTTL, DefaultAccessTTL)
	setDuration(&a.Session.RefreshTTL, DefaultRefreshTTL)
	setDuration(&a.Session.RememberTTL, DefaultRememberTTL)
	setDuration(&a.Session.RotationLeeway, DefaultRotationLeeway)
	setDuration(&a.Session.IdentityTTL, DefaultIdentityTTL)
	// CacheTTL is left alone: zero is the documented default and means the token
	// row is read on every request, which is what makes revocation immediate.

	policy := password.DefaultPolicy()
	if a.Password.MinLength == 0 {
		a.Password.MinLength = policy.MinLength
	}
	if a.Password.MaxLength == 0 {
		a.Password.MaxLength = policy.MaxLength
	}

	standard := throttle.Standard()
	for _, pair := range []struct {
		configured *AuthLimit
		standard   throttle.Limit
	}{
		{&a.Limits.LoginByEmail, standard.LoginByEmail},
		{&a.Limits.LoginByIP, standard.LoginByIP},
		{&a.Limits.PasswordReset, standard.PasswordReset},
		{&a.Limits.VerificationResend, standard.VerificationResend},
		{&a.Limits.Refresh, standard.Refresh},
		{&a.Limits.APIKeyFailures, standard.APIKeyFailures},
	} {
		if pair.configured.Max == 0 {
			pair.configured.Max = pair.standard.Max
		}
		setDuration(&pair.configured.Window, pair.standard.Window)
	}

	if len(a.OAuth.Providers) == 0 {
		// No providers, no provider routes, and nothing here to resolve. Leaving
		// the block unfilled keeps "OAuth is not configured" recognisable rather
		// than making it look configured with defaults.
		return
	}

	setDefault(&a.OAuth.SigningKeyEnv, DefaultSigningKeyEnv)
	setDuration(&a.OAuth.StateTTL, DefaultStateTTL)
	// base_url_env is deliberately not defaulted. An origin has to be configured
	// one way or the other, and inventing a variable name here would turn a
	// missing origin from something rig reports into a startup failure.

	for i := range a.OAuth.Providers {
		pr := &a.OAuth.Providers[i]
		pr.Name = strings.ToLower(strings.TrimSpace(pr.Name))
		prefix := strings.ToUpper(pr.Name)
		setDefault(&pr.ClientIDEnv, prefix+"_CLIENT_ID")
		setDefault(&pr.ClientSecretEnv, prefix+"_CLIENT_SECRET")
		if pr.Name == ir.ProviderMicrosoft {
			setDefault(&pr.TenantEnv, "MICROSOFT_TENANT")
		}
	}
}

func setDefault(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

func setDuration(field *Duration, value time.Duration) {
	if *field == 0 {
		*field = Duration(value)
	}
}

// DatabaseURL is the connection string for this project.
//
// An explicit URL wins; otherwise one is built for the throwaway container, with
// the session time zone pinned to UTC — see [dockerdb.Config.URL] for why that
// matters and what it does not affect. A URL somebody wrote themselves is left
// exactly as written: quietly appending a parameter to a connection string is a
// good way to break one that already carries its own.
func (p *Project) DatabaseURL() string {
	d := p.Config.Database
	if d.URL != "" {
		return d.URL
	}
	return fmt.Sprintf("postgres://%s:%s@localhost:%d/%s?sslmode=disable&TimeZone=UTC",
		url.QueryEscape(d.User), url.QueryEscape(d.Password), d.Port, d.Name)
}

// UsesContainer reports whether rig manages the database itself.
func (p *Project) UsesContainer() bool { return p.Config.Database.URL == "" }

// check validates what the JSON Schema cannot: relationships between values,
// and templates that must be able to name a table.
func (p *Project) check() diag.List {
	var diags diag.List
	c := p.Config

	// project.name and project.module are required by the schema, so there is
	// no check for them here.

	// A layout that cannot distinguish one table from another would make every
	// table share a single configuration file.
	if !hasTablePlaceholder(c.Layout.ConfigFile) && !hasTablePlaceholder(c.Layout.TableDir) {
		diags.Add(diag.CodeConfigInvalid, p.At("layout", "config_file"),
			"layout must name a table somewhere: use {table}, {Table} or {tables} in config_file or table_dir")
	}

	seen := make(map[string]int, len(c.Generators))
	for i, g := range c.Generators {
		at := p.At("generators", fmt.Sprint(i), "name")
		if g.Name == "" {
			diags.Add(diag.CodeConfigInvalid, at, "generator %d has no name", i)
			continue
		}
		if prev, dup := seen[g.Name]; dup {
			diags.Add(diag.CodeConfigInvalid, at,
				"generator %q is already configured at generators.%d", g.Name, prev)
			continue
		}
		seen[g.Name] = i
	}

	diags.Append(p.checkAuth())

	return diags
}

func hasTablePlaceholder(s string) bool {
	return strings.Contains(s, "{table}") ||
		strings.Contains(s, "{Table}") ||
		strings.Contains(s, "{tables}")
}

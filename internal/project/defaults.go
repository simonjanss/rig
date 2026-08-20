package project

import (
	"fmt"
	"net/url"
	"slices"
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

	// DefaultFoundation keeps rig's own migrations in the project's own
	// directory. It is the conservative half of the choice: the directory stays
	// the whole truth about the database, which is worth more than saving a
	// thousand lines of SQL until somebody asks to trade it.
	DefaultFoundation = FoundationVendored

	DefaultJSONCase = "camel"

	// DefaultTenantQuery is the parameter the query tenant source reads.
	DefaultTenantQuery = "tenant"
	// DefaultSigningKeyEnv is where the key that signs the OAuth state parameter
	// is read from. It has no alternative source: it is a secret, so a variable
	// is the only place it can come from and defaulting the name costs nothing.
	DefaultSigningKeyEnv = "OAUTH_SIGNING_KEY"

	// DefaultFilesBackend is memory, which is not durable. A project that means
	// to keep its uploads has to say so, and a default of s3 would need a bucket
	// nobody configured.
	DefaultFilesBackend = BackendMemory

	// DefaultFilesMaxBytes is 25 MiB: comfortably more than a photograph from a
	// phone, comfortably less than something that should have been a multipart
	// upload to a bucket.
	DefaultFilesMaxBytes = 25 << 20

	// DefaultFilesAbandonedAfter gives a pending upload a day before the sweeper
	// treats it as the remains of a request that died. Generous on purpose: the
	// cost of waiting is a row, and the cost of being wrong is deleting a file
	// somebody is still sending.
	DefaultFilesAbandonedAfter = 24 * time.Hour

	// DefaultFilesRestoreWindow matches the thirty days `restore_window_days`
	// scaffolds for an ordinary table, so a file and the row pointing at it go
	// out of reach together.
	DefaultFilesRestoreWindow = 30 * 24 * time.Hour

	// DefaultNotificationDigest is Immediate: an account that has said nothing
	// hears about things when they happen. Anything else would be rig deciding
	// on somebody's behalf that they wanted less mail than the application
	// thought it was sending.
	DefaultNotificationDigest = DigestImmediate

	// DefaultClaimTTL is five minutes, which is the wrong number for somebody
	// and has to be some number.
	//
	// It is chosen for the slowest channel most applications have, which is
	// mail: an SMTP conversation or a provider API call that has not finished in
	// five minutes has failed. A project whose only channel is a websocket push
	// that either works in 200ms or does not should set this far lower, and one
	// with a provider that retries internally for ten minutes has to set it
	// higher — the relationship to the channel's own timeout is the one
	// misconfiguration here worth understanding.
	DefaultClaimTTL = 5 * time.Minute

	// DefaultNotificationSendTimeout bounds one call into a channel.
	//
	// Thirty seconds, which is what rigclient allows a whole request. It is the
	// number that makes DefaultClaimTTL's paragraph above checkable rather than
	// advisory: the relationship it calls "the one misconfiguration here worth
	// understanding" is between these two values, and with both of them in this
	// file rig can refuse the pair instead of describing it.
	//
	// A channel is the one outbound call rig does not make itself, so it is the
	// one that could not bound itself. Everything else already does — three
	// seconds for the breach check, ten for a token exchange.
	DefaultNotificationSendTimeout = 30 * time.Second

	// DefaultMaxAttempts is five, after which a delivery is Failed and stops
	// being claimed. Without a cap a permanently broken address consumes a lease
	// and a log line forever.
	DefaultMaxAttempts = 5

	// DefaultBackoffBase is a minute, doubling: one, two, four, eight, sixteen.
	// Five attempts therefore span about half an hour, which outlasts the
	// ordinary provider blip and does not outlast anybody's patience.
	DefaultBackoffBase = time.Minute

	// DefaultNotificationRetention is ninety days. Long enough that "what was I
	// told in the spring" has an answer, and short enough that the busiest table
	// in the schema does not grow forever — which every other table in rig
	// currently does, and which is named as known and unfixed.
	DefaultNotificationRetention = 90 * 24 * time.Hour

	// The AWS SDK's own variable names, so a deployment that already has
	// credentials in the environment needs no configuration at all.
	DefaultAccessKeyEnv = "AWS_ACCESS_KEY_ID"
	DefaultSecretKeyEnv = "AWS_SECRET_ACCESS_KEY"
)

// DefaultInlineTypes are the sniffed types served without an attachment
// disposition.
//
// Images and nothing else. A file served inline from the API origin runs on
// that origin, and the URL is in a synced row that ends up in an <img> without
// anybody thinking about it — so text/html, image/svg+xml and application/pdf
// are all deliberately absent. SVG is the one that surprises people: it is an
// image, and it can carry script.
func DefaultInlineTypes() []string {
	return []string{"image/jpeg", "image/png", "image/gif", "image/webp"}
}

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
	if c.Migrations.Foundation == "" {
		// Resolved here rather than read as a zero value later, so that every
		// caller asking which mode a project is in gets the same answer — the
		// mode decides which migrations get applied, and a zero meaning
		// "something else decides" would be two behaviours behind one blank key.
		c.Migrations.Foundation = DefaultFoundation
	}

	setDefault(&c.Naming.JSONCase, DefaultJSONCase)

	p.applyAuthDefaults()
	p.applyFilesDefaults()
	p.applyNotificationsDefaults()
}

// applyNotificationsDefaults resolves every value the notifications block leaves
// out, for the same reason applyFilesDefaults does.
func (p *Project) applyNotificationsDefaults() {
	n := &p.Config.Notifications
	if !n.Enabled {
		return
	}

	setDefault(&n.DefaultDigest, DefaultNotificationDigest)
	setDuration(&n.ClaimTTL, DefaultClaimTTL)
	setDuration(&n.SendTimeout, DefaultNotificationSendTimeout)
	setDuration(&n.BackoffBase, DefaultBackoffBase)
	setDuration(&n.Retention, DefaultNotificationRetention)
	if n.MaxAttempts == 0 {
		n.MaxAttempts = DefaultMaxAttempts
	}
}

// applyFilesDefaults resolves every value the files block leaves out, for the
// same reason applyAuthDefaults does: the numbers here are the ones the
// generated wiring passes and the specification quotes, and a zero meaning
// "something else decides" would leave three places to ask.
func (p *Project) applyFilesDefaults() {
	f := &p.Config.Files
	if !f.Enabled {
		return
	}

	setDefault(&f.Backend, DefaultFilesBackend)
	if f.MaxBytes == 0 {
		f.MaxBytes = DefaultFilesMaxBytes
	}
	setDuration(&f.AbandonedAfter, DefaultFilesAbandonedAfter)
	setDuration(&f.RestoreWindow, DefaultFilesRestoreWindow)

	if len(f.InlineTypes) == 0 {
		f.InlineTypes = slices.Clone(DefaultInlineTypes())
	}

	if f.Backend == BackendS3 {
		setDefault(&f.S3.AccessKeyEnv, DefaultAccessKeyEnv)
		setDefault(&f.S3.SecretKeyEnv, DefaultSecretKeyEnv)
	}
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
	diags.Append(p.checkFiles())
	diags.Append(p.checkNotifications())

	return diags
}

func hasTablePlaceholder(s string) bool {
	return strings.Contains(s, "{table}") ||
		strings.Contains(s, "{Table}") ||
		strings.Contains(s, "{tables}")
}

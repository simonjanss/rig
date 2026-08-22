// Package project reads rig.yaml, the file that marks the root of a rig
// project and configures everything that is not per-table.
//
// Discovery walks up from the working directory, so rig commands work from
// anywhere inside a project. Every path in the file resolves relative to the
// file's own directory rather than the working directory, so the same
// configuration means the same thing regardless of where it is run from.
package project

// Config is the contents of rig.yaml.
type Config struct {
	Version int `yaml:"version,omitempty" json:"version,omitempty" jsonschema_description:"Configuration format version."`

	Project    ProjectInfo `yaml:"project" json:"project" jsonschema_description:"Identity of the application being generated."`
	Layout     Layout      `yaml:"layout,omitempty" json:"layout,omitempty" jsonschema_description:"Where table configuration and generated code live."`
	API        API         `yaml:"api,omitempty" json:"api,omitempty" jsonschema_description:"Shape of the generated HTTP API."`
	Database   Database    `yaml:"database,omitempty" json:"database,omitempty" jsonschema_description:"Where to run migrations and read the schema from."`
	Migrations Migrations  `yaml:"migrations,omitempty" json:"migrations,omitempty" jsonschema_description:"Migration file location."`
	Auth       Auth        `yaml:"auth,omitempty" json:"auth,omitempty" jsonschema_description:"How the authentication foundation's tables are treated."`
	Files      Files       `yaml:"files,omitempty" json:"files,omitempty" jsonschema_description:"Where uploaded files are kept, and what an upload may be."`

	Notifications Notifications `yaml:"notifications,omitempty" json:"notifications,omitempty" jsonschema_description:"The inbox, and how notifications are delivered."`

	Tracing Tracing `yaml:"tracing,omitempty" json:"tracing,omitempty" jsonschema_description:"Whether the generated code emits OpenTelemetry spans."`

	Monitoring Monitoring `yaml:"monitoring,omitempty" json:"monitoring,omitempty" jsonschema_description:"rig's own page over the spans this server wrote. It needs tracing."`

	Naming   Naming   `yaml:"naming,omitempty" json:"naming,omitempty" jsonschema_description:"How database names become Go and JSON names."`
	Validate Validate `yaml:"validate,omitempty" json:"validate,omitempty" jsonschema_description:"Severity of each configurable convention rule."`

	Generators []Generator `yaml:"generators,omitempty" json:"generators,omitempty" jsonschema_description:"Generators to run, in order."`
}

// ProjectInfo identifies the application.
type ProjectInfo struct {
	Name string `yaml:"name" json:"name" jsonschema_description:"Short name of the application."`
	// Module is the Go module path of the generated application. Generated
	// imports are built from it.
	Module string `yaml:"module" json:"module" jsonschema_description:"Go module path of the application, used to build generated import paths."`
}

// Auth is the authentication foundation: whether this project has one, how it
// behaves, and how its tables are treated.
//
// Everything that is a fixed choice lives here rather than in a Go literal, and
// that is the point of the block. The generated wiring, the emitted
// specification and a client library all read the same numbers, so how long a
// token lives is documented and typed rather than being a value one file happens
// to pass. What is left for Go is what YAML cannot hold: a function, and a
// secret.
//
// The table half needs no configuration and is what almost every project wants:
// rig generates nothing for the tables `rig setup-project` created, because
// their Go types, their stores and their endpoints already exist in the
// rig/auth module. A project is recognised as having them by its own migration
// files, so a table called `rig_account` that nobody scaffolded is an ordinary
// table and keeps getting a model and a repository like any other.
type Auth struct {
	// Enabled says this project authenticates its callers.
	//
	// Off by default, and deliberately explicit: a project with an `auth:` block
	// it never turned on is easier to spot than one that started mounting a
	// sign-in endpoint because a file appeared. It is what makes `server-go`
	// write the wiring at all, and everything else in this block is read only
	// when it is set.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether this project authenticates its callers. The server-go generator writes the wiring only when it is set."`

	// BasePath prefixes the endpoints. The provider routes sit under it, at
	// <base>/oauth/{provider}/start.
	BasePath string `yaml:"base_path,omitempty" json:"base_path,omitempty" jsonschema_description:"Prefix the authentication endpoints sit under. Defaults to /auth."`

	Tenant   AuthTenant   `yaml:"tenant,omitempty" json:"tenant,omitempty" jsonschema_description:"How a request names the tenant it is for."`
	Session  AuthSession  `yaml:"session,omitempty" json:"session,omitempty" jsonschema_description:"How long the credentials somebody holds stay valid."`
	Password AuthPassword `yaml:"password,omitempty" json:"password,omitempty" jsonschema_description:"What a new password must satisfy."`
	Limits   AuthLimits   `yaml:"limits,omitempty" json:"limits,omitempty" jsonschema_description:"Rate limits, counted in the database so replicas cannot disagree."`

	// AllowRegistration mounts POST <base>/register, where a stranger creates an
	// account with no tenant and lands in the tenant picker.
	//
	// Off by default. Whether anybody may sign themselves up is a product
	// decision — invite-only and open registration are both ordinary — and off
	// means the route does not exist rather than answering 403 to something
	// probeable.
	AllowRegistration bool `yaml:"allow_registration,omitempty" json:"allow_registration,omitempty" jsonschema_description:"Mount the registration endpoint, where a stranger creates an account. Off means the route does not exist."`

	// AllowTenantCreation mounts POST <base>/tenants, where somebody signed in
	// makes one and becomes its Owner. The policy — who may, what a name may be,
	// what else a new tenant needs — stays in Go, because each of those is a
	// function.
	AllowTenantCreation bool `yaml:"allow_tenant_creation,omitempty" json:"allow_tenant_creation,omitempty" jsonschema_description:"Mount the tenant-creation endpoint. Who may create one, and what else a new one needs, stay in Go."`

	// RequireVerifiedEmail refuses a sign-in until the address is verified.
	RequireVerifiedEmail bool `yaml:"require_verified_email,omitempty" json:"require_verified_email,omitempty" jsonschema_description:"Refuse a sign-in until the address has been verified."`

	// TrustedProxies are the networks whose X-Forwarded-For may be believed.
	//
	// Empty means none, and that is deliberate: an address read from a header a
	// client controls is an address a client chooses, and a rate limit keyed on
	// one is a rate limit an attacker walks around.
	TrustedProxies []string `yaml:"trusted_proxies,omitempty" json:"trusted_proxies,omitempty" jsonschema_description:"CIDR ranges whose X-Forwarded-For may be believed, for example 10.0.0.0/8. Empty trusts none of them."`

	// LogRetention is how long an entry in rig_auth_log is kept. Zero, the
	// default, keeps everything.
	//
	// Keeping everything is the honest default rather than the good one: nothing
	// prunes this table today, and the alternative — rig choosing a window — would
	// be rig deciding how long somebody's compliance obligation runs.
	//
	// It cannot be shorter than the longest rate limit window, and that is refused
	// rather than clamped. This table is what the limiter counts, so deleting rows
	// inside a window clears the lockout they were adding up to: a limiter that
	// silently stops limiting, which is the failure nobody notices until it is
	// being exploited.
	LogRetention Duration `yaml:"log_retention,omitempty" json:"log_retention,omitempty" jsonschema_description:"How long an entry in rig_auth_log is kept, for example 90d. Zero, the default, keeps everything. It cannot be shorter than the longest rate-limit window, because this table is what the limits count."`

	OAuth AuthOAuth `yaml:"oauth,omitempty" json:"oauth,omitempty" jsonschema_description:"Provider sign-in. No providers means the provider routes are not mounted."`

	// Expose names foundation tables to generate for anyway.
	//
	// It adds the model, the repository and whatever API the table's
	// configuration asks for — for an administration screen listing the people
	// in a tenant, most often. `rig setup-project --expose rig_account` writes the
	// configuration to go with it.
	//
	// It changes nothing about authentication: the auth package still reaches
	// these tables through its own queries, and a generated repository beside
	// them is a second door into the same rows, which is exactly why the
	// scaffolded configuration keeps `rig_account` free of a Create.
	Expose []string `yaml:"expose,omitempty" json:"expose,omitempty" jsonschema_description:"Foundation tables to generate a model, repository, and API for anyway."`

	// Own treats the foundation as ordinary tables and generates for all of it.
	//
	// For a project that has taken the schema over — forked the migrations,
	// stopped importing rig/auth, and now maintains the tables itself. It is a
	// one-way door in practice: the generated repositories will not enforce what
	// the auth package enforces, so a password reaching a table through one of
	// them is a password nobody hashed.
	Own bool `yaml:"own,omitempty" json:"own,omitempty" jsonschema_description:"Generate for every foundation table, for a project that has forked the schema and no longer imports rig/auth."`
}

// AuthTenant says how a request names the tenant it is for.
//
// It is asked only where the tenant cannot be known some other way: a sign-in, a
// password reset, the start of a provider flow. Everything else takes its tenant
// from the token, which is why getting this wrong shows up at the front door
// rather than everywhere.
type AuthTenant struct {
	// From lists the sources to try, in order, and the first that names a tenant
	// wins. Default: header.
	//
	// A request that names none is not an error — a single sign-in page cannot
	// know which tenants an address belongs to before the password has been
	// checked — so the sources are consulted and their silence is an answer.
	//
	//   header  a header, X-Tenant-Id by default
	//   host    the leftmost label of the Host, looked up as a tenant slug
	//   query   a query parameter, for a local demonstration
	//   hook    the application's own resolver, handed to the generated wiring
	//
	// host is what a real deployment uses, and the only one that survives a
	// provider redirect: a callback URL is registered with the provider and fixed,
	// so it carries no header and no parameter of yours. A host does.
	From []string `yaml:"from,omitempty" json:"from,omitempty" jsonschema:"enum=header,enum=host,enum=query,enum=hook" jsonschema_description:"Sources to try in order; the first that names a tenant wins. Defaults to [header]."`

	// Header is what the header source reads. Default X-Tenant-Id.
	Header string `yaml:"header,omitempty" json:"header,omitempty" jsonschema_description:"Header the header source reads. Defaults to X-Tenant-Id."`
	// Query is what the query source reads. Default tenant.
	Query string `yaml:"query,omitempty" json:"query,omitempty" jsonschema_description:"Query parameter the query source reads. Defaults to tenant."`

	// DefaultSlugEnv names an environment variable holding the slug to fall back
	// to when the host names no tenant.
	//
	// It exists for one real problem: Google and Microsoft will not register a
	// plain-http redirect URI for anything but localhost, so the subdomain a
	// host-based deployment is built around cannot be used on a laptop. Naming a
	// tenant for the host that has no subdomain is what lets a real sign-in be
	// tried there.
	DefaultSlugEnv string `yaml:"default_slug_env,omitempty" json:"default_slug_env,omitempty" jsonschema_description:"Environment variable holding a tenant slug to fall back to when the host names none."`
}

// AuthSession is how long the things somebody holds stay good for.
//
// Every value is a trade rather than a preference, so each one says what it
// costs. Zero means rig/auth's own default.
type AuthSession struct {
	// AccessTTL is the lifetime of the token that travels on every request.
	// Shorter is safer and refreshes more often. Default 10m.
	AccessTTL Duration `yaml:"access_ttl,omitempty" json:"access_ttl,omitempty" jsonschema_description:"Lifetime of the access token, which travels on every request. Shorter is safer and refreshes more often. Defaults to 10m."`
	// RefreshTTL is how long an ordinary session lasts. Default 12h.
	RefreshTTL Duration `yaml:"refresh_ttl,omitempty" json:"refresh_ttl,omitempty" jsonschema_description:"How long an ordinary session lasts. Defaults to 12h."`
	// RememberTTL is how long one lasts when somebody asked to stay signed in.
	// Default 30d.
	RememberTTL Duration `yaml:"remember_ttl,omitempty" json:"remember_ttl,omitempty" jsonschema_description:"How long a session lasts when somebody asked to stay signed in. Defaults to 30d."`

	// RotationLeeway is how long a refresh token stays usable after it has been
	// exchanged. Default 30s.
	//
	// Without one, a dropped response is a logout: the client never received the
	// new pair, retries with the old one, and has its whole family revoked for a
	// network blip. Longer forgives more retries and widens the replay window.
	RotationLeeway Duration `yaml:"rotation_leeway,omitempty" json:"rotation_leeway,omitempty" jsonschema_description:"How long a refresh token stays usable after it was exchanged, so a dropped response is a retry rather than a logout. Longer forgives more retries and widens the replay window. Defaults to 30s."`

	// IdentityTTL is the lifetime of the tenant-less credential somebody holds
	// between signing in and picking a tenant. Default 30m.
	IdentityTTL Duration `yaml:"identity_ttl,omitempty" json:"identity_ttl,omitempty" jsonschema_description:"Lifetime of the tenant-less credential held between signing in and picking a tenant. Defaults to 30m."`

	// CacheTTL keeps verified access tokens in memory. Zero, the default, reads
	// the row every time, which is what makes revocation immediate.
	//
	// Setting it trades that for throughput: a revoked session keeps working for
	// up to this long. Do not set it above a few seconds, and do not set it at
	// all unless the read is measurably a problem.
	CacheTTL Duration `yaml:"cache_ttl,omitempty" json:"cache_ttl,omitempty" jsonschema_description:"Keep verified access tokens in memory for this long. Zero, the default, reads the row every time, which makes revocation immediate. Anything above a few seconds is a revoked session that keeps working."`
}

// AuthPassword is what a new password must satisfy.
//
// There are no composition rules, and that is the recommendation rather than an
// omission: demanding an uppercase letter, a digit and a symbol pushes people
// toward Password1! and a sticky note. Length and a breach check are what help.
type AuthPassword struct {
	// MinLength is in characters, not bytes. Default 12.
	MinLength int `yaml:"min_length,omitempty" json:"min_length,omitempty" jsonschema:"minimum=8" jsonschema_description:"Minimum password length in characters. Defaults to 12."`
	// MaxLength bounds the work rather than the strength: argon2id over a
	// ten-megabyte password is a denial of service anybody can send. Default 1024.
	MaxLength int `yaml:"max_length,omitempty" json:"max_length,omitempty" jsonschema_description:"Maximum password length in characters. This bounds hashing work, not strength. Defaults to 1024."`

	// BreachCheck rejects passwords known to have leaked, against Have I Been
	// Pwned's range API.
	//
	// Off by default because it talks to a third party. The password never leaves
	// the process — five hex digits of its SHA-1 do — and a checker that cannot
	// reach its source fails open, because a third party's outage should not stop
	// somebody changing their password.
	BreachCheck bool `yaml:"breach_check,omitempty" json:"breach_check,omitempty" jsonschema_description:"Reject passwords that appear in Have I Been Pwned. Only a hash prefix is sent, and the check fails open."`
}

// AuthLimits are the rate limits. Zero in any of them means rig's own default.
//
// They are counted in the database rather than in memory, so two replicas cannot
// disagree about how many times a password has been tried and a restart does not
// clear somebody's lockout.
type AuthLimits struct {
	// LoginByEmail locks one account after a handful of wrong passwords.
	// Default 5 per 15m, cleared by a success.
	LoginByEmail AuthLimit `yaml:"login_by_email,omitempty" json:"login_by_email,omitempty" jsonschema_description:"Failed sign-ins per address before the account is locked out. Defaults to 5 per 15m."`
	// LoginByIP throttles one source spraying many accounts. Default 50 per 15m.
	//
	// Deliberately looser than the address limit and deliberately not cleared by
	// a success: an email-only limit lets an attacker lock a victim out on
	// purpose, and an IP-only limit lets a botnet spray one password everywhere.
	LoginByIP AuthLimit `yaml:"login_by_ip,omitempty" json:"login_by_ip,omitempty" jsonschema_description:"Failed sign-ins per source address. Looser than the per-address limit so an office behind one NAT does not trip it. Defaults to 50 per 15m."`

	// PasswordReset bounds reset requests per address. Default 5 per 1h.
	PasswordReset AuthLimit `yaml:"password_reset,omitempty" json:"password_reset,omitempty" jsonschema_description:"Password-reset requests per address. Defaults to 5 per 1h."`
	// VerificationResend bounds verification mail per address. Default 5 per 1h.
	VerificationResend AuthLimit `yaml:"verification_resend,omitempty" json:"verification_resend,omitempty" jsonschema_description:"Verification mails per address. Defaults to 5 per 1h."`
	// Refresh bounds one session's rotations. Default 60 per 1m: a client
	// refreshing sixty times a minute is looping, not working.
	Refresh AuthLimit `yaml:"refresh,omitempty" json:"refresh,omitempty" jsonschema_description:"Token rotations per session. A client refreshing this often is looping, not working. Defaults to 60 per 1m."`
	// APIKeyFailures bounds wrong keys per key. Default 20 per 1m.
	APIKeyFailures AuthLimit `yaml:"api_key_failures,omitempty" json:"api_key_failures,omitempty" jsonschema_description:"Failed API-key authentications per key. Defaults to 20 per 1m."`
}

// AuthLimit is one limit: how many, and over how long. Either field left at zero
// takes rig's default for that limit.
type AuthLimit struct {
	Max    int      `yaml:"max,omitempty" json:"max,omitempty" jsonschema:"minimum=1" jsonschema_description:"How many are allowed in one window."`
	Window Duration `yaml:"window,omitempty" json:"window,omitempty" jsonschema_description:"How long the window is."`
}

// AuthOAuth is provider sign-in.
//
// An empty provider list mounts nothing, so this block costs nothing to leave in
// place while a project decides.
type AuthOAuth struct {
	// BaseURL is this application's own origin. The callback URL is built from
	// it, and a provider compares that exactly.
	BaseURL string `yaml:"base_url,omitempty" json:"base_url,omitempty" jsonschema_description:"This application's own origin, for example https://app.example.com. The callback URL is built from it and a provider compares that exactly."`
	// BaseURLEnv names an environment variable that overrides BaseURL, which is
	// the ordinary case: the origin differs per deployment and the file does not.
	BaseURLEnv string `yaml:"base_url_env,omitempty" json:"base_url_env,omitempty" jsonschema_description:"Environment variable that overrides base_url, for an origin that differs per deployment. Not defaulted: one of base_url and base_url_env has to be set."`
	// OriginFromHost derives the origin per request from the Host instead.
	//
	// What an application serving a tenant per subdomain needs: the state cookie
	// carrying the PKCE verifier is set on the host the sign-in started at, and a
	// browser will not send it to a sibling subdomain — so the callback URL is per
	// host, and every one of them is registered with the provider.
	OriginFromHost bool `yaml:"origin_from_host,omitempty" json:"origin_from_host,omitempty" jsonschema_description:"Derive the callback origin from each request's Host, for an application serving a tenant per subdomain. Every origin must be registered with the provider."`

	// SigningKeyEnv names the environment variable holding the key that signs the
	// state parameter. Default OAUTH_SIGNING_KEY.
	//
	// A random key is generated when the variable is empty, which is fine for one
	// process and wrong for several: a callback may arrive at a different replica
	// than the one that started the flow.
	SigningKeyEnv string `yaml:"signing_key_env,omitempty" json:"signing_key_env,omitempty" jsonschema_description:"Environment variable holding the key that signs the state parameter, at least 32 bytes. Defaults to OAUTH_SIGNING_KEY. Unset generates one per process, which breaks across replicas."`

	// StateTTL bounds how long a sign-in round trip may take. Default 10m:
	// generous for a redirect, short enough that a stolen state is useless.
	StateTTL Duration `yaml:"state_ttl,omitempty" json:"state_ttl,omitempty" jsonschema_description:"How long a sign-in round trip may take. Generous for a redirect, short enough that a stolen state is useless. Defaults to 10m."`

	// AllowProvisioning creates an account the first time somebody signs in with
	// a provider.
	//
	// Off by default: a provider authenticates anybody, so joining a tenant stays
	// a decision. Without it a provider sign-in works only for an address that
	// already has an account.
	AllowProvisioning bool `yaml:"allow_provisioning,omitempty" json:"allow_provisioning,omitempty" jsonschema_description:"Create an account the first time somebody signs in with a provider. Off means a provider sign-in only works for an address that already has one."`

	// AllowedReturnTo bounds where a finished sign-in may land. An open redirect
	// is the classic mistake here.
	AllowedReturnTo []string `yaml:"allowed_return_to,omitempty" json:"allowed_return_to,omitempty" jsonschema_description:"Origins or paths a finished sign-in may return to. Anything else is refused, which is what stops an open redirect."`

	// Insecure allows the state cookie over plain HTTP, for local development
	// where a browser would refuse the __Host- prefixed one. Never anywhere real.
	Insecure bool `yaml:"insecure,omitempty" json:"insecure,omitempty" jsonschema_description:"Allow the state cookie over plain HTTP, for local development only. Never set this in a deployment."`

	// Providers are the identity providers to offer.
	Providers []AuthProvider `yaml:"providers,omitempty" json:"providers,omitempty" jsonschema_description:"Identity providers to offer. Empty mounts no provider routes."`
}

// AuthProvider is one identity provider.
//
// Credentials are named, never carried: a client secret in rig.yaml is a client
// secret in version control. The file says which environment variable holds it
// and the generated code reads that, which is also what lets one binary offer
// Google in production and nothing at all on a laptop.
type AuthProvider struct {
	// Name selects the provider, and is also the route segment: a provider called
	// google is reached at <base>/oauth/google/start.
	Name string `yaml:"name" json:"name" jsonschema:"enum=google,enum=microsoft,enum=github" jsonschema_description:"Which provider. It is also the route segment: google is reached at <base_path>/oauth/google/start."`

	// ClientIDEnv and ClientSecretEnv default to the provider's name in upper
	// case: GOOGLE_CLIENT_ID and GOOGLE_CLIENT_SECRET.
	ClientIDEnv     string `yaml:"client_id_env,omitempty" json:"client_id_env,omitempty" jsonschema_description:"Environment variable holding the client id. Defaults to <PROVIDER>_CLIENT_ID."`
	ClientSecretEnv string `yaml:"client_secret_env,omitempty" json:"client_secret_env,omitempty" jsonschema_description:"Environment variable holding the client secret. Defaults to <PROVIDER>_CLIENT_SECRET."`

	// TenantEnv is Microsoft's own idea of a tenant, which has nothing to do with
	// rig's: "common" accepts any account, "organizations" excludes personal
	// ones, and a directory id restricts sign-in to one organization. Empty means
	// common. Default MICROSOFT_TENANT.
	TenantEnv string `yaml:"tenant_env,omitempty" json:"tenant_env,omitempty" jsonschema_description:"Microsoft only: environment variable naming its directory. common accepts any account, a directory id restricts sign-in to one organization. Defaults to MICROSOFT_TENANT."`

	// Required fails startup when the credentials are absent.
	//
	// The default skips the provider instead, which is what makes one
	// configuration work in both places: set the pair and the button appears, set
	// nothing and it does not.
	Required bool `yaml:"required,omitempty" json:"required,omitempty" jsonschema_description:"Fail startup when the credentials are absent. The default skips the provider instead, so one configuration works with or without them."`
}

// Layout says where a table's configuration and code live.
//
// The templates take {table} for the snake_case name, {Table} for PascalCase,
// and {tables} for the plural. Putting a table's configuration next to the
// service code that implements it keeps everything about one table in one
// directory; a flat layout is a one-line change.
type Layout struct {
	TableDir   string `yaml:"table_dir,omitempty" json:"table_dir,omitempty" jsonschema_description:"Directory for a table's configuration and service code. Templates: {table} {Table} {tables}."`
	ConfigFile string `yaml:"config_file,omitempty" json:"config_file,omitempty" jsonschema_description:"Path to a table's configuration file. May reference {table_dir}."`
}

// SearchMethod selects how the Search operation is exposed.
type SearchMethod string

const (
	// SearchQuery exposes Search as HTTP QUERY only.
	SearchQuery SearchMethod = "query"
	// SearchPost exposes Search as POST to a /_search sub-path only.
	SearchPost SearchMethod = "post"
	// SearchBoth exposes QUERY with the POST sub-path as an alias, which is the
	// default: QUERY is the correct method for a read with a body, but some
	// intermediaries still reject methods they do not recognize.
	SearchBoth SearchMethod = "both"
)

// PermissionMode selects whether authorization checks are generated.
type PermissionMode string

const (
	// PermissionsDerived is the default: one permission per resource and action,
	// checked in the handler.
	PermissionsDerived PermissionMode = "derived"
	// PermissionsNone generates no checks. It is for a project that has no
	// authorization — a demonstration, or an API behind something else that does
	// the deciding.
	PermissionsNone PermissionMode = "none"
)

// API configures the generated HTTP surface.
type API struct {
	Name        string `yaml:"name,omitempty" json:"name,omitempty" jsonschema_description:"Name of the API, used in documentation and generated type prefixes."`
	Version     string `yaml:"version,omitempty" json:"version,omitempty" jsonschema_description:"API version segment, for example v1."`
	BasePath    string `yaml:"base_path,omitempty" json:"base_path,omitempty" jsonschema_description:"Prefix every route sits under, for example /api/v1."`
	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"What this API is for."`
	// Permissions decides whether generated handlers check one.
	//
	// derived, the default, gives every endpoint a permission taken from its
	// resource and its operation — note.read, note.write, note.delete — and
	// generates the check. An authenticated caller with no grants then reaches
	// nothing, which is the right posture and a real behaviour change for a
	// project that had none.
	//
	// none generates no checks, for a project with no authorization at all.
	// `public:` on a resource or an endpoint is the per-endpoint escape hatch and
	// works either way.
	Permissions PermissionMode `yaml:"permissions,omitempty" json:"permissions,omitempty" jsonschema:"enum=derived,enum=none" jsonschema_description:"Whether generated handlers check a permission derived from the resource and operation. Defaults to derived."`

	SearchMethod SearchMethod `yaml:"search_method,omitempty" json:"search_method,omitempty" jsonschema:"enum=query,enum=post,enum=both" jsonschema_description:"How the Search operation is exposed. Defaults to both: QUERY with a POST alias."`

	// RevisionHeader carries the revision a client was built against, and the
	// one the server was generated from on the way back.
	//
	// It is configurable because it is a name in a namespace rig does not own:
	// a gateway in front of the server may already mean something else by it.
	RevisionHeader string `yaml:"revision_header,omitempty" json:"revision_header,omitempty" jsonschema_description:"Header carrying the API revision, in both directions. Defaults to API-Revision."`
}

// Database says where rig runs migrations and reads the schema.
//
// With no URL, rig starts a throwaway container, applies the migrations, reads
// the schema back, and leaves the container running for the next command. Set
// URL to point at a database you manage instead, which is what CI does.
type Database struct {
	Image         string `yaml:"image,omitempty" json:"image,omitempty" jsonschema_description:"Container image for the throwaway database."`
	ContainerName string `yaml:"container_name,omitempty" json:"container_name,omitempty" jsonschema_description:"Name of the throwaway container."`
	Port          int    `yaml:"port,omitempty" json:"port,omitempty" jsonschema_description:"Host port the throwaway database listens on."`
	Name          string `yaml:"name,omitempty" json:"name,omitempty" jsonschema_description:"Database name."`
	User          string `yaml:"user,omitempty" json:"user,omitempty" jsonschema_description:"Database user."`
	Password      string `yaml:"password,omitempty" json:"password,omitempty" jsonschema_description:"Database password. This is a local throwaway database; do not put a real secret here."`
	Schema        string `yaml:"schema,omitempty" json:"schema,omitempty" jsonschema_description:"Postgres schema to read. Defaults to public."`

	// URL, when set, is used as-is and no container is started.
	URL string `yaml:"url,omitempty" json:"url,omitempty" jsonschema_description:"Connection URL of an existing database. Set this to skip the throwaway container entirely."`
}

// FoundationMode selects how rig's own tables reach the database.
//
// rig owns a dozen tables — the identities, the tokens, the file rows, the
// notifications — and their DDL has to get there somehow. There are two readings
// and the difference is who keeps the file.
type FoundationMode string

const (
	// FoundationVendored copies rig's migrations into the project's own
	// migrations directory, numbered into its sequence and recorded in its
	// bookkeeping table. It is the default, and the reason is readability: the
	// directory is then the whole truth about the database, and somebody can read
	// it at three in the morning without knowing which version of rig is
	// installed.
	//
	// Upgrades arrive as new files. rig's sets are append-only, so a project three
	// versions behind gets three migrations it can read the diff of before
	// applying.
	FoundationVendored FoundationMode = "vendored"

	// FoundationEmbedded leaves the DDL in the modules that own it — rig/auth,
	// rig/files, rig/notify — each applying and recording its own set. The
	// project's migrations directory then holds only the project's own schema.
	//
	// What it buys is that rig's tables stop being a thousand lines of somebody
	// else's SQL in the repository, and that an upgrade is a module version rather
	// than a file to copy. What it costs is that the migrations directory no
	// longer says what is in the database on its own.
	FoundationEmbedded FoundationMode = "embedded"
)

// Migrations says where migration files live, and whose they are.
type Migrations struct {
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty" jsonschema_description:"Directory holding goose migration files."`
	// Table is the goose bookkeeping table.
	Table string `yaml:"table,omitempty" json:"table,omitempty" jsonschema_description:"Name of the migration bookkeeping table."`
	// Foundation is who keeps rig's own migrations. See [FoundationMode].
	//
	// It is chosen once. The two modes record their history in different tables,
	// so a project that switches after it has a database would re-apply a schema
	// that is already there — which rig refuses to discover in psql, and says so
	// from `rig validate` instead.
	Foundation FoundationMode `yaml:"foundation,omitempty" json:"foundation,omitempty" jsonschema:"enum=vendored,enum=embedded" jsonschema_description:"Who keeps rig's own migrations. vendored copies them into this project's migrations directory; embedded leaves each module to apply its own. Defaults to vendored, and cannot be changed once the database exists."`
}

// Vendored reports whether rig's own migrations live in this project's migrations
// directory. It is the default, so an unset mode is this one.
func (m Migrations) Vendored() bool { return m.Foundation != FoundationEmbedded }

// Files is where uploaded bytes are kept, and what an upload may be.
//
// It is configuration rather than a Go literal for the reason M5.8 settled for
// `auth:`: a byte cap or a sweep interval written in code is a number the
// generated documentation cannot quote, and a number nobody can find without
// reading the wiring. Everything with a fixed answer lives here, and it is
// resolved when the file is read, so the generated wiring, the specification and
// a client library all quote the same number rather than three zeros meaning
// "something else decides".
//
// Zero means "left out" throughout, the way it does for every duration in this
// file. There is deliberately no way to write a zero that survives: an upload
// cap of nothing and a restore window of nothing are not configurations anybody
// wants, and reading them as "use the default" is kinder than a diagnostic.
type Files struct {
	// Enabled says this project accepts uploads. Off by default, and what makes
	// `server-go` write the wiring at all.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether this project accepts uploads. The server-go generator writes the wiring only when it is set."`

	// Expose projects rig_file as a read-only resource.
	//
	// It is here rather than assumed because the row is the only place the file's
	// URL lives, and a client that cannot sync rig_file cannot use the column
	// that exists for it. Off leaves the table managed and unprojected, which is
	// right for a project whose files are only ever fetched through the download
	// route.
	//
	// There is no write path to find either way: the endpoints rig synthesizes
	// are the upload and the download, and a client that could POST a row with an
	// arbitrary storage key and no bytes has found a way around the rules.
	Expose bool `yaml:"expose,omitempty" json:"expose,omitempty" jsonschema_description:"Project rig_file as a read-only resource, so its url column can be read and synced. There is no generated write path either way."`

	// Backend is where the bytes go: memory or s3.
	Backend string `yaml:"backend,omitempty" json:"backend,omitempty" jsonschema:"enum=memory,enum=s3" jsonschema_description:"Where the bytes are kept. memory is for tests and go run and is not durable. Defaults to memory."`

	S3 FilesS3 `yaml:"s3,omitempty" json:"s3,omitempty" jsonschema_description:"Bucket settings, read only when backend is s3."`

	// MaxBytes caps one upload. It is a hard per-file cap and not a quota: rig
	// does not do storage quotas, and saying so is better than implying one.
	MaxBytes int64 `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty" jsonschema_description:"Largest single upload, in bytes. A hard per-file cap, not a storage quota. Defaults to 26214400 (25 MiB)."`

	// InlineTypes are the sniffed content types served without an attachment
	// disposition.
	//
	// Everything else downloads. The list is short on purpose: a file served
	// inline from the API origin runs there, and the URL is in a synced row that
	// will end up in an <img> or an <a> without anybody thinking about it.
	// text/html is the one that must never be on it.
	InlineTypes []string `yaml:"inline_types,omitempty" json:"inline_types,omitempty" jsonschema_description:"Sniffed content types served inline rather than as an attachment. Kept short: a file served inline from the API origin runs there. Defaults to common image types."`

	// AbandonedAfter is how long a file row may sit with no bytes behind it
	// before the sweeper takes it.
	//
	// A row is inserted before the upload and finalized after it, so one with a
	// null uploaded_at is either in flight or the remains of a request that died.
	// This is how long it gets the benefit of the doubt.
	AbandonedAfter Duration `yaml:"abandoned_after,omitempty" json:"abandoned_after,omitempty" jsonschema_description:"How long a file row with no bytes behind it is left alone before the sweeper reaps it. Defaults to 24h."`

	// RestoreWindow is how long a deleted file stays restorable, and therefore
	// how long its bytes outlive the delete.
	//
	// It lives here rather than in a table configuration because rig_file does
	// not have one — an asymmetry with `restore_window_days` worth knowing about
	// before going to look for the key. The key is refused on rig_file for the
	// same reason: this number is how long the bytes are kept as well as how
	// long the row can be brought back, and a second copy of it could only ever
	// disagree.
	//
	// A projected rig_file gets a generated restore endpoint, whose window is a
	// whole number of days — see [Files.RestoreWindowDays] — so with
	// `files.expose` set this has to be at least 24h.
	//
	// If the bucket has a lifecycle rule of its own, that rule has to outlive
	// this, or a restore inside the window succeeds and hands back a row pointing
	// at nothing. The S3 adapter refuses to start when it can tell that is the
	// case.
	RestoreWindow Duration `yaml:"restore_window,omitempty" json:"restore_window,omitempty" jsonschema_description:"How long a deleted file stays restorable, and how long its bytes outlive the delete. A bucket lifecycle rule has to outlive it. Defaults to 720h (30 days)."`

	// CookieDownloads accepts the session cookie on file GET routes.
	//
	// A stored URL is stable and unsigned and sits in a synced row, which means
	// it exists to be used directly: <img src>, <a download>, a background-image.
	// None of those attach an Authorization header, and a bearer token is the
	// only credential rig's authentication otherwise understands.
	//
	// GET only, and that is not an oversight: widening it past GET reintroduces
	// exactly the cross-site request problem the bearer header avoids. It is
	// opt-in because an application that never renders a file in a browser should
	// not be carrying a cookie path at all.
	CookieDownloads bool `yaml:"cookie_downloads,omitempty" json:"cookie_downloads,omitempty" jsonschema_description:"Accept the session cookie on file download routes, so a stored URL works in an img or a download link. GET only, deliberately: widening it past GET reintroduces the CSRF problem bearer tokens avoid."`
}

// FilesS3 names the bucket and how to reach it.
//
// The credentials are environment variable names rather than values, the way
// the OAuth client secrets already are: rig.yaml is a file people commit.
type FilesS3 struct {
	Bucket string `yaml:"bucket,omitempty" json:"bucket,omitempty" jsonschema_description:"Bucket name. One of bucket and bucket_env has to be set."`
	// BucketEnv names an environment variable holding the bucket, for a name
	// that differs per deployment.
	BucketEnv string `yaml:"bucket_env,omitempty" json:"bucket_env,omitempty" jsonschema_description:"Environment variable holding the bucket name, for a bucket that differs per deployment."`

	Region string `yaml:"region,omitempty" json:"region,omitempty" jsonschema_description:"Bucket region."`
	// Endpoint points at something other than AWS — MinIO, R2, a test double.
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty" jsonschema_description:"Endpoint URL, for an S3-compatible service that is not AWS."`

	AccessKeyEnv string `yaml:"access_key_env,omitempty" json:"access_key_env,omitempty" jsonschema_description:"Environment variable holding the access key id. Defaults to AWS_ACCESS_KEY_ID."`
	SecretKeyEnv string `yaml:"secret_key_env,omitempty" json:"secret_key_env,omitempty" jsonschema_description:"Environment variable holding the secret access key. Defaults to AWS_SECRET_ACCESS_KEY."`
}

// Tracing says whether this project's generated code emits spans.
//
// One key, and the two things somebody expects to find beside it are
// deliberately elsewhere.
//
// The service name is the API's name. A project has already said what it is
// called, and a second name here could disagree with the first — which is the
// kind of thing nobody notices until two deployments show up in a collector
// under names neither of them recognises.
//
// Where the spans go is not generated at all. The same binary runs on a laptop,
// in CI and in production, and only the last of those has a collector; a
// project that had to regenerate to stop exporting would be a project that
// exports from its test suite. That is an environment variable, or a field on
// observe.Config in the main function — see docs/observability.md.
//
// What is left is the one thing that genuinely belongs in a file the generators
// read: whether the emitted code opens spans at all. Off means absent — no
// import of rig/observe, no otel in the go.mod, and not one span that calls a
// no-op.
type Tracing struct {
	// Enabled says this project emits spans.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Whether the generated server, repositories and client open OpenTelemetry spans. Off means the generated code names no tracing library at all."`
}

// Naming tunes the conversion from database names to code and wire names.
type Naming struct {
	JSONCase string `yaml:"json_case,omitempty" json:"json_case,omitempty" jsonschema:"enum=camel,enum=pascal,enum=snake" jsonschema_description:"Shape of generated JSON keys. Defaults to camel."`
	// Initialisms are added to the built-in list of acronyms that stay
	// uppercase in Go identifiers.
	Initialisms []string `yaml:"initialisms,omitempty" json:"initialisms,omitempty" jsonschema_description:"Extra acronyms that stay uppercase in Go identifiers, for example SCB."`
	// Plurals overrides derived plurals, keyed by the singular table name.
	Plurals map[string]string `yaml:"plurals,omitempty" json:"plurals,omitempty" jsonschema_description:"Plural overrides keyed by table name, for the words English inflection gets wrong."`
}

// Validate sets the severity of each configurable convention rule. Every value
// is one of off, warn, or error. Structural rules are not listed here: a
// schema that breaks them cannot be generated from at all.
type Validate struct {
	UnmentionedColumn    string `yaml:"unmentioned_column,omitempty" json:"unmentioned_column,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A column exists in the database but is not mentioned in its table configuration."`
	MissingComment       string `yaml:"missing_comment,omitempty" json:"missing_comment,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A table or column has no comment."`
	ForeignKeyNotIndexed string `yaml:"fk_needs_index,omitempty" json:"fk_needs_index,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A foreign-key column is not covered by an index."`
	TenantIndexLeading   string `yaml:"tenant_id_leading_index,omitempty" json:"tenant_id_leading_index,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"No index leads with the tenant column."`
	BooleanPrefix        string `yaml:"boolean_prefix,omitempty" json:"boolean_prefix,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A boolean column does not read as a predicate."`
	TimestampSuffix      string `yaml:"timestamp_suffix,omitempty" json:"timestamp_suffix,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A timestamp column is not named _at, or an _at column is not a timestamptz. A name ending in _at claims the column records when something happened, and only timestamptz can: a bare timestamp is a clock reading with nothing to anchor it."`
	DateSuffix           string `yaml:"date_suffix,omitempty" json:"date_suffix,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A date column is not named _date, or a _date column is not a date."`
	ForeignKeyNaming     string `yaml:"fk_naming,omitempty" json:"fk_naming,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A foreign-key column is not named after the table it references."`
	CascadeDelete        string `yaml:"cascade_delete,omitempty" json:"cascade_delete,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A foreign key declares ON DELETE CASCADE."`
	MigrationFilename    string `yaml:"migration_filename,omitempty" json:"migration_filename,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A migration file is not named NNNNN_snake_case.sql."`
}

// Generator selects one generator to run and configures it.
//
// Selection is by name rather than by output path, so adding a generator adds
// no fields to this struct.
type Generator struct {
	Name string `yaml:"name" json:"name" jsonschema_description:"Registered generator name, for example persist-go."`
	// OutDir overrides the generator's default output directory, relative to
	// the project root.
	OutDir string `yaml:"out_dir,omitempty" json:"out_dir,omitempty" jsonschema_description:"Output directory, relative to the project root."`
	// Options is validated against the generator's own schema, not this one.
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty" jsonschema_description:"Generator-specific options. Each generator publishes its own schema for this block."`
}

package ir

import "slices"

// Auth is the authentication foundation as this project configured it.
//
// Nil means the project has none: no endpoints are mounted, nothing imports
// rig/auth, and a document consumer has no authentication to describe.
//
// Every value here is resolved rather than optional. A zero in this struct means
// somebody wrote a zero, not "use the default" — the defaults were applied while
// the project configuration was read, so the generated wiring, the emitted
// specification, and a client library reading the document all see the same
// numbers instead of each having to know what rig/auth would have picked.
type Auth struct {
	// BasePath is the prefix the endpoints sit under, for example "/auth".
	BasePath string `json:"base_path"`

	Tenant   AuthTenant   `json:"tenant"`
	Session  AuthSession  `json:"session"`
	Password AuthPassword `json:"password"`
	Limits   AuthLimits   `json:"limits"`

	// AllowRegistration mounts POST <base>/register, where a stranger creates an
	// account with no tenant.
	AllowRegistration bool `json:"allow_registration"`
	// AllowTenantCreation mounts POST <base>/tenants, where somebody signed in
	// makes one and becomes its owner.
	AllowTenantCreation bool `json:"allow_tenant_creation"`
	// RequireVerifiedEmail refuses a sign-in until the address is verified.
	RequireVerifiedEmail bool `json:"require_verified_email"`

	// TrustedProxies are the CIDR ranges whose X-Forwarded-For may be believed.
	// Empty means none, and an address is then read from the connection.
	TrustedProxies []string `json:"trusted_proxies,omitempty"`

	// LogRetention is how long an authentication log entry is kept. Zero keeps
	// everything, which is the default because nothing prunes the table unless a
	// project says to.
	//
	// It is never shorter than the longest window in Limits: those limits are
	// counted from this table, so a shorter window would delete the rows a
	// lockout is adding up to. The configuration refuses one rather than clamping
	// it, and the server refuses to start on one assembled by hand.
	LogRetention Duration `json:"log_retention,omitempty"`

	// OAuth is nil when no provider sign-in is configured, which is also when
	// the provider routes do not exist.
	OAuth *AuthOAuth `json:"oauth,omitempty"`
}

// AuthTenantSource is one way a request can name the tenant it is for.
type AuthTenantSource string

const (
	// TenantFromHeader reads a header, [AuthTenant.Header].
	TenantFromHeader AuthTenantSource = "header"
	// TenantFromHost reads the leftmost label of the Host and looks the slug up
	// in the tenant table. This is the one that survives a redirect, which is
	// what a provider sign-in needs.
	TenantFromHost AuthTenantSource = "host"
	// TenantFromQuery reads a query parameter, [AuthTenant.Query]. Useful for a
	// local demonstration and rarely right in a deployment: it travels in links
	// and logs.
	TenantFromQuery AuthTenantSource = "query"
	// TenantFromHook calls the application's own resolver, which is the escape
	// hatch for a deployment none of the others describe.
	TenantFromHook AuthTenantSource = "hook"
)

// AuthTenant says how a request names the tenant it is for.
//
// It is consulted only where the tenant cannot be known some other way — a
// sign-in, a password reset, the start of a provider flow. Once there is a
// session the tenant comes from the token.
type AuthTenant struct {
	// Sources are tried in order, and the first that names a tenant wins. A
	// request that names none is not an error: a sign-in is allowed to arrive
	// without one.
	Sources []AuthTenantSource `json:"sources"`

	// Header is the header read by [TenantFromHeader].
	Header string `json:"header,omitempty"`
	// Query is the parameter read by [TenantFromQuery].
	Query string `json:"query,omitempty"`
	// DefaultSlugEnv names an environment variable holding the slug to fall back
	// to when the host names no tenant. It is the accommodation that lets a
	// host-based deployment be tried on localhost, where there is no subdomain
	// to read.
	DefaultSlugEnv string `json:"default_slug_env,omitempty"`
}

// Uses reports whether a source is configured.
func (t AuthTenant) Uses(source AuthTenantSource) bool { return slices.Contains(t.Sources, source) }

// AuthSession is how long the things somebody holds stay good for.
type AuthSession struct {
	// AccessTTL is the lifetime of the token that travels on every request.
	AccessTTL Duration `json:"access_ttl"`
	// RefreshTTL is how long an ordinary session lasts.
	RefreshTTL Duration `json:"refresh_ttl"`
	// RememberTTL is how long one lasts when somebody asked to stay signed in.
	RememberTTL Duration `json:"remember_ttl"`
	// RotationLeeway is how long a refresh token stays usable after it has been
	// exchanged, so a dropped response is a retry rather than a logout.
	RotationLeeway Duration `json:"rotation_leeway"`
	// IdentityTTL is the lifetime of the tenant-less credential somebody holds
	// between signing in and picking a tenant.
	IdentityTTL Duration `json:"identity_ttl"`
	// CacheTTL keeps verified access tokens in memory. Zero reads the row every
	// time, which is what makes revocation immediate.
	CacheTTL Duration `json:"cache_ttl"`
}

// AuthPassword is what a new password must satisfy.
type AuthPassword struct {
	MinLength int `json:"min_length"`
	MaxLength int `json:"max_length"`
	// BreachCheck consults Have I Been Pwned's range API when a password is set.
	// The password never leaves the process; five hex digits of its hash do.
	BreachCheck bool `json:"breach_check"`
}

// AuthLimits are the rate limits, counted in the database.
type AuthLimits struct {
	// LoginByEmail locks one account after a handful of wrong passwords.
	LoginByEmail AuthLimit `json:"login_by_email"`
	// LoginByIP throttles one source spraying many accounts. It is deliberately
	// looser than the address limit, so an office behind one NAT does not trip it.
	LoginByIP          AuthLimit `json:"login_by_ip"`
	PasswordReset      AuthLimit `json:"password_reset"`
	VerificationResend AuthLimit `json:"verification_resend"`
	// Refresh bounds one session's rotations.
	Refresh        AuthLimit `json:"refresh"`
	APIKeyFailures AuthLimit `json:"api_key_failures"`
}

// AuthLimit is one limit: how many, and over how long.
type AuthLimit struct {
	Max    int      `json:"max"`
	Window Duration `json:"window"`
}

// AuthOAuth is provider sign-in.
type AuthOAuth struct {
	// BaseURL is this application's own origin, which the callback URL is built
	// from and which the provider has registered exactly.
	BaseURL string `json:"base_url,omitempty"`
	// BaseURLEnv names an environment variable that overrides BaseURL, for the
	// ordinary case where the origin differs per deployment.
	BaseURLEnv string `json:"base_url_env,omitempty"`
	// OriginFromHost derives the origin per request from the Host instead, which
	// is what an application serving a tenant per subdomain needs: the state
	// cookie is set on the host the sign-in started at, and a browser will not
	// send it to a sibling.
	OriginFromHost bool `json:"origin_from_host,omitempty"`

	// SigningKeyEnv names the environment variable holding the key that signs
	// the state parameter. A generated key works for one process and fails
	// across replicas, since a callback may land on a different one.
	SigningKeyEnv string `json:"signing_key_env,omitempty"`

	// StateTTL bounds how long a sign-in round trip may take.
	StateTTL Duration `json:"state_ttl"`

	// AllowProvisioning creates an account the first time somebody signs in with
	// a provider. Off means a provider sign-in only works for an address that
	// already has one.
	AllowProvisioning bool `json:"allow_provisioning"`
	// AllowedReturnTo bounds where a finished sign-in may land. An open redirect
	// is the classic mistake here.
	AllowedReturnTo []string `json:"allowed_return_to,omitempty"`
	// Insecure allows the state cookie over plain HTTP, for local development
	// where a browser would refuse the __Host- prefixed one.
	Insecure bool `json:"insecure,omitempty"`

	Providers []AuthProvider `json:"providers"`
}

// The provider kinds rig knows how to build. Anything else is a provider the
// application constructs itself and hands over.
const (
	ProviderGoogle    = "google"
	ProviderMicrosoft = "microsoft"
	ProviderGitHub    = "github"
)

// AuthProvider is one identity provider to offer.
//
// Credentials are named rather than carried. A client secret in rig.yaml is a
// client secret in version control, so the configuration says which environment
// variable holds it and the generated code reads that.
type AuthProvider struct {
	// Name is one of [ProviderGoogle], [ProviderMicrosoft], [ProviderGitHub].
	// It is also the route segment: <base>/oauth/google/start.
	Name string `json:"name"`

	ClientIDEnv     string `json:"client_id_env"`
	ClientSecretEnv string `json:"client_secret_env"`
	// TenantEnv is Microsoft's own idea of a tenant, which has nothing to do
	// with rig's: "common" accepts any account, a directory id restricts sign-in
	// to one organization.
	TenantEnv string `json:"tenant_env,omitempty"`

	// Required fails startup when the credentials are absent. The default skips
	// the provider instead, so one deployment can offer Google and another not,
	// from the same binary and the same configuration.
	Required bool `json:"required,omitempty"`
}

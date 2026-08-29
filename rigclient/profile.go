package rigclient

import (
	"fmt"
	"time"
)

// AuthProfile is what the document says about this API's authentication.
//
// Generated code declares one; nothing in it is a client's guess. The lifetimes
// are the ones the server enforces, which is what makes refreshing ahead of
// expiry possible at all: a client that had to discover the access lifetime by
// being refused would refresh only after a failed request, and every session
// would cost one wasted 401.
//
// The Has flags are the routes rig mounts conditionally. A call to one that is
// not mounted is refused here, with a sentence saying which project setting
// would mount it — a bare 404 from the server says only that the URL is wrong.
type AuthProfile struct {
	// BasePath is where the endpoints sit, for example "/auth".
	BasePath string

	// AccessTTL is the lifetime of the token that travels on every request.
	AccessTTL time.Duration
	// RefreshTTL is how long an ordinary session lasts.
	RefreshTTL time.Duration
	// RememberTTL is how long one lasts when somebody asked to stay signed in.
	RememberTTL time.Duration
	// RotationLeeway is how long a refresh token stays usable after it has been
	// exchanged. It is also what this package refreshes ahead by: the server
	// having decided how much slack a swap deserves, a client has no business
	// picking a different number.
	RotationLeeway time.Duration
	// IdentityTTL is the lifetime of the tenant-less credential somebody holds
	// between signing in and picking a tenant.
	IdentityTTL time.Duration
	// CacheTTL is how long the server may go on answering out of memory after an
	// invalidation it never received. Zero means it caches nothing and every
	// request reads the row.
	//
	// It is not how long a revocation takes to take effect, and reading it that
	// way is the mistake worth naming. The server publishes an invalidation on
	// the transaction that revokes something, so the ordinary answer is "at
	// once"; this is the worst case for a replica that had lost the channel at
	// that moment.
	//
	// A client cannot act on either number. It is here so that somebody reading
	// the generated document can say how stale an answer could possibly be.
	CacheTTL time.Duration

	// TenantHeader is the header a sign-in names its tenant with, when the
	// project resolves tenants that way. Empty when it does not.
	TenantHeader string
	// TenantQuery is the query parameter that does the same, for the projects
	// configured to read one.
	TenantQuery string

	// HasRegistration is POST <base>/register.
	HasRegistration bool
	// HasTenantCreation is POST <base>/tenants.
	HasTenantCreation bool
	// HasIdentitySessions is the tenant picker: /me/tenants and its siblings,
	// which exist only where identity sessions are configured.
	HasIdentitySessions bool
	// HasAPIKeys is the /api-keys routes.
	HasAPIKeys bool

	// OAuthProviders are the provider names with a sign-in route, in the order
	// the project listed them. Empty when there is no provider sign-in.
	OAuthProviders []string
}

// DefaultRefreshLeeway is used when the profile names none, which is a project
// whose configuration left the rotation leeway at zero.
const DefaultRefreshLeeway = 30 * time.Second

// refreshLeeway is how long before expiry a session renews itself.
//
// Capped at a third of the access lifetime: a leeway longer than the token it
// guards would refresh on every request, which turns one call into two forever.
func (p AuthProfile) refreshLeeway() time.Duration {
	leeway := p.RotationLeeway
	if leeway <= 0 {
		leeway = DefaultRefreshLeeway
	}
	if p.AccessTTL > 0 && leeway > p.AccessTTL/3 {
		leeway = p.AccessTTL / 3
	}
	return leeway
}

// notMounted explains a route this project does not have.
func notMounted(route, because string) error {
	return fmt.Errorf("rigclient: this API does not mount %s: %s", route, because)
}

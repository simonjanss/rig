// Package auth is the authentication foundation, assembled.
//
// The packages under it are the parts: password hashing, session issue and
// rotation, API keys, permission resolution, the endpoints, and the Postgres
// stores behind all of them. Each is separately usable and separately testable,
// and putting them together is the same fifteen lines in every project.
//
// This package is those fifteen lines. Hand it a pool:
//
//	front, err := auth.New(auth.Config{Pool: pool})
//	if err != nil {
//		return nil, err
//	}
//
//	mux := api.Register(api.Handlers{
//		Server: api.Server{Auth: front},
//		…
//	})
//
// One field, because the two halves cannot be wired separately without one of
// them eventually being forgotten: Register takes its GetClaims from it and
// mounts its routes on the mux it returns. It is typed as an interface the
// generated package declares, so a project with no authentication does not
// depend on this module to serve a table.
//
// That is a working login, logout, refresh, password reset, email verification,
// session list and API key endpoint, over the tables `rig setup-project` wrote,
// with lockout and rate limits counted in the database. Nothing about it is
// generated and nothing about it is yours to maintain.
//
// The fields below are the choices an application actually has to make — where
// the tenant comes from, how long a session lasts, who sends the mail. Anything
// further is a reason to build the parts yourself: every one of them is exported,
// this package holds no logic of its own, and Parts returns what it made so that
// a project can go halfway rather than starting over.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/auth/authpg"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/auth/password"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/runtime/cache"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
	"github.com/simonjanss/rig/runtime/throttle"
)

// TenantHeader is where the default tenant resolver looks.
//
// A request has to say which tenant it is for before there is a session to read
// it from: the same address may exist in two of them, and which one is being
// signed into is part of the request. A header is the least surprising default
// and the easiest to try; a subdomain or a path segment is a Config.Tenant away.
const TenantHeader = "X-Tenant-Id"

// DefaultBasePath is where the endpoints mount when nothing says otherwise.
const DefaultBasePath = "/auth"

// Config is what an application chooses. Only Pool is required.
type Config struct {
	// Pool is the database holding the foundation tables.
	Pool *pgxpool.Pool

	// Tenant resolves which tenant a request belongs to. The default reads the
	// X-Tenant-Id header.
	//
	// Replace it with whatever your deployment actually uses — a subdomain, a
	// path segment, a claim in a proxy's header. It is consulted only where a
	// tenant cannot be known some other way: login, a password reset, the OAuth
	// start. Once there is a session the tenant comes from the token.
	Tenant func(*http.Request) (uuid.UUID, error)

	// BasePath prefixes the endpoints. Default [DefaultBasePath].
	//
	// The provider routes sit under it: BasePath + "/oauth/{provider}/start".
	BasePath string

	// OnError renders a failure. The default is this package's own; pass your
	// API's error mapper so that an authentication failure looks like every
	// other failure.
	OnError func(w http.ResponseWriter, r *http.Request, err error)

	// AccessTTL, RefreshTTL and RememberTTL are how long the two kinds of token
	// last, and how long "remember me" extends the refresh token to. Zero means
	// the session package's defaults: 10 minutes, 12 hours, 30 days.
	AccessTTL   time.Duration
	RefreshTTL  time.Duration
	RememberTTL time.Duration

	// RotationLeeway is how long a refresh token stays usable after it has been
	// exchanged. Zero is [session.DefaultRotationLeeway], thirty seconds.
	//
	// Without one, a dropped response is a logout: the client never received the
	// new pair, retries with the old one, and has its whole family revoked for a
	// network blip. See [session.Config.RotationLeeway].
	RotationLeeway time.Duration

	// Cache holds the reads this package makes on every request — verifying a
	// session, verifying an API key — and withdraws them when they stop being
	// true.
	//
	// The zero value caches nothing, which is a row read per authenticated
	// request and the behaviour rig had before this existed. Setting
	// [CacheOptions.Enabled] is not a time-to-live over authentication: it opens
	// a Postgres channel, and every revocation this package performs publishes
	// on the transaction that performed it, so a permission taken away stops
	// working at the moment the change commits rather than when a timer expires.
	//
	// It is [New]'s to own. The bus is built, registered and started here and
	// closed by [Auth.Close], so there is no cache to wire, no publish to
	// remember, and no write path that can be left out.
	Cache CacheOptions

	// Policy is the password policy. The zero value is a minimum length of 12
	// and no composition rules, which is current advice.
	Policy password.Policy

	// BreachChecker is consulted when somebody sets a password. Nil skips the
	// check. password.HIBP is a k-anonymity client for haveibeenpwned that fails
	// open, which is the right way round: a network problem should not stop
	// somebody changing their password.
	BreachChecker password.BreachChecker

	// Notifier sends the mail a flow needs — a reset link, a verification link,
	// an invitation.
	//
	// Nil means none is sent, and it means that silently: account.NoNotifier is
	// substituted and returns success from every method, so in production nobody
	// can reset a password and nothing anywhere says so. Set one. (The one
	// combination that is refused is Mail.Queue with no notifier, which would
	// write rows nothing could ever send.)
	Notifier account.Notifier

	// Mail is the queue for those links: off by default, and turning it on means
	// running a job.
	//
	// Off, a link is minted in the request that asked for it and handed straight
	// to the Notifier, so a provider having a bad minute fails that request,
	// spends the caller's rate-limit budget, and kills a token that was already
	// minted. On, the link is written to the database in the same transaction and
	// sent by [Auth.DispatchMail] — which nothing runs on its own. Turning it on
	// without a cron entry is turning mail off.
	//
	// The trade in the other direction is latency: a queued reset mail arrives up
	// to one dispatch interval late.
	Mail MailOptions

	// RequireVerifiedEmail refuses a login until the address is verified.
	RequireVerifiedEmail bool

	// Grants answers what an account may do. Nil means nobody holds anything,
	// which makes every endpoint carrying a permission answer 403.
	//
	// This is where an application's authorization model plugs in, and rig ships
	// none. It derives the permission *keys* from the schema — api.Permissions()
	// in the generated package is the whole catalogue — and generates the check
	// that reads them off the claims. Deciding who holds which is yours: a table
	// of roles, a switch on the account's level, a claim on a federated token, a
	// map in a constant. Every product answers it differently and a framework
	// that picked one would be wrong for most of them.
	//
	// examples/auth has a worked implementation over role tables of its own, if a
	// starting point is more useful than a blank page.
	Grants authhttp.Grants

	// OnSessionRefresh replaces a session's payload every time it is refreshed.
	//
	// The payload is the application's own context for one session — which device
	// it is, when step-up authentication was last satisfied, which mode somebody
	// picked. It is set at sign-in through session.IssueInput.Payload, carried
	// forward by every rotation, and reaches a handler as tenancy.Claims.Extra,
	// where [github.com/simonjanss/rig/runtime/tenancy.Extra] decodes it into a
	// type of your own.
	//
	// Nil, the default, carries the previous payload forward unchanged. Set this
	// only to recompute it — the hook is handed the token being exchanged, so the
	// current payload is prev.Payload.
	//
	// Not for permissions. It is written at sign-in and again only at refresh, so
	// anything here outlives its own revocation by up to the refresh lifetime.
	// Grants answers that question, per request.
	OnSessionRefresh func(ctx context.Context, prev *session.Token) (json.RawMessage, error)

	// IdentitySessionTTL is how long the tenant-less credential lasts. Zero is
	// [session.DefaultIdentityTTL], half an hour, which is what picking a
	// tenant takes.
	IdentitySessionTTL time.Duration

	// AllowTenantCreation mounts POST /auth/tenants, where somebody signed in
	// makes one and becomes its Owner.
	//
	// Off by default. rig owns the mechanism — the tenant row, the first account,
	// the slug, the transaction — because that is the same everywhere and getting
	// it half-right leaves a tenant nobody can reach. The policy is Tenants below:
	// who may, what a name is allowed to be, and what else a new one needs.
	AllowTenantCreation bool

	// Tenants is what this application decides about making them. Every field is
	// optional; the zero value lets anybody signed in make one called anything.
	Tenants account.TenantOptions

	// AllowRegistration mounts POST /auth/register, where a stranger creates an
	// account with no tenant and lands in the picker.
	//
	// Off by default. Whether anybody may sign themselves up is a product
	// decision — invite-only and open registration are both ordinary — and rig
	// declines to pick one. Off means the route does not exist, rather than
	// answering 403 to something that is there.
	AllowRegistration bool

	// OnRegistered runs inside the transaction that creates a self-registered
	// identity, and an error rolls the sign-up back. The ordinary body is
	// accounts.Provision with Invite set — an application that starts every
	// newcomer in a demo or sandbox tenant leaves an invitation waiting in the
	// picker they land in. Nil registers the person and nothing else.
	//
	// See [github.com/simonjanss/rig/auth/account.Config.OnRegistered].
	OnRegistered func(ctx context.Context, accounts *account.Service, in account.Registered) error

	// Limits override the rate limits. The zero value is the documented set: 5
	// failed logins per address per 15 minutes, 50 per address range, 5 password
	// resets an hour, and so on.
	Limits throttle.Defaults

	// LogRetention is how long an entry in rig_auth_log is kept, and it is only
	// read by [Auth.PruneLog]. Zero, the default, keeps everything and makes that
	// method a no-op.
	//
	// It cannot be shorter than the longest window in Limits, and [New] refuses a
	// configuration where it is rather than clamping it. Those limits are counted
	// from this table: pruning inside a window deletes the failures a lockout is
	// adding up to, so the limit stops limiting and nothing says so. Refusing at
	// startup is the last place that mistake can be caught cheaply.
	LogRetention time.Duration

	// TrustedProxies are the networks whose X-Forwarded-For may be believed.
	//
	// Empty means none, and that is deliberate: an address read from a header a
	// client controls is an address a client chooses, and a rate limit keyed on
	// one is a rate limit an attacker walks around.
	TrustedProxies []netip.Prefix

	// OAuth wires the provider endpoints. Leave Providers empty and none are
	// mounted.
	OAuth OAuth

	// Now is the clock, for tests.
	Now func() time.Time
}

// OAuth is what a provider sign-in needs beyond a provider list.
type OAuth struct {
	// Providers are the identity providers to offer, from the oauth package:
	// oauth.Google(id, secret), oauth.Microsoft(id, secret), and so on.
	Providers []oauth.Provider

	// BaseURL is this application's own address, because the callback URL it
	// registers with a provider has to be absolute.
	BaseURL string

	// Origin overrides BaseURL for one request, for an application served at
	// several origins — a tenant per subdomain, most often. See
	// [oauth.Config.Origin], including what a provider will and will not
	// register.
	Origin func(r *http.Request) string

	// SigningKey signs the state parameter. Required, and at least 32 bytes —
	// [oauth.New] refuses anything shorter.
	//
	// It has to be the same key in every replica: a callback may arrive at a
	// different one than the one that started the flow, and a key invented per
	// process is a sign-in that fails whenever a load balancer is doing its job.
	SigningKey []byte

	// StateTTL bounds how long a sign-in round trip may take. Zero is
	// [oauth.DefaultStateTTL], ten minutes: generous for a redirect, short
	// enough that a stolen state parameter is useless.
	StateTTL time.Duration

	// AllowProvisioning creates an account the first time somebody signs in
	// with a provider. Without it a provider sign-in only works for an address
	// that already has an account.
	AllowProvisioning bool

	// AllowedReturnTo are the paths a sign-in may return to. An open redirect
	// is the classic mistake here.
	AllowedReturnTo []string

	// Insecure allows the state cookie over plain HTTP, for local development
	// where a browser would refuse the __Host- prefixed one. Never anywhere real.
	Insecure bool

	// OnSignIn finishes a provider sign-in. Nil issues a session and answers
	// with the same body a login does, which is [authhttp.Handler.SignIn].
	//
	// A browser flow usually wants a cookie and a redirect instead, and that is
	// the sort of decision this package cannot make: it depends on whether the
	// client is a single-page application, a server-rendered one, or a mobile
	// app catching a deep link.
	OnSignIn func(w http.ResponseWriter, r *http.Request, in oauth.SignIn) error
}

// MailOptions is the mail queue's configuration.
//
// Queue is the switch; everything else has a default, and the defaults are
// notify's, so an operator tuning one dispatcher does not have to learn a second
// set of arithmetic for the other. See account.MailOptions for what each number
// does and which pairs are refused.
type MailOptions struct {
	// Queue writes links to rig_identity_verification_delivery and sends them
	// from [Auth.DispatchMail] instead of from the request.
	Queue bool

	// ClaimTTL is how long one dispatcher's claim is honoured, and SendTimeout
	// bounds one call into the Notifier. SendTimeout has to be the shorter of
	// the two, and rig refuses the pair rather than explaining it.
	ClaimTTL    time.Duration
	SendTimeout time.Duration

	// MaxAttempts, BackoffBase and BackoffCap are the retry arithmetic. The
	// defaults span about eight hours.
	MaxAttempts int
	BackoffBase time.Duration
	BackoffCap  time.Duration

	// Retention is how long a sent, failed or skipped delivery is kept before
	// [Auth.PruneMail] removes it. Zero keeps them forever, which is what every
	// other table in rig currently does and is not a recommendation.
	//
	// The link rows themselves are never pruned by this: they are the record of
	// who was invited and when.
	Retention time.Duration
}

// options is the account package's shape of the same numbers.
func (m MailOptions) options() account.MailOptions {
	return account.MailOptions{
		ClaimTTL:    m.ClaimTTL,
		SendTimeout: m.SendTimeout,
		MaxAttempts: m.MaxAttempts,
		BackoffBase: m.BackoffBase,
		BackoffCap:  m.BackoffCap,
	}
}

// Auth is the assembled foundation.
type Auth struct {
	endpoints *authhttp.Handler
	oauth     *oauth.Handler
	parts     Parts
	// cache is the invalidation channel, nil for a project that configured none.
	// Held here so that [Auth.Close] has something to close.
	cache *cache.Bus
	// retention and clock are what PruneLog needs, kept here rather than read
	// back out of a stored Config so there is one copy of each.
	retention time.Duration
	clock     func() time.Time
	// mailRetention is PruneMail's half of the same, and zero for a project that
	// did not turn the queue on.
	mailRetention time.Duration
}

// now is the clock, defaulting to the wall.
func (a *Auth) now() time.Time {
	if a.clock != nil {
		return a.clock()
	}
	return time.Now()
}

// Parts are the pieces New built, for a project that needs to reach past the
// endpoints: issuing a session after somebody signs in another way, minting a
// key from an admin screen, checking a permission in a service rule.
type Parts struct {
	Accounts *account.Service
	Sessions *session.Manager
	// Identities issues and verifies the credential somebody holds between
	// signing in and choosing a tenant.
	Identities *session.IdentityManager
	APIKeys    *apikey.Manager
	Limiter    *throttle.Limiter
	Stores     *authpg.Stores

	// Cache is the invalidation channel rig's own caches run over, or nil when
	// the project configured none.
	//
	// Here for the one thing rig cannot do on an application's behalf. rig
	// caches what it owns end to end — it makes the read and it makes every
	// write that withdraws it — and the read it does not own is Grants, over
	// role tables belonging to the application. A project that wants that answer
	// cached serves a topic of its own on this bus and publishes on it wherever
	// its roles change; see runtime/cache. Nothing in rig will do it for you,
	// because nothing in rig can see those writes.
	Cache *cache.Bus
}

// New assembles the foundation over a pool.
func New(cfg Config) (*Auth, error) {
	if cfg.Pool == nil {
		return nil, errors.New("auth: no Pool: the foundation lives in the database")
	}
	if err := checkRetention(cfg); err != nil {
		return nil, err
	}

	stores := authpg.New(cfg.Pool)

	// Built before anything that caches, because [cache.Bus.Serve] is what hands
	// back the publisher and a manager cannot be given one that does not exist
	// yet. Nil when the project configured no cache, and every constructor below
	// answers a nil bus with a nil cache — so "off" is a shape rather than a
	// condition threaded through five call sites.
	bus := newCacheBus(cfg)

	// Counted in the database rather than in memory, so two replicas cannot
	// disagree about how many times a password has been tried and a restart does
	// not clear somebody's lockout.
	limits := resolvedLimits(cfg)
	limiter := throttle.New(throttle.NewPostgres(cfg.Pool, throttle.PostgresConfig{}))
	if cfg.Now != nil {
		limiter = limiter.WithClock(cfg.Now)
	}

	// The tenant-less credential the tenant picker runs on. Always built: a
	// sign-in issues one whether or not it lands in a tenant, so there is no
	// configuration where it is absent.
	identities, err := session.NewIdentity(session.IdentityConfig{
		Store: stores.Sessions,
		TTL:   cfg.IdentitySessionTTL,
		Now:   cfg.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: identity sessions: %w", err)
	}

	sessions, err := session.New(session.Config{
		Store:          stores.Sessions,
		Log:            stores.Log,
		AccessTTL:      cfg.AccessTTL,
		RefreshTTL:     cfg.RefreshTTL,
		RememberTTL:    cfg.RememberTTL,
		RotationLeeway: cfg.RotationLeeway,
		Cache: session.NewTokenCache(bus, session.TokenCacheConfig{
			TTL:        cfg.Cache.cacheTTL(),
			MaxEntries: cfg.Cache.MaxEntries,
			Now:        cfg.Now,
		}),
		OnRotate: cfg.OnSessionRefresh,
		Now:      cfg.Now,

		// The same limiter the login limits use, over the same log. A session
		// refreshing sixty times a minute is a client looping.
		Limiter:      limiter,
		RefreshLimit: limits.Refresh,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: sessions: %w", err)
	}

	// The policy carries the breach check, and account.New fills in the hashing
	// parameters and the minimum length when they are zero — so this passes what
	// was chosen and leaves the rest to the package that owns the decision.
	policy := cfg.Policy
	if policy.Breached == nil {
		policy.Breached = cfg.BreachChecker
	}

	accounts, err := account.New(account.Config{
		Store:                stores.Accounts,
		Sessions:             sessions,
		Identities:           identities,
		Tenants:              cfg.Tenants,
		OnRegistered:         cfg.OnRegistered,
		Policy:               policy,
		Log:                  stores.Log,
		Notifier:             cfg.Notifier,
		Outbox:               mailOutbox(cfg, stores),
		Mail:                 cfg.Mail.options(),
		Limiter:              limiter,
		Limits:               cfg.Limits,
		RequireVerifiedEmail: cfg.RequireVerifiedEmail,
		Now:                  cfg.Now,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: accounts: %w", err)
	}

	keys, err := apikey.New(apikey.Config{
		Store: stores.APIKeys,
		Log:   stores.Log,
		Now:   cfg.Now,
		Cache: apikey.NewKeyCache(bus, apikey.KeyCacheConfig{
			TTL:        cfg.Cache.cacheTTL(),
			MaxEntries: cfg.Cache.MaxEntries,
			Now:        cfg.Now,
		}),

		// The other half of what an API-key request costs. Without it, turning
		// the cache on removes one of that request's two reads and leaves the
		// failure count, which is then the only one left — see
		// [apikey.FailureCache].
		FailureCache: apikey.NewFailureCache(bus, apikey.FailureCacheConfig{
			TTL:        cfg.Cache.cacheTTL(),
			MaxEntries: cfg.Cache.MaxEntries,
			Now:        cfg.Now,
		}),

		// A key id is the public half and turns up in logs and configuration,
		// so without this the only thing between one and its secret is how fast
		// somebody can ask.
		Limiter:      limiter,
		FailureLimit: limits.APIKeyFailures,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: api keys: %w", err)
	}

	tenant := cfg.Tenant
	if tenant == nil {
		tenant = TenantFromHeader
	}

	// Defaulted here rather than in each handler, because two of them need it and
	// the OAuth routes hang off the end of it. Leaving it empty and letting each
	// package pick its own default is what put the provider routes at
	// <base>/{provider}/start instead of <base>/oauth/{provider}/start whenever
	// somebody set a base at all.
	base := cfg.BasePath
	if base == "" {
		base = DefaultBasePath
	}
	base = strings.TrimRight(base, "/")

	endpoints, err := authhttp.New(authhttp.Config{
		Accounts:   accounts,
		Sessions:   sessions,
		APIKeys:    keys,
		Tenant:     tenant,
		Grants:     cfg.Grants,
		Identities: identities,
		// The same store the twenty writers write to, handed over as a reader.
		// One table, two contracts, and no second place for the trail to be.
		AuditLog:            stores.Log,
		AllowRegistration:   cfg.AllowRegistration,
		AllowTenantCreation: cfg.AllowTenantCreation,
		BasePath:            base,
		OnError:             cfg.OnError,
		TrustedProxies:      cfg.TrustedProxies,
	})
	if err != nil {
		return nil, fmt.Errorf("auth: endpoints: %w", err)
	}

	out := &Auth{
		endpoints: endpoints,
		cache:     bus,
		parts: Parts{
			Accounts:   accounts,
			Sessions:   sessions,
			Identities: identities,
			APIKeys:    keys,
			Limiter:    limiter,
			Stores:     stores,
			Cache:      bus,
		},
		retention: cfg.LogRetention,
		clock:     cfg.Now,
		// Zero for a project that left the queue off, which is what PruneMail
		// reads as "nothing to prune".
		mailRetention: cfg.Mail.Retention,
	}

	if len(cfg.OAuth.Providers) > 0 {
		signIn := cfg.OAuth.OnSignIn
		if signIn == nil {
			// The same session every other endpoint issues, answered in the same
			// shape a login is. It can be the default because a provider sign-in
			// now resolves to one account in one tenant, which is all an issue
			// needs.
			signIn = endpoints.SignIn
		}

		out.oauth, err = oauth.New(oauth.Config{
			Store:     stores.OAuth(),
			Providers: cfg.OAuth.Providers,
			BaseURL:   cfg.OAuth.BaseURL,
			Origin:    cfg.OAuth.Origin,
			// Under the auth base, always. A provider route beside login is a
			// wildcard beside a literal, which is a surprise nobody asked for.
			BasePath:          base + "/oauth",
			SigningKey:        cfg.OAuth.SigningKey,
			StateTTL:          cfg.OAuth.StateTTL,
			Tenant:            tenant,
			AllowedReturnTo:   cfg.OAuth.AllowedReturnTo,
			AllowProvisioning: cfg.OAuth.AllowProvisioning,
			Insecure:          cfg.OAuth.Insecure,
			Log:               stores.Log,
			Now:               cfg.Now,

			// A provider sign-in ends in a session, which is the same session
			// everything else issues.
			OnSignIn: signIn,
		})
		if err != nil {
			return nil, fmt.Errorf("auth: oauth: %w", err)
		}
	}

	// Last, after every error return in this function — including oauth.New's,
	// which is the one below the assembled Auth rather than above it. A bus
	// started any earlier would leave a goroutine and a connection behind on
	// whichever of them fired, in a function whose caller was handed nothing to
	// close.
	if bus != nil {
		bus.Start()
	}
	return out, nil
}

// Close stops the invalidation channel and waits for it.
//
// Register it with the shutdown, within a timeout — there is nothing in flight
// worth finishing, so this only needs long enough to close a connection:
//
//	app.CloseWithin("auth", 5*time.Second, front.Close)
//
// A project that configured no cache has nothing to close and this returns at
// once, which is what makes registering it unconditionally the right thing to
// write.
//
// Forgetting it is not a correctness problem, and that is deliberate. A bus that
// is not running reports itself as not live, and a cache that is not live reads
// through and stores nothing — so the failure mode of every mistake around this
// lifecycle is a slower server, never a server answering out of a cache nobody
// can withdraw from.
func (a *Auth) Close(ctx context.Context) error {
	if a.cache == nil {
		return nil
	}
	return a.cache.Close(ctx)
}

// Mount registers the endpoints on a mux.
//
// It takes the same mux the generated API is registered on, so that one server
// answers both and the paths cannot collide silently: /auth is separate from the
// API's base path, and Go's router refuses a duplicate pattern.
//
// Setting api.Server.Auth calls this for you, which is the ordinary way. Call it
// directly only when the API layer is not rig's — mounting these endpoints beside
// a hand-written router, for instance.
func (a *Auth) Mount(mux *http.ServeMux) {
	a.endpoints.Mount(mux)
	if a.oauth != nil {
		a.oauth.Mount(mux)
	}
}

// Claims identifies the caller of a request.
//
// Together with Mount this satisfies the Authenticator interface the generated
// API package declares, which is how api.Server.Auth takes a *Auth without the
// generated code importing this module.
//
// This is what to give the generated server as GetClaims. It accepts a session
// token or an API key, resolves the caller's permissions, and refuses anything
// else — so a token, a key, and the roles attached to either mean the same thing
// in a generated handler as they do in the endpoints here.
func (a *Auth) Claims(r *http.Request) (tenancy.Claims, error) {
	return a.endpoints.Claims(r)
}

// Session is the token a request carried, when it carried one.
func (a *Auth) Session(r *http.Request) (*session.Token, error) {
	return a.endpoints.Session(r)
}

// Parts are the pieces this assembled, for the things an endpoint cannot do for
// you.
func (a *Auth) Parts() Parts { return a.parts }

// PruneLog deletes authentication log entries older than
// [Config.LogRetention] and reports how many went. Zero retention keeps
// everything and prunes nothing.
//
// A task somebody runs, not a goroutine this starts. Housekeeping that schedules
// itself inside the server is housekeeping every replica does at once, and this
// one takes a lock on the table the whole authentication path writes to.
// `serve.Config.Tasks` makes it a subcommand for a cron job; the generated
// wiring registers it for a project whose rig.yaml set a window.
func (a *Auth) PruneLog(ctx context.Context) (int, error) {
	if a.retention <= 0 {
		return 0, nil
	}
	return a.parts.Stores.Log.Prune(ctx, a.now().Add(-a.retention))
}

// DispatchMail is one pass of the link queue: claim what is due, send it, mark it.
//
// **This is the guarantee behind every link rig mints, and nothing runs it for
// you.** Register it as a task an operator's cron invokes — a subcommand rather
// than a goroutine, so it is a cron job rather than something racing itself in
// every replica. With Mail.Queue set and no such entry, links are queued and never
// sent.
//
// An empty report and no error for a project that did not turn the queue on, so
// registering the task before deciding costs nothing.
func (a *Auth) DispatchMail(ctx context.Context) (account.MailReport, error) {
	return a.parts.Accounts.DispatchMail(ctx)
}

// PruneMail removes deliveries that are done and older than Mail.Retention, in
// the same task that dispatches them — the way the file sweeper's two rules share
// one.
//
// Never a pending row, and never a link row: those are the record of who was
// invited and when.
func (a *Auth) PruneMail(ctx context.Context) (int, error) {
	if a.mailRetention <= 0 || a.parts.Stores.Outbox == nil {
		return 0, nil
	}
	return a.parts.Stores.Outbox.Prune(ctx, a.mailRetention, a.now())
}

// StopClaimingMail stops taking new deliveries while the pass in flight finishes,
// for the drain half of a shutdown.
func (a *Auth) StopClaimingMail() { a.parts.Accounts.StopClaimingMail() }

// ReleaseMailClaims gives back every lease this process holds, for the close half.
//
// A lease given back is a row the next dispatcher takes immediately, rather than
// one that waits out the TTL. The attempts are not given back: the sends this
// process made were made.
func (a *Auth) ReleaseMailClaims(ctx context.Context) (int, error) {
	return a.parts.Accounts.ReleaseMailClaims(ctx)
}

// mailOutbox is the queue's store, or nil for a project that left it off.
//
// nil rather than a disabled store, because nil is what account.Config reads as
// "the inline path" — a store that existed and refused would be a second way to
// say the same thing.
func mailOutbox(cfg Config, stores *authpg.Stores) account.Outbox {
	if !cfg.Mail.Queue {
		return nil
	}
	return stores.Outbox
}

// resolvedLimits are the limits this configuration will actually enforce.
//
// The zero value means the documented set. It is resolved in one place because
// two callers need the same answer: the wiring below, which hands the numbers to
// the managers, and checkRetention, which refuses a retention window shorter
// than the longest of them. If those two disagreed, the check would be reasoning
// about limits nobody enforces.
func resolvedLimits(cfg Config) throttle.Defaults {
	if cfg.Limits == (throttle.Defaults{}) {
		return throttle.Standard()
	}
	return cfg.Limits
}

// checkRetention refuses a window the rate limits cannot survive.
//
// The same rule `rig.yaml` is checked against when a project is compiled, here
// again for the configuration somebody assembled in Go — which is the whole
// reason the parts are exported, so it has to hold on that path too. Refused
// rather than raised to the limit it would break: a value quietly changed to
// something else is a value nobody can reason about later.
func checkRetention(cfg Config) error {
	if cfg.LogRetention == 0 {
		return nil
	}
	if cfg.LogRetention < 0 {
		return fmt.Errorf("auth: LogRetention is %s; a negative window would delete everything",
			cfg.LogRetention)
	}

	if longest, name := resolvedLimits(cfg).LongestWindow(); cfg.LogRetention < longest {
		return fmt.Errorf("auth: LogRetention (%s) is shorter than the %s rate limit's window (%s), "+
			"and the limits are counted from rig_auth_log — pruning would clear a lockout by deleting "+
			"the failures it counts; keep entries for at least %s",
			cfg.LogRetention, name, longest, longest)
	}
	return nil
}

// TenantFromHeader is the default tenant resolver.
func TenantFromHeader(r *http.Request) (uuid.UUID, error) {
	raw := r.Header.Get(TenantHeader)
	if raw == "" {
		// Absent is not an error: it means "unspecified", and the only two
		// endpoints that ask are login and a password reset. Login signs somebody
		// in to one of their own tenants when nothing named one, which is what
		// a single sign-in page needs — a visitor cannot say which tenants an
		// address belongs to before the password has been checked. A header that
		// is present and malformed is still refused, because that is a caller
		// getting it wrong rather than a caller leaving it out.
		return uuid.Nil, nil
	}

	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, rigerr.BadRequest("%s is not a valid identifier", TenantHeader)
	}
	return id, nil
}

// Providers are the configured sign-in providers, for a sign-in page to draw a
// button per name. Empty when none are wired up.
//
// The names are what the routes take: a provider called Google is reached at
// <BasePath>/oauth/google/start, lowercased.
func (a *Auth) Providers() []string {
	if a.oauth == nil {
		return nil
	}
	return a.oauth.Providers()
}

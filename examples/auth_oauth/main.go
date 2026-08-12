// Command oauth signs people in through an external provider, and resolves which
// tenant they are signing in to from the host.
//
// The host part is not decoration. A provider sign-in has to know the tenant
// *before* the redirect, because that is what the callback joins somebody to — and
// the callback URL is registered with the provider and fixed, so it carries nothing
// an application could read on the way back. A header does not survive it. A query
// parameter does not survive it. A host does.
//
// So this example is served at a wildcard: acme.localhost:8083 is one tenant,
// beta.localhost:8083 is another, and the tenant resolver is three lines. That is
// what a real deployment does, and it is why rig seals the tenant into the state
// cookie at the start of a sign-in rather than resolving it twice.
//
// examples/auth is the other half of this subject: sessions, invitations, API keys,
// permissions, and a dashboard to see them in. This one is about who somebody is.
package main

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/oauth"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/examples/auth_oauth/internal/api"
	"github.com/simonjanss/rig/examples/auth_oauth/internal/store"
	"github.com/simonjanss/rig/examples/auth_oauth/services/bookmark"
	"github.com/simonjanss/rig/examples/auth_oauth/services/idp"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/serve"
)

//go:embed migrations/*.sql
var migrations embed.FS

// localDSN is what `rig db url` prints for this project. Its own port, so this and
// examples/auth can both be up.
const localDSN = "postgres://rig:rig@localhost:55443/rig?sslmode=disable&TimeZone=UTC"

// The two tenants the seed creates, and the addresses they answer to.
//
// *.localhost resolves to 127.0.0.1 without touching /etc/hosts, which is the whole
// reason this example can demonstrate host-based tenancy on a laptop.
const (
	acmeSlug = "acme"
	betaSlug = "beta"
)

func main() {
	serve.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8083"),

		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		Hint: "run `rig db up` to start a local Postgres for this project, " +
			"then open http://acme.localhost:8083",

		MaxStartup:  30 * time.Second,
		MaxShutdown: 20 * time.Second,

		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
			// Two tenants and one person with a password, so both interesting
			// sign-ins are reachable: a stranger joining, and an existing account
			// being linked.
			"seed": seed,
		},
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(ctx context.Context, app *serve.App) (http.Handler, error) {
		return newAPI(ctx, app.Pool, baseURL())
	})
}

// newAPI is everything this server is made of.
//
// base is the origin a provider redirects back to. It is a parameter rather than
// read from the environment in here because a test serves on an ephemeral port and
// has to be able to say so.
func newAPI(ctx context.Context, pool *pgxpool.Pool, base string) (http.Handler, error) {
	repos := store.New(pool, store.Config{})

	// Which providers this run offers, and — when it is the stand-in — the prop
	// that answers for one.
	origins := tenantOrigins(base)
	// The scheme every origin shares, read from the one that was configured. A
	// callback URL is compared exactly, so a hardcoded http here would break the
	// moment somebody ran this behind TLS — which the README tells them to do.
	scheme := schemeOf(base)
	live, demo := providers(origins)
	announce(live, origins)

	// Declared before New because the sign-in hook closes over it: finishing a
	// sign-in means issuing a session, and what issues one is what New returns.
	// The closure only runs on a request, long after this line.
	var (
		front *auth.Auth
		err   error
	)

	front, err = auth.New(auth.Config{
		Pool: pool,

		// The three lines this example exists for.
		//
		// A real deployment writes exactly this and never thinks about tenancy
		// again: every request carries its tenant in the one header no client can
		// forge and every redirect preserves.
		Tenant: tenantFromHost(pool),

		OAuth: auth.OAuth{
			Providers: live,
			BaseURL:   base,

			// A callback comes back to the host it started at.
			//
			// Not a nicety: the state cookie carrying the PKCE verifier is set on
			// the host that started the sign-in, and a browser will not send it to
			// a sibling subdomain. A callback that landed on the canonical host
			// would arrive without it and be refused — so with a tenant per host,
			// the callback URL is per host too, and every one of them is
			// registered with the provider.
			Origin: func(r *http.Request) string { return scheme + "://" + r.Host },

			SigningKey: []byte(cmp.Or(os.Getenv("OAUTH_SIGNING_KEY"),
				"a-development-signing-key-that-is-long-enough")),
			// On, so a stranger with a provider account joins the tenant they
			// signed in at. A business application leaves it off and a provider
			// sign-in then only works for somebody already invited.
			AllowProvisioning: true,
			AllowedReturnTo:   tenantOrigins(base),
			// Plain HTTP on localhost, so the __Host- cookie a browser would
			// insist on is unavailable. Never set this anywhere real.
			Insecure: true,

			// How a sign-in ends, which rig will not decide. A cookie and a
			// redirect, because this is a server-rendered page; the default
			// answers with JSON, for a client that would rather have it.
			OnSignIn: func(w http.ResponseWriter, r *http.Request, in oauth.SignIn) error {
				pair, err := front.Parts().Sessions.Issue(r.Context(), session.IssueInput{
					TenantID:  in.TenantID,
					AccountID: in.AccountID,
					Client:    session.ClientWeb,
					IPAddress: r.RemoteAddr,
					UserAgent: r.UserAgent(),
				})
				if err != nil {
					return err
				}
				setSession(w, in.TenantID, pair)

				note := "signed in with " + in.Provider
				if in.New {
					note = "joined with " + in.Provider
				}
				http.Redirect(w, r, cmp.Or(in.ReturnTo, "/")+"?flash="+url.QueryEscape(note),
					http.StatusFound)
				return nil
			},
		},
	})
	if err != nil {
		return nil, err
	}

	mux := api.Register(api.Handlers{
		Server: api.Server{
			Auth:      front,
			RequestID: func(r *http.Request) string { return r.Header.Get("X-Request-Id") },
		},
		Bookmark: bookmark.New(repos.Bookmarks),
	})

	// The stand-in provider, on the same mux, and only when it is the one in use. A
	// real deployment mounts nothing here: the provider is somebody else's server.
	if demo != nil {
		demo.Mount(mux)
	}

	// And the pages. Small on purpose — this example is one flow, not a dashboard.
	page := &pages{pool: pool, api: mux, providers: front.Providers(), demo: demo != nil}
	page.Mount(mux)

	return mux, nil
}

// baseURL is this application's own origin.
//
// It has to match what a provider has registered, exactly, which is why it is read
// in one place. The default names a subdomain rather than 127.0.0.1: the tenant
// comes from the host, so an origin with no tenant in it is an origin nothing can
// sign in at.
func baseURL() string {
	if raw := os.Getenv("BASE_URL"); raw != "" {
		return strings.TrimRight(raw, "/")
	}
	addr := cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8083")
	_, port, _ := strings.Cut(addr, ":")
	return "http://" + acmeSlug + ".localhost:" + cmp.Or(port, "8083")
}

// schemeOf is http or https, from the origin this run was given.
func schemeOf(base string) string {
	if u, err := url.Parse(base); err == nil && u.Scheme != "" {
		return u.Scheme
	}
	return "http"
}

// tenantOrigins is every address this application answers at: one per tenant.
//
// Two different things need the same list — the provider, which registers each as
// a redirect URI, and AllowedReturnTo, which bounds where a finished sign-in may
// land. A real deployment reads it from configuration, or derives it from the
// tenant table.
func tenantOrigins(base string) []string {
	u, err := url.Parse(base)
	if err != nil {
		return []string{base}
	}
	one := strings.TrimRight(u.String(), "/")

	// A single-host run — `http://localhost:8083`, the one plain-http origin a real
	// provider will register — answers for whatever DEFAULT_TENANT names and has no
	// sibling to offer.
	sibling := otherHost(u.Host)
	if sibling == "" {
		return []string{one}
	}
	other := *u
	other.Host = sibling
	return []string{one, strings.TrimRight(other.String(), "/")}
}

// tenantFromHost is the resolver, and the point of the example.
//
// Three lines of lookup around slugFor, which is the whole of rig's tenancy
// contract: answer which tenant a request is for, or answer that there is none.
// A request with no tenant is an ordinary answer rather than an error — login and a
// password reset are allowed to arrive without one, and everything else takes its
// tenant from the token.
//
// A cache would be the obvious next thing: this is one indexed lookup per request
// that names a tenant, and a slug does not change often. Left out because the
// example is about where the answer comes from, not about how fast.
func tenantFromHost(pool *pgxpool.Pool) func(*http.Request) (uuid.UUID, error) {
	return func(r *http.Request) (uuid.UUID, error) {
		slug := slugFor(r.Host)
		if slug == "" {
			return uuid.Nil, nil
		}

		var id uuid.UUID
		err := pool.QueryRow(r.Context(),
			`SELECT id FROM rig_tenant WHERE lower(slug) = $1 AND deleted_at IS NULL AND is_active`,
			slug).Scan(&id)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			// A host nobody is at. Answered as a bad request rather than as an
			// empty tenant, because a typo in a subdomain should say so instead of
			// quietly becoming "no tenant" and failing somewhere later.
			return uuid.Nil, rigerr.BadRequest("no tenant is served at %s", slug)
		case err != nil:
			return uuid.Nil, rigerr.Internal(err, "resolve the tenant")
		}
		return id, nil
	}
}

// slugFor is which tenant a host names: the leftmost label.
// `acme.localhost:8083` is Acme, and `beta.localhost:8083` is Beta.
//
// One function, because two things ask — the tenant resolver rig calls, and the
// page that names the tenant on screen — and a page disagreeing with the resolver
// about which tenant a host is would be worse than either answer.
//
// DEFAULT_TENANT is the accommodation, and it is worth knowing why it exists.
// Google and Microsoft register a redirect URI exactly, and neither accepts plain
// http for anything but `localhost` or `127.0.0.1` — so `acme.localhost:8083`,
// which is what this example is built around, cannot be registered with either of
// them. Naming a tenant for the host that has no subdomain is what lets a real
// sign-in be tried on a laptop: run at `http://localhost:8083` with
// `DEFAULT_TENANT=acme`, and register that one origin. A deployment on https with a
// wildcard record needs none of it.
func slugFor(host string) string {
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	// An address is not a name with a subdomain in front of it: the leading label
	// of 127.0.0.1 is not a tenant.
	if net.ParseIP(host) == nil {
		if slug, rest, found := strings.Cut(host, "."); found && rest != "" && slug != "" {
			return strings.ToLower(slug)
		}
	}
	return strings.ToLower(strings.TrimSpace(os.Getenv("DEFAULT_TENANT")))
}

// providers is what this run offers, and the stand-in when nothing real is
// configured.
//
// Credentials come from the environment because they are secrets and because they
// are the reader's, not this repository's. Set a pair and that provider appears;
// set none and the prop appears instead, so the example works the moment it is
// cloned. Nothing else changes either way — same routes, same state cookie, same
// subject matching, same refusal to link an unverified address. A provider is three
// URLs and a way to read a profile.
//
// The second return is the stand-in, and it is nil when a real provider was
// configured: a prop that kept serving beside Google would be a second way in that
// nobody registered.
func providers(origins []string) ([]oauth.Provider, *idp.Server) {
	var out []oauth.Provider

	// Google. The consent screen and the credentials are in the Google Cloud
	// console; see the README for which redirect URIs to give it.
	if id, secret := os.Getenv("GOOGLE_CLIENT_ID"), os.Getenv("GOOGLE_CLIENT_SECRET"); id != "" && secret != "" {
		out = append(out, oauth.Google(id, secret))
	}

	// Microsoft. MICROSOFT_TENANT is Microsoft's own idea of a tenant and has
	// nothing to do with rig's: "common" accepts any account, "organizations"
	// excludes personal ones, and a directory id restricts sign-in to one
	// organization. Empty means "common".
	if id, secret := os.Getenv("MICROSOFT_CLIENT_ID"), os.Getenv("MICROSOFT_CLIENT_SECRET"); id != "" && secret != "" {
		out = append(out, oauth.Microsoft(id, secret, os.Getenv("MICROSOFT_TENANT")))
	}

	if id, secret := os.Getenv("GITHUB_CLIENT_ID"), os.Getenv("GITHUB_CLIENT_SECRET"); id != "" && secret != "" {
		out = append(out, oauth.GitHub(id, secret))
	}

	if len(out) > 0 {
		return out, nil
	}

	// The prop. It registers the origins it will redirect back to, because a real
	// provider does, and a stand-in that skipped the check would be teaching the
	// wrong lesson.
	demo := idp.New(origins...)
	return []oauth.Provider{demo.Provider()}, demo
}

// announce prints what a reader has to do with a provider console.
//
// A redirect URI is registered exactly and a mismatch is refused by the provider
// with a message that names nothing useful, so the addresses are worth printing
// rather than deriving by hand. There is one per provider per origin, because with a
// tenant per host the callback comes back to the host the sign-in started at.
func announce(live []oauth.Provider, origins []string) {
	names := make([]string, 0, len(live))
	for _, p := range live {
		names = append(names, p.Name)
	}
	fmt.Printf("sign-in providers: %s\n", strings.Join(names, ", "))

	if len(live) == 1 && live[0].Name == idp.Name {
		fmt.Printf("  %s is a stand-in this example serves itself; set GOOGLE_CLIENT_ID "+
			"and GOOGLE_CLIENT_SECRET (or the MICROSOFT_ pair) to use a real one\n", idp.Name)
		return
	}

	fmt.Println("  register these redirect URIs, exactly:")
	for _, p := range live {
		for _, origin := range origins {
			fmt.Printf("    %s\n", callbackURL(origin, p))
		}
	}
}

// callbackURL is where a provider sends somebody back to.
//
// Derived rather than written down: the auth package owns the route, and a README
// that spelled it out by hand would be wrong the day the base path changed.
func callbackURL(origin string, p oauth.Provider) string {
	return origin + auth.DefaultBasePath + "/oauth/" + strings.ToLower(p.Name) + "/callback"
}

// The seed's fixed values, so the README and a test can name them.
const (
	// SeedEmail already has a password. Signing in with a provider as this address
	// links the two, if the provider says the address is verified.
	SeedEmail    = "ada@acme.test"
	SeedPassword = "correct horse battery staple"
)

// seed makes two tenants and one person, which is the least this example needs.
//
// Two tenants, because one tenant cannot show that the host decides anything. One
// person with a password, because linking an existing account is the interesting
// half of a provider sign-in and there has to be an account to link to.
func seed(ctx context.Context, pool *pgxpool.Pool) error {
	for slug, name := range map[string]string{acmeSlug: "Acme", betaSlug: "Beta"} {
		// The unique index is on lower(slug) and partial, so ON CONFLICT has to
		// name the same expression and predicate to match it.
		if _, err := pool.Exec(ctx, `
			INSERT INTO rig_tenant (id, created_at, name, slug, is_active)
			VALUES ($1, now(), $2, $3, true)
			ON CONFLICT (lower(slug)) WHERE deleted_at IS NULL DO NOTHING`,
			uuid.New(), name, slug); err != nil {
			return fmt.Errorf("tenant %s: %w", slug, err)
		}
	}

	var acme uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT id FROM rig_tenant WHERE lower(slug) = $1 AND deleted_at IS NULL`,
		acmeSlug).Scan(&acme); err != nil {
		return fmt.Errorf("find %s: %w", acmeSlug, err)
	}

	// The person, and their account in Acme only. Signing in at beta.localhost as
	// the same address is a stranger arriving somewhere new, which is the other
	// branch.
	identityID := uuid.New()
	err := pool.QueryRow(ctx,
		`SELECT id FROM rig_identity WHERE lower(email_address) = lower($1)`, SeedEmail).Scan(&identityID)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := pool.Exec(ctx, `
			INSERT INTO rig_identity (id, created_at, email_address, display_name, is_active)
			VALUES ($1, now(), $2, 'Ada', true)`, identityID, SeedEmail); err != nil {
			return fmt.Errorf("identity: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("find identity: %w", err)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO rig_account (id, tenant_id, identity_id, created_at, email_address,
		                     display_name, role, is_active)
		VALUES ($1, $2, $3, now(), $4, 'Ada', 'Owner', true)
		ON CONFLICT (tenant_id, identity_id) WHERE deleted_at IS NULL DO NOTHING`,
		uuid.New(), acme, identityID, SeedEmail); err != nil {
		return fmt.Errorf("account: %w", err)
	}

	// Through the account service, because a hash is not a value to invent.
	front, err := auth.New(auth.Config{Pool: pool})
	if err != nil {
		return err
	}
	if err := front.Parts().Accounts.SetPassword(ctx, identityID, SeedPassword); err != nil {
		return fmt.Errorf("password: %w", err)
	}

	fmt.Printf("seeded %s.localhost and %s.localhost; %s / %q has a password at %s\n",
		acmeSlug, betaSlug, SeedEmail, SeedPassword, acmeSlug)
	return nil
}

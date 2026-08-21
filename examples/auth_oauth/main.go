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
// beta.localhost:8083 is another, and the tenant resolver is two lines of
// rig.yaml — `tenant: {from: [host]}`, which generates the slug lookup. That is
// what a real deployment does, and it is why rig seals the tenant into the state
// cookie at the start of a sign-in rather than resolving it twice.
//
// Everything else about the sign-in is in that file too: the per-host callback
// origin, provisioning, the state cookie's settings, and which environment
// variable holds each provider's credentials. What is left in this file is one
// decision rig will not make — how a sign-in ends for a browser — and the
// stand-in provider the example serves itself.
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
	"log/slog"
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
	"github.com/simonjanss/rig/runtime/serve"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationSources is every set this example applies, in the order they go.
//
// `migrations.foundation` is `embedded` in rig.yaml, so rig's own tables are
// carried by rig/auth rather than vendored into the directory above — and
// api.MigrationSources is the wiring `rig generate` writes for that. It returns
// the module's set first and this example's last, which is the order that matters:
// this example's `00005_demo_oauth_provider.sql` references rig_tenant.
//
// Each set records itself in its own bookkeeping table, so `rig db up` here and a
// deployment of this binary agree about what has run. Which is why the directory
// and the table are named on the Source rather than on migrate.Options: they are
// per-set facts, so UpAll and RequireAll read them from each Source and ignore the
// ones on Options. This example leaves both at rig's defaults and says so anyway,
// because a project that changed `migrations.dir` or `migrations.table` in rig.yaml
// has to change them here and nowhere else.
func migrationSources() []migrate.Source {
	return api.MigrationSources(migrate.Source{
		Name:  "auth_oauth",
		FS:    migrations,
		Dir:   "migrations",
		Table: migrate.DefaultTable,
	})
}

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
			"migrate": migrate.ApplyAll(migrationSources(), migrate.Options{Log: os.Stdout}),
			// Two tenants and one person with a password, so both interesting
			// sign-ins are reachable: a stranger joining, and an existing account
			// being linked.
			"seed": seed,
			// Records of writes nobody is going to send again. A cron entry, not
			// a goroutine: one thing running rather than one per replica.
			"prune-idempotency": api.IdempotencyPruner(0),
		},
		Migrate: migrate.RequireAll(migrationSources(), migrate.Options{}),
	}, func(ctx context.Context, app *serve.App) (http.Handler, error) {
		return newAPI(ctx, app.Pool, baseURL(), app.Logger)
	})
}

// newAPI is everything this server is made of.
//
// base is the origin a provider redirects back to. It is a parameter rather than
// read from the environment in here because a test serves on an ephemeral port and
// has to be able to say so.
func newAPI(ctx context.Context, pool *pgxpool.Pool, base string, log *slog.Logger) (http.Handler, error) {
	repos := store.New(pool, store.Config{})

	// The origin this run answers at. It reaches the generated wiring through the
	// environment, because that is what rig.yaml says reads it: `base_url_env`.
	// One place decides, and a test on an ephemeral port can still say where it is.
	if err := os.Setenv("BASE_URL", base); err != nil {
		return nil, err
	}

	// Every address this application answers at: one per tenant. Two different
	// things need the same list — a provider, which registers each as a redirect
	// URI, and what bounds where a finished sign-in may land.
	origins := tenantOrigins(base)

	// What this run offers. rig builds the three configured providers from the
	// environment variables rig.yaml names; the stand-in is this example's own and
	// appears only when no real credentials were set, so the example works the
	// moment it is cloned.
	//
	// The prop is not a mock: single-use authorization codes, PKCE verified at the
	// token endpoint, and a consent screen that lets you choose whether it says
	// the address is verified.
	live, err := api.ConfiguredProviders(api.OAuthHooks{})
	if err != nil {
		return nil, err
	}
	var (
		demo  *idp.Server
		extra []oauth.Provider
	)
	if len(live) == 0 {
		// It registers the origins it will redirect back to, because a real
		// provider does, and a stand-in that skipped the check would be teaching
		// the wrong lesson.
		demo = idp.New(origins...)
		extra = []oauth.Provider{demo.Provider()}
		live = extra
	}
	announce(live, origins)

	// Declared before New because the sign-in hook closes over it: finishing a
	// sign-in means issuing a session, and what issues one is what New returns.
	// The closure only runs on a request, long after this line.
	var front *auth.Auth

	// Everything about tenancy and everything about the providers is in rig.yaml:
	// the host lookup, the per-host callback origin, provisioning, the state
	// cookie. What is left is the one decision rig will not make for a browser.
	front, err = api.New(pool, api.Hooks{
		// The same logger the Server below gets: an auth route answers on the
		// same mux and in the same shape, so the cause of a 500 from signing in
		// should not be the one line that goes somewhere else.
		Logger: log,

		OAuth: api.OAuthHooks{
			// The stand-in, when it is in use. A real deployment passes none of
			// these: the provider is somebody else's server.
			Extra: extra,

			// One origin per tenant, and a tenant can be created this morning — so
			// the set is not a list in a file. The configured entries are still
			// there; these are added to them.
			ReturnTo: origins,

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
			Logger:    log,
			// Where a write that carried an Idempotency-Key is recorded, so a
			// client that had to send one twice gets one row and one answer.
			DB: pool,
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
	return origin + api.BasePath + "/oauth/" + strings.ToLower(p.Name) + "/callback"
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

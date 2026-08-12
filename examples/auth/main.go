// Command auth runs an API with the authentication foundation wired up.
//
// The other examples have no authentication and say so. This one is the
// counterpart: nothing in it is about notes, and everything in it is about what
// `rig setup-project` writes and what you connect it to.
//
// Two things are worth noticing while reading it.
//
// The foundation is eleven tables and none of them are generated from. Migrations
// 1 to 5 create them and `rig generate` leaves them out: their Go types, their
// stores, their endpoints and their permission checks are all in the rig/auth
// module, so a projected copy would be a few thousand lines in internal/ that
// nothing here calls. What is generated is note — the application — and that is
// the whole of internal/.
//
// The tables are still ordinary rig. They follow the same column conventions as
// any other table, `rig validate` holds them to the same rules, and
// `auth.expose` in rig.yaml puts any of them back in the generated set for an
// administration screen. Nothing about them is hidden; they are simply somebody
// else's code.
package main

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth"
	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/internal/store"
	"github.com/simonjanss/rig/examples/auth/services/authz"
	"github.com/simonjanss/rig/examples/auth/services/note"
	"github.com/simonjanss/rig/examples/auth/services/outbox"
	"github.com/simonjanss/rig/examples/auth/web"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/serve"
)

//go:embed migrations/*.sql
var migrations embed.FS

// localDSN is what `rig db url` prints for this project.
//
// TimeZone is part of it because a timestamptz is an instant with no zone of
// its own: nothing stored depends on this, and `::date`, `date_trunc` and an
// offset-less input string all do. Pinning it means a daily total is the same
// daily total on every machine that runs this.
const localDSN = "postgres://rig:rig@localhost:55442/rig?sslmode=disable&TimeZone=UTC"

func main() {
	serve.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8082"),

		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		Hint: "run `rig db up` to start a local Postgres for this project, " +
			"or point $DATABASE_URL at one you already have",

		MaxStartup:  30 * time.Second,
		MaxShutdown: 20 * time.Second,

		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
			// `auth seed` makes a tenant, an account to sign in as, and the role
			// that lets it write. There is no registration endpoint in this
			// example: who may create an account is a product decision, and the
			// foundation deliberately does not make it for you.
			"seed": seed,
		},
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(ctx context.Context, app *serve.App) (http.Handler, error) {
		return newAPI(ctx, app.Pool)
	})
}

// newAPI is everything this server is made of.
//
// It is a function taking a pool rather than a closure inside Main so that a
// test can build the same thing: what is worth testing about authentication is
// the wiring — that the generated handlers and the auth endpoints agree about
// who the caller is — and a test that assembled its own would be testing
// something else.
func newAPI(ctx context.Context, pool *pgxpool.Pool) (http.Handler, error) {
	repos := store.New(pool, store.Config{})

	// The mail this example would have sent. A Notifier delivers the single-use
	// links the auth package mints; this one keeps them in memory so the
	// interface can show an invitation without a mail server standing by.
	mail := outbox.New(20)

	// The whole foundation, over the tables `rig setup-project` wrote. What it
	// assembles — the stores, the session manager, the account service, the API
	// keys, the rate limiter counted in the database — is all exported and all
	// separately usable; this is the assembly, which is the same in every
	// project and therefore not worth writing in any of them.
	front, err := auth.New(auth.Config{
		Pool:     pool,
		Notifier: mail,

		// Which tenant a request is for.
		//
		// The default reads X-Tenant-Id, and that is enough for everything except a
		// provider sign-in: a browser following a link cannot set a header, and the
		// tenant has to be known *before* the redirect because it is what the
		// callback joins somebody to.
		//
		// A real deployment answers this with the host — acme.example.com — and
		// never thinks about it again. This example runs one process on localhost,
		// so it reads the query string as well, which is what the provider buttons
		// put there.
		Tenant: func(r *http.Request) (uuid.UUID, error) {
			if raw := r.URL.Query().Get("tenant"); raw != "" {
				id, err := uuid.Parse(raw)
				if err != nil {
					return uuid.Nil, rigerr.BadRequest("tenant is not a valid identifier")
				}
				return id, nil
			}
			return auth.TenantFromHeader(r)
		},

		// The one thing rig asks an application to decide. It derives the
		// permission keys from the schema and generates the check; who holds them
		// is this function, which here reads the example's own role tables.
		Grants: authz.Grants(pool),

		// Anybody may sign themselves up, because this example's answer to "who
		// may create a tenant" is also anybody. A product with an invite-only
		// front door leaves this off and the route does not exist.
		//
		// Registering creates a person and nothing else: no tenant, no tenant
		// session, just proof of who they are and a look at the invitations
		// waiting for them.
		AllowRegistration: true,

		// The other half of the picker: making a tenant instead of joining one.
		// rig writes the tenant, the first account and the slug; everything below
		// is this application's policy.
		AllowTenantCreation: true,
		Tenants: account.TenantOptions{
			// Who may. This example says anybody signed in, and the hook is here to
			// show where a rule would go — a domain check is the usual one:
			//
			//	if !strings.HasSuffix(by.EmailAddress, "@rig.app") {
			//		return rigerr.Forbidden("only rig.app may create tenants")
			//	}
			Allow: nil,

			// What a name may be. It takes a pointer, so a rule can normalize
			// instead of refusing — this one insists rather than fixing, because
			// silently retitling what somebody typed is its own surprise.
			Validate: func(_ context.Context, t *account.TenantDraft) error {
				if r, _ := utf8.DecodeRuneInString(t.Name); !unicode.IsUpper(r) {
					return rigerr.Invalid("a tenant's name has to start with a capital letter")
				}
				return nil
			},

			// And what else a new tenant needs, in the transaction that made it:
			// the three roles and their grants. A tenant whose roles failed to seed
			// is a tenant whose Owner can do nothing, so it rolls back with it.
			OnCreated: authz.SeedFor(append(api.PermissionKeys(),
				account.PermissionProvision,
				authhttp.PermissionManageAPIKeys, authhttp.PermissionOwnAPIKey)),
		},

		// So an authentication failure looks like every other failure this API
		// returns. Everything else is a default: /auth for the paths, the
		// X-Tenant-Id header for the tenant, 10-minute access tokens, 12-hour
		// sessions, argon2id, and the documented rate limits.
		OnError: func(w http.ResponseWriter, r *http.Request, err error) {
			api.DefaultErrorMapper(w, r, api.RequestContext{
				RequestID: r.Header.Get("X-Request-Id"),
				Method:    r.Method,
				Path:      r.URL.Path,
			}, err)
		},
	})
	if err != nil {
		return nil, err
	}

	// One resource, because the application has one table. The foundation's
	// eleven are not here and not generated: `rig generate` leaves them out, and
	// everything they would have provided is imported from rig/auth instead.
	mux := api.Register(api.Handlers{
		Server: api.Server{
			// The one line that joins the two halves. Every generated handler
			// identifies its caller with the same verification that issued the
			// token — so a session, an API key, and the permissions attached to
			// either mean the same thing everywhere — and /auth/* lands on this
			// same mux without a second call to remember.
			//
			// The field is an interface the generated package declares, not this
			// package's type, which is why examples/todo does not depend on argon2
			// to serve a list of chores.
			Auth:      front,
			RequestID: func(r *http.Request) string { return r.Header.Get("X-Request-Id") },
		},

		Note: note.New(repos.Notes),
	})

	// The permission table, made to match what the handlers check.
	//
	// api.PermissionKeys() is derived from the schema, so this is what keeps the
	// database from lagging behind a deploy that added a table: a key the code
	// checks and the table does not have is a key no role can hold, which refuses
	// every caller. Rows nothing checks any more are left alone — they grant
	// nothing, and a grant points at the row.
	//
	// The application's job now, not rig's, because these are the application's
	// tables. It is here rather than inside auth.New because construction does no
	// I/O.
	if err := authz.SyncPermissions(ctx, pool, api.PermissionKeys()); err != nil {
		return nil, err
	}

	// And the interface, which is a client of everything above rather than a
	// second way in: every button it has makes an HTTP request to this same mux,
	// with a real Authorization header — including creating a tenant, which used
	// to be the one thing it reached past the API for.
	ui, err := web.New(mux, pool, mail)
	if err != nil {
		return nil, err
	}
	ui.Mount(mux)

	// A bare / goes to the interface, so `go run .` and a browser is the whole
	// getting-started path.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})

	return mux, nil
}

// accountService builds the account service on its own, for the work that
// happens outside a request: the seed, and a test that needs to set a password.
//
// Setting one directly is deliberately not a shortcut around anything — it is
// the same argon2id hashing, the same length policy and the same auth_log entry
// the endpoints go through.
func accountService(pool *pgxpool.Pool) (*account.Service, error) {
	front, err := auth.New(auth.Config{Pool: pool})
	if err != nil {
		return nil, err
	}
	return front.Parts().Accounts, nil
}

// The seed's fixed identifiers and grants, so that the README can show a curl
// that works and the tests can sign in without reading anything back.
const (
	SeedTenantID = "00000000-0000-0000-0000-000000000001"
	SeedEmail    = "ada@example.com"
	SeedPassword = "correct horse battery staple"
)

// seed creates a tenant, an account with a password, and a role that may write
// notes.
//
// The rows are plain SQL, because the foundation's tables have no generated
// repository to go through — that is the point of them belonging to rig/auth. The
// password does go through the account service, because a hash is not a field
// somebody sets: that is the same argon2id, the same policy and the same log
// entry the endpoints use.
func seed(ctx context.Context, pool *pgxpool.Pool) error {
	tenantID, err := uuid.Parse(SeedTenantID)
	if err != nil {
		return err
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	accountID := uuid.New()
	roleID := uuid.New()

	// Idempotent, so `auth seed` twice is not an error: somebody trying the
	// example should not have to know whether they have run it before.
	if _, err := tx.Exec(ctx, `
		INSERT INTO rig_tenant (id, created_at, name, slug, is_active, allowed_email_domains)
		VALUES ($1, now(), 'Example', 'example', true, ARRAY['example.com'])
		ON CONFLICT (id) DO UPDATE SET allowed_email_domains = excluded.allowed_email_domains`,
		tenantID); err != nil {
		return fmt.Errorf("tenant: %w", err)
	}

	// The person first, then their account here. Ada is one person with one
	// password; this tenant is one of the places she works, and the row that
	// says so is the account.
	//
	// Looked up rather than upserted: the address is unique only among live rows
	// — a deleted identity does not hold its address — and a conflict target
	// cannot be inferred from a partial index.
	identityID := uuid.New()
	err = tx.QueryRow(ctx, `
		SELECT id FROM rig_identity
		 WHERE lower(email_address) = lower($1) AND deleted_at IS NULL`,
		SeedEmail).Scan(&identityID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `
			INSERT INTO rig_identity (id, created_at, email_address, display_name, is_active)
			VALUES ($1, now(), $2, 'Ada', true)`,
			identityID, SeedEmail); err != nil {
			return fmt.Errorf("identity: %w", err)
		}
	case err != nil:
		return fmt.Errorf("identity: %w", err)
	}

	err = tx.QueryRow(ctx, `
		SELECT id FROM rig_account
		 WHERE tenant_id = $1 AND identity_id = $2 AND deleted_at IS NULL`,
		tenantID, identityID).Scan(&accountID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if _, err := tx.Exec(ctx, `
			INSERT INTO rig_account (id, tenant_id, identity_id, created_at, email_address,
			                     display_name, role, time_zone, is_active)
			VALUES ($1, $2, $3, now(), $4, 'Ada', 'Owner', 'Europe/Stockholm', true)`,
			accountID, tenantID, identityID, SeedEmail); err != nil {
			return fmt.Errorf("account: %w", err)
		}
	case err != nil:
		return fmt.Errorf("account: %w", err)
	}

	// Two permissions: one the application defines, one the foundation does.
	// They are the same kind of thing — a key in a table — which is why an API
	// key's scopes and a role's grants speak one vocabulary.
	// Derived, not listed. api.PermissionKeys() is every key the generated
	// handlers check, so adding a table cannot leave the seeded Owner unable to
	// touch it; the two administrative keys belong to the auth endpoints rather
	// than to a table, so they are named.
	keys := append(api.PermissionKeys(),
		account.PermissionProvision,
		authhttp.PermissionManageAPIKeys, authhttp.PermissionOwnAPIKey)

	permissions := make(map[string]uuid.UUID, len(keys))
	for _, key := range keys {
		permissions[key] = uuid.New()
	}
	for key, id := range permissions {
		if err := tx.QueryRow(ctx, `
			INSERT INTO permission (id, key, name, description)
			VALUES ($1, $2, $2, 'Seeded by the example.')
			ON CONFLICT (key) DO UPDATE SET name = excluded.name
			RETURNING id`, id, key).Scan(&id); err != nil {
			return fmt.Errorf("permission %s: %w", key, err)
		}
		permissions[key] = id
	}

	if err := tx.QueryRow(ctx, `
		INSERT INTO role (id, tenant_id, key, name, description, is_system)
		VALUES ($1, $2, 'author', 'Author', 'May write notes.', false)
		ON CONFLICT (tenant_id, key) DO UPDATE SET name = excluded.name
		RETURNING id`, roleID, tenantID).Scan(&roleID); err != nil {
		return fmt.Errorf("role: %w", err)
	}

	for _, grant := range []struct {
		what string
		sql  string
		args []any
	}{
		{"account_role", `INSERT INTO account_role (id, account_id, role_id) VALUES ($1, $2, $3)
			ON CONFLICT (account_id, role_id) DO NOTHING`, []any{uuid.New(), accountID, roleID}},
	} {
		if _, err := tx.Exec(ctx, grant.sql, grant.args...); err != nil {
			return fmt.Errorf("%s: %w", grant.what, err)
		}
	}

	// Every derived key, so the seeded Owner can use the whole API.
	for _, key := range keys {
		if _, err := tx.Exec(ctx, `
			INSERT INTO role_permission (role_id, permission_id) VALUES ($1, $2)
			ON CONFLICT DO NOTHING`, roleID, permissions[key]); err != nil {
			return fmt.Errorf("grant %s: %w", key, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// The password last and outside the transaction, because it goes through the
	// account service: argon2id, the length policy, and an auth_log entry are
	// all things this file should not be reimplementing.
	//
	// It is set on the identity, not on the account. One password, however many
	// tenants Ada is later invited into.
	accounts, err := accountService(pool)
	if err != nil {
		return err
	}
	if err := accounts.SetPassword(ctx, identityID, SeedPassword); err != nil {
		return fmt.Errorf("password: %w", err)
	}

	fmt.Printf("seeded tenant %s (addresses in example.com) with %s / %q — Owner, "+
		"holding %d permissions: %s\n", tenantID, SeedEmail, SeedPassword,
		len(keys), strings.Join(keys, ", "))

	return seedIntegration(ctx, pool, tenantID, accountID)
}

// seedIntegration adds a service account and an integration key for it.
//
// This is the shape a "connect your nightly import" button ends in: a service
// account of the integration's own, a key that acts as it, and the person who
// pressed the button recorded as having minted it. What the integration writes is
// then attributable to the integration, and Ada leaving the company does not stop
// the import.
func seedIntegration(ctx context.Context, pool *pgxpool.Pool, tenantID, byAccountID uuid.UUID) error {
	const address = "nightly-import@example.com"

	serviceID := uuid.New()
	err := pool.QueryRow(ctx, `
		SELECT id FROM rig_account
		 WHERE tenant_id = $1 AND lower(email_address) = lower($2) AND deleted_at IS NULL`,
		tenantID, address).Scan(&serviceID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		// kind = Service and no identity_id, which the CHECK on the table
		// requires: nobody signs in as an integration, so there is no person for
		// it to be. An address is still required — it is how somebody finds the
		// thing in a list — but nothing will ever be sent to it.
		if _, err := pool.Exec(ctx, `
			INSERT INTO rig_account (id, tenant_id, created_at, created_by_account_id,
			                     kind, role, email_address, display_name, is_active)
			VALUES ($1, $2, now(), $3, 'Service', 'Basic', $4, 'Nightly import', true)`,
			serviceID, tenantID, byAccountID, address); err != nil {
			return fmt.Errorf("service account: %w", err)
		}
	case err != nil:
		return fmt.Errorf("service account: %w", err)
	default:
		// Already there, and its key was shown once. Minting a second one on
		// every seed would leave a trail of live credentials nobody is tracking.
		//
		// Its scopes are refreshed, though. They are derived from what the code
		// checks, and the code changes: a table that gained an access mode — or a
		// new table altogether — would otherwise leave the integration holding
		// last month's list and failing with a 403 nobody connects to a seed. A
		// scope list is not a credential, so updating it in place costs nothing.
		if _, err := pool.Exec(ctx,
			`UPDATE rig_api_key SET scopes = $2
			  WHERE account_id = $1 AND revoked_at IS NULL`,
			serviceID, integrationScopes()); err != nil {
			return fmt.Errorf("refresh integration scopes: %w", err)
		}
		fmt.Printf("integration %s already exists; its key was shown when it was made "+
			"(scopes refreshed to %s)\n", address, strings.Join(integrationScopes(), ", "))
		return nil
	}

	front, err := auth.New(auth.Config{Pool: pool})
	if err != nil {
		return err
	}

	minted, err := front.Parts().APIKeys.Mint(ctx, apikey.MintInput{
		TenantID:           tenantID,
		AccountID:          serviceID,
		Kind:               apikey.KindIntegration,
		Name:               "Nightly import",
		Scopes:             integrationScopes(),
		CreatedByAccountID: &byAccountID,
	})
	if err != nil {
		return fmt.Errorf("integration key: %w", err)
	}

	// Once, here, and never again: only the hash is stored.
	fmt.Printf("integration key for %s: %s\n", address, minted.Secret)
	return nil
}

// integrationScopes is what the nightly import may do.
//
// One function so that minting a key and refreshing an existing one cannot
// disagree — a list written twice is a list that eventually differs, and the
// difference would show up as a 403 in whichever of the two paths was forgotten.
//
// The wide read is the interesting entry. A nightly import has to see every note
// in the tenant, not the ones it wrote itself, which is exactly what an
// ordinary member's key cannot do — and the reason widening is a permission rather
// than a property of being a machine.
//
// Both read keys, because the wide one is additional. The endpoint checks
// note.read and then ?scope=all checks note.read.all, so a key holding only the
// second could read nothing at all.
func integrationScopes() []string {
	return []string{
		note.PermissionRead,
		note.PermissionWrite,
		note.PermissionReadAll,
		account.PermissionProvision,
	}
}

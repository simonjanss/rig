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
//
// And the settings are not in this file. The `auth:` block in rig.yaml holds
// every fixed choice — the lifetimes, the rotation leeway, the password policy,
// the rate limits, which routes exist — because the reference documentation and
// the client libraries are generated from that file. What is here is the three
// functions a file cannot hold.
package main

import (
	"cmp"
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/examples/auth/internal/api"
	"github.com/simonjanss/rig/examples/auth/internal/store"
	"github.com/simonjanss/rig/examples/auth/services/authz"
	"github.com/simonjanss/rig/examples/auth/services/note"
	"github.com/simonjanss/rig/examples/auth/services/outbox"
	"github.com/simonjanss/rig/examples/auth/web"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/notify"
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
			// The guarantee behind the inbox. The engine the server runs is
			// latency — it turns a notification into an inbox line in
			// milliseconds rather than by the next tick — and this is what takes
			// everything it did not: a process that died mid-pass, a note whose
			// publish_at is next Friday. A subcommand rather than a goroutine, so
			// it is a cron job rather than something racing itself in every
			// replica.
			"dispatch-notifications": dispatchNotifications,
			// Ninety days of authentication trail, and then this takes it. The
			// window is `auth.log_retention` in rig.yaml, and the task only
			// exists because that key is set — a project that keeps everything
			// gets no subcommand rather than one that silently does nothing.
			"prune-auth-log": pruneAuthLog,
			// The same shape for the other thing kept only until nobody will ask
			// for it again: the records of writes that carried an
			// Idempotency-Key. Zero takes the default retention, a day.
			"prune-idempotency": api.IdempotencyPruner(0),
			// `auth seed` makes a tenant, an account to sign in as, and the role
			// that lets it write. There is no registration endpoint in this
			// example: who may create an account is a product decision, and the
			// foundation deliberately does not make it for you.
			"seed": seed,
		},
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(ctx context.Context, app *serve.App) (http.Handler, error) {
		mux, engine, err := newAPI(ctx, app.Pool, app.Logger)
		if err != nil {
			return nil, err
		}

		// A dependency with a shutdown of its own, registered beside the line
		// that built it. Draining stops it taking new work while the server is
		// still answering, which is the right order: the requests in flight are
		// the last ones whose commits will nudge it, and what is left of the
		// window is better spent finishing than starting. Closing runs before
		// the pool does, because what is in flight is a write.
		engine.Start()
		app.Drain("notifications", engine.StopClaiming)
		app.CloseWithin("notifications", 15*time.Second, engine.Close)

		return mux, nil
	})
}

// dispatchNotifications is the task, built the same way the server builds it.
//
// It needs the same object graph the server does — the audience is a method on
// a service — which is the honest cost of computing it late, and the reason this
// file has a constructor both callers share instead of building services inside
// the mount closure.
func dispatchNotifications(ctx context.Context, pool *pgxpool.Pool) error {
	_, engine, err := newAPI(ctx, pool, slog.Default())
	if err != nil {
		return err
	}
	report, err := engine.Resolve(ctx)
	fmt.Fprintln(os.Stdout, report)
	return err
}

// newAPI is everything this server is made of.
//
// It is a function taking a pool rather than a closure inside Main so that a
// test can build the same thing: what is worth testing about authentication is
// the wiring — that the generated handlers and the auth endpoints agree about
// who the caller is — and a test that assembled its own would be testing
// something else.
func newAPI(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (http.Handler, *notify.Engine, error) {
	repos := store.New(pool, store.Config{})

	// The inbox, and the two halves of it that have to be built in this order.
	//
	// The registry is how the dispatcher reaches each notifiable table's
	// answers, and it is populated below with the service that has them —
	// which is the whole reason this file has a constructor the server and
	// the task both call rather than building services inside the mount
	// closure. The audience is computed without a request, so the code that
	// computes it has to be reachable from a job.
	reg := notify.NewRegistry()
	notifier := api.NewNotifications(pool, reg)
	notes := note.New(repos.Notes, notifier, pool)
	reg.Register(api.NewNoteSubject(notes))

	// The mail this example would have sent. A Notifier delivers the single-use
	// links the auth package mints; this one keeps them in memory so the
	// interface can show an invitation without a mail server standing by.
	mail := outbox.New(20)

	// One channel, and it records rather than sends — the same ring buffer the
	// auth links go into, so the interface can show a mail nobody sent. rig
	// ships no transport: what it knows is who is owed what and when, and every
	// provider decision after that is one it would get wrong.
	engine := api.NewNotificationEngine(pool, reg, map[notify.Channel]notify.Sender{
		notify.ChannelEmail: mail.NotificationSender(),
	})

	// The whole foundation, over the tables `rig setup-project` wrote, wired from
	// the auth block in rig.yaml.
	//
	// What it assembles — the stores, the session manager, the account service,
	// the API keys, the rate limiter counted in the database — is all exported and
	// all separately usable; this is the assembly, which is the same in every
	// project and therefore generated rather than written in any of them.
	//
	// Everything with a fixed answer is in the file: the base path, the tenant
	// sources, the token lifetimes and the rotation leeway, the password policy,
	// the rate limits, and that registration and tenant creation are both open.
	// What is left here is the part that is code.
	front, err := api.New(pool, api.Hooks{
		// The same logger the Server below gets: an auth route answers on the
		// same mux and in the same shape, so the cause of a 500 from signing in
		// should not be the one line that goes somewhere else.
		Logger: log,

		Notifier: mail,

		// The one thing rig asks an application to decide. It derives the
		// permission keys from the schema and generates the check; who holds them
		// is this function, which here reads the example's own role tables.
		Grants: authz.Grants(pool),

		// What a tenant is beyond its row and its first account. rig.yaml says the
		// endpoint exists; this says what it does. rig writes the tenant, the first
		// account and the slug; everything below is this application's policy.
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
			OnCreated: authz.SeedFor(append(api.PermissionKeys(), authz.AuthKeys()...)),
		},

		// OnError is left out on purpose: the wiring is generated into this API's
		// own package, so an authentication failure goes through the same error
		// mapper as everything else and a 401 from the sign-in endpoint is shaped
		// like a 401 from anywhere else.
	})
	if err != nil {
		return nil, nil, err
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
			Logger:    log,
			// Where a write that carried an Idempotency-Key is recorded, so a
			// client that had to send one twice gets one row and one answer.
			DB: pool,
		},

		Note:          notes,
		Notifications: notifier,
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
		return nil, nil, err
	}

	// And the interface, which is a client of everything above rather than a
	// second way in: every button it has makes an HTTP request to this same mux,
	// with a real Authorization header — including creating a tenant, which used
	// to be the one thing it reached past the API for.
	ui, err := web.New(mux, pool, mail)
	if err != nil {
		return nil, nil, err
	}
	ui.Mount(mux)

	// A bare / goes to the interface, so `go run .` and a browser is the whole
	// getting-started path.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})

	return mux, engine, nil
}

// pruneAuthLog deletes authentication log entries past the retention window.
//
// It goes through the generated wiring for the same reason [accountService]
// does: the window lives in rig.yaml, and `api.New` is what reads it. Building a
// store by hand here would prune to whatever number this file happened to
// repeat — and the number is the one thing about this job that must not be
// guessed, because too short a window clears a lockout by deleting the failures
// the rate limit counts.
func pruneAuthLog(ctx context.Context, pool *pgxpool.Pool) error {
	front, err := api.New(pool, api.Hooks{Grants: authz.Grants(pool)})
	if err != nil {
		return err
	}
	// The generated task rather than front.PruneLog, the way examples/todo runs
	// the generated file sweeper: it is the wiring being exercised, and it exists
	// only because rig.yaml set a window.
	return api.AuthLogPruner(front)(ctx, pool)
}

// accountService builds the account service on its own, for the work that
// happens outside a request: the seed, and a test that needs to set a password.
//
// It goes through the generated wiring rather than assembling a bare one, so a
// password set here is held to the policy in rig.yaml and not to whatever the
// module's default happens to be. Setting one directly is deliberately not a
// shortcut around anything — it is the same argon2id hashing, the same length
// policy and the same auth_log entry the endpoints go through.
func accountService(pool *pgxpool.Pool) (*account.Service, error) {
	front, err := api.New(pool, api.Hooks{Grants: authz.Grants(pool)})
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

	// Two kinds of permission: the ones the application defines and the ones the
	// foundation does. They are the same kind of thing — a key in a table — which
	// is why an API key's scopes and a role's grants speak one vocabulary.
	//
	// Both derived, neither listed. api.PermissionKeys() is every key the
	// generated handlers check, so adding a table cannot leave the seeded Owner
	// unable to touch it, and authz.AuthKeys() is every key the auth endpoints
	// check, so the same holds when rig adds one of those.
	keys := append(api.PermissionKeys(), authz.AuthKeys()...)

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

	front, err := api.New(pool, api.Hooks{Grants: authz.Grants(pool)})
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

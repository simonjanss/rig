// Command linearlite runs the full-stack example: a kanban board over one
// table, with everything the other examples demonstrate one piece at a time
// wired together.
//
// The pieces, and where each is decided:
//
//   - Accounts, tenants, sessions and API keys are the auth foundation —
//     everything fixed is the `auth:` block in rig.yaml, and what is code is
//     the Hooks literal in newAPI below. The one hook the other examples do
//     not have is OnRegistered: a stranger who signs up is provisioned into
//     the seeded tenant inside the registration transaction, so the picker
//     they land in already lists an invitation.
//   - The board is live. Every table with `electric: enabled` gets shape
//     routes on this same mux, proxying the sync service `rig db up` runs;
//     the browser subscribes through the generated TypeScript client in
//     web/src/api and never talks to Electric directly.
//   - A status change notifies the item's stakeholders — services/todo
//     announces it inside the update's transaction, and the inbox reaches the
//     browser over the rig_notification_recipient shape.
//   - web/ is a React application served from web/dist by this binary, same
//     origin as the API, which is what keeps CORS and cookie stories out of a
//     demonstration that is not about them.
//   - import/ is a separate program driving the generated Go client with a
//     personal API key, slowly, so the board visibly fills while it runs.
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
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	genelectric "github.com/simonjanss/rig/examples/linearlite/internal/electric"
	"github.com/simonjanss/rig/examples/linearlite/internal/store"
	"github.com/simonjanss/rig/examples/linearlite/services/authz"
	"github.com/simonjanss/rig/examples/linearlite/services/rig_account"
	"github.com/simonjanss/rig/examples/linearlite/services/todo"
	"github.com/simonjanss/rig/examples/linearlite/services/todo_attachment"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/electric"
	"github.com/simonjanss/rig/runtime/serve"
)

//go:embed migrations/*.sql
var migrations embed.FS

// localDSN is what `rig db url` prints for this project. TimeZone is pinned
// for the reason every example pins it: date arithmetic reads the session's
// zone, and a daily total should not depend on the machine.
const localDSN = "postgres://rig:rig@localhost:55444/rig?sslmode=disable&TimeZone=UTC"

func main() {
	serve.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8084"),

		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		Hint: "run `rig db up` to start Postgres and the sync service for this project, " +
			"or point $DATABASE_URL at a database you already have",

		MaxStartup:  30 * time.Second,
		MaxShutdown: 20 * time.Second,

		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
			// The demo tenant, two people to sign in as, the level roles, and a
			// board's worth of items. Idempotent, so running it twice is not an
			// error.
			"seed": seed,
			// The inbox's guarantee. The engine the server runs is latency; this
			// is what takes everything it did not — a process that died mid-pass,
			// a replica that never ran.
			"dispatch-notifications": dispatchNotifications,
			// Uploads whose row never arrived, and file rows whose restore window
			// has closed.
			"sweep-files": func(ctx context.Context, pool *pgxpool.Pool) error {
				return api.FileSweeper(api.NewFiles(pool))(ctx, pool)
			},
			// The records of writes that carried an Idempotency-Key — the import
			// job's, mostly. Zero takes the default retention, a day.
			"prune-idempotency": api.IdempotencyPruner(0),
		},
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(ctx context.Context, app *serve.App) (http.Handler, error) {
		mux, engine, err := newAPI(ctx, app.Pool, app.Logger)
		if err != nil {
			return nil, err
		}

		// The engine turns a committed notification into inbox lines within
		// milliseconds; the cron task above is the guarantee behind it.
		// Draining stops it claiming while the server finishes what it has,
		// and closing runs before the pool goes, because what is in flight is
		// a write.
		engine.Start()
		app.Drain("notifications", engine.StopClaiming)
		app.CloseWithin("notifications", 15*time.Second, engine.Close)

		return mux, nil
	})
}

// newAPI is everything this server is made of, as a function taking a pool so
// the tasks and the tests can build exactly what ships.
func newAPI(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*http.ServeMux, *notify.Engine, error) {
	repos := store.New(pool, store.Config{})

	// The inbox's two halves, built in the order the knot demands: the
	// registry first and empty, the engine over it, the service that answers
	// NotifyWho registered into the registry once it exists. The engine can
	// come this early because it does nothing until Start.
	//
	// No channels: the inbox is not one and cannot be turned off, which is all
	// this example needs. examples/auth registers an email sender.
	reg := notify.NewRegistry()
	inbox := api.NewNotifications(pool, reg)
	engine := api.NewNotificationEngine(pool, reg, nil)

	// The engine's Nudge is handed to the service, so a status change becomes
	// an inbox line moments after its transaction commits rather than at the
	// engine's next tick. An optimization, not a guarantee — the cron task is
	// the guarantee — which is why losing it in a test that passes nil is fine.
	todos := todo.New(repos.Todos, inbox, engine.Nudge, pool)
	attachments := todo_attachment.New(repos.TodoAttachments, api.NewFiles(pool))
	members := rig_account.New(repos.RigAccounts)

	reg.Register(api.NewTodoSubject(todos))

	// The authentication foundation, wired from the auth block in rig.yaml.
	// What is left here is the part that is code: who holds a permission, what
	// a new tenant needs, and what happens to a stranger who signs up.
	front, err := api.New(pool, api.Hooks{
		Logger: log,

		// rig derives the permission keys and generates the check; who holds
		// them is this function, over the example's own role tables.
		Grants: authz.Grants(pool),

		// A new tenant gets the three level roles in the transaction that made
		// it, so its Owner can act the moment the response arrives.
		Tenants: account.TenantOptions{
			OnCreated: authz.SeedFor(append(api.PermissionKeys(), authz.AuthKeys()...)),
		},

		// The reason requirement-two of this example works: registering leaves
		// an invitation to the demo tenant waiting in the picker.
		OnRegistered: autoInvite(),
	})
	if err != nil {
		return nil, nil, err
	}

	mux := api.Register(api.Handlers{
		Server: api.Server{
			Auth:      front,
			DB:        pool,
			RequestID: func(r *http.Request) string { return r.Header.Get("X-Request-Id") },
			Logger:    log,
		},
		RigAccount:     members,
		Todo:           todos,
		TodoAttachment: attachments,
		Notifications:  inbox,
	})

	// The live-sync shapes, on the same mux as everything else. The proxy is
	// the only thing a browser talks to: it authenticates the subscriber with
	// the same claims lookup the handlers use, builds the tenant filter, and
	// forwards to the sync service `rig db up` started. The nil scope fields
	// mean the generated tenant filter is the whole scope, which is right for
	// a board the whole tenant shares.
	proxy, err := electric.New(electric.Config{
		URL: cmp.Or(os.Getenv("ELECTRIC_URL"), genelectric.DefaultElectricURL),
	})
	if err != nil {
		return nil, nil, err
	}
	genelectric.Register(mux, genelectric.Handlers{
		Server: genelectric.Server{Proxy: proxy, GetClaims: front.Claims},
	})

	// The permission table, made to match what the handlers check — including
	// the auth endpoints' own keys, because minting a personal API key is
	// gated on one of them and this example's settings page does exactly that.
	if err := authz.SyncPermissions(ctx, pool, api.PermissionKeys()); err != nil {
		return nil, nil, err
	}

	// The front end, same origin as everything above. web/dist is read from
	// disk so `make examples` — which has Go and Docker and deliberately not
	// pnpm — can build and test this server without building the browser half.
	mux.Handle("/", spaHandler(cmp.Or(os.Getenv("WEB_DIR"), "web/dist")))

	return mux, engine, nil
}

// autoInvite is what happens when a stranger signs themselves up: an account
// in the seeded tenant, an invitation waiting in the picker, and the member
// role attached so accepting it lands on a board they can use.
//
// It runs inside the registration transaction — dbx.Tx is how the role grant
// joins it — so a failure here rolls the whole sign-up back rather than
// leaving a person half-invited. A database nobody has seeded is not a
// failure: registration still works, and the newcomer lands in an empty
// picker with "create your own workspace" as the way forward.
func autoInvite() func(context.Context, *account.Service, account.Registered) error {
	tenantID := uuid.MustParse(SeedTenantID)

	return func(ctx context.Context, accounts *account.Service, in account.Registered) error {
		tx, ok := dbx.Tx(ctx)
		if !ok {
			return errors.New("linearlite: OnRegistered expected a transaction")
		}

		var seeded bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM rig_tenant WHERE id = $1 AND deleted_at IS NULL`,
			tenantID).Scan(&seeded); err != nil {
			// pgx answers a missing row as an error; any other failure would
			// resurface on the very next statement, so one branch covers both.
			return nil
		}
		if !seeded {
			return nil
		}

		acct, err := accounts.Provision(ctx, account.ProvisionInput{
			TenantID:     tenantID,
			EmailAddress: in.EmailAddress,
			DisplayName:  in.DisplayName,
			// The invitation is the point: it is what the picker lists, and
			// accepting it is what turns the identity session into a tenant one.
			Invite: true,
		})
		if err != nil {
			return err
		}

		// The role in the same transaction as the account, for the same reason
		// tenant creation seeds roles in its own: an invitation accepted onto a
		// board you cannot read would look exactly like a bug.
		if err := authz.SeedRoles(ctx, tx, tenantID,
			append(api.PermissionKeys(), authz.AuthKeys()...)); err != nil {
			return err
		}
		return authz.AttachRole(ctx, tx, tenantID, acct.ID, string(account.RoleBasic))
	}
}

// dispatchNotifications is the inbox's cron half, built from the same
// constructor the server uses because the audience is a method on a service.
func dispatchNotifications(ctx context.Context, pool *pgxpool.Pool) error {
	_, engine, err := newAPI(ctx, pool, slog.Default())
	if err != nil {
		return err
	}
	report, err := engine.Resolve(ctx)
	fmt.Fprintln(os.Stdout, report)
	return err
}

// accountService builds the account service on its own, for the seed: a
// password set through it is held to the policy in rig.yaml, hashed the same
// way, and recorded in the same trail as one set through the endpoints.
func accountService(pool *pgxpool.Pool) (*account.Service, error) {
	front, err := api.New(pool, api.Hooks{Grants: authz.Grants(pool)})
	if err != nil {
		return nil, err
	}
	return front.Parts().Accounts, nil
}

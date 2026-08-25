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
//   - Who is here, and what they are looking at. `presence:` in rig.yaml is the
//     whole configuration; a heartbeat is a write to rig_presence and the shape
//     over it is how everybody else hears. web/src/presence owns the one
//     heartbeat this application runs, and services/rig_presence is the scope
//     stub that keeps the fan-out to a board rather than a tenant.
//   - web/ is a React application served from web/dist by this binary, same
//     origin as the API, which is what keeps CORS and cookie stories out of a
//     demonstration that is not about them.
//   - import/ is a separate program driving the generated Go client with a
//     personal API key, slowly, so the board visibly fills while it runs.
//   - services/outbox is the mail this example would have sent: the links the
//     auth package mints, and the email copy of an inbox line. rig ships no
//     transport for either, so this is the shape a real one has with none of
//     the substance, and /_demo/outbox is where the front end reads it.
//   - Spans, and rig's own page over them at /_rig/monitor. This is the one
//     example where tracing is on with authentication, uploads, the
//     notification engine and the Electric proxy all running, which is the
//     only arrangement where a trace tells you something you did not know.
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
	"github.com/simonjanss/rig/examples/linearlite/services/outbox"
	"github.com/simonjanss/rig/examples/linearlite/services/rig_account"
	"github.com/simonjanss/rig/examples/linearlite/services/rig_notification_device"
	"github.com/simonjanss/rig/examples/linearlite/services/rig_notification_setting"
	"github.com/simonjanss/rig/examples/linearlite/services/rig_presence"
	"github.com/simonjanss/rig/examples/linearlite/services/todo"
	"github.com/simonjanss/rig/examples/linearlite/services/todo_attachment"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/observe"
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
	// The log file rig's own monitoring page reads. Opened before the server,
	// because the logger is built out of it and the lines written while
	// starting up are lines worth having on the page. Nothing is written unless
	// $RIG_LOG_FILE says where — `make demo` points it at .run/ — and then this
	// costs one branch per log call and nothing else.
	logs, err := observe.OpenLogs(observe.LogConfig{})
	if err != nil {
		// There is no logger yet: this is the thing that would have been half
		// of one.
		fmt.Fprintln(os.Stderr, "cannot open the log file:", err)
		os.Exit(1)
	}

	serve.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8084"),

		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		Hint: "run `rig db up` to start Postgres and the sync service for this project, " +
			"or point $DATABASE_URL at a database you already have",

		MaxStartup: 30 * time.Second,
		// Thirty-five, because the three closers below now declare twenty-five
		// between them — fifteen for the notification engine, five for a trace
		// flush, five for the presence sweeper — and a budget exactly spoken for
		// leaves nothing for the requests still in flight. serve warns about
		// that at startup rather than letting it turn up as a truncated
		// shutdown under load.
		MaxShutdown: 35 * time.Second,

		// Stderr, and the file the monitoring page reads. The two keep their
		// own levels: this one stays at whatever the default handler is set to,
		// and the file keeps debug — which is where rig's request line is, so
		// the page has requests to list without this process printing one per
		// request to a terminal nobody is watching.
		Logger: slog.New(observe.Tee(slog.Default().Handler(), logs.Handler())),

		// A span per statement, from the connection rather than from the
		// generated code: a tracer here sees every query, including the ones the
		// notification engine, the file sweeper and the Electric proxy run and
		// the ones no generator wrote.
		Pool: observe.Pool,

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
			// Presence rows past their window, for an operator who would rather
			// this were a cron job than the goroutine below. Unlike
			// dispatch-notifications it is not the guarantee behind anything:
			// who is present is decided by whoever is reading, against the
			// clock, and correctly within a second. This only keeps the table —
			// and every new subscriber's first fetch — from carrying yesterday.
			"sweep-presence": func(ctx context.Context, pool *pgxpool.Pool) error {
				return api.PresenceSweep(api.NewPresenceSweeper(api.NewPresence(pool)))(ctx, pool)
			},
			// The records of writes that carried an Idempotency-Key — the import
			// job's, mostly. Zero takes the default retention, a day.
			"prune-idempotency": api.IdempotencyPruner(0),
		},
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(ctx context.Context, app *serve.App) (http.Handler, error) {
		// Spans. Nothing is exported unless the environment says where to:
		// $OTEL_EXPORTER_OTLP_ENDPOINT for a collector, $RIG_TRACE_FILE for a
		// file. With neither, the spans cost nothing and the trace ids are
		// still real — which is what the request id below is.
		tracing, err := observe.Setup(ctx, api.Tracing())
		if err != nil {
			return nil, err
		}
		// Its own limit, because a flush to a collector that is not answering
		// must not spend the whole shutdown budget.
		app.CloseWithin("traces", 5*time.Second, tracing.Shutdown)

		// rig's own page over those spans and those log lines, at
		// /_rig/monitor. It reads the span file the provider above is writing,
		// which is why it hangs off it, and the log sink opened in main, which
		// is why that is set here rather than generated. With no
		// $RIG_MONITOR_PASSWORD it mounts nothing and says so once, rather than
		// serving every path, request id and error cause to anybody who asks.
		monitoring := api.Monitoring()
		monitoring.Logs = logs
		page, err := tracing.Page(monitoring)
		if err != nil {
			return nil, err
		}
		if why := page.Unarmed(); why != "" {
			app.Logger.Info("monitoring page not mounted", "reason", why)
		}

		mux, engine, err := newAPI(ctx, app.Pool, app.Logger, page)
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

		// Housekeeping for rig_presence, and a goroutine rather than only the
		// cron task above — which is a decision, not an inconsistency with the
		// engine beside it. The dispatcher takes a lease because resolving an
		// audience twice costs a read and sending twice costs somebody a
		// duplicate mail; deleting rows that have already expired is
		// idempotent, so two replicas sweeping at once agree and the loser
		// deletes nothing.
		//
		// No Drain either: there is nothing in flight worth finishing. A pass
		// interrupted mid-DELETE leaves rows the next pass takes.
		sweeper := api.NewPresenceSweeper(api.NewPresence(app.Pool))
		sweeper.Start()
		app.CloseWithin("presence", 5*time.Second, sweeper.Close)

		return mux, nil
	})
}

// newAPI is everything this server is made of, as a function taking a pool so
// the tasks and the tests can build exactly what ships.
//
// page is the monitoring page when there is a server to mount it on, and nil
// from a task — a cron entry that dispatched notifications and also served a
// page over its own five-minute lifetime would be a page nobody could reach.
// Everything else is identical either way, which is the reason this is a
// function rather than a block inside the mount closure: the audience for a
// notification is a method on a service, so a job has to be able to build one.
func newAPI(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, page *observe.Page) (*http.ServeMux, *notify.Engine, error) {
	// The tracer is a package-level value the provider in main installed, so a
	// task that never called observe.Setup gets a no-op here and every query
	// below runs untraced rather than differently.
	repos := store.New(pool, store.Config{Tracer: observe.Tracer()})

	// The mail this example would have sent — the links the auth package mints,
	// and the email copy of an inbox line. Two interfaces, one ring buffer, and
	// /_demo/outbox shows what went into it. It is per process on purpose:
	// dispatch-notifications gets its own, which is what a separate deployment
	// of the dispatcher actually has.
	mail := outbox.New(20)

	// The inbox's two halves, built in the order the knot demands: the
	// registry first and empty, the engine over it, the service that answers
	// NotifyWho registered into the registry once it exists. The engine can
	// come this early because it does nothing until Start.
	//
	// Two channels, and both record rather than send. rig ships no transport:
	// what it knows is who is owed what and when, and every provider decision
	// after that is one it would get wrong. The inbox itself is not a channel
	// and cannot be turned off — everything here is a copy of a line that was
	// written either way.
	//
	// The two are worth having side by side because they are the two shapes a
	// channel comes in: email has an address on the account and needs nothing
	// registered, and a push has to be told where, which is what a device row
	// is. Mobile is deliberately absent — a channel with no sender has no
	// delivery rows written for it at all, which is the right answer and is
	// what somebody's Mobile preference will show.
	reg := notify.NewRegistry()
	inbox := api.NewNotifications(pool, reg)
	senders := map[notify.Channel]notify.Sender{
		notify.ChannelEmail:   mail.NotificationSender(),
		notify.ChannelDesktop: mail.PushSender(notify.ChannelDesktop),
	}
	engine := api.NewNotificationEngine(pool, reg, senders)

	// The engine's Nudge is handed to the service, so a status change becomes
	// an inbox line moments after its transaction commits rather than at the
	// engine's next tick. An optimization, not a guarantee — the cron task is
	// the guarantee — which is why losing it in a test that passes nil is fine.
	todos := todo.New(repos.Todos, inbox, engine.Nudge, pool)
	attachments := todo_attachment.New(repos.TodoAttachments, api.NewFiles(pool))
	members := rig_account.New(repos.Accounts)

	// The two notification tables a person owns rather than reads: where a push
	// can reach them, and what they want on each channel. Ordinary generated
	// resources — `notifications: expose: true` and an `operations:` line each —
	// which is the whole reason there is no hand-written HTTP for either. They
	// are owner-scoped in the configuration, so a read is already narrowed to
	// the caller's own rows and the service layer only has to say that a row
	// cannot be created naming somebody else.
	devices := rig_notification_device.New(repos.RigNotificationDevices)
	prefs := rig_notification_setting.New(repos.RigNotificationSettings)

	reg.Register(api.NewTodoSubject(todos))

	// The authentication foundation, wired from the auth block in rig.yaml.
	// What is left here is the part that is code: who holds a permission, what
	// a new tenant needs, and what happens to a stranger who signs up.
	front, err := api.New(pool, api.Hooks{
		Logger: log,

		// Where a reset link, a confirmation link and an invitation go. Nil
		// would leave those flows unusable rather than silently broken — the
		// token is in the response for a test to read and nothing reaches a
		// person — and this example wants them walked in a browser.
		Notifier: mail,

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
			Auth: front,
			DB:   pool,
			// The trace this request belongs to, so the identifier in an error
			// body, the request_id on every log line and the span on the
			// monitoring page are one string. A caller that sent its own
			// X-Request-Id is honoured first.
			RequestID: func(r *http.Request) string {
				return cmp.Or(r.Header.Get("X-Request-Id"), observe.TraceID(r))
			},
			Logger: log,
			// Mounted with the resource routes and after them, and not itself
			// traced or logged: rig opens its spans inside each generated
			// handler, so looking at the page does not appear on the page.
			Monitor: page,
		},
		Account:                members,
		Todo:                   todos,
		TodoAttachment:         attachments,
		RigNotificationDevice:  devices,
		RigNotificationSetting: prefs,
		Notifications:          inbox,
		// Who is here. Setting it mounts the three routes under /presence; nil
		// leaves them unmounted, which is what a project that generated the
		// wiring and has not written the front end yet wants.
		//
		// No service layer, and nothing to register: presence has no rules of
		// this schema's to enforce. The account comes from the credential and
		// the target table is checked against PresenceTargets(), which rig
		// wrote from the compiled document.
		Presence: api.NewPresence(pool),
	})

	// The live-sync shapes, on the same mux as everything else. The proxy is
	// the only thing a browser talks to: it authenticates the subscriber with
	// the same claims lookup the handlers use, builds the tenant filter, and
	// forwards to the sync service `rig db up` started.
	//
	// The todo shapes leave their scope nil, so the generated tenant filter is
	// the whole scope — right for a board the whole tenant shares. Presence is
	// the one that does not, and the reason is in services/rig_presence: every
	// heartbeat is a row change delivered to every subscriber, so its shape is
	// the one place where narrowing is what makes the feature affordable.
	proxy, err := electric.New(electric.Config{
		URL: cmp.Or(os.Getenv("ELECTRIC_URL"), genelectric.DefaultElectricURL),
		// Why the sync service was not the one that answered. There is no logger
		// inside the proxy, on purpose, so this is the only way the reason for a
		// 502 on a shape route reaches the log everything else writes to.
		OnError: func(ctx context.Context, err error) {
			log.ErrorContext(ctx, "live sync", slog.Any("error", err))
		},
		// And whether it is there at all, which is twice per outage rather than
		// once per request: the line worth alerting on, where the errors above
		// are one per subscriber and mostly repeat each other.
		OnSyncState: func(ctx context.Context, reachable bool) {
			if reachable {
				log.InfoContext(ctx, "live sync is answering again")
				return
			}
			log.WarnContext(ctx, "live sync is not answering; shapes with a fallback are serving snapshots")
		},
	})
	if err != nil {
		return nil, nil, err
	}
	// TodoFallback is the one shape this example answers without the sync
	// service: the board is what the demonstration is, and a subscriber that
	// gets nothing gets a blank page. The other shapes leave it nil and keep
	// the 502 — presence in particular, where a snapshot of who was here a
	// moment ago is worth less than nothing.
	genelectric.Register(mux, genelectric.Handlers{
		Server:       genelectric.Server{Proxy: proxy, GetClaims: front.Claims},
		RigPresence:  rig_presence.Shape,
		TodoFallback: todo.Fallback(repos.Todos),
	})

	// The permission table, made to match what the handlers check — including
	// the auth endpoints' own keys, because minting a personal API key is
	// gated on one of them and this example's settings page does exactly that.
	if err := authz.SyncPermissions(ctx, pool, api.PermissionKeys()); err != nil {
		return nil, nil, err
	}

	// The demonstration's own two routes: the outbox, and what the tour can
	// offer. Neither is about a table, which is why neither is a resource.
	registerDemo(mux, mail, page, front.Claims, senders)

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
	// No page: this is a cron entry, and its sends land in its own outbox —
	// which is exactly what a separately deployed dispatcher has.
	_, engine, err := newAPI(ctx, pool, slog.Default(), nil)
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

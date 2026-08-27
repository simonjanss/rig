// Command linearlite runs the full-stack example: a kanban board over one
// table, with everything the other examples demonstrate one piece at a time
// wired together.
//
// The pieces, and where each is decided:
//
//   - Accounts, tenants, sessions and API keys are the auth foundation —
//     everything fixed is the `auth:` block in rig.yaml, and what is code is
//     the Hooks literal in internal/app's New. The one hook the other
//     examples do not have is OnRegistered: a stranger who signs up is
//     provisioned into the seeded tenant inside the registration transaction,
//     so the picker they land in already lists an invitation.
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
//
// Every route above is mounted by internal/app's New, which is a package
// rather than a function here so that the suite in integration/ builds exactly
// what ships. What is left in this file is the process around it: the log
// sink, the tracing provider, the embedded migrations, and the serve.Config
// naming the tasks a cron entry runs.
package main

import (
	"cmp"
	"context"
	"embed"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/serve"
)

//go:embed migrations/*.sql
var migrations embed.FS

// migrationSources is this example's migrations and rig's, in the order they have
// to be applied.
//
// `migrations.foundation: embedded` in rig.yaml is what makes this a list rather
// than the one embed above: rig's dozen tables are carried by the modules that own
// them — rig/auth, rig/files, rig/notify, rig/presence, rig/runtime — rather than
// vendored into migrations/, and api.MigrationSources is the wiring `rig generate`
// writes for that. It returns the module sets first and this example's last, which
// is the order that matters: 00001_create_todo.sql references rig_tenant, and
// 00003_roles_and_permissions.sql references rig_account.
//
// Each set records itself in its own bookkeeping table, so `rig db up` here and a
// deployment of this binary agree about what has run. Which is why the directory
// and the table are named on the Source rather than on migrate.Options: they are
// per-set facts, so ApplyAll and RequireAll read them from each Source and ignore
// the ones on Options.
func migrationSources() []migrate.Source {
	return api.MigrationSources(migrate.Source{
		Name:  "linearlite",
		FS:    migrations,
		Dir:   "migrations",
		Table: migrate.DefaultTable,
	})
}

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

	// Spans. Nothing is exported unless the environment says where to:
	// $OTEL_EXPORTER_OTLP_ENDPOINT for a collector, $RIG_TRACE_FILE for a file.
	// With neither, the spans cost nothing and the trace ids are still real —
	// which is what the request id in every error body is.
	//
	// Out here rather than inside the mount closure below, because the page
	// hanging off it is half of serve.Config and the closure runs after that is
	// built. context.Background rather than the startup budget for the same
	// reason; setting a provider up talks to nothing.
	tracing, err := observe.Setup(context.Background(), api.Tracing())
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot set tracing up:", err)
		os.Exit(1)
	}

	// And the flush, here as well as in the CloseWithin below, because a
	// provider built in main is reached by both ways out of this process. A
	// `Tasks:` entry — `migrate`, `seed`, the two cron ones — never reaches the
	// mount closure: serve.Main runs the task and returns. Without this, an
	// hourly dispatch-notifications with $RIG_TRACE_FILE set would open a
	// second rotating writer on the span file the server is already rotating
	// and then drop everything it buffered on the way out.
	//
	// The server path runs both, and the second finds nothing left to do:
	// Shutdown is idempotent.
	defer func() {
		flushing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracing.Shutdown(flushing); err != nil {
			fmt.Fprintln(os.Stderr, "cannot flush the spans:", err)
		}
	}()

	// rig's own page over those spans and those log lines. It reads the span
	// file the provider above is writing, which is why it hangs off it, and the
	// log sink opened above, which is why that is set here rather than
	// generated.
	//
	// It listens on 127.0.0.1:9084 — rig.yaml says so — rather than answering
	// on the API's 8084, because a page that lists every path, request id and
	// error cause this server has seen should be reachable on terms the kernel
	// enforces rather than on a path. With no $RIG_MONITOR_PASSWORD it opens no
	// port at all and says so once.
	monitoring := api.Monitoring()
	monitoring.Logs = logs
	page, err := tracing.Page(monitoring)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot build the monitoring page:", err)
		os.Exit(1)
	}

	serve.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8084"),

		// The monitoring page, on a listener of its own in this same process.
		// Both are zero when the page is unarmed, and then no second port is
		// opened — which is what a laptop with no password set gets.
		Monitor:     page.Handler(),
		MonitorAddr: page.Addr(),

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
			"migrate": migrate.ApplyAll(migrationSources(), migrate.Options{Log: os.Stdout}),
			// The demo tenant, two people to sign in as, the level roles, and a
			// board's worth of items. Idempotent, so running it twice is not an
			// error.
			"seed": app.Seed,
			// The inbox's guarantee. The engine the server runs is latency; this
			// is what takes everything it did not — a process that died mid-pass,
			// a replica that never ran.
			"dispatch-notifications": app.DispatchNotifications,
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
		Migrate: migrate.RequireAll(migrationSources(), migrate.Options{}),
	}, func(ctx context.Context, srv *serve.App) (http.Handler, error) {
		// Its own limit, because a flush to a collector that is not answering
		// must not spend the whole shutdown budget. The provider it stops was
		// built in main; this is the first place there is an App to register a
		// closer with, and it is the server's half of the pair — the defer in
		// main is the half a task run reaches.
		srv.CloseWithin("traces", 5*time.Second, tracing.Shutdown)

		// Said here rather than in main because this is where there is a logger
		// that writes to the file the page would have read.
		if why := page.Unarmed(); why != "" {
			srv.Logger.Info("monitoring page not listening", "reason", why)
		}

		mux, engine, err := app.New(ctx, srv.Pool, srv.Logger, page)
		if err != nil {
			return nil, err
		}

		// The engine turns a committed notification into inbox lines within
		// milliseconds; the cron task above is the guarantee behind it.
		// Draining stops it claiming while the server finishes what it has,
		// and closing runs before the pool goes, because what is in flight is
		// a write.
		engine.Start()
		srv.Drain("notifications", engine.StopClaiming)
		srv.CloseWithin("notifications", 15*time.Second, engine.Close)

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
		sweeper := api.NewPresenceSweeper(api.NewPresence(srv.Pool))
		sweeper.Start()
		srv.CloseWithin("presence", 5*time.Second, sweeper.Close)

		return mux, nil
	})
}

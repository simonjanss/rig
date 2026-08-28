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
// what ships. What is left in this file is the part of the process rig.yaml
// does not already decide: the embedded migrations, the two addresses, and the
// three tasks that are this application's own.
//
// The rest of the process is generated from the blocks that describe it. The
// log sink, the tracing provider and the monitoring page are api.NewProcess,
// out of `tracing:` and `monitoring:`; the four steps a shutdown has to hold are
// api.ShutdownBudget, out of the closers `tracing:`, `notifications:`,
// `presence:` and the shapes between them register; the housekeeping subcommands
// are api.Tasks, out of `files:` and `presence:`. None of those numbers is
// written down twice, which is the whole reason they are not written down here.
// MaxShutdown is the deliberate exception, and says why where it is set.
package main

import (
	"cmp"
	"context"
	"embed"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/simonjanss/rig/examples/linearlite/internal/api"
	"github.com/simonjanss/rig/examples/linearlite/internal/app"
	"github.com/simonjanss/rig/migrate"
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
	// The log file rig's own monitoring page reads, the provider writing the
	// spans it reads beside them, and the page over both. One call, because the
	// three have an order — the sink before the logger built out of it, the
	// provider before the page that reads its file — and rig.yaml is where every
	// part of it was decided.
	//
	// Nothing is written and nothing exported unless the environment says where:
	// $RIG_LOG_FILE, $RIG_TRACE_FILE, $OTEL_EXPORTER_OTLP_ENDPOINT. `make demo`
	// points the first two at .run/ and sets a monitoring password, which is what
	// gives the tour something to open. With none of them set the spans cost
	// nothing, the trace ids are still real — which is what the request id in
	// every error body is — and the page opens no port and says so once.
	process, err := api.NewProcess()
	if err != nil {
		// There is no application logger yet: this is the thing that would have
		// been half of one. slog.Default writes to stderr, which is where this
		// went before it was a structured line.
		slog.Error("cannot set this process up", "error", err)
		os.Exit(1)
	}
	// No `defer process.Close()`, and that is the fix rather than the omission
	// it looks like. Configure sets it as serve.Config.OnExit, so serve.Main runs
	// it on every way out — including the three that end in os.Exit, where a
	// deferred call runs not at all. A `migrate` that failed at three in the
	// morning is exactly the run whose spans somebody wants, and it was the run
	// that used to drop them.
	serve.Main(process.Configure(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8084"),

		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		Hint: "run `rig db up` to start Postgres and the sync service for this project, " +
			"or point $DATABASE_URL at a database you already have",

		MaxStartup: 30 * time.Second,
		// Two seconds of still serving after readiness turns false, before
		// anything stops. Not padding: taking an instance out of a load balancer
		// is not instant — the probe has to fail, and the change has to
		// propagate — and requests sent during that window arrive at a server
		// that has already stopped accepting them. It comes out of MaxShutdown,
		// which is why it is stated beside it.
		DrainDelay: 2 * time.Second,
		// Written out, because this is the number that goes into Kubernetes'
		// terminationGracePeriodSeconds and whoever writes that manifest should
		// be able to read it off this struct rather than run the binary to
		// learn it.
		//
		// Where it comes from: api.ShutdownBudget() is forty — the four steps
		// rig registers declare thirty between them, and ten is left for the
		// requests still in flight — plus the five the auth closer below takes
		// and the two seconds of drain delay above. Its documentation states
		// the breakdown in words.
		//
		// A literal rather than that sum, and it is not a number waiting to
		// drift: serve.App adds up every step actually registered, before the
		// server listens, and refuses a budget that cannot hold them with the
		// parts named. So a stale number here is a process that will not start
		// and says why, which is the one place a wrong number is safe.
		MaxShutdown: 47 * time.Second,

		Tasks: api.Tasks(map[string]serve.Task{
			"migrate": migrate.ApplyAll(migrationSources(), migrate.Options{Log: os.Stdout}),
			// The demo tenant, two people to sign in as, the level roles, and a
			// board's worth of items. Idempotent, so running it twice is not an
			// error.
			"seed": app.Seed,
			// The inbox's guarantee. The engine the server runs is latency; this
			// is what takes everything it did not — a process that died mid-pass,
			// a replica that never ran. It is written here rather than generated
			// because the audience for a notification is a method on a service,
			// and a task that dispatches has to build one.
			"dispatch-notifications": app.DispatchNotifications,
			// sweep-files, sweep-presence and prune-idempotency are api.Tasks's:
			// one generated call each, with every number from rig.yaml and
			// nothing left in them for this file to decide.
		}),
		Migrate: migrate.RequireAll(migrationSources(), migrate.Options{}),
	}), func(ctx context.Context, srv *serve.App) (http.Handler, error) {
		// The trace flush with a limit of its own, and a line about either half
		// of the page that is not armed. Here rather than in main because an App
		// is the first thing there is to register a shutdown step with, and
		// because this is where there is a logger writing to the file the page
		// would have read.
		process.Attach(srv)

		// Housekeeping for rig_presence, and a goroutine rather than only the
		// sweep-presence task above — which is a decision, not an inconsistency
		// with the engine below. The dispatcher takes a lease because resolving
		// an audience twice costs a read and sending twice costs somebody a
		// duplicate mail; deleting rows that have already expired is idempotent,
		// so two replicas sweeping at once agree and the loser deletes nothing.
		api.StartPresenceSweeper(srv)

		parts, err := app.New(ctx, srv.Pool, srv.Logger, process.Page())
		if err != nil {
			return nil, err
		}

		// The engine turns a committed notification into inbox lines within
		// milliseconds; dispatch-notifications above is the guarantee behind it.
		api.StartNotificationEngine(srv, parts.Engine)

		// The live subscriptions, let go at the start of the shutdown rather
		// than waited for at the end of it. Every open tab is holding a poll
		// this server is deliberately not answering yet — an in-flight request
		// as far as http.Server.Shutdown is concerned, and one nothing else can
		// end, since a poll is never late and Shutdown does not cancel a
		// request's context. Without this line one open board is a shutdown that
		// spends its whole budget waiting for the sync service to have news, and
		// the three steps after it each find a deadline that has already passed.
		api.AttachShapes(srv, parts.Shapes)

		// The auth package's invalidation channel, which is a connection and a
		// goroutine of its own. This example configures no auth cache, so today
		// this returns at once — it is here because it is the line rig's own
		// documentation says to write, and an example that skipped it would be
		// teaching the omission that costs a connection the day a cache is
		// turned on.
		srv.CloseWithin("auth", 5*time.Second, parts.Auth.Close)

		return parts.Handler, nil
	})
}

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
// does not already decide: the embedded migrations, the three addresses, and the
// three tasks that are this application's own.
//
// Everything else is api.Main, generated from the blocks that describe it. The
// log sink, the tracing provider and the monitoring page come out of `tracing:`
// and `monitoring:`; the four shutdown steps come out of the closers `tracing:`,
// `notifications:`, `presence:` and `auth:` register, plus the drain the shapes
// between them need; the housekeeping subcommands come out of `files:` and
// `presence:`. None of those numbers is written down twice, which is the whole
// reason they are not written down here — and none of those steps is a line
// this file could forget, which is why api.Parts is a struct rather than a
// paragraph in the documentation.
package main

import (
	"cmp"
	"context"
	"embed"
	"os"
	"time"

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
	api.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8084"),

		Hint: "run `rig db up` to start Postgres and the sync service for this project, " +
			"or point $DATABASE_URL at a database you already have",

		MaxStartup: 30 * time.Second,
		// Nothing below has a default. serve refuses a config that left any of
		// them empty, naming all of them at once, because a value it invented
		// would be one nobody chose — found only by what it costs when it is
		// wrong.
		//
		// Two questions rather than one. Liveness asks whether to restart this
		// process and consults nothing, so a slow database cannot turn one
		// outage into every replica being restarted at once; readiness asks
		// whether to send it work, pings the database, and turns false the
		// moment a shutdown begins.
		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		ConnectTimeout: 10 * time.Second,
		ProbeTimeout:   2 * time.Second,

		// The four the http.Server is built with. ReadHeaderTimeout is the one
		// worth never turning off: without it a single connection that opens
		// and sends nothing holds a goroutine until the process ends.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// Two seconds of still serving after readiness turns false, before
		// anything stops. Not padding: taking an instance out of a load balancer
		// is not instant — the probe has to fail, and the change has to
		// propagate — and requests sent during that window arrive at a server
		// that has already stopped accepting them.
		DrainDelay: 2 * time.Second,
		// And the whole that delay is spent inside. Forty-five is
		// api.ShutdownBudget — 5s for the trace flush, 15s for the notification
		// engine, 5s for the presence sweeper, 5s for the live subscriptions, 5s
		// for the auth cache's invalidation channel, and 10s left for the
		// requests in flight — plus the two above.
		//
		// The delay is inside the total for two separate reasons, and both are
		// worth having straight. serve counts it among the parts it checks
		// against this number — the two seconds here plus thirty-five of steps
		// is thirty-seven — so writing 45 would still start, and would leave the
		// requests in flight eight seconds rather than the ten the budget set
		// aside for them. And terminationGracePeriodSeconds is wall clock from
		// the signal, which the delay is spent inside: a manifest written from a
		// number that left it out sends SIGKILL two seconds before the sequence
		// it was sized for has finished.
		//
		// Written as a literal rather than api.ShutdownBudget()+2*time.Second,
		// because an expression is not something an operator can read off a
		// struct — and reading it off is the entire reason this field has no
		// default. A literal is not a number waiting to drift either: serve adds
		// up every step actually registered before the server listens, so a new
		// block that outgrows 47 is a process that refuses to start and names the
		// parts that no longer fit.
		MaxShutdown: 47 * time.Second,

		Tasks: map[string]serve.Task{
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
			// sweep-files, sweep-presence and prune-idempotency are merged in by
			// api.Main: one generated call each, with every number from rig.yaml
			// and nothing left in them for this file to decide.
		},
		Migrate: migrate.RequireAll(migrationSources(), migrate.Options{}),
	}, func(ctx context.Context, srv *serve.App, page *observe.Page) (api.Parts, error) {
		// The whole of what this application is, and the whole of what a main
		// function still writes. What comes back is api.Parts: the routes, and
		// the two things rig starts or shuts down on the other side of this call
		// — the notification engine and the auth cache's invalidation channel.
		// The trace flush and the presence sweeper need nothing from here, so
		// they are not fields; api.Main registers the one and starts the other
		// before this even runs. The live subscriptions are not one either: the
		// proxy is named in api.Handlers.Shapes, which mounts the shape routes
		// and registers their drain in the same call.
		//
		// A struct rather than five arguments, because the other two callers of
		// this constructor have less than all of it — dispatch-notifications
		// above builds the same graph from a task, where there is no App, no
		// page and no sync service to forward a subscription to.
		return app.New(ctx, app.Config{
			Pool:   srv.Pool,
			Logger: srv.Logger,
			App:    srv,
			Page:   page,
			// The third address this file decides, beside DatabaseURL and Addr
			// above. rig.yaml names the one `rig db up` starts, which is the
			// default a deployment overrides here rather than a value it is
			// stuck with.
			ElectricURL: cmp.Or(os.Getenv("ELECTRIC_URL"), api.DefaultElectricURL),
		})
	})
}

// Command todo runs the API.
//
// Two things to run: the server, and the migrations. They are separate on
// purpose — see the comment on migrateOnly — and both live in this binary, so
// the schema and the code that expects it ship together.
//
// The pool, the HTTP server and the shutdown are serve.Main. Everything between
// the request and the SQL statement was generated from the schema. What is left
// is the one call below: what the server is configured with, and what it is
// made of.
package main

import (
	"cmp"
	"context"
	"embed"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/todo/internal/api"
	"github.com/simonjanss/rig/examples/todo/internal/store"
	"github.com/simonjanss/rig/examples/todo/notify"
	"github.com/simonjanss/rig/examples/todo/services/todo"
	todo_attachment "github.com/simonjanss/rig/examples/todo/services/todo_attachment"
	"github.com/simonjanss/rig/examples/todo/web"
	"github.com/simonjanss/rig/migrate"
	// Aliased, because this example already has a package called notify: a
	// recorder that prints what it is handed, which exists to demonstrate the
	// shape of a background dependency. The two are worth telling apart — that
	// one is this application's own, and this one is rig's inbox.
	rignotify "github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/serve"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// migrations travel with the binary, so the schema a build expects is the
// schema that build carries. A deployment that ran `rig` instead would be
// running whatever version of the tool that machine happens to have.
//
//go:embed migrations/*.sql
var migrations embed.FS

// localDSN is what `rig db url` prints for this project, so `rig db up` and
// then `go run .` is the whole setup. $DATABASE_URL wins when it is set.
const localDSN = "postgres://rig:rig@localhost:55440/rig?sslmode=disable"

func main() {
	api.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8080"),

		// What to say when the database is not there, which is the first thing
		// to go wrong for somebody who has just cloned this. It is said within a
		// second rather than at the end of the connect timeout.
		Hint: "run `rig db up` to start a local Postgres for this project, " +
			"or point $DATABASE_URL at one you already have",

		// Where this project's log goes. A project with `tracing:` on has this
		// filled in by api.Main from the sink it builds; with no tracing block
		// there is nowhere else for it to come from, so it is said here.
		Logger: slog.Default(),

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
		// The field that leaves the program: it is what goes into
		// terminationGracePeriodSeconds, so it has no default and every project
		// writes it. serve adds up what it was actually given before the server
		// listens, so a number this arithmetic got wrong is a refusal to start
		// rather than a truncated shutdown under load.
		//
		// Forty rather than api.ShutdownBudget's thirty, and the ten are this
		// example's own: rig's steps are counted for us, and the recorder and
		// the store's cache subscription below are five seconds each that
		// nothing generated can know about. This is the arithmetic
		// ShutdownBudget's documentation describes — read the total, add your
		// own closers, write the sum.
		MaxShutdown: 40 * time.Second,

		// `todo migrate` applies the schema and exits: a job before the
		// rollout, so one process migrates and the replicas only serve.
		//
		// `todo sweep-files` and `todo prune-idempotency` are api.Tasks's, out
		// of the `files:` block and the idempotency table every project has: the
		// abandoned uploads and the trash past its restore window, and the
		// records of writes nobody is going to send again. Subcommands rather
		// than goroutines, so each is a cron job rather than something racing
		// itself in every replica.
		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
			// The guarantee behind the inbox, for everything the in-process
			// engine did not get to. It builds its own object graph because it
			// needs the same one the server does: the audience is a method on a
			// service, which is the honest cost of computing it late — and the
			// reason this one is not api.Tasks's.
			"dispatch-notifications": dispatchNotifications,
		},

		// The server does not apply them. It refuses to start without them,
		// which is what catches a deploy that got ahead of its migration job.
		// Swap Require for Apply to migrate on the way up instead — fine for a
		// single instance, and rig/migrate's package documentation says what
		// that costs when there is more than one.
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(_ context.Context, app *serve.App) (api.Parts, error) {
		// Everything the server is made of, in the order it comes to exist:
		// the notifier before the service that reports to it, the service
		// before the handler that routes to it.

		// A dependency with a shutdown of its own, registered beside the line
		// that builds it. Draining stops it recording while the server is
		// still answering, so the requests in flight are the last things it
		// hears about; closing writes what is left, after the server has
		// stopped, which is what makes that final batch complete.
		notifier := notify.New(os.Stdout, 30*time.Second)
		notifier.Start()
		app.Drain("notifier", notifier.StopRecording)
		app.CloseWithin("notifier", 5*time.Second, notifier.Close)

		// The store owns an invalidation channel of its own, because `todo` set
		// `cache: true`. It is built, served and started inside New, so there is
		// nothing to wire and no order to get right — and the shutdown below is the
		// whole application-facing surface of it.
		repos := store.New(app.Pool, store.Config{Logger: app.Logger})

		// Safe to leave out, which is the point: a listener that has stopped
		// reports itself as not live, and a cache that is not live reads through
		// and holds nothing. Forgetting this costs a Postgres connection held
		// until the process exits, not a row nobody can withdraw.
		app.CloseWithin("store", 5*time.Second, repos.Close)

		// Where uploads go. The pool is the repositories' own, and that is not
		// incidental: the transaction that finalizes a file and writes the row
		// pointing at it has to be one transaction.
		//
		// Everything about it — the byte cap, the types served inline, how long
		// an abandoned upload is left alone — came from the `files:` block in
		// rig.yaml, so none of it is a number in this file.
		fileSvc := api.NewFiles(app.Pool)

		// The inbox, and the knot in building it: a service needs the notify
		// service to announce anything, and the dispatcher needs the service to
		// ask who should be told. So the registry is made first and empty, and
		// filled once the service it points at exists.
		//
		// This is the whole of what a project without authentication has to do
		// differently, which is nothing: a notification is addressed to an
		// account, and where the claims naming that account come from — here, a
		// header — is not this wiring's business.
		reg := rignotify.NewRegistry()
		inbox := api.NewNotifications(app.Pool, reg)

		// One field per resource is the whole registration surface: adding a
		// table and forgetting to wire it up does not compile.
		svc := todo.New(repos.Todos, fileSvc, notifier, inbox, app.Pool, app.Logger)
		attachments := todo_attachment.New(repos.TodoAttachments, fileSvc)

		reg.Register(api.NewTodoSubject(svc))

		// A dependency with a shutdown of its own, registered beside the line
		// that builds it. The engine is latency — it turns a notification into
		// an inbox line in milliseconds rather than by the next tick — and the
		// task below is the guarantee.
		// No channels, deliberately. This example has no mail and no push, and
		// the inbox works anyway — it is not a channel, which is the whole
		// reason it cannot be turned off. examples/auth has the other half.
		// Handed back as api.Parts.Engine rather than started here: api.Main
		// starts it and registers both its shutdown steps, with the numbers the
		// budget above counted for them.
		engine := api.NewNotificationEngine(app.Pool, reg, nil)

		mux := api.Register(api.Handlers{
			Server: api.Server{
				GetClaims: headerClaims,
				// Where a write that carried an Idempotency-Key is recorded,
				// so a client that had to send one twice gets one row.
				DB: app.Pool,
				// No RequestID: the generated default already reads the caller's
				// own X-Request-Id, bounded and checked before it is believed.
				// This project does not trace, so that is the only identifier
				// there is — turning `tracing:` on is what would give every
				// request one whether or not the caller thought to send it.
				// app.Logger rather than the package default, so a request
				// line, a shutdown step and anything a dependency says all
				// land wherever the server was told to write.
				//
				// This used to be a PreHook logging the method and the path.
				// The generated server writes the line itself now, and writes a
				// better one: after the handler rather than before it, so it
				// carries the status and the size, and labelled by the route
				// that matched rather than by a path with an identifier in it.
				//
				// That line is debug, so this example does not print it: the
				// level belongs to whoever built the logger, and nothing here
				// builds one. The line that says why a 500 happened is an error
				// and comes out regardless, which is the one worth having.
				Logger: app.Logger,
			},
			Todo:           svc,
			TodoAttachment: attachments,
			Notifications:  inbox,
		})

		// A second caller of the same service, on the same mux. The UI renders
		// HTML rather than JSON and that is the only difference: the rules, the
		// hooks and the tenant scoping are the service's, not the transport's.
		//
		// It is what makes the lifecycle features visible — the trash, a row's
		// history, restore and revert are easier to understand as a page than as
		// a curl transcript.
		ui, err := web.New(svc, web.DemoClaims)
		if err != nil {
			return api.Parts{}, err
		}
		ui.Mount(mux)

		// Anything else this server answers is a Handle call on the same mux:
		// static files, a second API, the shape endpoints the electric
		// generator writes.

		// Anything that has to see the response rather than only the request —
		// tracing, a duration, a panic — wraps the handler instead:
		//
		//	return otelhttp.NewHandler(mux, "todo"), nil
		//
		// rig answers /livez and /readyz outside whatever is returned here, so
		// a probe every second is not a traced request.
		//
		// No Shapes, and that is a decision rather than an omission: this table
		// says `electric: enabled`, so the routes exist in internal/electric,
		// but nothing here mounts them and there is no sync service in this
		// example's `rig db up`. api.Main says so once at startup — which is the
		// point of the field being there to leave empty.
		return api.Parts{Handler: mux, Engine: engine}, nil
	})
}

// headerClaims reads the tenant out of a header.
//
// This example has no authentication — that is what `rig setup-project` is
// for — but tenancy is not optional: every generated query is scoped by it, so
// there has to be a tenant before a handler can run.
func headerClaims(r *http.Request) (tenancy.Claims, error) {
	raw := r.Header.Get("X-Tenant-Id")
	if raw == "" {
		return tenancy.Claims{}, rigerr.Unauthorized("X-Tenant-Id is required")
	}

	tenantID, err := uuid.Parse(raw)
	if err != nil {
		return tenancy.Claims{}, rigerr.Unauthorized("X-Tenant-Id is not a valid identifier")
	}

	claims := tenancy.Claims{TenantID: tenantID, Subject: tenancy.SubjectAccount}
	if actor := r.Header.Get("X-Account-Id"); actor != "" {
		accountID, err := uuid.Parse(actor)
		if err != nil {
			return tenancy.Claims{}, rigerr.Unauthorized("X-Account-Id is not a valid identifier")
		}
		claims.AccountID = accountID
	}
	return claims, nil
}

// dispatchNotifications is the inbox's cron half.
//
// It assembles what the server assembles, because the question it answers —
// who should hear about this row — is a method on a service and there is no
// other way to reach one from a task.
func dispatchNotifications(ctx context.Context, pool *pgxpool.Pool) error {
	repos := store.New(pool, store.Config{})

	reg := rignotify.NewRegistry()
	inbox := api.NewNotifications(pool, reg)
	svc := todo.New(repos.Todos, api.NewFiles(pool), nil, inbox, pool, nil)
	reg.Register(api.NewTodoSubject(svc))

	// The generated task rather than its steps written out again: it resolves,
	// dispatches and prunes, and this function used to do only the first of the
	// three. There are no senders here — this example is an inbox and nothing
	// else — so dispatching finds no delivery rows and returns immediately; what
	// is gained is the pruning, which was not happening at all.
	return api.NotificationDispatcher(
		api.NewNotificationEngine(pool, reg, nil), os.Stdout)(ctx, pool)
}

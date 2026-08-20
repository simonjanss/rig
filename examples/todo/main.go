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
	"fmt"
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
	serve.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8080"),

		// Two probes, two questions. Liveness asks whether to restart this
		// process; readiness asks whether to send it work. One check for both
		// means either a wedged process nobody restarts, or the whole fleet
		// restarted because the database was slow.
		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		// The two ends, both stated rather than worked out from the parts.
		// MaxShutdown is the number to copy into
		// terminationGracePeriodSeconds; MaxStartup is the one a startup probe
		// has to outlast. rig checks the parts fit inside each.
		// What to say when the database is not there, which is the first thing
		// to go wrong for somebody who has just cloned this. It is said within a
		// second rather than at the end of the connect timeout.
		Hint: "run `rig db up` to start a local Postgres for this project, " +
			"or point $DATABASE_URL at one you already have",

		MaxStartup:  30 * time.Second,
		MaxShutdown: 20 * time.Second,

		// `todo migrate` applies the schema and exits: a job before the
		// rollout, so one process migrates and the replicas only serve.
		// `todo sweep-files` is the file housekeeping: abandoned uploads, and
		// trash past the restore window. A subcommand rather than a goroutine,
		// so it is a cron job rather than something racing itself in every
		// replica.
		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
			"sweep-files": func(ctx context.Context, pool *pgxpool.Pool) error {
				return api.FileSweeper(api.NewFiles(pool))(ctx, pool)
			},
			// The guarantee behind the inbox, for everything the in-process
			// engine did not get to. It builds its own object graph because it
			// needs the same one the server does: the audience is a method on a
			// service, which is the honest cost of computing it late.
			"dispatch-notifications": dispatchNotifications,
		},

		// The server does not apply them. It refuses to start without them,
		// which is what catches a deploy that got ahead of its migration job.
		// Swap Require for Apply to migrate on the way up instead — fine for a
		// single instance, and rig/migrate's package documentation says what
		// that costs when there is more than one.
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(_ context.Context, app *serve.App) (http.Handler, error) {
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

		repos := store.New(app.Pool, store.Config{})

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
		engine := api.NewNotificationEngine(app.Pool, reg, nil)
		engine.Start()
		app.Drain("notifications", engine.StopClaiming)
		app.CloseWithin("notifications", 15*time.Second, engine.Close)

		mux := api.Register(api.Handlers{
			Server: api.Server{
				GetClaims: headerClaims,
				RequestID: func(r *http.Request) string { return r.Header.Get("X-Request-Id") },
				// app.Logger rather than the package default, so a request
				// line, a shutdown step and anything a dependency says all
				// land wherever the server was told to write.
				PreHooks: []func(http.ResponseWriter, *http.Request) bool{
					func(_ http.ResponseWriter, r *http.Request) bool {
						app.Logger.Info("request", "method", r.Method, "path", r.URL.Path)
						return true // false answers the request here and stops
					},
				},
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
			return nil, err
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
		return mux, nil
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

	report, err := api.NewNotificationEngine(pool, reg, nil).Resolve(ctx)
	fmt.Fprintln(os.Stdout, report)
	return err
}

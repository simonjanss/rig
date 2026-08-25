// Command fantasyfootball runs the API.
//
// The todo example is one table. This one is about what happens once tables
// point at each other: a squad has players, a match has two squads, and a
// filter can ask about any of them from either end.
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

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/fantasyfootball/internal/api"
	"github.com/simonjanss/rig/examples/fantasyfootball/internal/store"
	"github.com/simonjanss/rig/examples/fantasyfootball/services/fixture"
	"github.com/simonjanss/rig/examples/fantasyfootball/services/player"
	"github.com/simonjanss/rig/examples/fantasyfootball/services/team"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/serve"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// migrations travel with the binary, so the schema a build expects is the
// schema that build carries.
//
//go:embed migrations/*.sql
var migrations embed.FS

// localDSN is what `rig db url` prints for this project, so `rig db up` and
// then `go run .` is the whole setup. $DATABASE_URL wins when it is set.
const localDSN = "postgres://rig:rig@localhost:55441/rig?sslmode=disable"

func main() {
	// The log file rig's own monitoring page reads. It is opened before the
	// server, because the logger is built out of it and the lines written while
	// starting up are lines worth having on the page. Nothing is written unless
	// $RIG_LOG_FILE says where — the ordinary case on a laptop — and then this
	// costs one branch per log call and nothing else.
	logs, err := observe.OpenLogs(observe.LogConfig{})
	if err != nil {
		// There is no logger yet: this is the thing that would have been half of
		// one.
		fmt.Fprintln(os.Stderr, "cannot open the log file:", err)
		os.Exit(1)
	}

	// This example is the one that turns `tracing:` on, so it is where the
	// wiring is shown. Nothing is exported unless the environment says where to:
	// $OTEL_EXPORTER_OTLP_ENDPOINT for a collector, $RIG_TRACE_FILE for a file.
	// With neither, the spans cost nothing and the trace ids are still real,
	// which is what the request id in every error body is.
	//
	// Out here rather than in the mount closure below, because the page hanging
	// off it is half of the configuration that closure is passed with.
	tracing, err := observe.Setup(context.Background(), api.Tracing())
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot set tracing up:", err)
		os.Exit(1)
	}

	// And the flush, here as well as in the CloseWithin below, because a
	// provider built in main is reached by both ways out of this process. The
	// `Tasks:` entries below never reach the mount closure — serve.Main runs
	// the task and returns — so without this a `prune-idempotency` run with
	// $RIG_TRACE_FILE set would open a second rotating writer on the span file
	// and drop everything it buffered on the way out.
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
	// log file the sink above holds, which is why that is set on the
	// configuration rather than generated into it.
	//
	// It gets a listener of its own — 127.0.0.1:9081, from rig.yaml — rather
	// than a route beside the API's on 8081. A page that lists every path,
	// request id and error cause this server has seen should be reachable on
	// terms the kernel keeps rather than on a path anybody can ask for. With no
	// $RIG_MONITOR_PASSWORD it opens no port at all and says so once.
	monitoring := api.Monitoring()
	monitoring.Logs = logs
	page, err := tracing.Page(monitoring)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot build the monitoring page:", err)
		os.Exit(1)
	}

	serve.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8081"),

		// The page, on its own listener in this same process. Both are zero
		// when it is unarmed, and then there is no second port.
		Monitor:     page.Handler(),
		MonitorAddr: page.Addr(),

		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		// What to say when the database is not there, which is the first thing
		// to go wrong for somebody who has just cloned this. It is said within a
		// second rather than at the end of the connect timeout.
		Hint: "run `rig db up` to start a local Postgres for this project, " +
			"or point $DATABASE_URL at one you already have",

		MaxStartup:  30 * time.Second,
		MaxShutdown: 20 * time.Second,

		// Stderr, and the file the monitoring page reads. The two keep their own
		// levels: this one stays at whatever the default handler is set to, and
		// the file keeps debug — which is where rig's request line is, so the
		// page has requests to list without this process printing one per
		// request to a terminal nobody is watching.
		Logger: slog.New(observe.Tee(slog.Default().Handler(), logs.Handler())),

		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
			// Records of writes nobody is going to send again. A cron entry, not
			// a goroutine: one thing running rather than one per replica.
			"prune-idempotency": api.IdempotencyPruner(0),
		},
		Migrate: migrate.Require(migrations, migrate.Options{}),

		// A span per statement, from the connection rather than from the
		// generated code: a tracer here sees every query, including the ones a
		// hook or a task runs and the ones no generator wrote.
		Pool: observe.Pool,
	}, func(ctx context.Context, app *serve.App) (http.Handler, error) {
		// Its own limit, because a flush to a collector that is not answering
		// must not spend the whole shutdown budget. The provider it stops was
		// built in main; this is the first place there is an App to register a
		// closer with, and it is the server's half of the pair — the defer in
		// main is the half a task run reaches.
		app.CloseWithin("traces", 5*time.Second, tracing.Shutdown)

		// Said here rather than in main because this is where there is a logger
		// writing to the file the page would have read.
		if why := page.Unarmed(); why != "" {
			app.Logger.Info("monitoring page not listening", "reason", why)
		}

		repos := store.New(app.Pool, store.Config{Tracer: observe.Tracer()})

		// One field per resource is the whole registration surface: adding a
		// table and forgetting to wire it up does not compile.
		return api.Register(api.Handlers{
			Server: api.Server{
				GetClaims: headerClaims,
				// Where a write that carried an Idempotency-Key is recorded,
				// so a client that had to send one twice gets one row.
				DB: app.Pool,
				// The trace this request belongs to, so the identifier in an
				// error body, the request_id on every log line and the span in
				// a collector are one string. A caller that sent its own
				// X-Request-Id is honoured first.
				RequestID: func(r *http.Request) string {
					return cmp.Or(r.Header.Get("X-Request-Id"), observe.TraceID(r))
				},
				// So the cause of a 500 lands wherever the server writes, and
				// carries the identifier the client was handed.
				Logger: app.Logger,
			},
			Team:    team.New(repos.Teams),
			Player:  player.New(repos.Players),
			Fixture: fixture.New(repos.Fixtures),
		}), nil
	})
}

// headerClaims reads the tenant out of a header.
//
// This example has no authentication — that is what `rig setup-project` is
// for — but tenancy is not optional: every generated query is scoped by it,
// including the subquery a filter across a relation renders, so there has to be
// a tenant before a handler can run.
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

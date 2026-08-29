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
	// api.Main is the process this project's rig.yaml describes: the log file
	// the monitoring page reads, the provider writing the spans it reads beside
	// them, the page over both, and the flush on every way out — including the
	// ones that end in os.Exit, where a deferred call would not run at all.
	//
	// Nothing is written and nothing exported unless the environment says where:
	// $RIG_LOG_FILE, $RIG_TRACE_FILE, $OTEL_EXPORTER_OTLP_ENDPOINT — none of
	// which is the ordinary case on a laptop, and then this costs one branch per
	// log call, the spans cost nothing, and the trace ids are still real.
	//
	api.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8081"),

		// What to say when the database is not there, which is the first thing
		// to go wrong for somebody who has just cloned this. It is said within a
		// second rather than at the end of the connect timeout.
		Hint: "run `rig db up` to start a local Postgres for this project, " +
			"or point $DATABASE_URL at one you already have",

		MaxStartup: 30 * time.Second,
		// Fifteen seconds, which is api.ShutdownBudget for this project: five
		// for the trace flush, the one closer rig registers here, and ten left
		// for the requests still in flight. This example registers no closer of
		// its own, so there is no arithmetic on top of it — and it is still
		// written out, because it is the one number in this struct that leaves
		// the program. Whoever writes terminationGracePeriodSeconds reads it
		// here rather than out of a function they would have to run the binary
		// to evaluate.
		MaxShutdown: 15 * time.Second,

		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
			// prune-idempotency is merged in by api.Main: records of writes
			// nobody is going to send again, as a cron entry rather than a
			// goroutine — one thing running rather than one per replica.
		},
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(ctx context.Context, app *serve.App, _ *observe.Page) (api.Parts, error) {
		// No Tracer: the generated store.New settles a nil one to
		// observe.Tracer(), which is the value the provider above installed.
		repos := store.New(app.Pool, store.Config{})

		// One field per resource is the whole registration surface: adding a
		// table and forgetting to wire it up does not compile. api.Parts is the
		// same bargain for what outlives a request, and for this project it has
		// one field: there is nothing here to start, drain or close but the
		// trace flush api.Main already registered.
		return api.Parts{Handler: api.Register(api.Handlers{
			Server: api.Server{
				GetClaims: headerClaims,
				// Where a write that carried an Idempotency-Key is recorded,
				// so a client that had to send one twice gets one row.
				DB: app.Pool,
				// No RequestID: the generated default is already the trace this
				// request belongs to, with a caller's own X-Request-Id honoured
				// first — so the identifier in an error body, the request_id on
				// every log line and the span in a collector are one string.
				//
				// Logger, so the cause of a 500 lands wherever the server writes
				// and carries the identifier the client was handed.
				Logger: app.Logger,
			},
			Team:    team.New(repos.Teams),
			Player:  player.New(repos.Players),
			Fixture: fixture.New(repos.Fixtures),
		})}, nil
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

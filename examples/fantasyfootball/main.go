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
	serve.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8081"),

		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		// What to say when the database is not there, which is the first thing
		// to go wrong for somebody who has just cloned this. It is said within a
		// second rather than at the end of the connect timeout.
		Hint: "run `rig db up` to start a local Postgres for this project, " +
			"or point $DATABASE_URL at one you already have",

		MaxStartup:  30 * time.Second,
		MaxShutdown: 20 * time.Second,

		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
		},
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(_ context.Context, app *serve.App) (http.Handler, error) {
		repos := store.New(app.Pool, store.Config{})

		// One field per resource is the whole registration surface: adding a
		// table and forgetting to wire it up does not compile.
		return api.Register(api.Handlers{
			Server: api.Server{
				GetClaims: headerClaims,
				RequestID: func(r *http.Request) string { return r.Header.Get("X-Request-Id") },
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

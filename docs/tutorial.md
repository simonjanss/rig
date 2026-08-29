# Tutorial

Build a working, multi-tenant JSON API from an empty directory. About twenty
minutes. Every command and every response below is from a real run.

You will end up with the same thing as [examples/todo](../examples/todo), so
that is the place to look when you want to go further.

## What you need

- Go 1.26 or later
- Docker or Podman running — rig starts a throwaway Postgres for you
- `psql` if you want to poke at the database directly (optional)

## Install rig

rig is in early development and the repository is private, so there is no `go
install` from a proxy yet. Clone it and build:

```bash
git clone https://github.com/simonjanss/rig
cd rig
make build          # writes ./bin/rig
```

Put `bin/` on your `PATH`, or use the full path below.

Your application will also need to depend on `rig/runtime`. Until the repository
is public that means either a `replace` directive pointing at your clone, or
`GOPRIVATE=github.com/simonjanss/rig` with git credentials configured. This
tutorial uses `replace`, and says where.

---

## 1. Start a project

```bash
rig init todo --module example.com/todo
cd todo
```

```
created rig.yaml
created .gitignore
created AGENTS.md
created migrations/.keep
wrote .rig/table.schema.json
wrote .rig/rig.schema.json
```

Four files and a schema directory.

- **`rig.yaml`** is the project file. It marks the root — every rig command
  works from anywhere below it.
- **`.gitignore`** ignores `*.gen.go`. rig expects your build to regenerate;
  committing generated code instead is a legitimate choice, see
  [design.md](design.md).
- **`AGENTS.md`** explains the layout to whoever joins next, human or otherwise.
- **`.rig/*.schema.json`** are what your editor reads for completion in the YAML
  files. They are the same schemas `rig validate` uses.

One edit before you go on. `rig init` gives the throwaway database port 55432,
and two rig projects on one machine will fight over it. Give this one its own:

```yaml
# rig.yaml
database:
  port: 55450
```

While you are in there, turn off permission checks. rig defaults to `derived` —
every endpoint gets a permission and an authenticated caller holding no grants
reaches nothing — which is right for an application with accounts and noise for
this one:

```yaml
api:
  version: v1
  # No authorization in this example, so no permission checks are generated.
  # The default is `derived`. See docs/auth.md for the other half.
  permissions: none
  base_path: /api/v1
```

## 2. Write a migration

rig has no schema language. Your migrations *are* the schema.

```bash
rig migration new create_todo --table todo --soft-delete
```

```
created migrations/00001_create_todo.sql

Edit it, then run `rig sync` to pick todo up.
```

`--table` scaffolds a `CREATE TABLE` with the columns rig recognizes by name
already in place, and `--soft-delete` adds `deleted_at`. Open it and fill in the
middle — there is a marker showing where your columns go.

```sql
-- +goose Up
-- +goose StatementBegin

CREATE TYPE todo_priority AS ENUM ('low', 'normal', 'high');

CREATE TABLE todo (
    id                      uuid PRIMARY KEY,
    tenant_id               uuid NOT NULL,

    title                   text NOT NULL,
    notes                   text,
    is_done                 boolean NOT NULL DEFAULT false,
    priority                todo_priority NOT NULL DEFAULT 'normal',
    due_at                  timestamptz,

    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by_account_id   uuid,
    updated_at              timestamptz,
    updated_by_account_id   uuid,
    deleted_at              timestamptz,
    deleted_by_account_id   uuid
);

COMMENT ON TABLE todo IS 'One thing somebody means to do.';
COMMENT ON COLUMN todo.title IS 'What needs doing, in a few words.';
COMMENT ON COLUMN todo.notes IS 'Anything worth remembering that does not fit in the title.';
COMMENT ON COLUMN todo.is_done IS 'Whether the task has been completed.';
COMMENT ON COLUMN todo.priority IS 'How urgently the task wants attention.';
COMMENT ON COLUMN todo.due_at IS 'When the task is due, or null if it never expires.';

CREATE INDEX todo_tenant_created_idx ON todo (tenant_id, created_at DESC);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE todo;
DROP TYPE todo_priority;

-- +goose StatementEnd
```

Three things here are doing more work than they look like they are.

**`is_done`, not `done`.** rig wants a boolean to read as the question it
answers, so it accepts `is_`, `has_`, `can_`, `should_`, `was_`, `allow_`.

**The index leads with `tenant_id`.** Every generated query filters on the
tenant, so without this index every read is a full table scan. It is not an
optimization.

**The `COMMENT ON` lines.** These become Go doc comments and API descriptions.
`rig sync` picks them up in a moment, and a column with no comment fails
validation by default — which is what keeps generated documentation from being a
list of field names.

## 3. Start the database

```bash
rig db up
```

```
creating container todo-db (postgres:17-alpine) on port 55450
applied 1 migration(s)
database ready at postgres://rig:rig@127.0.0.1:55450/rig?sslmode=disable&TimeZone=UTC
```

Nothing in this container is precious. `rig db reset` throws it away and rebuilds
it from the migrations, which is what you do after editing a migration that has
already been applied.

## 4. Sync

```bash
rig sync
```

```
create  services/todo/todo.yaml (5 columns)

wrote 1 file(s). Replace the TODO comments, then run `rig validate`.
```

rig applied your migrations to a real Postgres, introspected the result, and
wrote a configuration file for the table it found:

```yaml
# services/todo/todo.yaml
# yaml-language-server: $schema=../../.rig/table.schema.json
table: todo
comment: One thing somebody means to do.

# Days a soft-deleted row stays restorable.
restore_window_days: 30

columns:
  title:
    comment: What needs doing, in a few words.
  notes:
    comment: Anything worth remembering that does not fit in the title.
  is_done:
    comment: Whether the task has been completed.
  priority:
    comment: How urgently the task wants attention.
  due_at:
    comment: When the task is due, or null if it never expires.

enums:
  todo_priority:
    name: TodoPriority
    description: 'TODO: what does this enumeration represent?'
    values:
      low:
        name: Low
        description: 'TODO: describe this.'
      ...
```

Your `COMMENT ON` text came across. The enum descriptions could not — Postgres
had nothing to say about them — so they arrived as TODOs.

Notice what is *not* in the file: `id`, `tenant_id`, and the six audit columns.
Those are managed by rig, never settable by a client, and so have nothing to
configure.

Fill in the enum, and add a default ordering while you are here:

```yaml
# Newest first, then by identifier so the order is total and paging is stable.
order_by: [-created_at, id]

enums:
  todo_priority:
    name: TodoPriority
    description: How urgently a task wants attention.
    values:
      low:
        name: Low
        description: Worth doing eventually.
      normal:
        name: Normal
        description: Worth doing soon. This is the default.
      high:
        name: High
        description: Worth doing before anything else.
```

The second sort term matters more than it looks. Without something unique at the
end the order is not total, and two pages of the same list can show you a row
twice or skip it.

## 5. Validate

```bash
rig validate
```

```
todo: 1 tables, 1 resources, no problems found
```

Try breaking it, because the failure mode is the useful part. Add a column to
your migration with no comment, `rig db reset && rig sync`, and:

```
/tmp/todo/services/todo/todo.yaml
  20:5: error[RIG6002]: column todo.estimate_minutes has no comment
    Describe the column in its `comment:` key.

1 error
```

Every diagnostic is anchored to the exact line that caused it and carries a code
you can look up: `rig codes RIG6002`. In CI, `rig validate --strict` also fails
on warnings — a warning nobody ever fails on is a warning nobody ever fixes.

## 6. Generate

```bash
rig generate
```

```
add       internal/api/api.gen.go
add       internal/api/server.gen.go
add       internal/api/todo.gen.go
add       internal/api/todo_routes.gen.go
add       internal/api/todo_service.gen.go
add       internal/model/model.gen.go
add       internal/model/todo.gen.go
add       internal/model/todo_input.gen.go
add       internal/model/todo_priority.gen.go
add       internal/model/todo_query.gen.go
add       internal/store/store.gen.go
add       internal/store/todo_repository.gen.go
add       services/todo/todo.go
13 add
```

Twelve of those have `.gen.` in the name. They are rewritten from scratch on
every run — a fix to something in one of them belongs in your schema or your
configuration.

The thirteenth, `services/todo/todo.go`, is different. rig wrote it once, and
will never touch it again. That is your service layer. Run `rig generate` a
second time and it reports `keep services/todo/todo.go`.

You now have, for one table:

| | |
|---|---|
| `internal/model/` | the `Todo` entity, the `TodoPriority` enum, the filter type, and the create and update inputs with their validation |
| `internal/store/` | `TodoRepository` and its pgx implementation |
| `internal/api/` | wire types, the service interface, routing, handlers |
| `services/todo/` | yours |

## 7. Wire it up

Two files you write by hand. First `go.mod` — this is the `replace` mentioned at
the top; point it at your rig clone:

```
module example.com/todo

go 1.26

require (
	github.com/google/uuid v1.6.0
	github.com/simonjanss/rig/migrate v0.0.0
	github.com/simonjanss/rig/runtime v0.0.0
)

replace github.com/simonjanss/rig/runtime => /path/to/rig/runtime
replace github.com/simonjanss/rig/migrate => /path/to/rig/migrate
```

Then `main.go`:

```go
// Command todo runs the API.
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

	"example.com/todo/internal/api"
	"example.com/todo/internal/store"
	"example.com/todo/services/todo"
	"github.com/simonjanss/rig/migrate"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/serve"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The migrations travel with the binary, so the schema a build expects is the
// schema that build carries.
//
//go:embed migrations/*.sql
var migrations embed.FS

// What `rig db url` prints for this project.
const localDSN = "postgres://rig:rig@localhost:55450/rig?sslmode=disable"

func main() {
	// api.Main is the whole of a main function: this project's configuration,
	// and the one function only this application can write. The housekeeping
	// subcommands come out of rig.yaml, so those are not lines here.
	//
	// Everything serve will not choose for itself is. It has no defaults: a
	// config that leaves a timeout or a probe path empty is refused before
	// anything listens, naming all of them at once, because a value nobody
	// chose is one you find out about from what it costs.
	api.Main(serve.Config{
		DatabaseURL: cmp.Or(os.Getenv("DATABASE_URL"), localDSN),
		Addr:        cmp.Or(os.Getenv("ADDR"), "127.0.0.1:8080"),
		Hint:        "run `rig db up` to start a local Postgres for this project",
		Logger:      slog.Default(),

		// Liveness asks whether to restart this process and consults nothing;
		// readiness asks whether to send it work, pings the database, and turns
		// false the moment a shutdown begins. serve.NoProbe declines either.
		LivenessPath:  "/livez",
		ReadinessPath: "/readyz",

		MaxStartup:        30 * time.Second,
		ConnectTimeout:    10 * time.Second,
		ProbeTimeout:      2 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       2 * time.Minute,

		// The shutdown budget is a line here, and it is the one number rig
		// knows and settles anyway not at all: it is what goes into
		// terminationGracePeriodSeconds, so it has to be readable off this
		// struct. api.ShutdownBudget() is ten seconds for a project with no
		// blocks yet — all of it for the requests in flight — and it grows as
		// you turn blocks on. Leave it out and the server refuses to start,
		// printing the number to write.
		MaxShutdown: 10 * time.Second,

		// `todo migrate` applies the schema and exits: a job before the
		// rollout, so one process migrates and the replicas only serve.
		//
		// api.Main merges in the housekeeping this project's blocks already
		// decided — `todo prune-idempotency` here, deleting the records of
		// writes nobody will send again — and yours wins on a shared name.
		Tasks: map[string]serve.Task{
			"migrate": migrate.Apply(migrations, migrate.Options{Log: os.Stdout}),
		},
		// The server refuses to start when the database is behind, which is
		// what catches a deploy that got ahead of its migration job.
		Migrate: migrate.Require(migrations, migrate.Options{}),
	}, func(_ context.Context, app *serve.App) (api.Parts, error) {
		repos := store.New(app.Pool, store.Config{})
		svc := todo.New(repos.Todos)

		// api.Parts is one field per thing whose lifetime is longer than a
		// request's. This project has none yet, so it is the handler and
		// nothing else — turning on `notifications:` or a table's `electric:`
		// is what makes a second field appear.
		return api.Parts{Handler: api.Register(api.Handlers{
			// DB is where a write carrying an Idempotency-Key is recorded, so a
			// client that had to send one twice gets one row and one answer.
			Server: api.Server{GetClaims: headerClaims, DB: app.Pool},
			Todo:   svc,
		})}, nil
	})
}

// headerClaims reads the tenant out of a header.
//
// This application has no authentication — that is what `rig setup-project` is
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
	return tenancy.Claims{TenantID: tenantID, Subject: tenancy.SubjectAccount}, nil
}
```

The registration struct is the whole wiring surface — one field per resource, so
adding a table and forgetting to wire it up does not compile.

```bash
go mod tidy
go run .
```

```
INFO migrating
INFO listening addr=127.0.0.1:8080 "started in"=39.856125ms
```

## 8. Use it

```bash
export T=11111111-1111-1111-1111-111111111111
```

**Create.**

```bash
curl -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"title":"Write the tutorial","priority":"high"}' \
  http://127.0.0.1:8080/api/v1/todos
```

```json
{
  "id": "01a01977-ac6f-7bf6-81b9-2f8e6fc29ffa",
  "tenantId": "11111111-1111-1111-1111-111111111111",
  "title": "Write the tutorial",
  "isDone": false,
  "priority": "high",
  "createdAt": "2026-08-19T10:01:08.463784Z"
}
```

**List**, which is paginated whether you ask or not:

```bash
curl -H "X-Tenant-Id: $T" http://127.0.0.1:8080/api/v1/todos
```

```json
{
  "data": [ ... ],
  "pagination": {"offset": 0, "limit": 50, "total": 1}
}
```

50 by default, 500 at most. There is no way to ask for the whole table, on
purpose — see [design.md](design.md).

**Update**, with PATCH semantics: a field you leave out is left alone, `null`
clears it.

```bash
curl -X PATCH -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"isDone":true}' \
  http://127.0.0.1:8080/api/v1/todos/01a01977-ac6f-7bf6-81b9-2f8e6fc29ffa
```

**Filter** on the collection:

```bash
curl -H "X-Tenant-Id: $T" \
  "http://127.0.0.1:8080/api/v1/todos?isDone=true&limit=10"
```

**Search**, for anything more structured than that:

```bash
curl -X POST -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"filter":{"equals":{"priority":"low"}}}' \
  http://127.0.0.1:8080/api/v1/todos/_search
```

`QUERY /api/v1/todos` is the same operation and the correct method for a read
with a body; the `POST /_search` alias exists because some intermediaries still
reject methods they do not recognize.

**Delete** — and because the table has `deleted_at`, the row is retired rather
than removed:

```bash
curl -X DELETE -H "X-Tenant-Id: $T" \
  http://127.0.0.1:8080/api/v1/todos/01a01977-ac6f-7bf6-81b9-2f8e6fc29ffa
# 204

curl -H "X-Tenant-Id: $T" http://127.0.0.1:8080/api/v1/todos/_deleted
```

```json
{"data":[{ ..., "deletedAt":"2026-08-19T10:01:15.453881Z"}], "pagination":{...}}
```

`POST /api/v1/todos/{id}/_restore` brings it back, for the 30 days
`restore_window_days` allows.

### Two failures worth seeing

No tenant is a 401, because there is no such thing as an unscoped read:

```bash
curl -i http://127.0.0.1:8080/api/v1/todos
# HTTP/1.1 401 Unauthorized
```

And validation answers with the shape of the request that failed:

```bash
curl -H "X-Tenant-Id: $T" -H content-type:application/json \
  -d '{"priority":"urgent"}' http://127.0.0.1:8080/api/v1/todos
```

```json
{
  "code": "UnprocessableEntity",
  "message": "todo is not valid: title CannotBeEmpty: cannot be empty; priority InvalidValue: \"urgent\" is not one of the allowed values",
  "fields": {
    "title":    {"code": "CannotBeEmpty", "message": "cannot be empty"},
    "priority": {"code": "InvalidValue",  "message": "\"urgent\" is not one of the allowed values"}
  }
}
```

`fields` is shaped like the body that failed — one member per field, holding the
problem with that field — so a form can put each message beside the control it
belongs to without parsing prose.

## 9. The endpoints you got

From one table and twenty lines of YAML:

```
GET     /api/v1/todos                    list
POST    /api/v1/todos                    create
QUERY   /api/v1/todos                    search
POST    /api/v1/todos/_search            search (alias)
GET     /api/v1/todos/_deleted           the trash
GET     /api/v1/todos/{id}               get
PATCH   /api/v1/todos/{id}               update
DELETE  /api/v1/todos/{id}               soft delete
POST    /api/v1/todos/{id}/_restore      restore
```

## The loop from here

```bash
rig migration new add_something   # change the schema
rig db reset                      # rebuild from the migrations
rig sync                          # pick up what changed
rig validate                      # check it
rig generate                      # write the code
```

That is the whole cycle, and it takes a few seconds.

## Where to go next

- **[concepts.md](concepts.md)** — the model behind what you just did
- **[services.md](services.md)** — writing business rules into `services/todo/todo.go`
- **[tables.md](tables.md)** — custom endpoints, hiding columns, embedding relations
- **[auth.md](auth.md)** — `rig setup-project`, and replacing `headerClaims` with real sign-in
- **[examples/todo](../examples/todo)** — the finished version of this, with a
  service layer, lifecycle hooks, an HTML UI, and tests

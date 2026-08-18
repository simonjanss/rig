# examples/sdk — using a generated Go client

This is not a rig project. It has no `rig.yaml`, no schema and no server: it is a
program that *calls* two of the other examples, using the Go clients rig
generated for them.

It exists because a client is the one generated thing whose point is that
somebody else imports it. The servers in `examples/todo` and `examples/auth`
keep their model, store and API packages under `internal/`, which is right — those
are the project's own business. The client goes to `./client` instead, so this
module can import it:

```go
import "github.com/simonjanss/rig/examples/todo/client"
```

## Running it

Each demonstration needs its example's server up. From the repository root:

```sh
# The todo demonstration, and the import job
cd examples/todo && rig db up && go run .        # leave this running
cd examples/sdk  && go run . todo
cd examples/sdk  && go run . import

# The authentication demonstration
cd examples/auth && rig db up && go run .        # leave this running
cd examples/sdk  && go run . auth
```

All three take `-base-url` if the server is somewhere else. `todo` and `import`
also take `-tenant`, because that example has no authentication and reads the
tenant from a header; `auth` takes `-email` and `-password`, defaulting to the
account the example seeds.

`go run . import -dry-run` needs no server at all.

## What each one shows

**`todo`** — the whole life of a resource without a credential in sight: create,
a validation failure decoded into the shape of the input that failed, a PATCH
that changes one field and leaves the rest alone, an explicit null that clears
one, a typed search, the iterator that walks every page, a custom endpoint, a
conflict recognized without reading the message, history, delete and restore.

**`auth`** — what changes when the API has sessions. One `SignIn` call and every
request afterwards carries the credential; the client renews the access token
before it expires, using the lifetimes it was generated with rather than numbers
somebody guessed. Then the tenant list, an API key minted and used by a second
client, and the key refused the moment it is revoked.

**`import`** — the batch job, which is what a client library actually gets used
for. It reads `testdata/todos.csv` and creates a todo per row. None of what makes
that hard is about HTTP; it is the four decisions a job over somebody else's data
has to make, and a `for` loop around a create call makes none of them:

| | |
|---|---|
| What a bad row costs | One unparseable date must not stop the other four thousand, and the report names the line to fix |
| Which failures to retry | A 429 or a 503 is the server asking for a moment; a 422 will fail identically forever |
| What a second run does | Somebody will run it twice |
| How hard to push | Unbounded goroutines against an API is a denial of service you wrote yourself |

Two of the four are the generated client doing the work. A 422 arrives already
shaped like the row that caused it, so the report can say `title: cannot be
empty` rather than quoting a sentence; and a rate limit arrives with the interval
the server asked for, so the backoff is the server's number rather than a guess.

The file has two deliberately bad rows, and they fail in different places — one
caught locally by the generated enum, one refused by the server:

```
  line 2   created  Write the tutorial
  ...
  line 6   failed   (no title)          title: cannot be empty
  line 7   failed   Buy milk            "urgent" is not a priority; use one of low, normal, high

5 created, 0 skipped, 0 parsed, 2 failed
```

Run it again and those five are `skipped` rather than duplicated. That check is a
check-then-act and it races — the honest answer to two copies of the job starting
at once is a unique index, which is a schema decision and belongs in the
migration. What it buys is the ordinary case: somebody re-running the import
after fixing three rows.

## The parts worth stealing

Three things in here are the whole argument for a generated client, and they are
each one line at the call site:

```go
// A field left out is left alone; a field set to null is cleared. Nothing else
// in Go says both.
c.Todos.Update(ctx, id, client.TodoUpdateInput{Title: patch.NewOptional("new")})

// A failure says which failure it was, and a 422 says it per field.
fields, ok := rigclient.FieldsAs[client.TodoCreateFields](err)

// A parameter left nil is not sent, so the server's own default applies.
c.Todos.List(ctx, client.TodoListQuery{Limit: rigclient.P(10)})
```

`demo_test.go` pins all three down against a stub server, with no database, so
they fail in `go test ./...` rather than only where Docker is available, and
`import_test.go` does the same for the four decisions above. The end-to-end tests
against a real generated server live beside the servers, in
`examples/todo/client_docker_test.go` and `examples/auth/client_docker_test.go`.

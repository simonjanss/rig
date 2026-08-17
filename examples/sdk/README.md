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
# The todo demonstration
cd examples/todo && rig db up && go run .        # leave this running
cd examples/sdk  && go run . todo

# The authentication demonstration
cd examples/auth && rig db up && go run .        # leave this running
cd examples/sdk  && go run . auth
```

Both take `-base-url` if the server is somewhere else. `go run . todo` also takes
`-tenant`, because that example has no authentication and reads the tenant from
a header; `go run . auth` takes `-email` and `-password`, defaulting to the
account the example seeds.

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
they fail in `go test ./...` rather than only where Docker is available. The
end-to-end tests against a real generated server live beside the servers, in
`examples/todo/client_docker_test.go` and `examples/auth/client_docker_test.go`.

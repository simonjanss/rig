# Observability

What your server says about the requests it served, and how to find the one that
went wrong.

Today that means logging, and this page is all of it. Tracing is not built —
[what rig does not do here](#what-rig-does-not-do-here) says where that stands.

## The short version

Give the server a logger. That is the whole setup:

```go
api.Register(api.Handlers{
    Server: api.Server{
        GetClaims: yourClaimsFunc,
        RequestID: func(r *http.Request) string { return r.Header.Get("X-Request-Id") },
        Logger:    app.Logger,
    },
    Todo: todo.New(repos.Todos),
})
```

`Logger` is optional and nil is not silence — it means `slog.Default()`. There is
no switch that turns logging off, because there is one line you want whether or
not you ever thought about logging, and it is the next section.

## Why a 500 happened

When a handler fails, the client is told as little as possible:

```json
{ "code": "Internal", "message": "something went wrong", "requestId": "req-42" }
```

That is deliberate. The real error is the kind of thing that names a table, a
column, or a connection string, and none of that belongs in a response. So the
detail goes to the log instead, at `ERROR`, in full:

```json
{"level":"ERROR","msg":"request failed",
 "request":{"request_id":"req-42","method":"GET","route":"GET /api/v1/todos",
            "path":"/api/v1/todos","remote_addr":"10.1.0.4:52122","user_agent":"curl/8.4.0"},
 "status":500,"code":"Internal",
 "error":"Internal: listing todos: dial tcp 10.0.0.7:5432: connection refused"}
```

Everything about the request is one `request` group; everything beside it is
about the answer.

The `requestId` in the body and the `request_id` in the log are the same string.
That pair is the whole mechanism: somebody sends you the identifier from their
screenshot, and you have the cause.

Which is why `RequestID` is worth setting. Without it the line still says what
failed and on which route — you just cannot tie it to a particular complaint.

**Anything that is not a 500 is `DEBUG`, not `ERROR`.** A 404, a 422, a refused
permission: the server did its job. A log that reports those as errors is a log
people learn to skim.

## The request line

At `DEBUG`, one line per request, after the handler has finished:

```json
{"level":"DEBUG","msg":"request served",
 "request":{"request_id":"req-42","method":"GET","route":"GET /api/v1/todos", "…":"…"},
 "status":200,"bytes":1284}
```

`route` is the pattern that matched, not the path that was requested — you get
`GET /api/v1/todos/{id}` rather than one distinct value per identifier anybody
has ever fetched. That is what makes it usable as a label.

It is debug because it is one line per request forever. Turn it on when you are
looking at something:

```go
Logger: slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
    Level: slog.LevelDebug,
})),
```

The liveness and readiness probes are not logged at any level. A check every
second is not a request anybody wants a line about.

## Correlating with a trace

`RequestID` is a function you supply, so it can return whatever identifies a
request in the rest of your system. If you already run OpenTelemetry, return the
trace id:

```go
RequestID: func(r *http.Request) string {
    return trace.SpanContextFromContext(r.Context()).TraceID().String()
},
```

Now the `requestId` in the error body, the `request_id` in every log line, and
the trace in your collector are all the same string. rig has no otel dependency
and does not need one for this — the import is yours, and the only thing rig has
to do is not invent an identifier of its own.

## Logging from a service

There is no logger on the context, and no `api.Logger`. A service is constructed
by your `main`, so hand it a logger there — the same one you gave the server:

```go
svc := todo.New(repos.Todos, app.Logger)
```

To put the request on your own lines, take it off the context and log it as one
attribute, which is what the server does:

```go
func (s *Service) Create(ctx context.Context, r api.Request[...]) (*model.Todo, error) {
    rc, _ := api.RequestContextFrom(ctx)
    s.logger.InfoContext(ctx, "importing",
        slog.Any("request", rc),
        slog.Int("rows", len(r.Body.Items)))
    ...
}
```

`RequestContext` implements `slog.LogValuer`, so `slog.Any("request", rc)` is a
group with the fields that have something in them and no keys for the ones that
do not. `ok` is false when there is no request — a migration, a background task —
and the zero value logs as an empty group rather than a row of empty strings.

**Pass the context.** `InfoContext`, not `Info`. A `slog.Handler` is given the
context, which is how anything that decorates records finds what the line belongs
to. A call that drops the context drops that with it, and rig's own linter
(`sloglint`, `context: all`) refuses one.

## Authentication routes

The `auth:` block's routes are mounted on the same mux and answer in the same
shape, and they log the same way. They take the logger separately, because the
configuration is built before the server it gets attached to:

```go
front, err := api.New(pool, api.Hooks{
    Grants: authz.Grants(pool),
    Logger: app.Logger,
})
```

Give it the same logger as `Server.Logger`. Nothing enforces that they match, and
a 500 from signing in going somewhere else is exactly the kind of thing you find
out at the wrong moment.

## What rig does not do here

- **No metrics, and rig emits no spans.** Spans are a separate decision with a
  separate cost — an OpenTelemetry dependency in a project that may not want one
  — and rig does not generate them yet. Tracing your own server works today, and
  the seams say so in their own doc comments: `serve.Config.Pool` takes a
  `*pgxpool.Config` and is where a tracer goes, `Mount` returns an `http.Handler`
  you can wrap in `otelhttp`, and `rigclient.Config.HTTPClient` takes an
  `*http.Client` and so a `RoundTripper`. Point `RequestID` at the trace id, as
  above, and the logs join up with all of it.
- **No log shipping.** `slog` writes where you point it. A collector, a file, a
  rotation policy and a retention period are the deployment's, not rig's.
- **No sampling and no rate limit on the lines.** A server under a flood of 500s
  writes one line per failure. If that is a problem, it is a problem about the
  500s.
- **rig does not redact your fields.** The error line carries the whole wrapped
  error. If a service puts something secret in an error message, it will be in
  the log — that is true of any logger, and it is the reason the *client* is told
  nothing rather than the reason the log is.

## See also

- [services.md](services.md#running-it) — where `Server` is wired up
- [api.md](api.md) — the error body, and every code that can appear in it
- [auth.md](auth.md) — the `auth:` block and what `api.Hooks` is for

# Observability

What your server says about the requests it served, and how to find the one that
went wrong.

Four things, at four prices:

| | Costs | Turned on by |
|---|---|---|
| **The log** | nothing | nothing — it is always on |
| **Spans** | an OpenTelemetry dependency | `tracing: {enabled: true}` in rig.yaml |
| **Exporting them** | a collector, or a file | an environment variable, at run time |
| **Keeping the log lines** | a bounded file | a sink you tee into your logger, at run time |
| **The monitoring page** | a port of its own, and a password to guard it | `monitoring: {enabled: true, addr: …}`, over a span file |

They are separate because they are separately worth it. The line that says why a
500 happened is worth having in every project, including the one you started
this morning. Spans are worth having in a project big enough that "which part
was slow" is a question — and that project may still spend most of its life on a
laptop with nothing to export to.

## The short version

Give the server a logger. That is the whole setup:

```go
api.Register(api.Handlers{
    Server: api.Server{
        GetClaims: yourClaimsFunc,
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

## Spans

One line in `rig.yaml`:

```yaml
tracing:
  enabled: true
```

and the generated code starts opening spans: one per request, one per repository
call, one per hook, and one per statement. Off — the default — the generated
code names no tracing library at all, and OpenTelemetry is not in your go.mod.
Optional here means absent, not a switch that is false.

There is nothing else in the block. The service name is your `project.name`,
because you already said it once. Where the spans **go** is not generated at
all: the same binary runs on a laptop, in CI and in production, and only the
last of those has a collector.

### Wiring it up

No lines in your `main`. Turning the block on is what makes `api.Main` build a
process at all:

```go
api.Main(serve.Config{
    ...
}, func(ctx context.Context, app *serve.App) (api.Parts, error) {
    repos := store.New(app.Pool, store.Config{})
    ...
```

Turning `monitoring:` on as well adds a third argument, `page *observe.Page`,
which is [the page](#the-monitoring-page) for an application with somewhere to
link to it from.

`api.NewProcess`, `Process.Configure` and `Process.Attach` are all still
exported — `api.Main` is the three of them in the order they have to happen in,
and a project keeping the sequence itself calls them directly. What follows is
what each does, because it is what `api.Main` is doing on your behalf.

`NewProcess` installs the provider from `api.Tracing()`, which carries the
service name out of `rig.yaml`. `Configure` sets `serve.Config.Pool` to
`observe.Pool` — a span per statement, from the connection, so it sees every
query including the ones your hooks and tasks run — and `Attach` registers the
flush with a limit of its own, because a flush to a collector that is not
answering must not spend the whole shutdown budget. `Configure` also sets
`serve.Config.OnExit` to `process.Close`, which is the same flush for every other
way out: a `Tasks:` entry never reaches the mount closure, and the three paths
that end in `os.Exit` reach no `defer` at all. `Provider.Shutdown` is idempotent,
so the server path running both halves costs nothing. Five seconds, and
`serve.Config.Shutdown: api.Shutdown{Traces: ...}` is how a deployment asks for
another — `Configure` reads it, so the cron half is sized with it too. See
[services.md](services.md).

`store.Config` needs no `Tracer`: the generated `store.New` settles a nil one to
`observe.Tracer()`, which is the value the provider installed. A task that never
called `observe.Setup` gets a no-op there and runs untraced rather than
differently.

`examples/fantasyfootball` turns it on over a schema with nothing else going on,
which is the smallest version of the wiring above. `examples/linearlite` turns
it on with authentication, uploads, the notification engine and the Electric
proxy all running, which is the arrangement where a trace tells you something
you did not already know — a request that signs in, a request that uploads and
a shape subscription do not cost the same thing.

### Where the spans go

Nowhere, until you say. Set one of:

```
OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318   # a collector
RIG_TRACE_FILE=/var/log/myapp/spans.jsonl           # a file
```

or the `Endpoint` and `File` fields of `observe.Config` if a variable is the
wrong place for it.

**With neither, nothing is recorded and nothing is exported** — and the trace
and span ids still exist. That is deliberate: it is what makes the request id in
your logs a real trace id even on a machine that has never heard of a collector,
and it is why `go test` and `go run .` cost nothing. An exporter pointed at a
host that is not there retries, and the retry comes due during shutdown.

### The file

The file is one JSON object per line — a finished span each — which is grep, and
which is what a deployment too small for a collector can still read:

```json
{"time":"2026-08-21T09:14:02.113Z","trace_id":"4bf92f...","span_id":"00f067...",
 "parent_id":"a3ce92...","name":"repository.Team.Create.Validator","kind":"internal",
 "duration_ms":38.4,"status":"error","error":"Invalid: a team needs a name",
 "service":"fantasyfootball"}
```

It is bounded: `observe.Config.FileMaxBytes` (8 MiB by default) with one
generation kept beside it as `<name>.1`, so the disk cost is twice the cap
rather than "however long the process ran". A span that succeeded has no
`status`, which makes `grep '"status":"error"'` the first thing to try.

### What you get

- **One span per request**, named by the route — `GET /api/v1/teams/{id}` — and
  not by the path, so it groups. A caller that arrives with a `traceparent`
  stays in its trace.
- **One span per repository call**: `repository.Team.Create`.
- **One per stage of a write**: `.Validator`, `.Before`, `.After`,
  `.AfterCommit`. This is the point of the whole thing. "The create was slow" is
  not worth collecting; "the validator was slow" is.
- **One per statement**, `INSERT team`, with the SQL on it as `db.query.text`.
  It comes from the connection rather than from generated code, so a query your
  own hook runs is on the trace too.
- A failure is recorded on the stage that failed. A 500 makes the request span
  red; a 404 or a 422 does not, for the same reason those are debug lines and
  not error lines.

The liveness and readiness probes are not traced, the same as they are not
logged.

**Not traced yet:** the `auth:` block's routes and the hand-written inbox routes
under `/notifications`. They log, and their failures carry a request id, but
they are mounted rather than generated and no span is opened for them — so they
do not appear on the monitoring page either.

## The monitoring page

The last few hundred requests, what each of them spent its time on, and the log
lines they wrote — on a page this same binary serves, on a port of its own. It
is for the deployment too small to be worth a collector and a Grafana in front
of it, which is most deployments for most of their life.

```yaml
tracing:
  enabled: true

monitoring:
  enabled: true
  addr: 127.0.0.1:9090
```

It reads the span file above and stores nothing of its own — no table, no
retention policy, nothing to run beside the server — which is why it cannot be
turned on without `tracing:`. rig refuses the combination when it reads
rig.yaml rather than leaving you with a page that is empty forever.

**`addr:` is required and there is no default.** The page listens there, in this
process, on a listener that is not your API's. See
[its own port](#its-own-port) for why that is not a route, and
[restricting it](#restricting-it-to-an-address) for what to put in it.

**Serving it is nothing extra.** The same process that installed the provider
builds the page over it, and `Configure` fills in the two `serve.Config` fields
that serve it:

```go
serve.Config{
    Monitor:     page.Handler(),   // Configure sets both, as a pair
    MonitorAddr: page.Addr(),
}
```

The page hangs off the provider because it reads the file that provider is
writing: naming the path twice is one place too many to get it wrong, and that
is why the constructor that installs the one builds the other. Both fields are
zero when the page is unarmed, so a laptop with no password set opens no second
port rather than one that refuses — and `Attach` says which half is not
armed, and why, on the logger that writes to the file the page would have read.

The `page` argument your wiring is handed is that page, for an application with
somewhere else to name it: a link to it from a page of its own, most likely,
since the page is on an origin of its own and a relative href does not reach it.
It is nil when there is no process to take one from — a task, or a `Mount` built
without one — and `Process.Page()` is the accessor behind it. A page nobody armed
is *not* nil: it is a real page whose `Addr()` is empty, so that, or `Unarmed()`,
is what says whether there is anywhere to send somebody.

**A `Tasks:` entry never reaches the mount function.** `serve.Main` runs the
task and returns, so `Attach` is not the only path out of the process:
`process.Close()` is the flush a cron run reaches. You do not write it —
`process.Configure` sets it as `serve.Config.OnExit`, and `serve.Main` runs it on
every way out. That includes the three that end in `os.Exit`: an unknown
subcommand, a task that failed, a boot that failed. A `defer process.Close()` in
`main` would have run on none of them, and those are the runs whose spans
somebody actually wants. The server path runs both halves and finds the second
already done, because `Provider.Shutdown` is idempotent.

`api.Monitoring()` is still exported too, for the one thing `NewProcess` does
not let you choose — a log sink at a level of its own:

```go
cfg := api.Monitoring()
cfg.Logs = logs
page, err := provider.Page(cfg)
```

`examples/linearlite/api/main.go` is the arrangement in full.

### Its own port

The page does not go on your API's mux, and there is no field that would put it
there. That is a deliberate narrowing, and the reason is one paragraph further
down: `allow:` is matched against the connection's own address, so **behind a
load balancer it matches everything or nothing**. A path is something any client
can ask for. Which interface a socket is bound to is not — `127.0.0.1:9090` is
reachable from that machine and from nowhere else, in every deployment, behind
every proxy, without anything being configured correctly for it to hold.

So the boundary is the address, and rig picks neither half of it. A default port
is one two rig services on a host would fight over. A default interface is a
decision about who can reach a page that lists every path, request id and error
cause your server has seen. `rig validate` refuses an enabled block without an
`addr:` (**RIG3009**) rather than choosing for you.

```
RIG_MONITOR_ADDR=127.0.0.1:9090
```

overrides it at run time, for the reason the span destination is an environment
variable: where something listens is a property of the deployment, and moving a
port should not need a regenerate.

Three things follow from the page having a listener of its own:

- **Nothing else is on it**, so `base_path` has nothing to collide with. It is
  kept — `/_rig/monitor` still — because it is the URL projects already have and
  because a reverse proxy in front of the port needs a prefix to key on.
- **The probes stay on the API's port.** `/livez` and `/readyz` have to be
  reachable by whatever restarts and routes to this process, and that is not
  necessarily on the network you bound the page to.
- **It is the last thing in the process to close.** The API drains, the
  `App.Close` hooks run, the pool closes, and the page is still answering — so a
  shutdown is something you can watch rather than something you read about
  afterwards.

Anything else that should be reachable on those terms and not on your API's —
a `pprof` mux, an operator endpoint — goes on `serve.Config.Monitor` too. It is
an `http.Handler`, not rig's page: `runtime/serve` is imported by every
generated server, and rig/observe brings OpenTelemetry with it.

### The password

```
RIG_MONITOR_PASSWORD=…      # twelve characters or more
```

**With nothing in it the page does not listen at all** — no port, not one that
answers 401 — and `Unarmed()` says so in the line above. That is the default on
a laptop and in CI, and it is the right one: the page lists paths, request ids,
user agents and the cause of every 500, which together are a record of what
every caller did.

It is HTTP Basic, so a browser asks and nothing has to store a session. The user
name is not checked; there is one credential here. There is **no lockout and no
rate limit** behind it — that would mean `rig/observe` depending on
`rig/runtime` for the throttle. The length minimum, the bind address, and the
allowlist below are what stand in for one.

There is no TLS on this listener, and rig terminates none. Bound to loopback
that does not matter; bound to anything else, put something in front of it that
does, the same as you did for your API.

### Restricting it to an address

Three layers, in the order a request meets them: the port decides who can open a
connection, `allow:` decides which of those are answered, and the password
decides who gets the page.

```yaml
monitoring:
  enabled: true
  addr: 127.0.0.1:9090
  allow:
    - 10.0.0.0/8
    - 127.0.0.1
```

`addr:` is the one that holds everywhere — see [its own port](#its-own-port).
Start there: if the only thing that should reach the page is the machine it runs
on, `127.0.0.1` is the whole answer and the rest is defence in depth.

`allow:` takes CIDR ranges or single addresses. An address that is not on the
list gets **404** — the same answer as a page that is not there — and its
password is never compared, so a scan learns nothing and a leaked password is
not enough on its own. It **narrows the password; it does not replace it**, and
there is no way to have one without the other.

**It reads the connection's own address and never a forwarded header** — no
`X-Forwarded-For`, no `X-Real-IP` — for the reason
[`auth.trusted_proxies`](rig-yaml.md#auth) exists: an address read from a header
a client controls is an address a client chooses. Which means that **behind a
load balancer this list is not a boundary**: every request arrives from the
balancer, so it matches everything or nothing.

That is exactly the case `addr:` covers. Bind the page somewhere the balancer is
not — a private interface, or loopback and reach it with `kubectl port-forward`
— and the boundary is the kernel's rather than a header's.

### The rest of the block

```yaml
monitoring:
  enabled: true
  addr: 127.0.0.1:9090  # required; $RIG_MONITOR_ADDR overrides it
  password_env: MY_APP_MONITOR_PASSWORD
  password: ""          # a secret in a checked-in file; rig warns (RIG3006)
  base_path: /_rig/monitor
  max_traces: 200
  max_logs: 500
```

### The logs

The page's other half, and it needs one more thing than the spans do: something
has to keep the log lines where the page can read them. `slog` writes to
wherever you pointed it, which is usually a terminal or a collector's agent, and
neither is a file rig can open.

So `observe` has a sink — and `api.NewProcess()` is what opens it, tees its
handler into the logger `Configure` hands the server, and gives the same object
to the page. That is the reason the three are one call, and the reason `api.Main`
makes it for you: the sink has to exist before the logger built out of it, and
the page has to read the sink the logger is writing rather than a second one
opened from the same path.

```go
// api.Main does this, then Configure(serve.Config{...}), which sets:
//     Logger:      stderr and the file, both, each at its own level
//     Monitor:     the page, on its own port
//     MonitorAddr: zero with it when the page is unarmed
```

The two levels are deliberate. The logger keeps whatever the default handler is
set to; the file keeps debug — which is where rig's request line is, so the page
has requests to list without the process printing one per request to a terminal
nobody is watching.

For an application with a handler of its own, build the process yourself, set
`Logger`, and tee `Process.LogHandler()` into it — otherwise the page has
nothing to list:

```go
Logger: slog.New(observe.Tee(myHandler, process.LogHandler()))
```

```
RIG_LOG_FILE=/var/log/myapp/rig.jsonl
```

**With nothing in it nothing is written**, the same as `$RIG_TRACE_FILE`, and
the page says so instead of showing an empty list. `Attach` already
writes that line at startup, with the reason `logs.Unarmed()` gave.

It is the same store as the spans and makes the same promise: one JSON object
per line, `observe.LogConfig.FileMaxBytes` (8 MiB) with one generation kept
beside it as `<name>.1`. Writes are unbuffered, so there is nothing to flush and
no shutdown step to add — which is also why `process.Close()` does not close it:
the last lines a server writes are written during its shutdown, and a step that
closed the file would throw them away.

```json
{"time":"2026-08-21T09:14:02.113Z","level":"ERROR","msg":"request failed",
 "trace_id":"4bf92f...","span_id":"00f067...",
 "attrs":{"request":{"request_id":"4bf92f...","method":"POST","route":"POST /api/v1/teams"},
          "status":500,"error":"creating team: connection refused"}}
```

Three things about that shape are deliberate:

- **The trace id is on the line**, taken from the context the log call was
  given. That is what makes a request and the lines it wrote one view rather
  than two searches — and it is why every log call in rig passes the context.
- **Everything else is under `attrs`.** A line is free to carry a field called
  `level` or `msg`, and a flat record would let it overwrite one.
- **The file keeps debug lines even when your terminal does not.** `observe.Tee`
  asks each handler its own level, and the sink's default is `DEBUG`. This
  matters more than it sounds: rig's request line and its refusal line are debug
  lines, so a server running at info writes them nowhere — and the page would
  have nothing to list. Set `LogConfig.Level` if you want the file narrower.

`observe.ReadLogs` reads such a file from a script, and it will read a file your
own `slog.JSONHandler` wrote too — `time`, `level` and `msg` are slog's own
keys, and anything else lands in `attrs`.

**The span file and the log file have to be different files.** Two writers on
one path interleave their lines and rotate each other's data away; `Page` says
so rather than leaving you with a file that reads as neither.

### What it shows

Two tabs over one search box.

**Requests**, newest first: the verb, the route, the status, how long it took.
Opening one gives the spans underneath it on a timeline — the repository call,
the stage of the write that was slow, the statement that ran, each with its own
time and its self time — the cause on the span that failed, and **the log lines
that request wrote**, offset from when it started. Above the list are four
numbers over the requests on the page: how many, how many failed, p50/p95/p99,
and a sparkline of when they arrived.

**Logs**, newest first: the level, the message, the route. A line expands to
every attribute it carried, and to the way back to the request it belongs to.
The level filter is a threshold — `WARN` means warn and worse.

One search box for both halves, over routes, messages, error text, attributes
and trace ids. Paste the `requestId` from somebody's screenshot and you have
their request and everything it logged.

**Dependencies**, in the header beside the live pulse: one pill per thing this
server cannot work without, green when it answered and red when it did not, with
the reason on hover. rig registers the database, and the sync service where a
table has shapes; `Page.Watch` adds your own.

```go
page.Watch("search index", func(ctx context.Context) error { return idx.Ping(ctx) })
```

The page holds the probe rather than a state something pushed to it, so a pill
answers whether the dependency is there *now* — a sync service that comes back
shows up on the next poll without any traffic having to discover it. All of them
run at once, under one two-second budget, and a probe still running when it is
spent is reported as not answering rather than waited on — including one that
ignores the context it is handed. The endpoint answers 200 whatever they say:
this is a report, not a verdict. The verdict is `serve`'s
`ReadinessPath`, which is deliberately a different endpoint with a different
opinion about which dependencies are worth taking an instance out of rotation
for — see [electric.md](electric.md#running-electric-alongside-your-application).

The state is in the URL, so a view is a link: reload it, share it, or use the
back button. `?` lists the keyboard shortcuts.

`/_rig/monitor/traces.json`, `/_rig/monitor/logs.json` and
`/_rig/monitor/checks.json` — on the page's own port, not your API's — are the
same data, and
`observe.ReadTraces` and `observe.ReadLogs` read the files straight from a script
if you would rather not have a page in the loop.

The empty states say which empty they are: no span file configured
(`$RIG_TRACE_FILE`), no log sink wired, no log file configured
(`$RIG_LOG_FILE`), nothing served yet, or a filter that nothing here matches.

**Looking at the page does not appear on the page.** rig opens its spans and
writes its request lines inside each generated handler, so anything on the mux
that is not one — the page, the probes — is invisible to both.

### What it is not

- **It is not a tracing backend.** One process, one file, a few hundred
  requests, no index and no query language. A second replica shows its own
  spans, because a file is per process. When that stops being enough, point
  `$OTEL_EXPORTER_OTLP_ENDPOINT` at a collector: everything is already
  instrumented.
- **It does not survive the file's ceiling.** `FileMaxBytes` with one rotation
  is what bounds it, so the oldest requests go. A request whose beginning has
  rotated away is still listed, by trace id, with the spans it still has.
- **Its numbers describe the page, not the server.** The count, the error rate,
  the percentiles and the sparkline are computed over the requests in the window
  you are looking at — a few hundred of them, from one process. Nothing is
  counted over time and nothing is retained, so these are a way of reading the
  window rather than metrics. For metrics, see *What rig does not do here*.

## Correlating a log line with a trace

**`RequestID` is a field you almost never set.** Nil does not mean nothing: it
means the caller's own `X-Request-Id` when it sent one, and — with `tracing:` on
— this request's trace id when it did not. The `requestId` in the error body,
the `request_id` on every log line and the trace in your collector are then the
same string, and you wrote nothing.

The order is the point. A client that labelled its own request is the one
correlating two sides, so it is believed; only a request nobody named gets a
name invented for it.

In the log file the sink writes, the trace is also `trace_id` on the line itself,
taken from the context rather than from the request — which is what the
monitoring page joins the two halves on. That is why a caller-supplied
`requestId` does not cost you the join: the page still has the trace.

**A caller's header is bounded before it is believed** — at most 128 bytes of
printable ASCII, and refused rather than truncated otherwise. The value reaches
an error body and every log line the request writes, so it is client-controlled
text in two places read by machines; a header rig does not understand is not one
it should half-quote into a log file. What a caller gets for sending nonsense is
the identifier it would have got for sending nothing.

Without the `tracing:` block the caller's own is the only identifier there is,
and a request that sent none gets an empty one. Turning `tracing:` on is what
gives every request one whether or not the caller thought to send it.

**The authentication routes are the one place the trace fallback does not
reach.** `/auth/*` is mounted by `Auth.Mount` rather than emitted per endpoint,
so no span is opened over it — a sign-in that sent no header gets no `requestId`,
where a resource route in the same project gets a trace id. The caller's own
header does reach it, and that is the case worth having: a client that wants its
sign-in correlated with the rest of its requests sends one.

Set `RequestID` only to answer the question differently — an identifier from the
rest of your system, including a trace id from an OpenTelemetry setup that is
entirely your own. rig needs no dependency for that. If you do set it, set
`Hooks.RequestID` to the same function: the authentication routes are configured
before the `Server` literal exists, so that is how the one answer reaches them.

## Tracing a client

A generated Go client traces through a seam rather than a dependency, because
`rigclient` is imported by every client and most of them do not want otel:

```go
client, err := todoclient.New(rigclient.Config{
    BaseURL: "https://api.example.com",
    Trace:   observe.Call,
})
```

Two spans per call, and the nesting is the point: the outer one is the operation
(`listTodos`), the inner ones are the attempts. One call can be three attempts —
the QUERY a proxy refused, the POST to the alias, and the retry after the
credential refreshed — and tracing only the attempts would show that as three
unrelated requests.

For the transport's own view — DNS, connect, TLS — wrap `Config.HTTPClient` in
`otelhttp.NewTransport`. That import is yours; rig does not take it.

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

- **No metrics.** Counters, histograms and a `/metrics` endpoint are a separate
  decision with a separate cost, and rig makes none of it for you. The spans
  carry durations — the monitoring page reads them off the last few hundred
  requests — which is where most of the questions a histogram answers can be
  asked instead, one request at a time.
- **No middleware of somebody else's.** rig opens the request span itself rather
  than wrapping the mux in `otelhttp`, because the route is only known once the
  mux has dispatched — outside it, every span would be named by path. If you
  want otelhttp anyway, `Register` returns an `*http.ServeMux` you can wrap.
- **No log shipping.** `slog` writes where you point it. A collector, a
  retention period and somewhere to search from are the deployment's, not rig's.
  The sink above is not a counter-example: it is a bounded file with one
  generation, written so that the monitoring page has something to read, and it
  keeps nothing longer than the disk cost you agreed to.
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

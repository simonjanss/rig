# Clients

> **Half written.** The Go client's configuration, authentication and pagination
> are still to come.
>
> Until they are, [examples/sdk](../examples/sdk) is a working program that
> calls two rig applications through their generated clients, and
> [examples/todo/client](../examples/todo/client) is what the generator emits.

rig generates two clients, and they answer the same shape because they read the
same document: methods grouped per resource, a QUERY that falls back once and
remembers, query parameters that are absent rather than zero, a per-input shape
for the 422 body, and an upload that carries its bytes. The Go one is below;
[the TypeScript one](#the-typescript-client) is after it.

`go-client` generates a typed Go SDK from the same document the router is
generated from, so the client and the server cannot disagree about what the API
looks like.

```yaml
generators:
  - name: go-client
    out_dir: client
    options:
      package: client
```

It is opt-in: not every project wants an SDK of its own.

The generated half is the wire types and one method per endpoint. The other half
— the transport, credentials, retries, pagination, error decoding — is the
`rig/rigclient` module, which your client imports. A program that *calls* a rig
application depends on `rig/rigclient`; it never depends on rig itself.

## Where the client points

An SDK that cannot say where its API is has handed the question back to whoever
imports it. Name the deployments in
[`rig.yaml`'s `servers:` block](rig-yaml.md#servers) and both clients carry them:

```yaml
servers:
  - name: production
    url: https://api.example.com
    default: true
  - name: local
    url: http://localhost:8080
```

```go
c, err := client.New(rigclient.Config{})            // production
c, err := client.New(rigclient.Config{BaseURL: client.ServerLocal})
c, err := client.New(rigclient.Config{BaseURL: mock.URL})
```

An empty `BaseURL` is the deployment marked `default:` — that is what
`DefaultBaseURL` is, and it is a constant you can read rather than a rule hidden
in the constructor. Anything you pass wins, which is what keeps a mock server and
a local build reachable.

The same three in TypeScript, where `baseUrl` becomes optional:

```ts
const client = createClient({});                    // production
const client = createClient({ baseUrl: servers.local });
const client = createClient({ baseUrl: mockUrl });
```

**`""` is not "leave it out".** It is the same-origin answer — a front end served
by its own API names no host at all — and the default never replaces it. That is
why `createClient` reads `config.baseUrl ?? defaultBaseUrl` and not `||`: the
empty string is falsy, and the difference between the two operators is a browser
app quietly calling somebody's laptop.

A project that names no deployment gets neither constant, and both constructors
go on requiring a URL. That is the right shape for a client generated before
there is a host to name.

## When a call is refused

Every method that sends a body has a reader of its own, named after the call. It
takes the error the method returned and hands back everything the server said:

```go
todo, err := client.Todos.Create(ctx, client.TodoCreateInput{Title: "   "})

if refused, ok := client.TodoCreateError(err); ok {
    if refused.Fields != nil && refused.Fields.Title != nil {
        form.Title.Problem = refused.Fields.Title.Message
    }
    log.Printf("%s (%d) request %s", refused.Code, refused.Status, refused.RequestID)
}
```

`Fields` is shaped like the input you sent — one member per member, nil where
nothing was wrong — so each message goes beside the control it belongs to
instead of being parsed out of a sentence. `Code`, `Message`, `RequestID`,
`Status` and `RetryAfter` are the envelope, on the same value, because a caller
who wants one usually wants both.

**The shape comes from the call, not from you.** `rigclient.FieldsAs` still
works, and is what a request made by hand through `client.Runtime()` uses — but
it asks you to name the shape, and naming the wrong one is not an error. Every
member of a field shape is optional, so `FieldsAs[client.TodoUpdateFields]` on a
failed create decodes perfectly and hands back an empty struct with `ok` true.
`client.TodoCreateError` cannot be given the wrong shape: there is only one that
compiles.

**`Fields` is nil for every refusal but a 422.** A 404 has a code and a message
and nothing to put beside a control, and a zero-valued shape there would read as
a body nobody complained about. The second value is false for anything that is
not a refusal at all — a DNS failure, a cancelled context — because there is no
envelope for a code or a field to have come from.

Nothing about the error itself changed, so everything written before this keeps
answering; the reader is a second way to look at it rather than a different
error:

```go
rigclient.IsInvalid(err)     // still true
rigclient.CodeOf(err)        // still the code
errors.As(err, &rigErr)      // still finds *rigclient.Error
```

Three calls have no reader. A read sends no body, so nothing about a `Get` can be
wrong per field. A search's body is a filter — a question rather than something
filled in field by field — which nothing validates, so a reader for it would be a
function per resource that could only ever answer nil. And a revert is refused by
the update rules it replays: its 422 arrives shaped like
`client.TodoUpdateFields`, not like the version identifier it was asked about, so
read one back with `client.TodoUpdateError`.

### The other half of the shape

A custom endpoint gets the same treatment, and its server half is generated
beside the body: `LessonPublishBodyError` in your API package has one member per
member of `LessonPublishBody`, and returning it from the service is what makes
`client.LessonPublishError(err)` answer with fields rather than with prose.
Nothing generated fills it in — only your service knows what its own body means
— which is why it comes with `Empty()` and no validator.

## When the server says not now

A 503 from an instance being taken out of a load balancer, a 429 from a rate
limiter, a connection the server had already closed — none of these mean your
request was wrong, and all of them are usually over by the time you could ask
again. The SDK asks again for you.

Four attempts. The first retry goes out immediately, then after about a second,
then about two — so a call that was going to fail takes roughly five seconds to
say so. The immediate one is there because the commonest failure worth retrying
is a pooled connection the server closed while it was idle, and opening another
one fixes that; waiting a second first would only be slower.

The waits are randomised across half their interval. A hundred clients that all
failed together would otherwise all come back together, which is the thing the
interval was for.

Retried: **429, 500, 502, 503, 504**, and a request that never got an answer.
Not retried: every other 4xx, a 501, and a 505. A 501 is what something in the
chain says when it has never heard of the `QUERY` method — the SDK falls back to
the `_search` alias for that, and asking again a second later would not help.

### Seeing the limit before you hit it

The retry above is the reaction. There is also a signal that arrives *before*
anything is refused: a server with [`throttle:`](rig-yaml.md#throttle) configured
puts `RateLimit-Limit` and `RateLimit-Remaining` on **every** response, not only
on the 429. A client that watches them can slow down, shed work or raise an alarm
while its calls are still succeeding.

Both SDKs hand you those numbers through one callback.

```go
rt, err := rigclient.New(rigclient.Config{
    BaseURL: "https://api.example.com",
    OnRateLimit: func(s rigclient.RateLimitStatus) {
        if s.Fraction() > 0.8 {
            log.Warn("close to the API limit",
                "op", s.Op, "remaining", s.Remaining, "limit", s.Limit)
        }
    },
}, api)
```

```ts
import { fraction } from "@rig-ts/client";
import { createClient } from "./api";

const client = createClient({
    baseUrl: "https://api.example.com",
    onRateLimit: (s) => {
        if (fraction(s) > 0.8) {
            console.warn(`${s.op}: ${s.remaining} of ${s.limit} left`);
        }
    },
});
```

The generated `createClient` takes `@rig-ts/client`'s own `Config`, so this is one
setting rather than something the generator had to learn.

It runs once per attempt — including attempts a retry replaced, because a 429
that was retried away spent budget too — and only when the server said something.
A server with no `throttle:` block sends none of these headers and the callback
never fires, which is why an absent header is not read as a limit of zero.

`resetAfter` is only stated on a refusal. An allowed response says how much is
left, not when it comes back.

**The SDKs deliberately do not slow themselves down for you.** A client library
that silently waited because `remaining` was low would turn a batch job's
throughput into a mystery, and it cannot know whether you would rather go slower
or fail sooner. You get the numbers; the policy is yours.

### Writes are retried too, because they go out named

A `POST` whose answer went missing may already have written the row. Sending it
again would ordinarily write a second one — so every write the SDK might have to
repeat carries an `Idempotency-Key`, generated per call and kept the same across
attempts. A rig server that sees a key it has seen before answers with what it
answered the first time instead of doing the work again, and it records that
answer in the same transaction as the write, so there is no window where one
exists without the other.

That is what makes the ambiguous failure stop being ambiguous. A connection that
dies mid-`POST` may have been applied and may never have arrived; you cannot tell
and neither can the SDK, but the server can, and that is the only place the
question can be answered.

You do not have to do anything for this. It is worth knowing about for three
reasons:

- **A key names one request, not a slot to put requests in.** Reusing one for a
  different body is a **422**, not a replay. Replaying the wrong response would
  hand you a success describing something you never asked for.
- **A write that failed is not remembered.** A create that came back 422 wrote
  nothing, so there is nothing to be idempotent about — fix the body, reuse the
  key, and you get the write you wanted rather than a cached complaint about the
  old one.
- **`WithIdempotencyKey` is still worth reaching for**, when you want to choose
  the name yourself. A key derived from your data — an import job naming a row by
  the line it came from — deduplicates a re-run of the whole job, which a fresh
  random name cannot.

**A delete that had to be sent twice can come back `NotFound`.** Deletes are not
keyed: a delete is already idempotent in what it leaves behind, and buying a
smoother answer would cost a transaction on every one. If the first attempt
worked and its answer was lost, the second is telling the truth — the row is
gone. `rigclient.WithRetry(1)` is how you decline to pay that.

**An upload is never sent twice.** It is the one write with no key on it, and
the reason is the server's: an upload route's body is still arriving when the
handler calls your service, so recording it would mean holding a database
transaction open for the length of a transfer — a pooled connection per upload,
for as long as the slowest client takes. A few of those are the whole pool. So
an upload that fails comes back to you, and sending it again is your call to
make.

That costs a create carrying a file its retry too, even though the server does
record one: the SDK cannot tell that route from an upload route, and guessing
wrong in the other direction stores the file twice. `WithIdempotencyKey` still
names such a create if you re-send it yourself.

For a create that does carry a file, the fingerprint that catches a reused key
covers the fields and the path, not the file bytes. Hashing those would mean
buffering a file that may be larger than memory, which is the one thing the whole
file path exists to avoid.

### The interval the server asked for

`Retry-After` wins over the SDK's own backoff, in both the forms it takes — the
seconds rig's own server sends, and the date a CDN or WAF in front of it might.
It is not jittered and it cancels the immediate first retry: a server that has
just said when to come back has answered that question.

An interval longer than your call has left is not waited for, and neither is one
longer than thirty seconds. Whether your program has a minute is a question about
your program, so the refusal comes back with the interval on it and you decide:

```go
if rigclient.IsRateLimited(err) {
    var e *rigclient.Error
    errors.As(err, &e)
    log.Printf("rate limited; the server wants %s", e.RetryAfter)
}
```

### The error is still the server's own

Nothing about a refusal changed. When the attempts run out you get the last one
whole — code, message, request ID, fields — not a wrapper saying how many times
it was tried. `IsRateLimited`, `errors.As`, `CodeOf` and
`client.TodoCreateError(err)` all read it exactly as they did before any of this
existed. If the budget ran out you still get the server's refusal rather than a
deadline error, because blaming your clock for somebody else's outage sends you
to the wrong logs.

### Retries do not lengthen your call

**They happen inside the timeout you already had**, retries and backoff included.
Turning them on cannot make a call slower than it was — which is the whole reason
they are on by default. What it costs is the other side of that: a call that
spends its entire budget on the first attempt has nothing left for a second, so a
slow server gets one try where a fast-failing one gets four.

Raising the attempt count without raising the timeout buys more tries inside the
same wall clock, not more wall clock:

```go
rigclient.Config{Retry: rigclient.Retry{Attempts: 6}}         // every call
client.Todos.List(ctx, q, rigclient.WithRetry(1))             // and off for this one
```

### What is not retried

A body that fails while *you* are reading it. Once the headers are a success the
call succeeded as far as the SDK is concerned, so a download that dies halfway
through `io.Copy` is yours to make again — and `WithRange` is what resumes it
rather than starting over.

A request that never went out, either: a body that would not encode and a
credential that would not refresh are your call's own failures, and repeating
them would only make a bug take four times as long to surface.

And any upload, for the reason above. A form body is still *re-sent* by the two
things that are not retries — the `_search` fallback and a credential refresh on
a 401 — and an upload whose body cannot be produced a second time comes back
there as `rigclient.ErrCannotRetry` joined to whatever prompted it. See [a body
that cannot seek](#sending-files).

## Bounding one call

`rigclient.Config.HTTPClient` defaults to a client with a thirty-second timeout,
and that is a limit on the whole exchange rather than on a stalled read. It is
right for a JSON call and wrong for anything that moves a file, and a context
deadline cannot raise it — `http.Client.Timeout` is a ceiling, not a default.

So bound the call rather than the client:

```go
file, err := client.Todos.UploadCoverFile(ctx, todoID, upload,
    rigclient.WithTimeout(10*time.Minute))
```

Raising the default instead would weaken every other route to suit the one that
transfers files. `WithTimeout` takes a shallow copy of your client with a
different deadline, so the transport and its connection pool are still shared.

It bounds the call and not each attempt, so ten minutes means ten minutes however
many times the SDK had to send it — see [When the server says not
now](#when-the-server-says-not-now).

## Asking for the whole tenant

A read that normally answers with the caller's own rows widens with `Wide()`:

```go
sessions, err := client.Auth.Sessions(ctx, rigclient.Wide())   // needs session.read.all
trail, err := client.Auth.AuditLog(ctx, rigclient.AuditQuery{}, rigclient.Wide())
```

It sets the `scope` parameter [`tenancy.Scope`](auth.md) describes, and asking for
more than the credential holds is a **403** rather than a smaller answer — a
narrower result would leave the caller unable to tell "you may not see that" from
"there is nothing else". A generated read carries the same thing as a typed field
on its query, which is the same request written the other way.

`Auth.AuditLogAll` walks the authentication trail page by page, stopping at the
first failure — which arrives as the second value of the last pair, so a loop that
ignores it is a loop that silently stops early.

## Sending files

These methods appear on a generated client once your project has a file column.
A `<role>_file_id` column is the whole declaration — see
[schema.md](schema.md#files).

A file goes out as a `multipart/form-data` body, which the SDK builds for you
from a `rigclient.Upload`:

```go
f, err := os.Open("cover.png")
if err != nil {
    return err
}
defer f.Close()

upload := rigclient.Upload{Name: "cover.png", ContentType: "image/png", Body: f}
```

`Name` is what the file is called: the server records it on the row and puts it
in the download path, and it never becomes the storage key. `ContentType` is
what you claim the bytes are, and it makes little difference either way — the
server sniffs the content, and the sniffed type is the one it stores and the one
it serves back. `rigclient.UploadBytes` is the same thing for a file you already
hold in memory.

Nothing is buffered. The form is written to a pipe as the request goes out, so a
file larger than memory is an ordinary upload. Two consequences worth knowing:

**The request is chunked.** The length of the form is not known when the headers
are written, so no `Content-Length` is sent. An intermediary that insists on one
is a deployment problem, and buffering every upload to satisfy it is not a trade
the SDK makes for you.

**A body that cannot seek cannot be retried.** When a call comes back 401 and
your credential is a `Session`, the SDK refreshes the token and sends the request
again — and the second send needs the bytes again. A `*os.File` seeks, so the
ordinary case works. A pipe, another response body, or a decompressor does not,
and rather than buffering the whole upload against a retry that almost never
happens, the SDK returns `rigclient.ErrCannotRetry`:

```go
if errors.Is(err, rigclient.ErrCannotRetry) {
    // The credential was refreshed. Make the call again from a reader that
    // still has the bytes.
}
```

The failure still answers `rigclient.IsUnauthorized`, because both facts are
true and you need both to decide what to do.

## Receiving files

A download comes back as a `rigclient.Content`, which is the response and not a
copy of it:

```go
c, err := client.Todos.DownloadCoverFile(ctx, todoID, fileID, "cover.png")
if err != nil {
    return err
}
defer c.Body.Close()

_, err = io.Copy(dst, c.Body)
```

**Close the body.** Nothing reads ahead, which is what lets a file larger than
memory go straight to disk — and it means the connection is held until you are
done with it. It is the one thing the generated method cannot do for you.

`ContentType` is the type the server sniffed at upload, `Length` is the size or
`-1`, and `Filename` is the name from `Content-Disposition`, decoded from the
RFC 5987 form when it is not ASCII. It is a suggestion and not a path: if you
write it to disk, you decide where and you sanitize what.

Downloads flow through the API rather than through a signed storage URL, because
the endpoint is where the permission check lives. The URL on the row is stable
and unsigned, so holding it grants nothing.

### Ranges and conditional reads

The server answers range and conditional requests, so a resumed download does
not start over and a `<video>` can seek. The document says nothing about ranges —
they are a question about one call, not part of what an endpoint takes — so they
are call options rather than generated parameters:

```go
c, err := client.Todos.DownloadCoverFile(ctx, todoID, fileID, "clip.mp4",
    rigclient.WithRange(1<<20, -1))          // the rest of the file from 1 MiB
```

`WithRange` gets you a 206 and `WithIfNoneMatch` gets you a 304, both of which
arrive as `Content.Status` rather than as an error: you asked the question, so
the answer is a result. On a 304 the body is empty and closing it is all there
is to do with it.

## Sending a file with the row

A table whose file column is `not null` cannot be filled in two requests: the row
would have to exist before the upload had anywhere to go. So a create on such a
table also accepts a form, and the client has a second method for it:

```go
attachment, err := client.TodoAttachments.CreateWithFiles(ctx,
    client.TodoAttachmentCreateInput{TodoID: id, Caption: rigclient.P("On the summit")},
    client.TodoAttachmentCreateFiles{
        AttachmentFile: rigclient.UploadBytes("summit.png", "image/png", b),
    },
    rigclient.WithTimeout(10*time.Minute))
```

The row and the bytes are committed together, so a create that fails leaves
neither. `CreateFiles` has one member per file column — a plain `Upload` where
the column is not null and a pointer where it is — so leaving out a file the
schema requires does not compile.

It is a second method rather than a wider `Create` because `Create` is the
most-called method rig generates: adding a parameter to it would break every
existing caller the day somebody adds a file column to a table they already had.

## Tracing a call

If the program calling the API runs OpenTelemetry, hand the client the seam:

```go
client, err := todoclient.New(rigclient.Config{
    BaseURL: "https://api.example.com",
    Trace:   observe.Call,
})
```

`Trace` is a function rather than a tracer, so this module depends on no tracing
library and neither does a client that never sets it. Two spans come out per
call: the operation — `listTodos` — and one per attempt underneath it. The
distinction matters because one call can be three attempts: the QUERY a proxy
refused, the POST to the alias, and the retry after the credential refreshed.

For DNS, connect and TLS as well, wrap `Config.HTTPClient` in
`otelhttp.NewTransport`. That import is yours.

## The TypeScript client

`ts-client` generates the same SDK for a browser.

```yaml
generators:
  - name: ts-client
    out_dir: web/src/api
```

> **The packages.** `@rig-ts/client`, `@rig-ts/electric` and `@rig-ts/presence`
> are on npm, at the same version as the rig that generated the code importing
> them — `rig version` with its leading `v` stripped. The names below are what
> the generator emits by default; point them somewhere else with the
> `client_import` and `electric_import`
> [options](generators.md#options-briefly) — a file: specifier, a workspace, or
> a registry of your own.

The generated half is the wire types, one method per endpoint, and — when a
table has opted into live sync — one factory per stream. The other half is
`@rig-ts/client`: the request, the credential, the retries, the pagination, the
error decoding. It has no dependencies of its own and nothing in it is React.

```ts
import { createClient } from "./api";

// The empty string is the ordinary same-origin case: it resolves against the
// page, so a front end served beside its API names no host at all. It is a base
// URL rather than an absent one, so a project that named a default deployment
// keeps getting the page's own origin here.
const client = createClient({ baseUrl: "" });

const page = await client.todos.list({ limit: 20 });
const todo = await client.todos.create({ title: "write it down" });
```

### When a call is refused

A refusal throws a `RigError`, and every method that sends a body has a guard of
its own named after the call:

```ts
import { isTodoCreateError } from "./api";

try {
    await client.todos.create({ title: "   " });
} catch (err) {
    if (isTodoCreateError(err)) {
        form.title.problem = err.fields?.title?.message;
        console.log(err.code, err.status, err.requestId);
    }
    throw err;
}
```

`fields` is shaped like the input you sent — one member per member, absent where
nothing was wrong — so each message goes beside the control it belongs to. It is
`undefined` for every refusal but a 422, for the same reason `Fields` is nil in
Go: a 404 has nothing to put beside a control.

The guard cannot be given the wrong shape, which is the whole reason it exists.
Every member of a field shape is optional, so a hand-named
`fieldsAs<TodoUpdateFields>` on a failed create matches perfectly and hands back
an empty object.

### An update says which of three things it means

```ts
await client.todos.update(id, {
    title: "a new title", // set it
    notes: null,          // clear it
    // priority is absent, so it is left alone
});
```

`JSON.stringify` drops an absent key, so leaving a field out is what leaves it
alone — no wrapper type needed. A column that cannot hold null has no `| null`
in its member, so clearing it does not compile rather than being refused at
runtime.

### Files

A file column gets an upload, a download and a delete, and the upload is the one
method whose shape is not the ordinary one — it takes the bytes:

```ts
const file = form.elements.cover.files[0]; // a File is already a Blob
const stored = await client.todos.uploadCoverFile(id, {
    name: file.name,
    body: file,
});
```

`contentType` is optional and is a claim rather than the answer: the server
sniffs the bytes, and the sniffed type is what it stores and what a download
announces. A download hands the `Response` back unread, so a large file is not
buffered first — `await res.blob()`, or read `res.body` as a stream.

**An upload is never sent twice.** A rig server records no upload against an
idempotency key, because its body is still arriving when the service is called,
so the SDK does not retry one — a second send would store the file again. A
failure comes back as it happened and retrying is yours to decide, which is right
because only you still have the bytes.

A not-null file column cannot be reached that way at all: the row would have to
exist before an upload had anywhere to go. So a table with file columns also gets
a create that carries them, beside the ordinary one rather than instead of it:

```ts
await client.todoAttachments.createWithFiles(
    { todoId, caption: "the plan" },
    { attachmentFile: { name: file.name, body: file } },
);
```

The row and the bytes are committed together, so a create that fails leaves
neither. The row travels as a part named `json` — the same body `create` sends,
through the same validation, so a 422 is read back with the same
`isTodoAttachmentCreateError`.

Which members of that second argument are optional is the schema's answer, not a
convenience: `attachmentFile` above is required because the column is not null,
and `TodoCreateFiles.coverFile` is optional because `cover_file_id` is nullable.
Leaving out a file the schema insists on does not compile.

### Live sync

A table with `electric: {enabled: true}` gets a factory per shape, in
`electric.gen.ts`. Those need the second package, `@rig-ts/electric`:

```ts
import { useLiveQuery } from "@tanstack/react-db";
import { createTodoStream } from "./api";

function Todos() {
    // Safe to call during render: the same client and params always give back
    // the same collection, so the stream survives a navigation and two callers
    // share one subscription.
    const todos = createTodoStream(client.runtime, {});
    const { data } = useLiveQuery((q) => q.from({ todos }));
    …
}
```

It is a second package because it is not free: the sync client and TanStack DB
come with it. A project with no streamed table gets no `electric.gen.ts` and
installs neither.

The trash and the history are separate factories, and the columns decide whether
they exist — `createTodoDeletedStream` when the table has a `deleted_at`,
`createTodoVersionsStream` when it has the snapshot columns. That is the same
rule the [shape routes](electric.md) follow.

`@rig-ts/electric` is framework-free. `useLiveQuery` above is
`@tanstack/react-db`'s, which your application installs; the collection a
factory returns is a plain TanStack DB collection and works with any binding.

### Two shapes for one row

**A streamed row is not the same shape as the row the API sends**, and the
generated types say so: `Todo` has `createdAt`, and `TodoRow` has `created_at`.

The reason is that a shape endpoint is a proxy in front of the sync service. A
REST response is what Go's `encoding/json` wrote, under the keys
[`api.json_case`](rig-yaml.md) produced — camelCase by default. A stream is what
Postgres printed, under column names, and nothing in between rewrites it. Using
one type for both would compile and be wrong about every key, so the generator
declares both and lets the compiler keep them apart.

`TodoRow` carries the shape's projection, which is the resource's readable
fields — so a column excluded from the API is excluded from the stream too. Its
members are nullable rather than optional: the sync service sends every column
on every row, with a null where the column is null.

The values agree even where the keys do not. `@rig-ts/electric` corrects what
Postgres prints so a `timestamptz` decodes to the same RFC 3339 string the API
would have sent, and an `int8` to a `number` rather than a BigInt.

### Cross-origin

rig serves the shape routes from the same mux as the rest of the API, and
same-origin is what everything above assumes.

A front end on a different origin needs two things from whatever sits in front
of the server. `Authorization` is not a CORS-safelisted header, so a stream's GET
becomes a preflighted request that has to be allowed. And the sync protocol's
cursor travels in response headers, so those have to be exposed:

```
Access-Control-Expose-Headers: electric-handle, electric-offset, electric-schema, electric-cursor
```

Without the second one the browser hides the cursor from the client and the
subscription ends after one response, which looks like a stream that stopped
rather than like a configuration problem. rig adds no CORS headers of its own.

## Testing against a generated client

Both SDKs take the same two things from a test, for the same reason they answer
the same shape: the transport, and the clock.

```go
api, err := client.New(rigclient.Config{
    BaseURL:    "https://api.example.com",
    HTTPClient: &http.Client{Transport: recorded},
    Now:        func() time.Time { return at },
})
```

```ts
const client = createClient({
    baseUrl: "https://api.example.com",
    fetch: recorded,
    now: () => at,
});
```

`Now` and `now` are there so a test can cross a token expiry without waiting for
one, and `retry: { baseMs: 0, capMs: 0 }` is how the TypeScript one stops a
backoff schedule from being spent for real.

### Faking one call, rather than the whole server

Swapping the transport tests the client. Most tests are not about the client:
they are about the code that calls it, and what they want is one method answering
something chosen.

Go asks nothing of rig for this. Interfaces there are structural, so a caller
declares the surface it actually uses and `*client.TodoClient` satisfies it
without the generator being told:

```go
type todoLister interface {
    List(ctx context.Context, q client.TodoListQuery,
        opts ...rigclient.CallOption) (*client.TodoListResponse, error)
}
```

The options are part of it. Every generated method ends with
`opts ...rigclient.CallOption`, and an interface that leaves them off is one the
client does not satisfy — which the compiler says at the assignment rather than
at the declaration, so it reads as a problem with the wrong line.

In TypeScript `client.todos` is an interface the generator emits — `TodoClient`,
in `todo_client.gen.ts` — so an object satisfies it. Spread a real client and
replace the call under test:

```ts
const real = createClient({ baseUrl: "" });

const client: Client = {
    ...real,
    todos: { ...real.todos, list: async () => page },
};
```

Nothing there is asserted, and that is the point of writing it this way. The
inner spread is what supplies the dozen methods the test does not care about, and
it works because a resource's methods are its own properties rather than a
prototype's — so the compiler still checks the one method you did write, and
still complains if you misspell it.

**Build the real client rather than assembling one.** `createClient` opens no
connection and sends nothing, so it costs nothing; the outer spread is there
because `Client.runtime` is a real `Runtime`, which a test has no way to
construct a stand-in for and no reason to.

### Reading back a refusal

`RigError` is exported so a double can raise one, which is what a test of the
error path needs — the guards are ordinary functions over an ordinary error, so
`isTodoCreateError` answers for a hand-built refusal exactly as it does for one
that arrived:

```ts
import { ErrorCode, FieldCode, RigError } from "@rig-ts/client";

create: async () => {
    throw new RigError({
        status: 422,
        code: ErrorCode.UnprocessableEntity,
        fields: {
            title: { code: FieldCode.CannotBeEmpty, message: "required" },
        },
    });
};
```

**The `code` is what the guards read, not the status.** `isInvalid` — and so
every generated guard over it — asks whether the code is
`UnprocessableEntity`; a refusal built with a 422 and no code is a `RigError`
that no guard answers for, which looks like a broken guard rather than an
incomplete double.

### What this does not make easy

**A live-sync fake has to answer the sync protocol**, not just hand back rows. A
collection reaches the network through `runtime.fetch` like everything else, so
the same fake transport intercepts it — but what it has to send back is an
offset, an `electric-handle`, an `electric-schema`, an `electric-up-to-date`, and
then an answer to each `live=true` long poll that follows. rig ships no helper
for this. `ts/packages/electric/src/fallback.test.ts` in the rig repository is a
worked example, and it is long enough to be a fair warning: a test that only
needs rows on screen is better served by faking the resource method above and
leaving the stream alone.

## Presence

A project with `presence: {enabled: true}` gets a third package,
`@rig-ts/presence`. It is not a generated one — no generator writes it — and it
mirrors the hand-written `/presence` routes the way `web/src/auth` mirrors
`/auth/*` in every rig front end.

```tsx
import { createPresence } from "@rig-ts/presence";
import { usePresence } from "@rig-ts/presence/react";

const presence = createPresence({
    runtime: client.runtime,
    scope: "board",
    stream: createPresenceStream(client.runtime, { scope: "board" }),
});
```

Create it **once**, above your router, and hand it down. Everything with a timer
in it lives inside — the heartbeat, the write throttle, the tick that ages rows
out, the visibility rule, the leave on teardown — so a second one is a second
heartbeat, and every heartbeat is a row change fanned out to everybody else in
the tenant.

Then two questions, from anywhere:

```tsx
presence.focus({ table: "todo", id, field: "title" }, "editing");
const others = usePresence(presence, { table: "todo", id });
```

`focus` is throttled and de-duplicated, so it is safe to call from a handler that
fires on every render — and it belongs on `onFocus`, never on `onChange`. Typing a
two-hundred-character title should be one presence write.

**It is the first rig package with side effects.** `@rig-ts/client` retries and
`@rig-ts/electric` maps; neither does anything until it is called. This one runs a
timer and listens on `window`, which is why it is a package of its own rather than
part of `@rig-ts/electric`: a project that streams a table but shows no presence
should install none of it.

`@rig-ts/presence/react` is a second entry point, three lines over
`useSyncExternalStore`, behind an optional `react` peer dependency. The core
exports `subscribe` and `others` in exactly that contract, so a binding for
another framework is the same size.

### One row, two spellings — again

The trap [above](#two-shapes-for-one-row) has a third instance, and this package
exists partly to hide it. A presence row arrives two ways: off the live shape it
carries Postgres column names (`seen_at`, `account_id`, `target_field`), and off
`GET /presence` it carries what the hand-written route's Go struct declares
(`seenAt`, `accountId`, `targetField`). Those routes answer camelCase whatever
`api.json_case` you set, for the reason `/auth/*` does — they are rig's, identical
in every project, and this package is compiled against them once.

`@rig-ts/presence` normalises both to camelCase at its boundary, so a `Person` means
one thing whichever door it came through.

### The parts this package cannot own

Where the handle is built, and where a spot ends, are decisions about your
component tree rather than about presence — and both have a wrong answer that
only shows up under a dev server. `examples/linearlite/web/src/presence` is the
worked version, and [presence.md](presence.md#three-things-the-package-deliberately-does-not-do-for-you)
says what each one is and why.

## See also

- [observability.md](observability.md) — the server end of the same trace
- [electric.md](electric.md) — the shapes a table gets, and how they are scoped
- [presence.md](presence.md) — what presence costs, why a subscriber decides who
  is here, and [the three things](presence.md#three-things-the-package-deliberately-does-not-do-for-you)
  `@rig-ts/presence` leaves to your component tree
- [generators.md](generators.md) — `go-client` and `ts-client` options
- [examples/sdk](../examples/sdk) — a program built on two generated clients
- [examples/linearlite](../examples/linearlite) — all three packages in one
  React front end

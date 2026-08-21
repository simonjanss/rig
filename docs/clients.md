# Clients

> **Not written yet.** This page will document the generated Go client — how to
> configure it, how it authenticates, and how it paginates.
>
> Until it exists, [examples/sdk](../examples/sdk) is a working program that
> calls two rig applications through their generated clients, and
> [examples/todo/client](../examples/todo/client) is what the generator emits.

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

## See also

- [observability.md](observability.md) — the server end of the same trace
- [generators.md](generators.md) — `go-client` options
- [examples/sdk](../examples/sdk) — a program built on two generated clients

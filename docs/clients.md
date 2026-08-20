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

Two calls have no reader. A read sends no body, so nothing about a `Get` can be
wrong per field. And a search's body is a filter — a question rather than
something filled in field by field — which nothing validates, so a reader for it
would be a function per resource that could only ever answer nil.

### The other half of the shape

A custom endpoint gets the same treatment, and its server half is generated
beside the body: `LessonPublishBodyError` in your API package has one member per
member of `LessonPublishBody`, and returning it from the service is what makes
`client.LessonPublishError(err)` answer with fields rather than with prose.
Nothing generated fills it in — only your service knows what its own body means
— which is why it comes with `Empty()` and no validator.

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

## See also

- [generators.md](generators.md) — `go-client` options
- [examples/sdk](../examples/sdk) — a program built on two generated clients

# Clients

> **Not written yet.** This page will document the generated Go client — how to
> configure it, how it authenticates, how it paginates, and what it does with
> errors.
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

## See also

- [generators.md](generators.md) — `go-client` options
- [examples/sdk](../examples/sdk) — a program built on two generated clients

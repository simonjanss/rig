package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

const idempotencyModule = runtimeModule + "/idempotency"

// fingerprintOf renders the expression a write is identified by, beyond its key
// and its route.
//
// The path is in it, and has to be: the endpoint recorded against a key is the
// route pattern rather than the path, so without this one key used against
// `POST /todos/{id}/cover-file` for two different todos would replay the first
// one's answer at the second.
//
// body is left out for an upload, whose body is a reader over bytes that may be
// larger than memory. Hashing those would mean buffering them, which is the one
// thing the whole file path exists to avoid — so two uploads to the same row
// under one key have the same fingerprint, and a client that meant them to be
// two different uploads has reused a key it should not have.
func fingerprintOf(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, e *emitter) string {
	part := func(name string, present bool) string {
		if present {
			return name
		}
		return "struct{}{}"
	}

	hasBody := e.bodyTypeOf(b, res, ep) != "" && ep.File == nil
	return "[]any{" +
		part("path", len(ep.Request.PathParams) > 0) + ", " +
		part("query", len(ep.Request.QueryParams) > 0) + ", " +
		part("body", hasBody) + "}"
}

// idempotencyFields emits the members of the literal identifying this write.
//
// The braces belong to the caller, because the opening one has to share a line
// with the call it is an argument to: a composite literal that starts its own
// line ends one too, and Go's semicolon goes in after the closing brace and
// splits the argument list in half.
func (e *emitter) idempotencyFields(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	idem := b.Import(idempotencyModule)

	b.L("TenantID: claims.TenantID,")
	b.L("Key: r.Header.Get(\"Idempotency-Key\"),")
	b.Comment("The route pattern rather than the path, so the same key against " +
		"the same endpoint is one record however many rows it names. What the " +
		"path said is in the fingerprint.")
	b.L("Endpoint: rc.Route,")
	b.L("Fingerprint: %s.Fingerprint(%s),", idem, fingerprintOf(b, res, ep, e))
	b.L("RequestID: rc.RequestID,")
}

// guardedCall emits a write wrapped in the idempotency record of it.
//
// The service call goes inside the closure, which is what puts it in the same
// transaction as the record: a claim stored separately from the write it claims
// can outlive a rolled-back write, and then a key that wrote nothing replays a
// success forever.
//
// With no header the whole thing costs one comparison — see
// [github.com/simonjanss/rig/runtime/idempotency.Run] — so this is emitted for
// every write rather than for the ones somebody remembered to configure.
func (e *emitter) guardedCall(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, extra string) {
	var (
		httpPkg = b.Import("net/http")
		ctxPkg  = b.Import("context")
		idem    = b.Import(idempotencyModule)
	)

	status := statusExpr(httpPkg, successStatus(ep))

	b.L("result, err := %s.Run(ctx, s.DB, %s.Request{", idem, idem)
	e.idempotencyFields(b, res, ep)
	b.L("}, func(ctx %s.Context) (int, any, error) {", ctxPkg)
	if successBodyObject(ep) == "" {
		b.L("return %s, nil, svc.%s(ctx, req%s)", status, ep.Impl.ServiceMethod, extra)
	} else {
		b.L("out, err := svc.%s(ctx, req%s)", ep.Impl.ServiceMethod, extra)
		b.L("return %s, out, err", status)
	}
	b.L("})")
	b.L("if err != nil { fail(s, w, r, rc, err); return }")
	b.L("writeResult(w, result)")
}

package servergo

import (
	"net/http"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

const idempotencyModule = runtimeModule + "/idempotency"

// idempotentWrite reports whether an endpoint's work is worth recording against
// an Idempotency-Key.
//
// The methods that can produce a second row if a client sends the same request
// twice, and only those. A GET has nothing to record. A DELETE is idempotent in
// what it leaves behind — its second answer is a 404 about a row that is
// genuinely gone — so buying a smoother answer for it would cost a transaction
// per delete to change an error nobody was wrong to receive.
//
// A search is a QUERY here even where it is also mounted as a POST alias, so it
// is excluded by the same switch that excludes reads. The alias is a second
// route to one handler, and the handler is generated from the operation.
//
// A dedicated upload route never reaches this: it takes a branch of its own in
// [emitter.handler], and [emitter.uploadHandler] says why it is not guarded.
func idempotentWrite(ep *ir.Endpoint) bool {
	switch ep.Method {
	case http.MethodPost, http.MethodPatch, http.MethodPut:
		return true
	}
	return false
}

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

// idempotencyPrunerFunc emits the retention task.
//
// It is generated for every project rather than only for the ones that ask,
// because the record it deletes is written for every project: the header is what
// engages it, and no configuration turns it off.
func (e *emitter) idempotencyPrunerFunc(b *gobuf.Buf) {
	var (
		ctxPkg   = b.Import("context")
		timePkg  = b.Import("time")
		poolPkg  = b.Import("github.com/jackc/pgx/v5/pgxpool")
		servePkg = b.Import(runtimeModule + "/serve")
		idem     = b.Import(idempotencyModule)
	)

	b.Comment("IdempotencyPruner deletes the records of writes past their " +
		"retention, and so decides how long after a write its Idempotency-Key " +
		"still replays.\n\n" +
		"Zero takes idempotency.DefaultRetention, a day: long enough to cover any " +
		"retry, short enough that a key reused a week later is a new request " +
		"rather than a write that silently does nothing.\n\n" +
		"A task rather than a goroutine, for the reason FileSweeper is one — a " +
		"cron job is one thing running, and a goroutine in every replica is as " +
		"many as there are replicas, all racing to delete the same rows. Register " +
		"it in serve.Config.Tasks and run `<binary> prune-idempotency`:\n\n" +
		"\tTasks: map[string]serve.Task{\"prune-idempotency\": api.IdempotencyPruner(0)},\n\n" +
		"Nothing schedules it for you. Without it rig_idempotency keeps every " +
		"record ever written, and the retention above is a sentence rather than a " +
		"behaviour.")
	b.L("func IdempotencyPruner(retention %s.Duration) %s.Task {", timePkg, servePkg)
	b.L("return func(ctx %s.Context, pool *%s.Pool) error {", ctxPkg, poolPkg)
	b.L("_, err := %s.Prune(ctx, pool, retention)", idem)
	b.L("return err")
	b.L("}")
	b.L("}")
	b.NL()
}

// writeResultFunc emits the one place a guarded write's answer reaches the wire.
func (e *emitter) writeResultFunc(b *gobuf.Buf) {
	var (
		httpPkg = b.Import("net/http")
		idem    = b.Import(idempotencyModule)
	)

	b.Comment("writeResult answers with what a write produced, or with what it " +
		"produced the first time somebody sent it.\n\n" +
		"The bytes are written rather than re-encoded, because a replay that " +
		"round-tripped through a decoder would come back with its keys in " +
		"whatever order that decoder chose — and a client that hashes or signs " +
		"what it received would see two different answers to one request.")
	b.L("func writeResult(w %s.ResponseWriter, res %s.Result) {", httpPkg, idem)
	b.L("if res.Replayed {")
	b.Comment("Nothing a client must act on, and worth saying: it is the " +
		"difference between a write that happened just now and one that " +
		"happened the first time this key was seen.")
	b.L("w.Header().Set(\"Idempotency-Replayed\", \"true\")")
	b.L("}")
	b.L("if len(res.Body) == 0 {")
	b.L("w.WriteHeader(res.Status)")
	b.L("return")
	b.L("}")
	b.L("w.Header().Set(\"Content-Type\", \"application/json; charset=utf-8\")")
	b.L("w.WriteHeader(res.Status)")
	b.L("_, _ = w.Write(res.Body)")
	b.L("}")
	b.NL()
}

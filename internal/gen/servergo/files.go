package servergo

import (
	"fmt"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The two modules a project with file columns imports. Neither appears in a
// project without one, which is what keeps examples/auth free of a multipart
// reader it never calls.
const (
	filesModule    = "github.com/simonjanss/rig/files"
	filehttpModule = "github.com/simonjanss/rig/files/filehttp"
)

// Delete is deliberately not one of the shapes here. Clearing a column and
// answering 204 is exactly what the generic handler already does, and giving it
// a special case would be three more branches saying the same thing.

// uploadHandler emits the multipart upload.
//
// The deadline is set on this request rather than on the server, because
// ReadTimeout is set once on the one http.Server and raising it for a
// two-hundred-megabyte upload weakens every other route in the application.
//
// This is the one write that is not wrapped in an idempotency record, and the
// reason is the body. OnePart hands back the form part itself, so the bytes are
// still on the wire when the service is called — putting that call inside a
// transaction would hold a pooled connection open for the whole transfer, up to
// filehttp.DefaultDeadline. A handful of slow uploads would then be the whole
// pool, and every other route would be waiting on them.
//
// files.Service already draws this line: Attach stores the bytes and only then
// opens its transaction, and the multipart create takes the same order — Prepare
// runs before the guarded call, not inside it. A dedicated upload route has no
// equivalent seam, because the service method takes the reader.
//
// What follows from it is that the SDK does not repeat a form body either — see
// rigclient.Op.writes.
func (e *emitter) uploadHandler(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	var (
		httpPkg  = b.Import("net/http")
		filehttp = b.Import(filehttpModule)
	)

	b.L("%s.Deadline(w, %s.DefaultDeadline)", filehttp, filehttp)
	b.L("body, err := %s.OnePart(r, %s)", filehttp, gobuf.Quote(ep.Request.FileParts[0].Name))
	b.L("if err != nil { fail(s, w, r, rc, err); return }")
	b.NL()

	b.L("req := NewRequest(claims, path, struct{}{}, body, rc)")
	b.L("out, err := svc.%s(ctx, req)", ep.Impl.ServiceMethod)
	b.L("if err != nil { fail(s, w, r, rc, err); return }")
	b.L("writeJSON(w, %s, out)", statusExpr(httpPkg, successStatus(ep)))
}

// downloadHandler emits the streaming read.
//
// The body is the store's reader and is closed here, because nothing else can:
// the service hands it back open on purpose, so that a file larger than memory
// goes straight to the response.
func (e *emitter) downloadHandler(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	filehttp := b.Import(filehttpModule)

	b.L("%s.Deadline(w, %s.DefaultDeadline)", filehttp, filehttp)
	b.L("req := NewRequest(claims, path, struct{}{}, struct{}{}, rc)")
	b.NL()

	b.L("content, err := svc.%s(ctx, req)", ep.Impl.ServiceMethod)
	b.L("if err != nil { fail(s, w, r, rc, err); return }")
	b.L("defer content.Body.Close()")
	b.NL()

	b.Comment("ServeContent from here, so a range request and a conditional " +
		"request are answered rather than ignored: a resumed download does not " +
		"start over and a media element can seek.")
	b.L("%s.Serve(w, r, content, svc.Files().InlineTypes())", filehttp)
}

// multipartCreate emits the create's second body, for a resource with a file
// column.
//
// The parts are consumed in the order they arrive and each is handed to the file
// service before the reader moves on, because a part's body is only valid until
// the next one is asked for. That is also why this cannot be a decode step
// followed by an upload step: there is no point at which every part is in hand.
//
// A request with no multipart content type never reaches any of this and takes
// the JSON path byte for byte.
func (e *emitter) multipartCreate(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	var (
		errorsPkg = b.Import("errors")
		ioPkg     = b.Import("io")
		filesPkg  = b.Import(filesModule)
		filehttp  = b.Import(filehttpModule)
	)

	b.L("var pending []*%s.Pending", filesPkg)
	b.L("if %s.IsMultipart(r) {", filehttp)
	b.L("%s.Deadline(w, %s.DefaultDeadline)", filehttp, filehttp)
	b.L("form, err := %s.ReadForm(r)", filehttp)
	b.L("if err != nil { fail(s, w, r, rc, err); return }")
	b.NL()

	b.L("for {")
	b.L("part, err := form.Next()")
	b.L("if %s.Is(err, %s.EOF) { break }", errorsPkg, ioPkg)
	b.L("if err != nil { fail(s, w, r, rc, err); return }")
	b.NL()
	b.L("switch part.Name {")

	b.L("case %s.JSONPart:", filehttp)
	b.Comment("The same body the JSON form carries, through the same decoder, " +
		"so an unknown key is refused here exactly as it is there and a 422 " +
		"comes back with the same field errors.")
	b.L("if err := decodeReader(part.Body, &body); err != nil { fail(s, w, r, rc, err); return }")

	for _, part := range ep.Request.FileParts {
		b.L("case %s:", gobuf.Quote(part.Name))
		b.L("p, err := svc.Files().Prepare(ctx, part.Name, %s.AttachRequest{", filesPkg)
		b.L("TenantID: claims.TenantID,")
		b.L("Upload: part.Upload(),")
		b.L("})")
		b.L("if err != nil { fail(s, w, r, rc, err); return }")
		b.L("pending = append(pending, p)")
	}

	b.L("default:")
	b.Comment("Refused rather than skipped, for the reason an unknown JSON key " +
		"is refused: a client that misspelled a part name has uploaded a file " +
		"into nowhere, and a 201 would tell it that worked.")
	b.L("fail(s, w, r, rc, %s.ErrUnknownPart(part.Name))", filehttp)
	b.L("return")
	b.L("}")
	b.L("}")
	b.NL()

	// A not-null file column with no part is the whole reason the multipart
	// create exists, so it fails as a field error rather than as a constraint
	// violation nobody can read.
	for _, part := range ep.Request.FileParts {
		if !part.Required {
			continue
		}
		b.L("if !hasPart(pending, %s) {", gobuf.Quote(part.Name))
		b.L("fail(s, w, r, rc, %s.ErrMissingPart(%s))", filehttp, gobuf.Quote(part.Name))
		b.L("return")
		b.L("}")
	}

	b.L("} else if err := decodeBody(r, &body); err != nil {")
	b.L("fail(s, w, r, rc, err)")
	b.L("return")
	b.L("}")
	b.NL()
}

// hasPartHelper emits the one predicate the multipart create needs, once per
// package rather than inline in every create that has a required file.
func (e *emitter) hasPartHelper(b *gobuf.Buf) {
	filesPkg := b.Import(filesModule)

	b.Comment("hasPart reports whether a form carried the named file.")
	b.L("func hasPart(pending []*%s.Pending, name string) bool {", filesPkg)
	b.L("for _, p := range pending {")
	b.L("if p.Part == name { return true }")
	b.L("}")
	b.L("return false")
	b.L("}")
	b.NL()
}

// anyRequiredFilePart reports whether any create has a not-null file column, so
// the shared predicate and the import it brings are emitted only where they are
// called.
func (e *emitter) anyRequiredFilePart() bool {
	for i := range e.doc.API.Resources {
		for _, f := range e.doc.API.Resources[i].Files {
			if f.Required {
				return true
			}
		}
	}
	return false
}

// hasFiles reports whether this project stores uploads at all.
//
// Without the block there is no files.gen.go, no blob store and no multipart
// reader — and nothing in [emitter.tasksFunc] for an operator to sweep.
func (e *emitter) hasFiles() bool {
	return e.doc.API.Files != nil && e.doc.API.Files.Enabled
}

// filesFile emits the wiring for a project that accepts uploads.
//
// It is written here rather than by a generator of its own for the reason the
// authentication wiring is: it belongs to this package, and a project without a
// `files:` block gets no file at all — which is what keeps an application that
// serves a list of chores free of a multipart reader and a blob store.
//
// Everything in it comes from the resolved block, so a byte cap or a sweep
// interval is a line in rig.yaml that the generated documentation can quote,
// rather than a literal in a main function nobody diffs.
func (e *emitter) filesFile() (gen.Artifact, error) {
	cfgIR := e.doc.API.Files
	if cfgIR.Backend != backendMemory {
		// The adapter is a module of its own and has not shipped. Refusing here
		// is the honest failure: the alternative is an application that names a
		// bucket, starts cleanly, and keeps every upload in a map a restart
		// empties.
		return gen.Artifact{}, fmt.Errorf(
			"files.backend is %q, and only %q has shipped: the S3 adapter is a module of its own",
			cfgIR.Backend, backendMemory)
	}

	b := gobuf.New(e.cfg.Package)

	var (
		ctxPkg   = b.Import("context")
		timePkg  = b.Import("time")
		filesPkg = b.Import(filesModule)
		blobPkg  = b.Import(filesModule + "/blob")
		servePkg = b.Import(runtimeModule + "/serve")
		poolPkg  = b.Import("github.com/jackc/pgx/v5/pgxpool")
	)

	b.Comment("NewFiles builds this project's file handling from its `files:` block.\n\n" +
		"The pool is the same one the repositories use, and that is not a detail: " +
		"the transaction that finalizes a file and writes the row pointing at it " +
		"has to be one transaction, and it cannot be if the two live in different " +
		"pools.")
	b.L("func NewFiles(db %s.DB) *%s.Service {", filesPkg, filesPkg)
	b.L("return %s.New(%s.Config{", filesPkg, filesPkg)
	b.L("Store: %s.NewMemory(),", blobPkg)
	b.L("DB: db,")
	b.L("MaxBytes: %d,", cfgIR.MaxBytes)
	b.L("InlineTypes: []string{%s},", quoteAll(cfgIR.InlineTypes))
	b.L("AbandonedAfter: %d * %s.Second,", cfgIR.AbandonedAfterSeconds, timePkg)
	b.L("RestoreWindow: %d * %s.Second,", cfgIR.RestoreWindowSeconds, timePkg)
	b.L("})")
	b.L("}")
	b.NL()

	b.Comment("FileSweeper is the housekeeping task: abandoned uploads, and trash " +
		"past the restore window.\n\n" +
		"A task rather than a goroutine, so it is a subcommand in a cron job " +
		"rather than something racing itself in every replica. Register it in " +
		"serve.Config.Tasks and run `<binary> sweep-files`.")
	b.L("func FileSweeper(svc *%s.Service) %s.Task {", filesPkg, servePkg)
	b.L("return func(ctx %s.Context, _ *%s.Pool) error {", ctxPkg, poolPkg)
	b.L("_, err := svc.Sweep(ctx)")
	b.L("return err")
	b.L("}")
	b.L("}")
	b.NL()

	return artifact("files.gen.go", b)
}

// backendMemory is the only backend that has shipped. It is spelled here rather
// than imported from the project package because a generator reads the document
// and not the configuration.
const backendMemory = "memory"

// quoteAll renders a list of strings as Go literals.
func quoteAll(values []string) string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, gobuf.Quote(v))
	}
	return strings.Join(out, ", ")
}

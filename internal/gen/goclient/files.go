package goclient

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// The day Expand synthesizes a multipart endpoint, a client generator that knows
// nothing about forms emits a JSON-only method for it: something that compiles,
// calls the right route, and sends the wrong thing. So the SDK is half of this
// feature rather than a milestone after it.

// fileMethod emits an upload or a download, which are the two the ordinary
// method shape cannot express.
//
// A delete is not one of them: it is a DELETE with a path and no body, which is
// what every other delete already is.
func (e *emitter) fileMethod(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, rig string) bool {
	switch {
	case ep.File != nil && ep.Method == "POST":
		e.uploadMethod(b, res, ep, rig)
		return true
	case ep.File != nil && ep.Method == "GET":
		e.downloadMethod(b, res, ep, rig)
		return true
	default:
		return false
	}
}

// uploadMethod sends one file as a form.
//
// The form is assembled by the transport rather than here, which is the same
// line this generator already draws for path escaping: the method knows which
// argument goes where, and it does not know what a boundary is.
func (e *emitter) uploadMethod(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, rig string) {
	sig := e.signature(b, res, ep, rig)
	part := ep.Request.FileParts[0]

	params := strings.Replace(sig.params, ", opts ..."+rig+".CallOption",
		", file "+rig+".Upload, opts ..."+rig+".CallOption", 1)

	e.methodDoc(b, res, ep)
	b.Comment("The default client times the whole exchange out after thirty " +
		"seconds, which is right for a JSON call and wrong for anything moving a " +
		"file — and a context deadline cannot raise it, because http.Client.Timeout " +
		"is a ceiling rather than a default. Bound this call instead:\n\n" +
		"\trigclient.WithTimeout(10 * time.Minute)\n\n" +
		"Nothing is buffered: the form is written to a pipe as the request goes " +
		"out, so a file larger than memory is an ordinary upload. Two consequences " +
		"follow. The request is chunked, because the length of the form is not " +
		"known when the headers are written. And a body that cannot seek cannot be " +
		"retried — if the call comes back 401 and the credential is refreshed, the " +
		"second send needs the bytes again, and rather than buffering every upload " +
		"against a retry that almost never happens the SDK answers " +
		"rigclient.ErrCannotRetry.")
	b.L("func (c *%sClient) %s(%s) %s {", res.Name, ep.Impl.ServiceMethod, params, sig.results)

	b.L("op := %s.Op{", rig)
	b.L("Method: %s,", methodExpr(b, ep, rig))
	b.L("Path: %s,", sig.path)
	b.L("Multipart: &%s.Multipart{Files: []%s.Upload{%s.Part(%s, file)}},",
		rig, rig, rig, gobuf.Quote(part.Name))
	b.L("}")
	b.L("return %s.Do[%s](ctx, c.rt, op, opts...)", rig, sig.returns)
	b.L("}")
	b.NL()
}

// downloadMethod reads the bytes back.
//
// It answers a *rigclient.Content, which is the response and not a copy of it.
func (e *emitter) downloadMethod(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, rig string) {
	sig := e.signature(b, res, ep, rig)

	e.methodDoc(b, res, ep)
	b.Comment("**Close the body.** Nothing reads ahead, which is what lets a file " +
		"larger than memory go straight to disk — and it means the connection is " +
		"held until you are done with it. It is the one thing this method cannot " +
		"do for you, which is why it is said here even though the document does " +
		"not say it.\n\n" +
		"ContentType is the type the server sniffed at upload, Length is the size " +
		"or -1, and Filename is the name from Content-Disposition. That last one " +
		"is a suggestion and not a path: if you write it to disk, you decide where " +
		"and you sanitize what.\n\n" +
		"rigclient.WithRange and rigclient.WithIfNoneMatch get you a 206 and a " +
		"304, both of which arrive as Content.Status rather than as an error — you " +
		"asked the question, so the answer is a result.")
	b.L("func (c *%sClient) %s(%s) (*%s.Content, error) {",
		res.Name, ep.Impl.ServiceMethod, sig.params, rig)

	b.L("op := %s.Op{", rig)
	b.L("Method: %s,", methodExpr(b, ep, rig))
	b.L("Path: %s,", sig.path)
	b.Comment("A download is whatever the file turned out to be, and the " +
		"document cannot know that at generation time.")
	b.L("Accept: \"*/*\",")
	b.L("}")
	b.L("return %s.DoContent(ctx, c.rt, op, opts...)", rig)
	b.L("}")
	b.NL()
}

// createFilesType emits the struct a multipart create takes its files in.
//
// One member per file column, a plain Upload where the column is not null and a
// pointer where it is — so the one thing this method exists for is a compile
// error rather than a 422.
func (e *emitter) createFilesType(b *gobuf.Buf, res *ir.Resource, rig string) {
	name := res.Name + "CreateFiles"

	b.Comment(name + " is the files a create carries with it.\n\n" +
		"A not-null file column is unreachable without this: the row would have to " +
		"exist before an upload had anywhere to go. Such a column is a plain " +
		"Upload here and a nullable one is a pointer, so leaving out a file the " +
		"schema requires does not compile.")
	b.L("type %s struct {", name)
	for i, fc := range res.Files {
		if i > 0 {
			b.NL()
		}
		if fc.Required {
			b.L("%s %s.Upload", fc.GoName(), rig)
			continue
		}
		b.L("%s *%s.Upload", fc.GoName(), rig)
	}
	b.L("}")
	b.NL()
}

// createWithFilesMethod emits the create that carries its files.
//
// Beside Create rather than instead of it. Create is the most-called method rig
// emits and the server's change to it is additive byte for byte; adding a
// parameter to it is not additive, and would break every existing caller the day
// somebody adds a file column to a table they already had. A variadic
// `files ...Upload` is source-compatible and worse: it puts two wire shapes
// behind one call site, so which one went out becomes a runtime property of a
// slice's length.
func (e *emitter) createWithFilesMethod(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, rig string) {
	sig := e.signature(b, res, ep, rig)

	params := strings.Replace(sig.params, ", opts ..."+rig+".CallOption",
		", files "+res.Name+"CreateFiles, opts ..."+rig+".CallOption", 1)

	b.Comment("CreateWithFiles creates a " + res.Name + " and its files in one " +
		"request.\n\n" +
		"The row and the bytes are committed together, so a create that fails " +
		"leaves neither. The JSON body travels as a part named \"json\", which is " +
		"the same body Create sends and goes through the same validation — a 422 " +
		"comes back as the same [" + errorTypeName(res, ep) + "].\n\n" +
		"Bound the call: the default thirty-second client timeout covers the whole " +
		"exchange, and this one carries files.")
	b.L("func (c *%sClient) CreateWithFiles(%s) %s {", res.Name, params, sig.results)

	b.L("op := %s.Op{", rig)
	b.L("Method: %s,", methodExpr(b, ep, rig))
	b.L("Path: %s,", sig.path)
	b.L("Multipart: &%s.Multipart{", rig)
	b.Comment("The JSON goes first, and the transport writes it first, because " +
		"the server reads the parts in order and wants the row before the bytes.")
	b.L("JSON: %s,", sig.body)
	b.L("Files: []%s.Upload{", rig)
	b.L("},")
	b.L("},")
	b.L("}")
	b.NL()

	for _, fc := range res.Files {
		if fc.Required {
			b.L("op.Multipart.Files = append(op.Multipart.Files, %s.Part(%s, files.%s))",
				rig, gobuf.Quote(fc.Part), fc.GoName())
			continue
		}
		b.L("if files.%s != nil {", fc.GoName())
		b.L("op.Multipart.Files = append(op.Multipart.Files, %s.Part(%s, *files.%s))",
			rig, gobuf.Quote(fc.Part), fc.GoName())
		b.L("}")
	}
	b.NL()

	b.L("return %s.DoTyped[%s, %s](ctx, c.rt, op, opts...)", rig, sig.returns, sig.fields)
	b.L("}")
	b.NL()
}

package tsclient

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/tsbuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// The three shapes a file column adds to a resource's client, and the reason
// they are here rather than left to the ordinary method shape.
//
// It is the reason [github.com/simonjanss/rig/internal/gen/goclient] states in
// its own files.go, and it bites the same way in either language: a client
// generator that knows nothing about forms emits a JSON-only method for an
// upload — something that compiles, calls the right route, and sends nothing at
// all. The route is right and the request is empty, so the failure arrives as a
// 422 about a part nobody sent rather than as anything a reader would connect to
// the method they called.
//
// A delete is not one of them: it is a DELETE with a path and no body, which is
// what every other delete already is. A download is not either — it is an
// ordinary GET whose response is handed over unread, and [emitter.isDownload]
// settles that from the declared media types.

// methodVariant says which of an endpoint's methods is being emitted.
//
// Most endpoints have exactly one. A create whose table has file columns has
// two, because the multipart create is emitted beside the JSON one rather than
// instead of it.
type methodVariant int

const (
	// variantPlain is a JSON call: the shape every endpoint without a form takes.
	variantPlain methodVariant = iota
	// variantUpload is the POST that sends one file to an existing row.
	variantUpload
	// variantCreateWithFiles is the create that carries its files with it.
	variantCreateWithFiles
)

// variantFor is the method an endpoint gets by default.
func variantFor(ep *ir.Endpoint) methodVariant {
	if ep.File != nil && ep.Method == "POST" && len(ep.Request.FileParts) > 0 {
		return variantUpload
	}
	return variantPlain
}

// createWithFiles is the create endpoint that carries its files, or nil for a
// resource with no file columns.
func createWithFiles(res *ir.Resource) *ir.Endpoint {
	ep := res.Endpoint(ir.OpCreate)
	if ep == nil || len(ep.Request.FileParts) == 0 {
		return nil
	}
	return ep
}

// createFilesTypeName is the shape a multipart create takes its files in.
func createFilesTypeName(res *ir.Resource) string { return res.Name + "CreateFiles" }

// filePartMember is what one file is called on that shape.
//
// Built from the column's field rather than from the part's name on the wire,
// which is the rule [ir.FilePart] states: a member named after the part reads
// nothing like the row it belongs to. rig's convention makes the two the same
// string, so this is a decision about which of them is the source and not about
// what the output says.
func filePartMember(p ir.FilePart) string {
	return lowerFirst(strings.TrimSuffix(p.Field, "ID"))
}

// formBody is the multipart body one method sends.
type formBody struct {
	// row is the expression for the `json` part, or "undefined" for an upload
	// route: it binds bytes to a row that already exists, so there is nothing to
	// send beside them and a `json` part nobody reads is a part the server has to
	// skip.
	row string
	// parts is one entry per file the method takes, in the order the document
	// declares them.
	parts []formPart
}

// formPart is one file in that body.
type formPart struct {
	// name is the part's name on the wire, which is what the server binds the
	// bytes to. A client that spells it differently has uploaded a part nobody
	// claimed.
	name string
	// value is the expression holding the upload.
	value string
}

// uploadForm is the body a single-file upload sends.
func uploadForm(ep *ir.Endpoint) *formBody {
	return &formBody{
		row:   "undefined",
		parts: []formPart{{name: ep.Request.FileParts[0].Name, value: "file"}},
	}
}

// requiredFileMembers are the json-part members a multipart create does not
// take, quoted for an Omit: the identifiers of the NOT NULL file columns, whose
// values the server assigns from the parts beside the row. Looked up on the
// resource's fields rather than derived from the Go field name, because the
// wire key is the one thing this must not misspell.
func requiredFileMembers(res *ir.Resource, ep *ir.Endpoint) []string {
	var out []string
	for _, p := range ep.Request.FileParts {
		if !p.Required {
			continue
		}
		for i := range res.Fields {
			if res.Fields[i].Name == p.Field {
				out = append(out, tsbuf.Quote(res.Fields[i].Wire))
				break
			}
		}
	}
	return out
}

// createForm is the body a multipart create sends: the row, and a part per file
// column.
//
// Every column is named unconditionally, absent or not. `multipart` leaves out
// an upload it was not given, so what makes a required file impossible to leave
// out is the shape it arrives in rather than a branch here — a compile error
// instead of a 422.
func createForm(ep *ir.Endpoint) *formBody {
	parts := make([]formPart, 0, len(ep.Request.FileParts))
	for _, p := range ep.Request.FileParts {
		parts = append(parts, formPart{
			name:  p.Name,
			value: "files." + tsbuf.Key(filePartMember(p)),
		})
	}
	return &formBody{row: "input", parts: parts}
}

// formPreamble writes the multipart body a method sends.
//
// A local rather than an expression inside the op literal, because a create
// names one part per file column and two of them do not fit on a line.
func (e *emitter) formPreamble(b *tsbuf.Buf, form *formBody) {
	multipart := b.Import(e.cfg.ClientImport, "multipart")

	if len(form.parts) == 1 {
		b.L("const form = %s(%s, [[%s, %s]]);", multipart, form.row,
			tsbuf.Quote(form.parts[0].name), form.parts[0].value)
		b.NL()
		return
	}

	b.L("const form = %s(%s, [", multipart, form.row)
	b.Indent()
	for _, p := range form.parts {
		b.L("[%s, %s],", tsbuf.Quote(p.name), p.value)
	}
	b.Outdent()
	b.L("]);")
	b.NL()
}

// createFilesType emits the shape a multipart create takes its files in.
func (e *emitter) createFilesType(b *tsbuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := createFilesTypeName(res)
	upload := b.ImportType(e.cfg.ClientImport, "Upload")

	b.Comment(name + " is the files a create carries with it.\n\n" +
		"A not-null file column is unreachable without this: the row would have " +
		"to exist before an upload had anywhere to go, and it cannot exist " +
		"without one. So a required column is a plain member here and a nullable " +
		"one is optional, and leaving out a file the schema insists on does not " +
		"compile rather than coming back a 422.")
	b.L("export type %s = {", name)
	b.Indent()
	for _, p := range ep.Request.FileParts {
		optional := "?"
		if p.Required {
			optional = ""
		}
		b.L("%s%s: %s;", tsbuf.Key(filePartMember(p)), optional, upload)
	}
	b.Outdent()
	b.L("};")
	b.NL()
}

// uploadDoc is what an upload says beyond what the document says about it.
func uploadDoc(ep *ir.Endpoint) string {
	return "The file travels as the part named `" + ep.Request.FileParts[0].Name +
		"`, which is the one this route accepts. `contentType` on the upload is " +
		"a claim and not the answer: the server sniffs the bytes, and the sniffed " +
		"type is what it stores and what a download announces.\n\n" +
		"**This call is never sent twice.** A rig server records no upload " +
		"against an idempotency key — its body is still arriving when the service " +
		"is called — so a second send would store the file again. A failure here " +
		"comes back as it happened, and retrying is the caller's decision because " +
		"only the caller still has the bytes."
}

// createWithFilesDoc is what the multipart create says about itself.
func createWithFilesDoc(res *ir.Resource, ep *ir.Endpoint) string {
	doc := "Creates a " + res.Name + " and its files in one request.\n\n" +
		"The row and the bytes are committed together, so a create that fails " +
		"leaves neither. The row travels as a part named `json` — the same body " +
		"`" + methodName(ep) + "` sends, through the same validation"

	if name := guardName(res, ep); name != "" {
		doc += ", so a 422 comes back with the same field errors and is read back " +
			"with the same `" + name + "`"
	}
	doc += ".\n\n" + strings.ReplaceAll(ep.Pattern, "  ", " ") +
		"\n\nOperation " + ep.OperationID + ".\n\n" +
		"Beside `" + methodName(ep) + "` rather than instead of it: the server's " +
		"change is additive, and adding a parameter to the most-called method a " +
		"client has is not. **This call is never sent twice**, for the reason an " +
		"upload is not — a form is the one write no idempotency key names."
	return doc
}

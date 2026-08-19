package servicego

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// The modules a project with file columns imports. A project without one gets
// neither, which is what keeps an application that serves a list of chores free
// of a multipart reader.
const filesModule = "github.com/simonjanss/rig/files"

// hasFiles reports whether this resource has any file column.
func hasFiles(res *ir.Resource) bool { return len(res.Files) > 0 }

// fileColumnOf is the column an endpoint acts on, as the resource declares it.
func fileColumnOf(res *ir.Resource, ep *ir.Endpoint) ir.FileColumn {
	for _, fc := range res.Files {
		if ep.File != nil && fc.Column == ep.File.Column {
			return fc
		}
	}
	if ep.File != nil {
		return *ep.File
	}
	return ir.FileColumn{}
}

// fileServiceField emits the handle on the file service and the accessor the
// generated handler reads it through.
//
// The service holds it rather than the handler, because uploading goes through
// the service like every other write: refusing a content type, capping a size or
// starting a derivative are hooks, and none of them can exist if the bytes never
// pass through here.
func (e *emitter) fileServiceField(b *gobuf.Buf, res *ir.Resource) {
	filesPkg := b.Import(filesModule)

	b.Comment("Files is the file service this resource's uploads go through.\n\n" +
		"It is on the service rather than reached for from the handler so that " +
		"nothing about a file is a reason to abandon the generated service and " +
		"hand-write a route.")
	b.L("func (s Default%sService) Files() *%s.Service { return s.files }", res.Name, filesPkg)
	b.NL()
}

// ownerLiteral renders the row a file hangs off.
//
// The table and column names are constants here, written from the document. That
// is what makes it safe for the files module to build a statement around them:
// nothing a request carries reaches the string.
func (e *emitter) ownerLiteral(b *gobuf.Buf, res *ir.Resource, fc ir.FileColumn, id string) string {
	filesPkg := b.Import(filesModule)

	tenant := ""
	if res.Storage.Tenant != nil {
		tenant = ", TenantColumn: " + gobuf.Quote(res.Storage.Tenant.Name)
	}
	pk := "id"
	if len(res.Storage.PrimaryKey) > 0 {
		pk = res.Storage.PrimaryKey[0]
	}

	return filesPkg + ".Owner{Table: " + gobuf.Quote(res.Storage.Table) +
		", IDColumn: " + gobuf.Quote(pk) + tenant +
		", FileColumn: " + gobuf.Quote(fc.Column) + ", ID: " + id + "}"
}

// fileURLHelper emits the function that renders a file's stable URL.
//
// The URL is written onto the row at upload time and then synced, which is the
// whole reason a client can render a file without asking where it is. Two costs
// come with that and both are deliberate: it is denormalized routing, so
// renaming a path segment is a backfill migration; and it is not a capability,
// because the URL is unsigned and the endpoint behind it still authorizes.
func (e *emitter) fileURLHelper(b *gobuf.Buf, res *ir.Resource, fc ir.FileColumn) {
	var (
		uuidPkg = b.Import("github.com/google/uuid")
		urlPkg  = b.Import("net/url")
	)

	// The download route with its wildcards replaced by the values. Taken from
	// the endpoint's own pattern, so a base path or a renamed segment moves this
	// with it.
	ep := res.Endpoint("Download" + fc.GoName())
	if ep == nil {
		return
	}
	prefix, middle := splitDownloadPattern(ep.Pattern)

	b.Comment(fileURLName(res, fc) + " is where a " + res.Name + "'s " +
		strings.ToLower(fc.Role) + " file is served from.\n\n" +
		"Stable and unsigned, so it is safe to store on the row and to sync: " +
		"holding it grants nothing, because the endpoint behind it still checks " +
		"the caller. Downloads flow through the API rather than through a signed " +
		"storage URL for exactly that reason, and the honest cost is bandwidth " +
		"through the application.")
	b.L("func %s(ownerID, fileID %s.UUID, name string) string {", fileURLName(res, fc), uuidPkg)
	b.L("return %s + ownerID.String() + %s + fileID.String() + \"/\" + %s.PathEscape(name)",
		gobuf.Quote(prefix), gobuf.Quote(middle), urlPkg)
	b.L("}")
	b.NL()
}

func fileURLName(res *ir.Resource, fc ir.FileColumn) string {
	return e2Lower(res.Name) + fc.GoName() + "URL"
}

// e2Lower lowercases the first letter, so the helper is unexported.
func e2Lower(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// splitDownloadPattern cuts the route into the two literal pieces the owner and
// file identifiers sit between.
//
// "GET /api/v1/todos/{id}/cover-file/{fileId}/{filename}" becomes
// "/api/v1/todos/" and "/cover-file/".
func splitDownloadPattern(pattern string) (prefix, middle string) {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	prefix, rest, _ := strings.Cut(pattern, "{")
	_, rest, _ = strings.Cut(rest, "}")
	middle, _, _ = strings.Cut(rest, "{")
	return prefix, middle
}

// uploadBody emits the default implementation of an upload.
func (e *emitter) uploadBody(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	var (
		uuidPkg  = b.Import("github.com/google/uuid")
		filesPkg = b.Import(filesModule)
		fc       = fileColumnOf(res, ep)
	)

	b.Comment("The owning row first, through the repository, which is what makes " +
		"this tenant-scoped and owner-scoped without a second rule: you cannot " +
		"upload to a row you cannot read.")
	b.L("row, err := s.repo.Get(ctx, r.Path.ID%s)", e.scopeArgs(b, res))
	b.L("if err != nil { return nil, err }")
	b.NL()

	b.L("f, err := s.files.Attach(ctx, %s.AttachRequest{", filesPkg)
	b.L("TenantID: r.Claims.TenantID,")
	b.L("Upload: r.Body,")
	b.L("Owner: %s,", e.ownerLiteral(b, res, fc, "row.ID"))
	b.L("URL: func(fileID %s.UUID, name string) string { return %s(row.ID, fileID, name) },",
		uuidPkg, fileURLName(res, fc))
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.L("return %s(f), nil", e.fileResponseName())
}

// downloadBody emits the default implementation of a download.
func (e *emitter) downloadBody(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	var (
		errPkg = b.Import(runtimeModule + "/rigerr")
		fc     = fileColumnOf(res, ep)
	)

	b.L("row, err := s.repo.Get(ctx, r.Path.ID%s)", e.scopeArgs(b, res))
	b.L("if err != nil { return nil, err }")
	b.NL()

	b.Comment("The identifier has to be this row's. Opening any file the tenant " +
		"owns from under any row would make the nesting decorative, and the " +
		"nesting is what the permission check hangs off.")
	b.L("if %s { return nil, %s.NotFound(\"no such file\") }", fileMismatch(fc), errPkg)
	b.NL()
	b.L("return s.files.Open(ctx, r.Claims.TenantID, r.Path.FileID, r.Path.Filename)")
}

// deleteFileBody emits the default implementation of a file delete.
func (e *emitter) deleteFileBody(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	fc := fileColumnOf(res, ep)

	b.L("row, err := s.repo.Get(ctx, r.Path.ID%s)", e.scopeArgs(b, res))
	b.L("if err != nil { return err }")
	b.NL()

	b.Comment("Nothing there is the state the caller asked for. Answering " +
		"otherwise would make a retry of a request whose response went missing " +
		"look like a failure.")
	b.L("if row.%s == nil { return nil }", fc.Field)
	b.NL()
	b.L("return s.files.Detach(ctx, r.Claims.TenantID, %s, *row.%s)",
		e.ownerLiteral(b, res, fc, "row.ID"), fc.Field)
}

// fileMismatch is the condition that says the path's file is not this row's.
//
// A nullable column is a pointer and a not-null one is not, so the check differs
// in exactly one dereference.
func fileMismatch(fc ir.FileColumn) string {
	if fc.Required {
		return "row." + fc.Field + " != r.Path.FileID"
	}
	return "row." + fc.Field + " == nil || *row." + fc.Field + " != r.Path.FileID"
}

// createWithFilesBody emits the create for a resource with a file column.
//
// The row and its files are committed together. The alternative — create, then
// upload — is two requests, which is why a not-null file column would otherwise
// be unreachable and why a client that made the first and not the second would
// leave a row rig has no business sweeping.
func (e *emitter) createWithFilesBody(b *gobuf.Buf, res *ir.Resource) {
	var (
		ctxPkg   = b.Import("context")
		filesPkg = b.Import(filesModule)
		entity   = e.entity(b, res)
	)

	b.L("if len(pending) == 0 { return s.write.Create(ctx, r.Body) }")
	b.NL()

	b.Comment("Each prepared file's identifier goes on the column its part " +
		"names, before the row is written: the file rows already exist, so the " +
		"foreign key is satisfiable, and they are still invisible until the " +
		"commit below finalizes them.")
	b.L("for _, p := range pending {")
	b.L("switch p.Part {")
	for _, fc := range res.Files {
		b.L("case %s:", gobuf.Quote(fc.Part))
		b.L("id := p.File.ID")
		if fc.Required {
			b.L("r.Body.%s = id", fc.Field)
		} else {
			b.L("r.Body.%s = &id", fc.Field)
		}
	}
	b.L("}")
	b.L("}")
	b.NL()

	b.L("var out *%s", entity)
	b.L("err := s.files.Commit(ctx, pending, func(ctx %s.Context) error {", ctxPkg)
	b.L("row, err := s.write.Create(ctx, r.Body)")
	b.L("if err != nil { return err }")
	b.L("out = row")
	b.L("return nil")
	b.L("}, func(p *%s.Pending) string {", filesPkg)
	b.L("switch p.Part {")
	for _, fc := range res.Files {
		b.L("case %s:", gobuf.Quote(fc.Part))
		b.L("return %s(out.ID, p.File.ID, p.File.Name)", fileURLName(res, fc))
	}
	b.L("}")
	b.L("return \"\"")
	b.L("})")
	b.L("if err != nil { return nil, err }")
	b.L("return out, nil")
}

// fileResponseName is the conversion from a stored file to the shape a client
// sees.
func (e *emitter) fileResponseName() string { return "newRigFile" }

// fileResponseHelper emits that conversion, one line per field the document says
// the shape has.
//
// Generated from the object rather than written out, because the shape is the
// builtin in a project that does not expose rig_file and the projection in one
// that does — and the difference between them is exactly which columns the table
// configuration left in.
func (e *emitter) fileResponseHelper(b *gobuf.Buf) {
	obj := e.object(objectRigFile)
	if obj == nil {
		return
	}
	filesPkg := b.Import(filesModule)

	b.Comment(e.fileResponseName() + " is the stored file as a client sees it.\n\n" +
		"What is left out is the point of it. The storage key, the checksum and " +
		"the declared type are the server's bookkeeping, and the storage key is " +
		"the one that would actually matter — it is what a signed URL is built " +
		"from, and putting it in a shape a client can reach is the same class of " +
		"mistake as handing out a password hash.")
	b.L("func %s(f *%s.File) *%s {", e.fileResponseName(), filesPkg, e.fileShapeRef(b))
	b.L("if f == nil { return nil }")
	b.L("return &%s{", e.fileShapeRef(b))
	for _, f := range obj.Fields {
		if expr, ok := fileFieldExpr(f); ok {
			b.L("%s: %s,", f.Name, expr)
		}
	}
	b.L("}")
	b.L("}")
	b.NL()
}

// objectRigFile is the shape an upload answers with, spelled the way the
// compiler injects it.
const objectRigFile = "RigFile"

// fileShapeRef is how this package names that shape in Go: the model's, when the
// project exposes rig_file and the ordinary generators have already emitted a
// struct for it, and this package's own otherwise.
func (e *emitter) fileShapeRef(b *gobuf.Buf) string {
	for i := range e.doc.API.Resources {
		if e.doc.API.Resources[i].Name == objectRigFile {
			return e.model(b) + "." + objectRigFile
		}
	}
	return objectRigFile
}

// fileFieldExpr maps one field of the file shape to the stored file it comes
// from. A field nothing here can answer is left out rather than guessed at.
func fileFieldExpr(f ir.Field) (string, bool) {
	switch f.Name {
	case "ID":
		return "f.ID", true
	case "URL":
		if f.IsNullable() {
			return "&f.URL", true
		}
		return "f.URL", true
	case "FileName":
		return "f.Name", true
	case "ContentType":
		return "f.ContentType", true
	case "SizeBytes":
		return "f.Size", true
	default:
		return "", false
	}
}

// fileShapeType emits the shape itself, for a project that does not expose
// rig_file and so has no model struct for it.
func (e *emitter) fileShapeType(b *gobuf.Buf) {
	obj := e.object(objectRigFile)
	if obj == nil || e.fileShapeRef(b) != objectRigFile {
		return
	}

	b.Comment(obj.Description)
	b.L("type %s struct {", objectRigFile)
	for _, f := range obj.Fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name, e.goType(b, f), jsonTag(f))
	}
	b.L("}")
	b.NL()
}

// anyFileColumn reports whether anything in the document has one, so the shared
// conversion and the import it brings are emitted only where they are called.
func (e *emitter) anyFileColumn() bool {
	for i := range e.doc.API.Resources {
		if len(e.doc.API.Resources[i].Files) > 0 {
			return true
		}
	}
	return false
}

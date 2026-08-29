package tsclient

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/tsbuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// enumFile emits an enum as a frozen object plus a union of its values.
//
// Not a TypeScript `enum`. Three reasons, and the first settles it: a
// TypeScript enum with string values is a runtime object, so importing one to
// name a value drags a module into the bundle for what should be a string
// literal. The second is that `enum Priority { InProgress = "in_progress" }`
// makes the identifier and the wire value two things nothing will convert
// between. And the third is that a union type is assignable from a string
// literal, so a caller can write `"in_progress"` where the value is obvious and
// reach for the constant where it is not.
func (e *emitter) enumFile(enum ir.Enum) (gen.Artifact, error) {
	b := e.open(snake(enum.Name) + ".gen")

	b.Comment(describe(enum.Description,
		enum.Name+" is a value of the "+enum.PgType+" enumeration."))
	b.L("export const %s = {", enum.Name)
	b.Indent()
	for _, v := range enum.Values {
		if v.Description != "" {
			b.Comment(v.Description)
		}
		b.L("%s: %s,", tsbuf.Key(v.Name), tsbuf.Quote(v.Wire))
	}
	b.Outdent()
	b.L("} as const;")
	b.NL()

	b.Comment("One of the values of " + enum.Name + ".\n\n" +
		"The type and the object share a name on purpose: TypeScript keeps types " +
		"and values in separate namespaces, so `" + enum.Name + "." + firstValue(enum) +
		"` and `: " + enum.Name + "` both read the way somebody would write them.")
	b.L("export type %s = (typeof %s)[keyof typeof %s];", enum.Name, enum.Name, enum.Name)
	b.NL()

	b.Comment("Every value of " + enum.Name + ", in declaration order. It is what " +
		"a picker is drawn from.")
	b.P("export const all%s: readonly %s[] = [", enum.Name, enum.Name)
	for i, v := range enum.Values {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s.%s", enum.Name, v.Name)
	}
	b.L("];")

	return e.close(b)
}

func firstValue(enum ir.Enum) string {
	if len(enum.Values) == 0 {
		return "Value"
	}
	return enum.Values[0].Name
}

// typesFile emits what a resource's reads answer with: the entity, the page it
// arrives in, and — for a streamed resource — the row as the sync service sends
// it.
func (e *emitter) typesFile(res *ir.Resource) (gen.Artifact, error) {
	b := e.open(snake(res.Name) + ".gen")

	if obj := e.object(res.Name); obj != nil {
		e.objectType(b, obj.Name,
			describe(obj.Description, res.Name+" as the API sends it.")+
				"\n\nEvery field the server returns, and no field it does not: this is "+
				"the readable projection of the row rather than the row.",
			obj.Fields)
	}

	if obj := e.listResponse(res); obj != nil {
		e.objectType(b, obj.Name,
			describe(obj.Description, "A page of "+res.Name+"."), obj.Fields)
	}

	if res.Electric != nil {
		e.rowType(b, res)
	}

	return e.close(b)
}

// rowType emits the same row as a live-sync stream sends it.
//
// It is a second type for one row, and that is not duplication but the truth:
// REST answers under the keys `json_case` produced and a stream answers under
// column names, because rig's shape endpoint is a proxy in front of the sync
// service and does not rewrite what Postgres printed. One type used for both
// would compile and then be wrong about every key.
//
// The members are the shape's projection, which is the resource's readable
// fields — the same set a GET returns, so a column excluded from the API is
// excluded here without anybody having to remember.
func (e *emitter) rowType(b *tsbuf.Buf, res *ir.Resource) {
	name := res.Name + "Row"

	b.Comment(name + " is a " + res.Name + " as a live-sync stream sends it.\n\n" +
		"The same row as " + res.Name + ", under different keys. A stream carries " +
		"what Postgres printed, so the keys are column names — `created_at` where " +
		"the API sends `createdAt` — and the values have been through the " +
		"corrections in the streaming runtime, which is what makes a timestamp " +
		"here the same string the API would have sent.\n\n" +
		"A member is nullable rather than optional: the sync service sends every " +
		"column of the projection on every row, with a null where the column is " +
		"null, so nothing here is ever absent.")
	b.L("export type %s = {", name)
	b.Indent()
	for _, f := range streamFields(res) {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		t := e.tsType(b, f.Field)
		if f.IsNullable() {
			t += " | null"
		}
		b.L("%s: %s;", tsbuf.Key(f.Column.Name), t)
	}
	b.Outdent()
	b.L("};")
	b.NL()
}

// filterFile emits the search shapes.
//
// Splitting the operators into separate shapes is what keeps a search typed: the
// range shape carries only orderable columns, so "createdAt contains 3" is not
// expressible rather than merely rejected — and a caller finds that out from the
// compiler instead of from a 422.
func (e *emitter) filterFile(res *ir.Resource, objects []*ir.Object) (gen.Artifact, error) {
	b := e.open(snake(res.Name) + "_query.gen")

	for _, obj := range objects {
		e.objectType(b, obj.Name,
			describe(obj.Description, obj.Name+" narrows a search of "+res.Name+"."),
			obj.Fields)
	}

	return e.close(b)
}

// objectFile emits the shapes no resource claimed: the page envelope, and
// whatever a project declared for itself.
func (e *emitter) objectFile(objects []*ir.Object) (gen.Artifact, error) {
	b := e.open("objects.gen")

	for _, obj := range objects {
		e.objectType(b, obj.Name, describe(obj.Description, obj.Name+"."), obj.Fields)
	}

	return e.close(b)
}

// inputFile emits everything a caller fills in: the create and update inputs,
// each endpoint's own body and query, and the shapes a validation failure comes
// back in.
func (e *emitter) inputFile(res *ir.Resource) (gen.Artifact, error) {
	b := e.open(snake(res.Name) + "_input.gen")

	// First, because it is the one input a reader is least likely to expect: a
	// create that carries its files is a second method, and this is what says
	// which of them the schema requires.
	if ep := createWithFiles(res); ep != nil {
		e.createFilesType(b, res, ep)
	}

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]

		switch {
		case genutil.ModelInputName(ep) == ir.OpCreate:
			e.createInput(b, res, ep)
		case genutil.ModelInputName(ep) == ir.OpUpdate:
			e.updateInput(b, res, ep)
		case len(ep.Request.BodyParams) > 0 && ep.Request.BodyObject == "":
			e.objectType(b, bodyTypeName(res, ep),
				"The body "+ep.OperationID+" sends.", ep.Request.BodyParams)
		}

		// Every endpoint that sends a body says how that body can be wrong, and
		// says it in one place rather than once per operation that happens to
		// have one.
		if name := genutil.FieldsTypeName(res, ep); name != "" {
			e.fieldErrors(b, name, ep.Request.BodyParams)
		}

		if len(ep.Request.QueryParams) > 0 {
			e.queryType(b, res, ep)
		}
	}

	return e.close(b)
}

// createInput emits the body of a create: plain members, because a create has
// nothing to leave alone.
func (e *emitter) createInput(b *tsbuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := res.Name + "CreateInput"

	b.Comment(name + " is what creating a " + res.Name + " takes.\n\n" +
		"A field the server fills in — the identifier, the tenant, the audit " +
		"columns — is not here: those are not a caller's to send.")
	b.L("export type %s = {", name)
	b.Indent()
	for _, f := range ep.Request.BodyParams {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s", e.member(b, f))
	}
	b.Outdent()
	b.L("};")
	b.NL()
}

// updateInput emits the body of a PATCH.
//
// The three states are the point, and TypeScript says them where Go needed a
// wrapper type: a member left out leaves the field alone, `null` clears it, and
// a value sets it. `JSON.stringify` drops an absent key, so "leave it alone"
// happens without anything being encoded for it — which is the distinction
// patch.Optional and patch.Nullable exist to make on the Go side, arrived at
// from the other direction.
//
// A column that cannot hold null gets no `| null`, so clearing it does not
// compile rather than being refused at runtime. Immutable fields are not here at
// all.
func (e *emitter) updateInput(b *tsbuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := res.Name + "UpdateInput"

	b.Comment(name + " is what changing a " + res.Name + " takes.\n\n" +
		"A field left out is left alone. A nullable field set to `null` is " +
		"cleared — which is why only some members accept one: a column that " +
		"cannot hold null has no way to be given one.\n\n" +
		"`undefined` reads as left out, because JSON.stringify drops the key " +
		"either way. It is `null` that is a request to clear.")
	b.L("export type %s = {", name)
	b.Indent()
	for _, f := range ep.Request.BodyParams {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		t := e.tsType(b, f)
		if f.IsNullable() {
			t += " | null"
		}
		b.L("%s?: %s;", tsbuf.Key(f.Wire), t)
	}
	b.Outdent()
	b.L("};")
	b.NL()
}

// fieldErrors emits the shape a 422 on this endpoint arrives in.
//
// One member per member of the body, each optional and each holding what was
// wrong with it — so a message goes beside the control it belongs to instead of
// being parsed out of a sentence. Every member being optional is also why the
// per-call guard in the methods file exists: a shape where everything is
// optional matches anything, so naming one by hand cannot be checked.
func (e *emitter) fieldErrors(b *tsbuf.Buf, name string, fields []ir.Field) {
	fieldError := b.ImportType(e.cfg.ClientImport, "FieldError")

	b.Comment(name + " is what a validation failure says, shaped like the body it " +
		"is about — one member per member, so each message can be put beside the " +
		"control it belongs to.\n\n" +
		"A member is absent when there was nothing wrong with that field. It " +
		"arrives as the `fields` of the error the call threw.")
	b.L("export type %s = {", name)
	b.Indent()
	for _, f := range fields {
		b.L("%s?: %s;", tsbuf.Key(f.Wire), fieldError)
	}
	b.Comment("What was wrong with the request as a whole rather than with any " +
		"one field.")
	b.L("entity?: %s;", fieldError)
	b.Outdent()
	b.L("};")
	b.NL()
}

// queryType emits an endpoint's query parameters.
//
// Every member is optional, and that is the server's rule rather than a
// convenience: a parameter's default is applied when the parameter is absent, so
// a client that helpfully sent `limit: 0` would get an empty page instead of the
// default one.
func (e *emitter) queryType(b *tsbuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := genutil.QueryTypeName(res, ep)

	b.Comment(name + " is the query string " + ep.OperationID + " takes.\n\n" +
		"Every member is optional. The server applies a parameter's default when " +
		"the parameter is absent, so leaving one out is how to ask for the default " +
		"rather than for zero.")
	b.L("export type %s = {", name)
	b.Indent()
	for _, f := range ep.Request.QueryParams {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s?: %s;", tsbuf.Key(f.Wire), e.tsType(b, f))
	}
	b.Outdent()
	b.L("};")
	b.NL()
}

// objectType emits one named object.
func (e *emitter) objectType(b *tsbuf.Buf, name, doc string, fields []ir.Field) {
	b.Comment(doc)
	b.L("export type %s = {", name)
	b.Indent()
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s", e.member(b, f))
	}
	b.Outdent()
	b.L("};")
	b.NL()
}

// open starts a file and records which module it is, so [emitter.ref] can tell a
// local type from one that has to be imported.
func (e *emitter) open(stem string) *tsbuf.Buf {
	e.cur = moduleFor(stem)
	return tsbuf.New()
}

// close renders the file currently open.
func (e *emitter) close(b *tsbuf.Buf) (gen.Artifact, error) {
	path := strings.TrimSuffix(strings.TrimPrefix(e.cur, "./"), ".js") + ".ts"
	e.cur = ""
	return e.artifact(path, b)
}

// listResponse is the page a resource's list answers with.
func (e *emitter) listResponse(res *ir.Resource) *ir.Object {
	return e.object(res.Name + "ListResponse")
}

// filterObjects are the search shapes belonging to a resource.
func (e *emitter) filterObjects(res *ir.Resource) []*ir.Object {
	return genutil.FilterObjects(e.doc, e.reachable, res)
}

// unclaimedObjects are the objects no other file emits.
//
// A resource's own shapes are its files'. What is left is the page envelope and
// whatever a project declared for itself, and both have to land somewhere —
// which is why Pagination is not claimed here the way the Go client claims it.
//
// Error is the exception and is emitted nowhere. Its decoded form is
// `RigError` in the runtime, which is what a caller actually holds — and a
// second type named `Error`, exported from a barrel, would shadow the global one
// in every file that imported it.
func (e *emitter) unclaimedObjects() []*ir.Object {
	return genutil.UnclaimedObjects(e.doc, e.reachable, map[string]bool{"Error": true})
}

// bodyTypeName is the shape an endpoint's body is built from, for the endpoints
// whose body is not one of the shared inputs.
func bodyTypeName(res *ir.Resource, ep *ir.Endpoint) string {
	return res.Name + ep.Name + "Body"
}

// guardName reads back what a refused call said.
//
// Named for the call rather than for the input, which is the difference between
// the two halves: by the time a caller holds the error the input is gone, and
// what they are asking about is the method they called.
func guardName(res *ir.Resource, ep *ir.Endpoint) string {
	if genutil.FieldsTypeName(res, ep) == "" {
		return ""
	}
	return "is" + res.Name + ep.Name + "Error"
}

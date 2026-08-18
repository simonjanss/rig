package goclient

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// inputFile emits everything a caller fills in: the create and update inputs,
// each endpoint's own body and query, and the shapes a validation failure comes
// back in.
func (e *emitter) inputFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]

		switch {
		case genutil.ModelInputName(ep) == ir.OpCreate:
			e.createInput(b, res, ep)
		case genutil.ModelInputName(ep) == ir.OpUpdate:
			e.updateInput(b, res, ep)
		case len(ep.Request.BodyParams) > 0 && ep.Request.BodyObject == "":
			e.bodyStruct(b, res, ep)
		}

		if len(ep.Request.QueryParams) > 0 {
			e.queryStruct(b, res, ep)
		}
	}

	return e.artifact(fileName(res, "_input"), b)
}

// createInput emits the body of a create: plain values, because a create has
// nothing to leave alone.
func (e *emitter) createInput(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := res.Name + "CreateInput"

	b.Comment(name + " is what creating a " + res.Name + " takes.\n\n" +
		"A field the server fills in — the identifier, the tenant, the audit " +
		"columns — is not here: those are not a caller's to send.")
	b.L("type %s struct {", name)
	e.structFields(b, ep.Request.BodyParams)
	b.L("}")
	b.NL()

	e.fieldErrors(b, name, ep.Request.BodyParams)
}

// updateInput emits the body of a PATCH.
//
// The wrappers are the point. A field left out is untouched, a nullable field
// set to null is cleared, and the two are different requests — which a plain Go
// struct cannot say, since a zero value and an absent one are the same thing.
//
// omitzero is what makes that true on the wire. The wrappers encode absence as
// null, because a marshaler cannot remove its own field, so without the tag every
// untouched field would arrive as an explicit null: a refusal on the columns that
// cannot hold one, and a silent clearing of the columns that can.
func (e *emitter) updateInput(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := res.Name + "UpdateInput"
	patchPkg := b.Import(genutil.RuntimeModule + "/patch")

	b.Comment(name + " is what changing a " + res.Name + " takes.\n\n" +
		"A field left out is left alone. A nullable field set to patch.Null is " +
		"cleared — which is why there are two wrappers: a column that cannot hold " +
		"null has no way to be given one, so clearing it is a compile error rather " +
		"than a refusal at runtime. Immutable fields are not here at all.")
	b.L("type %s struct {", name)
	for _, f := range ep.Request.BodyParams {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name,
			genutil.PatchType(patchPkg, f, e.goType(b, f)),
			gobuf.Quote(f.Wire+",omitzero"))
	}
	b.L("}")
	b.NL()

	e.fieldErrors(b, name, ep.Request.BodyParams)
}

// bodyStruct emits the body of an endpoint that has one of its own — a custom
// endpoint, or one of the lifecycle operations.
func (e *emitter) bodyStruct(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := bodyTypeName(res, ep)

	b.Comment(name + " is the request body for " + res.Name + "." + ep.Name + ".")
	b.L("type %s struct {", name)
	e.structFields(b, ep.Request.BodyParams)
	b.L("}")
	b.NL()
}

// queryStruct emits an endpoint's query parameters.
//
// Every member is a pointer, and nil means the parameter is not sent at all.
// That is not a style: the server applies a parameter's default only when it is
// absent, so a client that helpfully sent limit=0 would be asking for an empty
// page rather than for the default one.
func (e *emitter) queryStruct(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint) {
	name := queryTypeName(res, ep)

	b.Comment(name + " is the query for " + res.Name + "." + ep.Name + ".\n\n" +
		"A nil member is left out of the request, and the server applies its own " +
		"default. rigclient.P takes the address of a value, for filling one in.")
	b.L("type %s struct {", name)
	for _, f := range ep.Request.QueryParams {
		if doc := queryDoc(f); doc != "" {
			b.Comment(doc)
		}
		b.L("%s %s `json:%s`", f.Name, pointerTo(e.goType(b, f)), gobuf.Quote(f.Wire))
	}
	b.L("}")
	b.NL()
}

// queryDoc is a parameter's description, with the server's default named after
// it — the number that applies when the member is nil.
func queryDoc(f ir.Field) string {
	doc := f.Description
	if f.Default == "" {
		return doc
	}
	if doc != "" && !strings.HasSuffix(doc, ".") {
		doc += "."
	}
	return strings.TrimSpace(doc + " Left out, the server uses " + f.Default + ".")
}

// pointerTo makes a type optional, unless it already is: a slice and a pointer
// both have a nil to mean absent, and a pointer to either would be a second one.
func pointerTo(t string) string {
	if strings.HasPrefix(t, "*") || strings.HasPrefix(t, "[]") {
		return t
	}
	return "*" + t
}

// fieldErrors emits the shape a 422 for this input arrives in.
//
// One member per input member, so a client can put each message beside the
// control it belongs to. Read with rigclient.FieldsAs, which is the only way the
// generic Fields on an error becomes something typed.
func (e *emitter) fieldErrors(b *gobuf.Buf, input string, fields []ir.Field) {
	name := strings.TrimSuffix(input, "Input") + "Fields"
	rigerr := b.Import(genutil.RuntimeModule + "/rigerr")

	b.Comment(name + " is what a validation failure on a " + input + " says, " +
		"shaped like the input it is about — one member per member, so each " +
		"message can be put beside the control it belongs to.\n\n" +
		"It is read out of a failed call with rigclient.FieldsAs[" + name + "], " +
		"and a member is nil when there was nothing wrong with that field.")
	b.L("type %s struct {", name)
	for _, f := range fields {
		b.L("%s *%s.FieldError `json:%s`", f.Name, rigerr, gobuf.Quote(f.Wire+",omitempty"))
	}
	b.Comment("Entity carries what was wrong with the request as a whole rather " +
		"than with any one field.")
	b.L("Entity *%s.FieldError `json:\"entity,omitempty\"`", rigerr)
	b.L("}")
	b.NL()
}

package goclient

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// enumFile emits an enum as a Go string type.
//
// A string type rather than an integer one, for the same reason the model layer
// uses one: the value on the wire is the label, and an integer would make the Go
// constant and the JSON two different things to keep in step.
//
// It carries no parser. The model's exists because the server accepts any casing
// from a client; a client sending a value it holds as a constant has nothing to
// parse, and a client reading one back is reading a value the server has already
// validated.
func (e *emitter) enumFile(enum ir.Enum) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	b.Comment(genutil.Describe(enum.Description,
		enum.Name+" is a value of the "+enum.PgType+" enumeration."))
	b.L("type %s string", enum.Name)
	b.NL()

	b.L("// The values of %s.", enum.Name)
	b.L("const (")
	for _, v := range enum.Values {
		if v.Description != "" {
			b.Comment(v.Description)
		}
		b.L("%s%s %s = %s", enum.Name, v.Name, enum.Name, gobuf.Quote(v.Wire))
	}
	b.L(")")
	b.NL()

	b.Comment("All" + enum.Name + " is every value, in declaration order. It is " +
		"what a picker is drawn from.")
	b.P("var All%s = []%s{", enum.Name, enum.Name)
	for i, v := range enum.Values {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s%s", enum.Name, v.Name)
	}
	b.L("}")
	b.NL()

	b.Comment("String implements fmt.Stringer.")
	b.L("func (v %s) String() string { return string(v) }", enum.Name)

	return e.artifact(naming.Snake(enum.Name)+".gen.go", b)
}

// typesFile emits what a resource's reads answer with: the entity, and the page
// it arrives in.
func (e *emitter) typesFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	if obj := e.object(res.Name); obj != nil {
		b.Comment(genutil.Describe(obj.Description, res.Name+" as the API sends it.") +
			"\n\nEvery field the server returns, and no field it does not: this is " +
			"the readable projection of the row rather than the row.")
		b.L("type %s struct {", obj.Name)
		e.structFields(b, obj.Fields)
		b.L("}")
		b.NL()
	}

	if obj := e.listResponse(res); obj != nil {
		e.objectStruct(b, obj)
	}

	return e.artifact(fileName(res, ""), b)
}

// filterFile emits the search shapes.
//
// Splitting the operators into separate structs is what keeps a search typed:
// the range struct carries only orderable columns, so "createdAt contains 3" is
// not expressible rather than merely rejected — and a client finds that out from
// the compiler instead of from a 422.
func (e *emitter) filterFile(res *ir.Resource, objects []*ir.Object) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	for _, obj := range objects {
		e.objectStruct(b, obj)
	}

	if e.object(res.Name+"Filter") != nil {
		b.Comment("New" + res.Name + "Filter builds an empty filter, which matches " +
			"every row the caller is allowed to see.")
		b.L("func New%sFilter() %sFilter { return %sFilter{} }", res.Name, res.Name, res.Name)
	}

	return e.artifact(fileName(res, "_query"), b)
}

// objectFile emits the shapes no resource claimed — whatever a project declared
// for itself and a custom endpoint returns.
func (e *emitter) objectFile(objects []*ir.Object) (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	for _, obj := range objects {
		e.objectStruct(b, obj)
	}

	return e.artifact("objects.gen.go", b)
}

// queryTypeName is the struct an endpoint's query parameters arrive in.
func queryTypeName(res *ir.Resource, ep *ir.Endpoint) string {
	return res.Name + ep.Name + "Query"
}

// bodyTypeName is the struct an endpoint's body is built from, for the endpoints
// whose body is not one of the shared inputs.
func bodyTypeName(res *ir.Resource, ep *ir.Endpoint) string {
	return res.Name + ep.Name + "Body"
}

// fieldsTypeName is the shape a validation failure on this endpoint's body
// arrives in, and is empty when there is no body to be wrong about.
//
// Two kinds of body are left out. A search's is a filter, which is a question
// rather than something filled in field by field — nothing validates one, so a
// shape for what was wrong with it would be a struct per resource that is always
// empty. And a body that is a named object is shared between endpoints, so its
// failure shape would have to be too, and nothing on the server emits one to fill
// in. Both fail as a plain rigclient.Error, which is what everything did before
// any of this existed.
func fieldsTypeName(res *ir.Resource, ep *ir.Endpoint) string {
	if ep.Request.BodyObject != "" || len(ep.Request.BodyParams) == 0 {
		return ""
	}
	if ep.Name == ir.OpSearch {
		return ""
	}
	return res.Name + ep.Name + "Fields"
}

// errorFuncName reads back what a refused call said.
//
// Named for the call rather than for the input, which is the difference between
// the two halves: by the time a caller holds the error the input is gone, and
// what they are asking about is the method they called. The server names the
// same JSON after the value that failed, because a validator is holding one.
func errorFuncName(res *ir.Resource, ep *ir.Endpoint) string {
	if fieldsTypeName(res, ep) == "" {
		return ""
	}
	return res.Name + ep.Name + "Error"
}

// inputTypeName is the create or update input an endpoint takes, or empty when
// its body is a shape of its own.
func inputTypeName(res *ir.Resource, ep *ir.Endpoint) string {
	if name := genutil.ModelInputName(ep); name != "" {
		return res.Name + name + "Input"
	}
	return ""
}

// searchFilterField is the member of a search body that carries the conditions.
//
// Found rather than assumed: the body is one object field, and taking it from
// the document means a rename in the compiler does not silently produce a client
// that sends the wrong key.
func searchFilterField(ep *ir.Endpoint) (ir.Field, bool) {
	for _, f := range ep.Request.BodyParams {
		if f.TypeKind == ir.TypeKindObject && strings.HasSuffix(f.Type, "Filter") {
			return f, true
		}
	}
	return ir.Field{}, false
}

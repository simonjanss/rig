package modelgo

import (
	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// inputError emits the typed failure for one input.
//
// It mirrors the input one for one: where the request has a title, the failure
// has the problem with the title. A client that built the request can walk the
// answer the same way it built it, which a flat list of {field, message}
// objects makes it do by string matching and a single sentence makes it do by
// parsing prose.
//
// The name is the input's, with Error on the end: TodoCreateInput fails as a
// TodoCreateInputError.
func (e *emitter) inputError(b *gobuf.Buf, res *ir.Resource, op string, fields []ir.ResourceField) {
	var (
		errPkg = b.Import(genutil.RuntimeModule + "/rigerr")
		strPkg = b.Import("strings")
		name   = res.Name + op + "InputError"
		input  = res.Name + op + "Input"
	)

	b.Comment(name + " says what was wrong with each field of a " + input + ".\n\n" +
		"Its shape is the input's shape, so a client can attach every message to " +
		"the field it is about without matching on strings. A member is nil when " +
		"that field was fine, and the whole value is nil when the input was. It " +
		"is what the 422 carries.")
	b.L("type %s struct {", name)
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s *%s.FieldError `json:%s`", f.Name, errPkg, gobuf.Quote(f.Wire+",omitempty"))
	}
	b.NL()
	b.Comment("Entity is a problem with the row as a whole rather than with one " +
		"field: what the Entity rule said.")
	b.L("Entity *%s.FieldError `json:\"entity,omitempty\"`", errPkg)
	b.L("}")
	b.NL()

	b.Comment("Empty reports whether anything went wrong. A validator that found " +
		"nothing returns nil rather than one of these.")
	b.L("func (e *%s) Empty() bool {", name)
	b.L("if e == nil { return true }")
	b.NL()
	b.P("return ")
	for i, f := range fields {
		if i > 0 {
			b.P(" && ")
		}
		b.P("e.%s == nil", f.Name)
	}
	if len(fields) > 0 {
		b.P(" && ")
	}
	b.L("e.Entity == nil")
	b.L("}")
	b.NL()

	b.Comment("Error implements error. The sentence is for logs and for a person; " +
		"the structure above is what a client acts on.")
	b.L("func (e *%s) Error() string {", name)
	b.L("var parts []string")
	for _, f := range fields {
		b.L("if e.%s != nil { parts = append(parts, %s+e.%s.Error()) }",
			f.Name, gobuf.Quote(f.Wire+" "), f.Name)
	}
	b.L("if e.Entity != nil { parts = append(parts, e.Entity.Error()) }")
	b.NL()
	b.L("return %s + %s.Join(parts, \"; \")",
		gobuf.Quote(res.Storage.Table+" is not valid: "), strPkg)
	b.L("}")
	b.NL()

	b.Comment("ErrorCode implements [rigerr.Coder]: the request was understood " +
		"and its content is what is wrong, which is 422 and not 400.")
	b.L("func (e *%s) ErrorCode() %s.Code { return %s.CodeUnprocessableEntity }",
		name, errPkg, errPkg)
	b.NL()

	b.Comment("ErrorFields implements [rigerr.FieldReporter], which is how the " +
		"HTTP layer finds this and answers with it rather than with prose.")
	b.L("func (e *%s) ErrorFields() any { return e }", name)
	b.NL()
}

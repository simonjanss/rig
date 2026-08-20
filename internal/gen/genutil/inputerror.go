package genutil

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// InputError is one typed validation failure to emit: a struct that mirrors an
// input one member for one, plus the four methods that make it an error the HTTP
// layer can answer with.
//
// It lives here because two generators emit one. The model layer emits one per
// create and update, and the service layer emits one per custom endpoint body —
// the same shape, in a different package, about a body no model owns. Two copies
// would eventually disagree about a wire name or an omitempty, and the client
// decoding them would find out at runtime.
//
// The prose is per generator rather than shared: what the type is about is the
// one thing the two do not have in common, and a comment written to cover both
// would say nothing about either.
type InputError struct {
	// Name is the type to declare, LessonCreateInputError.
	Name string
	// Doc is the comment above it.
	Doc string
	// Subject begins the sentence Error builds — the table for a model input, so
	// that a log line says which row was refused.
	Subject string
	// Entity is the comment on the Entity member, which is the one member that
	// stands for no field.
	Entity string
	// Fields are the input's members, in the input's order.
	Fields []ir.Field
}

// Emit writes the declaration and its methods.
func (e InputError) Emit(b *gobuf.Buf) {
	var (
		errPkg = b.Import(RuntimeModule + "/rigerr")
		strPkg = b.Import("strings")
	)

	b.Comment(e.Doc)
	b.L("type %s struct {", e.Name)
	for _, f := range e.Fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s *%s.FieldError `json:%s`", f.Name, errPkg, gobuf.Quote(f.Wire+",omitempty"))
	}
	b.NL()
	b.Comment(e.Entity)
	b.L("Entity *%s.FieldError `json:\"entity,omitempty\"`", errPkg)
	b.L("}")
	b.NL()

	b.Comment("Empty reports whether anything went wrong. A validator that found " +
		"nothing returns nil rather than one of these.")
	b.L("func (e *%s) Empty() bool {", e.Name)
	b.L("if e == nil { return true }")
	b.NL()
	b.P("return ")
	for i, f := range e.Fields {
		if i > 0 {
			b.P(" && ")
		}
		b.P("e.%s == nil", f.Name)
	}
	if len(e.Fields) > 0 {
		b.P(" && ")
	}
	b.L("e.Entity == nil")
	b.L("}")
	b.NL()

	b.Comment("Error implements error. The sentence is for logs and for a person; " +
		"the structure above is what a client acts on.")
	b.L("func (e *%s) Error() string {", e.Name)
	b.L("var parts []string")
	for _, f := range e.Fields {
		b.L("if e.%s != nil { parts = append(parts, %s+e.%s.Error()) }",
			f.Name, gobuf.Quote(f.Wire+" "), f.Name)
	}
	b.L("if e.Entity != nil { parts = append(parts, e.Entity.Error()) }")
	b.NL()
	b.L("return %s + %s.Join(parts, \"; \")",
		gobuf.Quote(e.Subject+" is not valid: "), strPkg)
	b.L("}")
	b.NL()

	b.Comment("ErrorCode implements [rigerr.Coder]: the request was understood " +
		"and its content is what is wrong, which is 422 and not 400.")
	b.L("func (e *%s) ErrorCode() %s.Code { return %s.CodeUnprocessableEntity }",
		e.Name, errPkg, errPkg)
	b.NL()

	b.Comment("ErrorFields implements [rigerr.FieldReporter], which is how the " +
		"HTTP layer finds this and answers with it rather than with prose.")
	b.L("func (e *%s) ErrorFields() any { return e }", e.Name)
	b.NL()
}

// PlainFields drops the per-field operations from a resource's fields, for the
// emitters that only need the shape.
func PlainFields(fields []ir.ResourceField) []ir.Field {
	out := make([]ir.Field, len(fields))
	for i, f := range fields {
		out[i] = f.Field
	}
	return out
}

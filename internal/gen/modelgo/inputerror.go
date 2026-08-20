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
		name  = res.Name + op + "InputError"
		input = res.Name + op + "Input"
	)

	genutil.InputError{
		Name: name,
		Doc: name + " says what was wrong with each field of a " + input + ".\n\n" +
			"Its shape is the input's shape, so a client can attach every message to " +
			"the field it is about without matching on strings. A member is nil when " +
			"that field was fine, and the whole value is nil when the input was. It " +
			"is what the 422 carries.",
		Subject: res.Storage.Table,
		Entity: "Entity is a problem with the row as a whole rather than with one " +
			"field: what the Entity rule said.",
		Fields: genutil.PlainFields(fields),
	}.Emit(b)
}

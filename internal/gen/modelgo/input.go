package modelgo

import (
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// inputFile emits everything that changes a row, and everything that checks a
// change before it happens.
func (e *emitter) inputFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)

	e.createInput(b, res)
	e.updateInput(b, res)
	e.deleteInput(b, res)

	e.validatorContext(b, res)
	e.validators(b, res)

	return e.artifact(naming.Snake(res.Name)+"_input.gen.go", b)
}

// createInput emits what a caller supplies to create a row.
//
// Plain values, not patches. On create there is nothing to leave alone, so the
// three-state distinction an update needs would only be a wrapper to unwrap.
// The framework's own columns are absent: an identifier, a tenant, and the
// audit stamps are not the caller's to provide, and offering them would invite
// a client to set a tenant it does not belong to.
func (e *emitter) createInput(b *gobuf.Buf, res *ir.Resource) {
	fields := genutil.WritableFields(res, ir.FieldOpCreate)

	b.Comment(res.Name + "CreateInput is what creating a " + res.Name + " takes.\n\n" +
		"The identifier, the tenant, and the audit columns are absent: those are " +
		"stamped by the repository from the request's claims.")
	b.L("type %sCreateInput struct {", res.Name)
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name, e.goType(b, f.Field), gobuf.Quote(f.Wire))
	}
	b.L("}")
	b.NL()

	e.createNormalize(b, res, fields)
	e.inputError(b, res, "Create", fields)
	e.createValidate(b, res, fields)
}

// updateInput emits what a caller supplies to change a row.
func (e *emitter) updateInput(b *gobuf.Buf, res *ir.Resource) {
	fields := genutil.WritableFields(res, ir.FieldOpUpdate)
	patchPkg := b.Import(genutil.RuntimeModule + "/patch")

	b.Comment(res.Name + "UpdateInput is what changing a " + res.Name + " takes.\n\n" +
		"A field left out is untouched. A nullable field set to null is cleared — " +
		"which is why the two wrappers differ: a column that cannot hold null has " +
		"no way to be given one, so clearing it is a compile error rather than a " +
		"rejection at runtime. Immutable fields are not here at all.")
	b.L("type %sUpdateInput struct {", res.Name)
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name,
			genutil.PatchType(patchPkg, f.Field, e.goType(b, f.Field)), gobuf.Quote(f.Wire))
	}
	b.L("}")
	b.NL()

	e.updateNormalize(b, res, fields)
	e.merged(b, res, fields)
	e.inputError(b, res, "Update", fields)
	e.updateValidate(b, res, fields)
}

// deleteInput emits the delete and restore requests.
func (e *emitter) deleteInput(b *gobuf.Buf, res *ir.Resource) {
	uuidPkg := b.Import("github.com/google/uuid")

	b.Comment(res.Name + "DeleteInput is what deleting a " + res.Name + " takes.")
	b.L("type %sDeleteInput struct {", res.Name)
	b.L("// ID is the row to delete.")
	b.L("ID %s.UUID `json:\"id\"`", uuidPkg)
	if res.Storage.IsSoftDeletable() {
		b.Comment("Hard removes the row outright instead of retiring it. A hard " +
			"delete cannot be undone and takes the row's snapshots with it.")
		b.L("Hard bool `json:\"hard,omitempty\"`")
	}
	b.L("}")
	b.NL()

}

// createNormalize emits the cleanup that runs before create validation.
func (e *emitter) createNormalize(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	b.Comment("Normalize tidies what was given before anything checks it.\n\n" +
		"It runs first so that validation sees the value that will actually be " +
		"stored: a title with a trailing space and one without are the same title, " +
		"and rejecting the second for a length rule the first passes would be " +
		"indefensible.")
	b.L("func (i *%sCreateInput) Normalize() {", res.Name)

	wrote := false
	for _, f := range fields {
		if e.normalizeField(b, f, "i."+f.Name, f.IsNullable()) {
			wrote = true
		}
		if e.applyDefault(b, res, f) {
			wrote = true
		}
	}
	if !wrote {
		b.L("// Nothing about this input needs tidying.")
	}

	b.L("}")
	b.NL()
}

// normalizeField emits the cleanup for one field, reporting whether it emitted
// anything.
func (e *emitter) normalizeField(b *gobuf.Buf, f ir.ResourceField, ref string, nullable bool) bool {
	switch {
	case f.TypeKind == ir.TypeKindEnum:
		// Accept any casing. One client sending "InProgress" and another
		// sending "in_progress" mean the same thing, and making one of them a
		// validation failure helps nobody.
		if nullable {
			b.L("if %s != nil {", ref)
			b.L("if v, ok := Parse%s(string(*%s)); ok { *%s = v }", f.Type, ref, ref)
			b.L("}")
			return true
		}
		b.L("if v, ok := Parse%s(string(%s)); ok { %s = v }", f.Type, ref, ref)
		return true

	case f.IsTextual():
		strPkg := b.Import("strings")
		if nullable {
			b.L("if %s != nil { *%s = %s.TrimSpace(*%s) }", ref, ref, strPkg, ref)
			return true
		}
		b.L("%s = %s.TrimSpace(%s)", ref, strPkg, ref)
		return true

	default:
		return false
	}
}

// applyDefault fills in a column's declared default when nothing was given.
//
// It is the price of plain values on create: with no way to tell "omitted" from
// "the zero value", a NOT NULL enum arrives as the empty string and reaches the
// database as one. Postgres would have filled it in had the column been left
// out of the INSERT, so filling it in here says the same thing in a place that
// can also be validated.
//
// Only a default that differs from the zero value is worth emitting, and only
// one that can be read as a Go literal at all — now() and gen_random_uuid()
// belong to columns nobody supplies.
func (e *emitter) applyDefault(b *gobuf.Buf, res *ir.Resource, f ir.ResourceField) bool {
	if f.IsNullable() {
		// A nil pointer already means "let the column decide", and the
		// repository leaves it out of the statement.
		return false
	}

	literal, ok := e.defaultLiteral(res, f)
	if !ok {
		return false
	}

	b.L("if i.%s == %s { i.%s = %s }", f.Name, zeroLiteral(f), f.Name, literal)
	return true
}

// defaultLiteral renders a column's default as Go, and reports whether it is
// worth applying.
func (e *emitter) defaultLiteral(res *ir.Resource, f ir.ResourceField) (string, bool) {
	c := e.doc.Column(res.Storage.Table, f.Column.Name)
	if c == nil || !c.HasDefault {
		return "", false
	}

	// Strip the cast Postgres reports defaults with: 'normal'::todo_priority.
	raw := strings.TrimSpace(c.Default)
	if i := strings.LastIndex(raw, "::"); i > 0 {
		raw = strings.TrimSpace(raw[:i])
	}

	switch {
	case f.TypeKind == ir.TypeKindEnum:
		label := strings.Trim(raw, "'")
		for _, enum := range e.doc.API.Enums {
			if enum.Name != f.Type {
				continue
			}
			for _, v := range enum.Values {
				if v.Wire == label {
					return enum.Name + v.Name, true
				}
			}
		}
		return "", false

	case f.Type == ir.TypeBool:
		// false is already the zero value, so there is nothing to apply.
		return "true", raw == "true"

	case f.IsTextual():
		value := strings.Trim(raw, "'")
		return gobuf.Quote(value), value != ""

	case f.Type == ir.TypeInt || f.Type == ir.TypeInt64 || f.Type == ir.TypeFloat64:
		if _, err := strconv.ParseFloat(raw, 64); err != nil {
			return "", false
		}
		return raw, raw != "0"

	default:
		return "", false
	}
}

// zeroLiteral is what an untouched field of this type holds.
func zeroLiteral(f ir.ResourceField) string {
	switch {
	case f.TypeKind == ir.TypeKindEnum, f.IsTextual():
		return `""`
	case f.Type == ir.TypeBool:
		return "false"
	default:
		return "0"
	}
}

// createValidate emits the checks the schema knows how to make.
func (e *emitter) createValidate(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	b.Comment("Validate checks what the schema can decide on its own.\n\n" +
		"Everything a column declares — NOT NULL, a length, an enumeration's " +
		"values — is checked here, so a service only writes the rules that are " +
		"actually about the business. Every field is checked before returning, " +
		"because a form that reports one problem per round trip is a form people " +
		"give up on.\n\n" +
		"What comes back is a *" + res.Name + "CreateInputError, shaped like the " +
		"input itself.")
	b.L("func (i *%sCreateInput) Validate() error {", res.Name)
	b.L("var failed %sCreateInputError", res.Name)
	b.NL()

	for _, f := range fields {
		e.checkField(b, res, f, "failed", "i."+f.Name, f.IsNullable())
	}

	b.NL()
	b.L("if failed.Empty() { return nil }")
	b.L("return &failed")
	b.L("}")
	b.NL()
}

// updateNormalize tidies only what was sent.
func (e *emitter) updateNormalize(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	patchPkg := b.Import(genutil.RuntimeModule + "/patch")

	b.Comment("Normalize tidies the fields this request actually carries.\n\n" +
		"It does not fill in the ones it does not: the repository writes exactly " +
		"the columns that were sent, and filling them here would turn every update " +
		"into a write of every column — so two requests changing different fields " +
		"of one row would start overwriting each other instead of composing.")
	b.L("func (i *%sUpdateInput) Normalize() {", res.Name)

	wrote := false
	for _, f := range fields {
		if !f.IsTextual() && f.TypeKind != ir.TypeKindEnum {
			continue
		}
		wrote = true

		b.L("if v, ok := i.%s.Get(); ok {", f.Name)
		switch {
		case f.TypeKind == ir.TypeKindEnum:
			b.L("if parsed, ok := Parse%s(string(v)); ok { v = parsed }", f.Type)
		default:
			b.L("v = %s.TrimSpace(v)", b.Import("strings"))
		}
		b.L("i.%s = %s.New%s(v)", f.Name, patchPkg, wrapperKind(f))
		b.L("}")
	}
	if !wrote {
		b.L("// Nothing about this input needs tidying.")
	}

	b.L("}")
	b.NL()
}

// merged emits the intended end state, for validation to work against.
func (e *emitter) merged(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	b.Comment("Merged is the row as it will be once this update is applied.\n\n" +
		"It is what validation runs against, and the reason it exists: a rule " +
		"spanning two fields cannot be checked from a partial request. \"Ends " +
		"after starts\" is unanswerable when only one of them was sent.\n\n" +
		"It returns a copy. The input keeps its patches, so the repository still " +
		"writes only the columns that were actually given.")
	b.L("func (i %sUpdateInput) Merged(prev *%s) %s {", res.Name, res.Name, res.Name)
	b.L("out := *prev")
	b.NL()

	for _, f := range fields {
		if !f.IsNullable() {
			b.L("if v, ok := i.%s.Get(); ok { out.%s = v }", f.Name, f.Name)
			continue
		}

		// Touched covers both a value and an explicit clear: those are two
		// different intentions, and both of them are intentions.
		b.L("if i.%s.Touched() {", f.Name)
		if strings.HasPrefix(f.GoType, "*") {
			b.L("out.%s = i.%s.Ptr()", f.Name, f.Name)
		} else {
			// A slice is already nilable, so the model holds it without a
			// pointer and a cleared value is simply the nil slice — which is
			// what Get returns when there is nothing to get.
			b.L("v, _ := i.%s.Get()", f.Name)
			b.L("out.%s = v", f.Name)
		}
		b.L("}")
	}

	b.NL()
	b.L("return out")
	b.L("}")
	b.NL()
}

// updateValidate checks the intended end state.
func (e *emitter) updateValidate(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	b.Comment("Validate checks the row this update would produce.\n\n" +
		"Against the merged state, not the request: a length rule on a field " +
		"nobody sent still has to hold, and a rule about two fields needs both.\n\n" +
		"What comes back is a *" + res.Name + "UpdateInputError, shaped like the " +
		"input itself.")
	b.L("func (i *%sUpdateInput) Validate(prev *%s) error {", res.Name, res.Name)
	b.L("var failed %sUpdateInputError", res.Name)
	b.NL()

	// A table whose updatable columns are all unconstrained gets no checks at
	// all, and then the merged row would be a declared and unused variable.
	// Validate is still emitted, because the caller's code should not have to
	// know which tables happen to have a rule today.
	if slices.ContainsFunc(fields, func(f ir.ResourceField) bool { return e.checksField(res, f) }) {
		b.L("merged := i.Merged(prev)")
		b.NL()

		for _, f := range fields {
			e.checkField(b, res, f, "failed", "merged."+f.Name, f.IsNullable())
		}
	}

	b.NL()
	b.L("if failed.Empty() { return nil }")
	b.L("return &failed")
	b.L("}")
	b.NL()
}

// checkField emits the generated checks for one field.
//
// Each writes into the failure struct under the field's own member, with the
// code that says what kind of wrong it is: a client that wants to highlight
// what is missing differently from what is too long can, without reading the
// message.
func (e *emitter) checkField(b *gobuf.Buf, res *ir.Resource, f ir.ResourceField, into, ref string, nullable bool) {
	errPkg := b.Import(genutil.RuntimeModule + "/rigerr")

	fail := func(code, format string, args ...string) {
		call := errPkg + ".NewFieldError(" + errPkg + ".FieldCode" + code + ", " + gobuf.Quote(format)
		for _, a := range args {
			call += ", " + a
		}
		b.L("%s.%s = %s)", into, f.Name, call)
	}

	// A NOT NULL text column with no default has to carry something. An empty
	// string satisfies the database and almost never satisfies the intent.
	if !nullable && f.IsTextual() && !e.hasDefault(res, f) {
		b.L("if %s.TrimSpace(%s) == \"\" {", b.Import("strings"), ref)
		fail("CannotBeEmpty", "cannot be empty")
		b.L("}")
	}

	if n := e.maxLength(res, f); n > 0 {
		value := ref
		if nullable {
			b.L("if %s != nil {", ref)
			value = "*" + ref
		}
		b.L("if len([]rune(%s)) > %d {", value, n)
		fail("TooLong", "cannot be longer than "+itoa(n)+" characters")
		b.L("}")
		if nullable {
			b.L("}")
		}
	}

	if f.TypeKind == ir.TypeKindEnum {
		value := ref
		if nullable {
			b.L("if %s != nil {", ref)
			value = "*" + ref
		}
		b.L("if !%s.Valid() {", value)
		fail("InvalidValue", "%q is not one of the allowed values", value)
		b.L("}")
		if nullable {
			b.L("}")
		}
	}
}

// checksField reports whether checkField would emit anything for a field.
//
// It answers the same three questions in the same order, so the two stay
// together: a new kind of check needs a clause here as well, and the compile
// test on a table with no rules is what says so.
func (e *emitter) checksField(res *ir.Resource, f ir.ResourceField) bool {
	if !f.IsNullable() && f.IsTextual() && !e.hasDefault(res, f) {
		return true
	}
	return e.maxLength(res, f) > 0 || f.TypeKind == ir.TypeKindEnum
}

func itoa(n int) string { return strconv.Itoa(n) }

// hasDefault reports whether the column fills itself in when nothing is given.
func (e *emitter) hasDefault(res *ir.Resource, f ir.ResourceField) bool {
	c := e.doc.Column(res.Storage.Table, f.Column.Name)
	return c != nil && c.HasDefault
}

// maxLength is a varchar's declared limit, or zero when there is none.
//
// Checking it here turns a database error nobody can act on into a field error
// naming the field and the limit.
func (e *emitter) maxLength(res *ir.Resource, f ir.ResourceField) int {
	if !f.IsTextual() {
		return 0
	}
	c := e.doc.Column(res.Storage.Table, f.Column.Name)
	if c == nil {
		return 0
	}

	_, rest, found := strings.Cut(c.SQLType, "(")
	if !found {
		return 0
	}
	digits, _, found := strings.Cut(rest, ")")
	if !found {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(digits))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

func wrapperKind(f ir.ResourceField) string {
	if f.IsNullable() {
		return "Nullable"
	}
	return "Optional"
}

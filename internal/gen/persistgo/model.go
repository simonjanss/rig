package persistgo

import (
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// enumFile emits a Postgres enum as a Go string type.
//
// A string type rather than an integer one, because the value that reaches the
// database has to be the label: an integer would make the Go constant and the
// column contents two different things to keep in step.
func (e *emitter) enumFile(enum ir.Enum) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)

	b.Comment(describe(enum.Description, enum.Name+" is a value of the "+enum.PgType+" enumeration."))
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

	b.Comment("All" + enum.Name + " is every value, in declaration order.")
	b.P("var All%s = []%s{", enum.Name, enum.Name)
	for i, v := range enum.Values {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s%s", enum.Name, v.Name)
	}
	b.L("}")
	b.NL()

	b.Comment("Valid reports whether the value is one the database will accept.")
	b.L("func (v %s) Valid() bool {", enum.Name)
	b.L("switch v {")
	b.P("case ")
	for i, v := range enum.Values {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s%s", enum.Name, v.Name)
	}
	b.L(":")
	b.L("return true")
	b.L("default:")
	b.L("return false")
	b.L("}")
	b.L("}")
	b.NL()

	b.Comment("String implements fmt.Stringer.")
	b.L("func (v %s) String() string { return string(v) }", enum.Name)

	return artifact(naming.Snake(enum.Name)+".gen.go", b)
}

// modelFile emits the row struct and its create and update inputs.
func (e *emitter) modelFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)
	fields := storedFields(res)

	// The row.
	b.Comment(describe(res.Description, res.Name+" is a row of the "+res.Storage.Table+" table."))
	b.L("type %s struct {", res.Name)
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s", f.Name, e.goType(b, f.Field))
	}
	b.L("}")
	b.NL()

	e.columnConstants(b, res)
	e.createInput(b, res)
	e.updateInput(b, res)
	e.deleteInput(b, res)

	return artifact(naming.Snake(res.Name)+".gen.go", b)
}

// columnConstants names every column once, so nothing downstream writes a
// column name as a bare string.
func (e *emitter) columnConstants(b *gobuf.Buf, res *ir.Resource) {
	t := e.table(res)

	b.Comment("Column names for " + res.Storage.Table + ", so nothing has to spell one out.")
	b.L("const (")
	for i := range t.Columns {
		c := &t.Columns[i]
		b.L("Column%s%s = %s", res.Name, naming.New(naming.Config{}).Go(c.Name), gobuf.Quote(c.Name))
	}
	b.L(")")
	b.NL()

	b.Comment(res.Name + "Columns is every column, in the order the row is scanned.")
	b.P("var %sColumns = []string{", res.Name)
	for i := range t.Columns {
		if i > 0 {
			b.P(", ")
		}
		b.P("%s", gobuf.Quote(t.Columns[i].Name))
	}
	b.L("}")
	b.NL()
}

// createInput emits what a caller supplies to create a row.
//
// The framework's own columns are absent: an identifier, a tenant, and the
// audit stamps are not the caller's to provide, and offering them would invite
// a client to set a tenant it does not belong to.
func (e *emitter) createInput(b *gobuf.Buf, res *ir.Resource) {
	fields := writableFields(res, ir.FieldOpCreate)

	b.Comment(res.Name + "Create is the input for creating a " + res.Name + ".\n\n" +
		"The identifier, the tenant, and the audit columns are absent: those are " +
		"stamped by the repository from the request's claims.")
	b.L("type %sCreate struct {", res.Name)
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s", f.Name, e.goType(b, f.Field))
	}
	b.L("}")
	b.NL()
}

// updateInput emits what a caller supplies to change a row.
//
// Every field is a patch, so leaving one out and clearing it are different
// requests. Immutable fields are absent entirely — not rejected at runtime,
// simply not expressible.
func (e *emitter) updateInput(b *gobuf.Buf, res *ir.Resource) {
	fields := writableFields(res, ir.FieldOpUpdate)
	patchPkg := b.Import(runtimeModule + "/patch")

	b.Comment(res.Name + "Update is the input for changing a " + res.Name + ".\n\n" +
		"A field left absent is untouched and a field set to null is cleared, " +
		"which is the distinction a pointer cannot make. Immutable fields are " +
		"not here at all.")
	b.L("type %sUpdate struct {", res.Name)
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s.Patch[%s]", f.Name, patchPkg, elemType(e.goType(b, f.Field)))
	}
	b.L("}")
	b.NL()
}

// deleteInput emits the delete request.
func (e *emitter) deleteInput(b *gobuf.Buf, res *ir.Resource) {
	uuidPkg := b.Import("github.com/google/uuid")

	b.Comment(res.Name + "Delete is the input for deleting a " + res.Name + ".")
	b.L("type %sDelete struct {", res.Name)
	b.L("// ID is the row to delete.")
	b.L("ID %s.UUID", uuidPkg)
	if res.Storage.IsSoftDeletable() {
		b.Comment("Hard removes the row outright instead of retiring it. " +
			"A hard delete cannot be undone and takes the row's snapshots with it.")
		b.L("Hard bool")
	}
	b.L("}")
	b.NL()
}

// describe falls back to a generated sentence when there is no comment, so
// every exported type carries documentation even before anyone writes any.
func describe(comment, fallback string) string {
	if strings.TrimSpace(comment) != "" {
		return comment
	}
	return fallback
}

package modelgo

import (
	"github.com/simonjanss/rig/internal/gen/genutil"
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

	b.Comment("Parse" + enum.Name + " reads a value, accepting any casing and " +
		"surrounding space.\n\n" +
		"Normalization uses it, so \"IN_PROGRESS\" from one client and " +
		"\"in_progress\" from another mean the same thing rather than one of them " +
		"being a validation failure nobody can explain. The spelling still has to " +
		"be the label\u2019s: a value is a name the database knows, not a phrase.")
	b.L("func Parse%s(s string) (%s, bool) {", enum.Name, enum.Name)
	b.L("switch %s.ToLower(%s.TrimSpace(s)) {", b.Import("strings"), b.Import("strings"))
	for _, v := range enum.Values {
		b.L("case %s:", gobuf.Quote(lower(v.Wire)))
		b.L("return %s%s, true", enum.Name, v.Name)
	}
	b.L("default:")
	b.L("return \"\", false")
	b.L("}")
	b.L("}")
	b.NL()

	b.Comment("String implements fmt.Stringer.")
	b.L("func (v %s) String() string { return string(v) }", enum.Name)

	return e.artifact(naming.Snake(enum.Name)+".gen.go", b)
}

// entityFile emits the row and the names of its columns.
func (e *emitter) entityFile(res *ir.Resource) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)

	b.Comment(genutil.Describe(res.Description,
		res.Name+" is a row of the "+res.Storage.Table+" table.") + "\n\n" +
		"It is the one definition of a " + res.Name + ": the repository scans into " +
		"it and the API returns it, so there is no conversion between two shapes " +
		"of the same thing and no field that can go missing from one of them.")
	b.L("type %s struct {", res.Name)
	for _, f := range genutil.StoredFields(res) {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s %s `json:%s`", f.Name, e.goType(b, f.Field), jsonTag(f))
	}
	b.L("}")
	b.NL()

	e.columnConstants(b, res)
	return e.artifact(naming.Snake(res.Name)+".gen.go", b)
}

// jsonTag renders a field's tag.
//
// A field that is not readable is hidden with "-". It belongs in the struct,
// because the repository scans it, and must not reach the wire — a column
// configured write-only is a column somebody decided clients should not see.
func jsonTag(f ir.ResourceField) string {
	if !f.In(ir.FieldOpRead) {
		return gobuf.Quote("-")
	}

	return genutil.JSONTag(f.Field)
}

// columnConstants name every column once, so nothing downstream writes a column
// name as a bare string.
func (e *emitter) columnConstants(b *gobuf.Buf, res *ir.Resource) {
	t := e.table(res)
	n := naming.New(naming.Config{})

	b.Comment("Table" + res.Name + " is the table this entity is stored in.")
	b.L("const Table%s = %s", res.Name, gobuf.Quote(res.Storage.Table))
	b.NL()

	b.Comment("Column names for " + res.Storage.Table + ", so nothing has to spell one out.")
	b.L("const (")
	for i := range t.Columns {
		b.L("Column%s%s = %s", res.Name, n.Go(t.Columns[i].Name), gobuf.Quote(t.Columns[i].Name))
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

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + ('a' - 'A')
		}
	}
	return string(out)
}

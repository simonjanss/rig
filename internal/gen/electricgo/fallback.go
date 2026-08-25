package electricgo

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// fallbackType emits the function an application supplies to answer one shape
// while the sync service is unreachable.
//
// It returns the model, not a wire shape: the whole point is that the answer
// comes from a read the application already has, and every read it has returns
// the model. Turning those rows into what a subscriber expects is generated
// below, where the column types are known.
func (e *emitter) fallbackType(b *gobuf.Buf, res *ir.Resource, sh shape) {
	var (
		ctxPkg  = b.Import("context")
		httpPkg = b.Import("net/http")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		modPkg  = b.Import(e.cfg.ModelImport)
	)

	doc := sh.name + "Fallback answers this shape from the application's own read " +
		"path when the sync service cannot be reached.\n\n" +
		"It is called for a subscriber reading the shape from the beginning, and " +
		"never for one resuming a subscription — so what it returns is every row " +
		"the shape holds, not a change to one. The rows go out in the sync " +
		"protocol's own format, which is why a subscriber needs to know nothing " +
		"about this and why what it gets is a snapshot rather than a stream: " +
		"correct when it was read, and not updated until the sync service is back.\n\n" +
		"The context already carries the subscriber's claims, so a generated " +
		"repository read scopes itself to the right tenant without being asked.\n\n" +
		"The read this corresponds to is " + e.correspondingRead(res, sh) + ".\n\n" +
		"**Whatever the scope narrows, narrow here too.** A scope is a filter the " +
		"proxy sends to the sync service and can therefore promise; this is a read " +
		"the proxy cannot see inside. A shape scoped to less than its table, with " +
		"a fallback that is not, shows a subscriber rows the subscription would " +
		"have withheld — and only while something else is broken, which is the " +
		"worst time to find out.\n\n" +
		"Returning an error answers 502, which is what a shape with no fallback " +
		"answers anyway."

	b.Comment(doc)
	if sh.kind == shapeVersions {
		uuidPkg := b.Import("github.com/google/uuid")
		b.L("type %sFallback func(ctx %s.Context, r *%s.Request, claims %s.Claims, id %s.UUID, p %sShapeParams) ([]*%s.%s, error)",
			sh.name, ctxPkg, httpPkg, tenPkg, uuidPkg, res.Name, modPkg, res.Name)
	} else {
		b.L("type %sFallback func(ctx %s.Context, r *%s.Request, claims %s.Claims, p %sShapeParams) ([]*%s.%s, error)",
			sh.name, ctxPkg, httpPkg, tenPkg, res.Name, modPkg, res.Name)
	}
	b.NL()
}

// correspondingRead names the read a shape's fallback should be answering with,
// so the doc comment says it rather than leaving somebody to work it out.
func (e *emitter) correspondingRead(res *ir.Resource, sh shape) string {
	switch sh.kind {
	case shapeDeleted:
		return "ListDeleted — the trash, which the API also exposes as GET /_deleted. " +
			"Note that the repository applies the restore window and this shape does " +
			"not, so the fallback is the narrower of the two"
	case shapeVersions:
		return "ListSnapshots on the id this shape is the history of"
	default:
		return "List, with the repository's default filters: this tenant, not deleted, " +
			"not a snapshot"
	}
}

// fallbackAdapter emits the bridge between the application's read and the proxy.
//
// It exists so that neither side has to know about the other: the application
// returns models, the proxy asks for a snapshot, and the two things that have to
// happen in between — putting the claims on the context and rendering the rows —
// happen here rather than in code somebody writes per shape.
func (e *emitter) fallbackAdapter(b *gobuf.Buf, res *ir.Resource, sh shape) {
	var (
		ctxPkg  = b.Import("context")
		httpPkg = b.Import("net/http")
		elecPkg = b.Import(runtimeModule + "/electric")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
	)

	name := e.namer.GoUnexported(sh.name) + "Fallback"

	b.Comment(name + " adapts one of these reads to what the proxy asks for.\n\n" +
		"The claims go onto the context here, and not in the function an " +
		"application writes, for the reason the tenant condition is built in the " +
		"handler: a generated read takes its claims from the context, and one that " +
		"reached it without them is a read with no tenant filter. Making that " +
		"somebody's job to remember is making it somebody's job to forget.\n\n" +
		"Nil stays nil, which is how the proxy knows there is nothing to fall back " +
		"to.")

	if sh.kind == shapeVersions {
		uuidPkg := b.Import("github.com/google/uuid")
		b.L("func %s(fn %sFallback, r *%s.Request, claims %s.Claims, id %s.UUID, p %sShapeParams) %s.Fallback {",
			name, sh.name, httpPkg, tenPkg, uuidPkg, res.Name, elecPkg)
	} else {
		b.L("func %s(fn %sFallback, r *%s.Request, claims %s.Claims, p %sShapeParams) %s.Fallback {",
			name, sh.name, httpPkg, tenPkg, res.Name, elecPkg)
	}
	b.L("if fn == nil { return nil }")
	b.L("return func(ctx %s.Context) (%s.Snapshot, error) {", ctxPkg, elecPkg)
	if sh.kind == shapeVersions {
		b.L("rows, err := fn(%s.NewContext(ctx, claims), r, claims, id, p)", tenPkg)
	} else {
		b.L("rows, err := fn(%s.NewContext(ctx, claims), r, claims, p)", tenPkg)
	}
	b.L("if err != nil {")
	b.L("return %s.Snapshot{}, err", elecPkg)
	b.L("}")
	b.NL()
	b.L("out := make([]%s.Row, 0, len(rows))", elecPkg)
	b.L("for _, m := range rows {")
	b.Comment("A nil in the slice is not a row, and rendering one would send a " +
		"subscriber a row of nulls with an empty key.")
	b.L("if m == nil { continue }")
	b.L("out = append(out, %sShapeRow(m))", e.namer.GoUnexported(res.Name))
	b.L("}")
	b.L("return %s.Snapshot{Rows: out, Schema: %sShapeSchema}, nil", elecPkg, res.Name)
	b.L("}")
	b.L("}")
	b.NL()
}

// rowEncoder emits the model-to-row rendering, one column at a time.
//
// Column by column rather than through reflection over the struct, because the
// projection is the promise: this renders exactly what ShapeColumns names, so a
// column the API does not expose cannot reach a subscriber over this path
// either. A loop over the struct's fields would send whatever the struct has.
func (e *emitter) rowEncoder(b *gobuf.Buf, res *ir.Resource) {
	var (
		elecPkg = b.Import(runtimeModule + "/electric")
		modPkg  = b.Import(e.cfg.ModelImport)
		fn      = e.namer.GoUnexported(res.Name) + "ShapeRow"
	)

	b.Comment(fn + " renders one row the way the sync service renders it.\n\n" +
		"Every value is the text Postgres prints for it, or null, because that is " +
		"what a subscriber's parsers expect — the type each column is read as " +
		"comes from " + res.Name + "ShapeSchema and not from the JSON.")
	b.L("func %s(m *%s.%s) %s.Row {", fn, modPkg, res.Name, elecPkg)
	b.L("return %s.Row{", elecPkg)
	b.L("Key: %s.RowKey(%s%s),", elecPkg, gobuf.Quote(res.Storage.Table), e.keyArgs(b, res))
	b.L("Value: map[string]any{")
	for _, f := range e.readFields(res) {
		b.L("%s: %s,", gobuf.Quote(f.Column.Name), e.valueCall(elecPkg, res, f))
	}
	b.L("},")
	b.L("}")
	b.L("}")
	b.NL()
}

// keyArgs is the primary key, as arguments to RowKey.
//
// Printed rather than converted per type: a key column is a uuid on every table
// rig generates and an integer or a text on one somebody wrote, and all three
// print as themselves.
func (e *emitter) keyArgs(b *gobuf.Buf, res *ir.Resource) string {
	fmtPkg := b.Import("fmt")

	var out string
	for _, col := range res.Storage.PrimaryKey {
		f := e.fieldFor(res, col)
		if f == nil {
			continue
		}
		out += fmt.Sprintf(", %s.Sprint(m.%s)", fmtPkg, f.Name)
	}
	return out
}

// valueCall is how one column's value is rendered.
//
// Almost every column goes through electric.Value, which reads the Go value it
// is given. The two exceptions are the two it cannot tell apart from a
// timestamp, because a date and a time of day are both a time.Time by then —
// and this is where that is still known.
func (e *emitter) valueCall(elecPkg string, res *ir.Resource, f *ir.ResourceField) string {
	switch f.Type {
	case ir.TypeDate:
		return fmt.Sprintf("%s.DateOnly(m.%s)", elecPkg, f.Name)
	case ir.TypeTime:
		return fmt.Sprintf("%s.TimeOnly(m.%s)", elecPkg, f.Name)
	default:
		return fmt.Sprintf("%s.Value(m.%s)", elecPkg, f.Name)
	}
}

// schemaConst emits the electric-schema a snapshot of this shape carries.
//
// A subscriber picks a parser per column from it, so this is not decoration: a
// snapshot without it leaves every value as the string it arrived as, and a
// timestamp read over the API and the same timestamp read here would decode
// differently. Which is the one thing a generated client exists to prevent.
func (e *emitter) schemaConst(b *gobuf.Buf, res *ir.Resource) error {
	schema, err := e.shapeSchema(res)
	if err != nil {
		return err
	}

	b.Comment(res.Name + "ShapeSchema describes the columns this shape carries, in " +
		"the form the sync service describes them.\n\n" +
		"It is sent with a fallback snapshot and is how a subscriber knows to read " +
		"a count as a number and a timestamp as a moment. The types are Postgres's " +
		"own names — int8, timestamptz, an enum's type name — because those are " +
		"what the sync service sends and a subscriber has one set of parsers for " +
		"both paths.")
	b.L("const %sShapeSchema = %s", res.Name, gobuf.Quote(schema))
	b.NL()
	return nil
}

// shapeSchema builds the schema document for a shape's columns.
func (e *emitter) shapeSchema(res *ir.Resource) (string, error) {
	out := map[string]map[string]any{}

	for _, f := range e.readFields(res) {
		col := e.doc.Resolve(f.Column)
		if col == nil {
			return "", fmt.Errorf("%s.%s: the column is not in the schema", res.Storage.Table, f.Column.Name)
		}

		entry := map[string]any{"type": pgTypeName(col)}
		if col.ArrayElem != "" {
			// One dimension. Postgres does not record how many a column has —
			// int4[] and int4[][] are the same type to it — so this is the number
			// the sync service reports for a column declared with brackets at all.
			entry["dims"] = 1
		}
		if !col.Nullable {
			entry["not_null"] = true
		}
		if i := slices.Index(res.Storage.PrimaryKey, col.Name); i >= 0 {
			entry["pk_index"] = i
		}
		if col.NumericPrec > 0 {
			entry["precision"] = col.NumericPrec
			entry["scale"] = col.NumericScale
		}
		out[col.Name] = entry
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// pgTypeName is the type name the sync service reports for a column: the
// pg_type name rather than the SQL spelling, so "smallint" is int2 — and an
// array's element type, because the dimension is reported separately.
func pgTypeName(col *ir.Column) string {
	if col.ArrayElem != "" {
		return col.ArrayElem
	}
	if col.EnumType != "" {
		return col.EnumType
	}
	if col.UDTName != "" {
		return col.UDTName
	}
	return col.SQLType
}

// readFields are the columns a shape carries: the resource's readable ones, the
// same set ShapeColumns names.
func (e *emitter) readFields(res *ir.Resource) []*ir.ResourceField {
	var out []*ir.ResourceField
	for i := range res.Fields {
		f := &res.Fields[i]
		if f.Column == nil || !f.In(ir.FieldOpRead) {
			continue
		}
		out = append(out, f)
	}
	return out
}

// fieldFor is the field backed by a column.
func (e *emitter) fieldFor(res *ir.Resource, column string) *ir.ResourceField {
	for i := range res.Fields {
		if f := &res.Fields[i]; f.Column != nil && f.Column.Name == column {
			return f
		}
	}
	return nil
}

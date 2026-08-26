package electricgo

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// keyVar emits the primary key, and schemaConst the column types. They are
// emitted from one place because the schema names the key too — pk_index, per
// column — and two answers to what a table's key is would be one answer too
// many.

// keyVar lists the columns that identify a row.
//
// It is what the proxy builds a row's key from when it answers this shape from
// the database, and it is the table's own primary key in the table's own order,
// so a composite key comes out the way the sync service composes it.
func (e *emitter) keyVar(b *gobuf.Buf, res *ir.Resource) {
	b.Comment(res.Name + "ShapeKey are the columns that identify a row of this shape.\n\n" +
		"The table's primary key, in the order the table declares it. A snapshot " +
		"names each row by it, the way the sync service does.")
	b.L("var %sShapeKey = []string{", res.Name)
	for _, col := range res.Storage.PrimaryKey {
		b.L("%s,", gobuf.Quote(col))
	}
	b.L("}")
	b.NL()
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

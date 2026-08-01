package compile

import (
	"cmp"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/pgtypes"
	"github.com/simonjanss/rig/pkg/ir"
)

// NormalizeOptions tune the first pure stage.
type NormalizeOptions struct {
	// IgnoreTables are never projected. Migration bookkeeping lives here.
	IgnoreTables []string
}

// Normalize canonicalizes an introspected schema.
//
// It sorts everything into a stable order, links each column to the enum type
// or foreign key behind it, classifies pure join tables, and checks that every
// column has a type rig can map. It changes no facts: what comes out describes
// exactly the database that went in.
//
// The input is not modified.
func Normalize(raw ir.Schema, opt NormalizeOptions) (ir.Schema, diag.List) {
	var diags diag.List

	out := ir.Schema{
		Name:   cmp.Or(raw.Name, "public"),
		Tables: make([]ir.Table, 0, len(raw.Tables)),
		Enums:  slices.Clone(raw.Enums),
	}

	// Enums first: columns need to be able to look up the type behind them.
	for i := range out.Enums {
		e := &out.Enums[i]
		e.Values = slices.Clone(e.Values)
		if len(e.Values) == 0 {
			diags.Add(diag.CodeEnumWithoutValues, diag.At("enums."+e.Name),
				"enum type %q has no values", e.Name)
		}
	}
	slices.SortFunc(out.Enums, func(a, b ir.PgEnum) int { return cmp.Compare(a.Name, b.Name) })

	enumNames := make(map[string]bool, len(out.Enums))
	for _, e := range out.Enums {
		enumNames[e.Name] = true
	}

	ignored := make(map[string]bool, len(opt.IgnoreTables))
	for _, t := range opt.IgnoreTables {
		ignored[t] = true
	}

	for _, t := range raw.Tables {
		if ignored[t.Name] {
			continue
		}
		nt, d := normalizeTable(t, enumNames)
		diags.Append(d)
		out.Tables = append(out.Tables, nt)
	}

	slices.SortFunc(out.Tables, func(a, b ir.Table) int { return cmp.Compare(a.Name, b.Name) })

	// Link-table classification needs every table's keys resolved first, so it
	// runs as a second pass over the sorted set.
	byName := make(map[string]*ir.Table, len(out.Tables))
	for i := range out.Tables {
		byName[out.Tables[i].Name] = &out.Tables[i]
	}
	for i := range out.Tables {
		out.Tables[i].LinkTable = classifyLinkTable(&out.Tables[i], byName)
	}

	return out, diags
}

func normalizeTable(t ir.Table, enumNames map[string]bool) (ir.Table, diag.List) {
	var diags diag.List

	out := t
	out.Kind = cmp.Or(t.Kind, ir.TableKindBase)
	out.Columns = slices.Clone(t.Columns)
	out.PrimaryKey = slices.Clone(t.PrimaryKey)
	out.Uniques = slices.Clone(t.Uniques)
	out.Indexes = slices.Clone(t.Indexes)
	out.ForeignKeys = slices.Clone(t.ForeignKeys)
	out.Checks = slices.Clone(t.Checks)
	out.LinkTable = nil // recomputed below, never carried in from the input

	// Ordinals are the CREATE TABLE order, which is the order a reader of the
	// generated struct expects. Introspection may hand them over in any order.
	slices.SortStableFunc(out.Columns, func(a, b ir.Column) int {
		return cmp.Compare(a.Ordinal, b.Ordinal)
	})

	pk := make(map[string]bool, len(out.PrimaryKey))
	for _, c := range out.PrimaryKey {
		pk[c] = true
	}

	// Single-column foreign keys are denormalized onto their column: that is
	// how nearly every generator needs them, and re-deriving it per generator
	// is how the two views drift apart.
	fkByColumn := make(map[string]*ir.FKRef, len(out.ForeignKeys))
	for _, fk := range out.ForeignKeys {
		if len(fk.Columns) != 1 || len(fk.ForeignColumns) != 1 {
			continue
		}
		fkByColumn[fk.Columns[0]] = &ir.FKRef{
			Table:    fk.ForeignTable,
			Column:   fk.ForeignColumns[0],
			OnDelete: fk.OnDelete,
			OnUpdate: fk.OnUpdate,
		}
	}

	for i := range out.Columns {
		c := &out.Columns[i]
		c.IsPrimaryKey = pk[c.Name]

		if ref, ok := fkByColumn[c.Name]; ok && c.ForeignKey == nil {
			c.ForeignKey = ref
		}

		if c.UDTName == "" {
			c.UDTName = c.SQLType
		}

		// An enum column carries its type name so later stages do not have to
		// re-recognize it from the SQL type string.
		if c.EnumType == "" {
			if enumNames[c.SQLType] {
				c.EnumType = c.SQLType
			} else if enumNames[c.UDTName] {
				c.EnumType = c.UDTName
			}
		}

		if elem, isArr := pgtypes.ArrayElementName(c.SQLType, c.UDTName); isArr && c.ArrayElem == "" {
			c.ArrayElem = elem
		}

		if c.Comment != "" && c.CommentSource == "" {
			c.CommentSource = ir.CommentSourceDatabase
		}
		if c.Comment == "" {
			c.CommentSource = ir.CommentSourceNone
		}

		// Refusing an unmapped type is deliberate. Guessing produces code that
		// compiles and then misbehaves, which is far worse to debug than a
		// message naming the column.
		if c.EnumType == "" {
			if _, _, ok := pgtypes.Lookup(c.SQLType, c.UDTName); !ok {
				diags.Add(diag.CodeUnmappableType, diag.At(t.Name+"."+c.Name),
					"column %s.%s has type %q, which rig cannot map to a Go type",
					t.Name, c.Name, c.SQLType)
			}
		}
	}

	applyAutoComments(&out)

	slices.SortFunc(out.Indexes, func(a, b ir.Index) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(out.ForeignKeys, func(a, b ir.ForeignKey) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(out.Checks, func(a, b ir.Check) int { return cmp.Compare(a.Name, b.Name) })
	slices.SortFunc(out.Uniques, func(a, b []string) int {
		return cmp.Compare(strings.Join(a, ","), strings.Join(b, ","))
	})

	if out.Comment != "" {
		// A table comment from the database is a real comment; the config can
		// still override it later.
		out.Comment = strings.TrimSpace(out.Comment)
	}

	return out, diags
}

// classifyLinkTable recognizes a pure many-to-many join: a table whose primary
// key is exactly two foreign-key columns.
//
// Such a table is a relation between two resources, not a resource of its own.
// Exposing it as one would give clients a CRUD surface over a join row, which
// is never what anyone wants.
func classifyLinkTable(t *ir.Table, byName map[string]*ir.Table) *ir.LinkTable {
	if t.Kind != ir.TableKindBase || len(t.PrimaryKey) != 2 {
		return nil
	}

	left := t.Column(t.PrimaryKey[0])
	right := t.Column(t.PrimaryKey[1])
	if left == nil || right == nil {
		return nil
	}
	if left.ForeignKey == nil || right.ForeignKey == nil {
		return nil
	}
	if byName[left.ForeignKey.Table] == nil || byName[right.ForeignKey.Table] == nil {
		return nil
	}

	// Anything beyond the two keys and rig's own managed columns means the row
	// carries data of its own, which makes it a resource rather than a link.
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.Name == left.Name || c.Name == right.Name {
			continue
		}
		if isManagedColumn(t.Name, c.Name) {
			continue
		}
		return nil
	}

	return &ir.LinkTable{
		Table:       t.Name,
		LeftTable:   left.ForeignKey.Table,
		LeftColumn:  left.Name,
		RightTable:  right.ForeignKey.Table,
		RightColumn: right.Name,
	}
}

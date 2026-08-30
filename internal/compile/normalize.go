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
	if raw.Replication != nil {
		r := *raw.Replication
		r.Publications = slices.Clone(raw.Replication.Publications)
		slices.SortFunc(r.Publications, func(a, b ir.Publication) int {
			return cmp.Compare(a.Name, b.Name)
		})
		out.Replication = &r
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

	out.Enums = usedEnums(out)

	return out, diags
}

// usedEnums drops the enum types no remaining column refers to.
//
// A Go type for an enum exists so that a field can have it. When the only tables
// that used one have been left out — the authentication foundation's, whose Go
// constants live in the rig/auth module already — the type would be generated
// for nobody to hold, which is the duplication that leaving those tables out was
// meant to avoid.
func usedEnums(schema ir.Schema) []ir.PgEnum {
	used := make(map[string]bool)
	for i := range schema.Tables {
		for _, c := range schema.Tables[i].Columns {
			if c.EnumType != "" {
				used[c.EnumType] = true
			}
		}
	}

	out := make([]ir.PgEnum, 0, len(schema.Enums))
	for _, e := range schema.Enums {
		if used[e.Name] {
			out = append(out, e)
		}
	}
	return out
}

// denormalizableColumn reports which of a foreign key's columns carries its
// meaning, when one of them does.
//
// A single-column key is itself. A two-column key that pairs the tenant with
// one other column is that other column: the tenant half is a scope, not a
// reference, and it is already the same column every generated query filters on.
//
// Anything else — a genuine composite key over two meaningful columns — has no
// single column to hang the reference off, and stays on the table where the
// generators that care can read it whole.
func denormalizableColumn(fk ir.ForeignKey) (int, bool) {
	if len(fk.Columns) != len(fk.ForeignColumns) {
		return 0, false
	}

	switch len(fk.Columns) {
	case 1:
		return 0, true
	case 2:
		// Both halves have to be the tenant on both sides. A key pairing
		// tenant_id with something that lands on a different column over there
		// is not the shape this is about.
		for i, name := range fk.Columns {
			other := 1 - i
			if name == ColTenantID && fk.ForeignColumns[i] == ColTenantID &&
				fk.Columns[other] != ColTenantID {
				return other, true
			}
		}
	}
	return 0, false
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
	out.Publications = slices.Clone(t.Publications)
	out.LinkTable = nil // recomputed below, never carried in from the input

	// Two catalog views answer "which publications carry this table", and a
	// table both of them name arrives twice. Sorted and deduped here, so the
	// document is the same bytes whichever order the rows came back in.
	slices.Sort(out.Publications)
	out.Publications = slices.Compact(out.Publications)

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
	//
	// A two-column key carrying the tenant is denormalized the same way, onto
	// the column that is not the tenant. `(tenant_id, todo_id) references todo
	// (tenant_id, id)` says exactly what `todo_id references todo (id)` says and
	// one thing more — that the row is in this tenant — and it is the shape rig
	// recommends wherever the target is tenant-scoped, because it turns pointing
	// at another tenant's row into a constraint violation rather than something
	// a hook has to remember.
	//
	// Reading only the convenient field meant rig failed to recognize precisely
	// the shape it was recommending: no relation, no filter across it, no naming
	// check, and no index check either.
	fkByColumn := make(map[string]*ir.FKRef, len(out.ForeignKeys))
	for _, fk := range out.ForeignKeys {
		i, ok := denormalizableColumn(fk)
		if !ok {
			continue
		}
		fkByColumn[fk.Columns[i]] = &ir.FKRef{
			Table:    fk.ForeignTable,
			Column:   fk.ForeignColumns[i],
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
		if IsManagedColumn(t.Name, c.Name) {
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

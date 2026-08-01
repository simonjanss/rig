package compile

import (
	"cmp"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/internal/pgtypes"
	"github.com/simonjanss/rig/pkg/ir"
)

// ProjectOptions describe the API being projected.
type ProjectOptions struct {
	Name        string
	Version     string
	Description string
	BasePath    string
	Namer       *naming.Namer
}

// Project turns a normalized schema into a naked API surface.
//
// Every base table that is not a join table becomes a resource; every column
// becomes a field; every foreign key and join table becomes a relation. No
// opinions are applied yet — no CRUD endpoints, no filters, no defaults. Those
// come from [Expand], after the configuration has had its say, so that a
// hand-written endpoint can shadow a generated one.
func Project(schema ir.Schema, opt ProjectOptions) (ir.API, diag.List) {
	var diags diag.List

	n := namerOrDefault(opt.Namer)

	api := ir.API{
		Name:        opt.Name,
		Version:     opt.Version,
		Description: opt.Description,
		BasePath:    opt.BasePath,
	}

	// Enums come first so field types can name them.
	enumTypeNames := make(map[string]string, len(schema.Enums))
	for _, pe := range schema.Enums {
		e := ir.Enum{Name: n.Go(pe.Name), PgType: pe.Name, Description: pe.Comment}
		for _, v := range pe.Values {
			e.Values = append(e.Values, ir.EnumValue{
				Name:        n.Go(v.Value),
				Wire:        v.Value,
				Description: v.Description,
			})
		}
		enumTypeNames[pe.Name] = e.Name
		api.Enums = append(api.Enums, e)
	}

	// Resource names are needed before relations can point at them.
	resourceForTable := make(map[string]string)
	for i := range schema.Tables {
		t := &schema.Tables[i]
		if !projectable(t) {
			continue
		}
		resourceForTable[t.Name] = n.Go(t.Name)
	}

	seenName := make(map[string]string, len(resourceForTable))
	for i := range schema.Tables {
		t := &schema.Tables[i]
		if !projectable(t) {
			continue
		}

		res, d := projectResource(t, schema, n, enumTypeNames, resourceForTable)
		diags.Append(d)

		if prev, dup := seenName[res.Name]; dup {
			diags.Add(diag.CodeNameCollision, diag.At(t.Name),
				"tables %q and %q both project to the resource name %q", prev, t.Name, res.Name)
			continue
		}
		seenName[res.Name] = t.Name

		api.Resources = append(api.Resources, res)
	}

	return api, diags
}

// projectable reports whether a table becomes a resource. Join tables become
// relations on the resources they join, and views are read-only relations rig
// does not yet project.
func projectable(t *ir.Table) bool {
	return t.Kind == ir.TableKindBase && t.LinkTable == nil
}

func projectResource(
	t *ir.Table,
	schema ir.Schema,
	n *naming.Namer,
	enumTypeNames map[string]string,
	resourceForTable map[string]string,
) (ir.Resource, diag.List) {
	var diags diag.List

	life := scanLifecycle(t)

	res := ir.Resource{
		Name:        n.Go(t.Name),
		Description: t.Comment,
	}
	res.Plural = n.Go(n.Plural(t.Name))
	if res.Plural == res.Name {
		diags.Add(diag.CodeUnpluralizable, diag.At(t.Name),
			"table %q has no distinct plural; set `plural:` on the table or add a naming.plurals entry",
			t.Name)
	}
	res.PathSegment = n.PathSegment(res.Plural)

	// Every operation is offered by default. Configuration narrows the set; it
	// is easier to remove an operation you do not want than to remember to add
	// the five you do.
	res.Operations = []string{ir.OpCreate, ir.OpGet, ir.OpList, ir.OpSearch, ir.OpUpdate, ir.OpDelete}

	seenField := make(map[string]string, len(t.Columns))
	for i := range t.Columns {
		c := &t.Columns[i]

		f, ok := projectField(t, c, n, enumTypeNames)
		if !ok {
			continue
		}
		if prev, dup := seenField[f.Name]; dup {
			diags.Add(diag.CodeNameCollision, diag.At(t.Name+"."+c.Name),
				"columns %q and %q both project to the field name %q", prev, c.Name, f.Name)
			continue
		}
		seenField[f.Name] = c.Name

		res.Fields = append(res.Fields, f)
	}

	res.Storage = projectStorage(t, schema, life, resourceForTable, n)

	return res, diags
}

// projectField turns a column into a resource field. The second return is false
// for columns that never reach the API at all.
func projectField(t *ir.Table, c *ir.Column, n *naming.Namer, enumTypeNames map[string]string) (ir.ResourceField, bool) {
	f := ir.Field{
		Name:        n.Go(c.Name),
		Description: c.Comment,
		Column: &ir.ColumnRef{
			Table:    t.Name,
			Name:     c.Name,
			SQLType:  c.SQLType,
			Nullable: c.Nullable,
		},
	}
	f.Wire = n.JSON(f.Name)

	switch {
	case c.EnumType != "":
		typeName := enumTypeNames[c.EnumType]
		if typeName == "" {
			typeName = n.Go(c.EnumType)
		}
		m := pgtypes.EnumMapping(typeName)
		f.Type = typeName
		f.TypeKind = ir.TypeKindEnum
		f.GoType = pgtypes.GoTypeFor(m, c.Nullable)
		f.Column.Scan = m.Scan

	default:
		m, isArray, ok := pgtypes.Lookup(c.SQLType, c.UDTName)
		if !ok {
			// Normalize already reported this; projecting a broken field would
			// only produce a second, less useful message.
			return ir.ResourceField{}, false
		}
		f.Type = m.IRType
		f.TypeKind = ir.TypeKindPrimitive
		f.GoType = pgtypes.GoTypeFor(m, c.Nullable)
		f.Column.Scan = m.Scan
		if isArray {
			f.Modifiers = append(f.Modifiers, ir.ModifierArray)
		}
	}

	if c.Nullable {
		f.Modifiers = append(f.Modifiers, ir.ModifierNullable)
	}
	if c.Default != "" {
		f.Default = c.Default
	}

	// A column rig writes itself, or that the database computes, is readable
	// and never writable.
	managed := isManagedColumn(t.Name, c.Name)
	f.ReadOnly = managed || !c.Writable()

	rf := ir.ResourceField{Field: f}
	rf.Operations = []string{ir.FieldOpRead}
	if !f.ReadOnly {
		rf.Operations = append(rf.Operations, ir.FieldOpCreate, ir.FieldOpUpdate)
	}

	return rf, true
}

func projectStorage(
	t *ir.Table,
	schema ir.Schema,
	life lifecycle,
	resourceForTable map[string]string,
	n *naming.Namer,
) *ir.ResourceStorage {
	s := &ir.ResourceStorage{
		Table:      t.Name,
		PrimaryKey: slices.Clone(t.PrimaryKey),
		AuditLog:   true,
	}

	if life.Tenant != nil {
		s.Tenant = columnRef(t, life.Tenant)
	}

	if life.HasAudit() {
		s.Audit = &ir.AuditColumns{
			CreatedAt: columnRef(t, life.CreatedAt),
			CreatedBy: columnRef(t, life.CreatedBy),
			UpdatedAt: columnRef(t, life.UpdatedAt),
			UpdatedBy: columnRef(t, life.UpdatedBy),
			DeletedAt: columnRef(t, life.DeletedAt),
			DeletedBy: columnRef(t, life.DeletedBy),
		}
	}

	if life.SoftDeletable() {
		s.SoftDelete = &ir.SoftDelete{
			Column: columnRef(t, life.DeletedAt),
			Actor:  columnRef(t, life.DeletedBy),
			// RestoreWindowDays comes from the configuration; validation
			// insists on it for exactly these tables.
		}
	}

	if life.Snapshotable() {
		s.Snapshot = &ir.Snapshot{
			VersionType: columnRef(t, life.VersionType),
			FromID:      columnRef(t, life.SnapshotID),
			FromAt:      columnRef(t, life.SnapshotAt),
		}
	}

	// Default ordering: newest first when there is a creation time, otherwise
	// by primary key. Either way the order is total, so pagination is stable.
	if life.CreatedAt != nil {
		s.DefaultOrder = append(s.DefaultOrder, ir.OrderTerm{Column: ColCreatedAt, Desc: true})
	}
	for _, k := range t.PrimaryKey {
		s.DefaultOrder = append(s.DefaultOrder, ir.OrderTerm{Column: k})
	}

	s.Relations = projectRelations(t, schema, resourceForTable, n)

	for i := range t.Columns {
		c := &t.Columns[i]
		s.Filterable = append(s.Filterable, c.Name)
		if !c.Generated {
			s.Sortable = append(s.Sortable, c.Name)
		}
	}

	return s
}

func columnRef(t *ir.Table, c *ir.Column) *ir.ColumnRef {
	if c == nil {
		return nil
	}
	ref := &ir.ColumnRef{
		Table:    t.Name,
		Name:     c.Name,
		SQLType:  c.SQLType,
		Nullable: c.Nullable,
		Scan:     ir.ScanDirect,
	}
	if c.EnumType != "" {
		ref.Scan = ir.ScanEnumText
	} else if m, _, ok := pgtypes.Lookup(c.SQLType, c.UDTName); ok {
		ref.Scan = m.Scan
	}
	return ref
}

// projectRelations finds every link this table takes part in: the foreign keys
// it holds, the foreign keys pointing at it, and the join tables that reference
// it.
func projectRelations(t *ir.Table, schema ir.Schema, resourceForTable map[string]string, n *naming.Namer) []ir.Relation {
	var rels []ir.Relation

	// Outgoing foreign keys: this row belongs to one of those.
	for i := range t.Columns {
		c := &t.Columns[i]
		if c.ForeignKey == nil {
			continue
		}
		target, ok := resourceForTable[c.ForeignKey.Table]
		if !ok {
			continue
		}
		rels = append(rels, ir.Relation{
			Name:          relationName(n, c.Name, target),
			Kind:          ir.RelationBelongsTo,
			Target:        target,
			LocalColumn:   c.Name,
			ForeignTable:  c.ForeignKey.Table,
			ForeignColumn: c.ForeignKey.Column,
		})
	}

	for i := range schema.Tables {
		other := &schema.Tables[i]

		// A join table this row participates in becomes a many-to-many.
		if lt := other.LinkTable; lt != nil {
			switch t.Name {
			case lt.LeftTable:
				if target, ok := resourceForTable[lt.RightTable]; ok {
					rels = append(rels, ir.Relation{
						Name:         n.Go(n.Plural(lt.RightTable)),
						Kind:         ir.RelationManyToMany,
						Target:       target,
						ForeignTable: lt.RightTable,
						LinkTable:    lt,
					})
				}
			case lt.RightTable:
				if target, ok := resourceForTable[lt.LeftTable]; ok {
					rels = append(rels, ir.Relation{
						Name:         n.Go(n.Plural(lt.LeftTable)),
						Kind:         ir.RelationManyToMany,
						Target:       target,
						ForeignTable: lt.LeftTable,
						LinkTable:    lt,
					})
				}
			}
			continue
		}

		if other.Name == t.Name || !projectable(other) {
			continue
		}

		// Incoming foreign keys: those rows belong to this one.
		for j := range other.Columns {
			c := &other.Columns[j]
			if c.ForeignKey == nil || c.ForeignKey.Table != t.Name {
				continue
			}
			target, ok := resourceForTable[other.Name]
			if !ok {
				continue
			}
			// Two foreign keys from the same table would otherwise collide —
			// home_team_id and away_team_id both yielding "Fixtures". Naming
			// the relation after the column that points at us disambiguates
			// them into HomeFixtures and AwayFixtures, which is also what a
			// reader would have called them.
			name := n.Go(n.Plural(other.Name))
			if q := foreignKeyQualifier(c.Name, t.Name); q != "" {
				name = n.Go(q) + name
			}

			rels = append(rels, ir.Relation{
				Name:          name,
				Kind:          ir.RelationHasMany,
				Target:        target,
				ForeignTable:  other.Name,
				ForeignColumn: c.Name,
			})
		}
	}

	slices.SortStableFunc(rels, func(a, b ir.Relation) int {
		return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Target, b.Target))
	})
	return dedupeRelationNames(rels)
}

// relationName derives the accessor for an outgoing foreign key. A column named
// after its target — fixture_id pointing at fixture — reads best as the target
// itself; a qualified one keeps its qualifier, so home_team_id becomes HomeTeam.
func relationName(n *naming.Namer, column, target string) string {
	base, hadSuffix := trimIDSuffix(column)
	if !hadSuffix {
		return target
	}
	if n.Go(base) == target {
		return target
	}
	return n.Go(base)
}

// foreignKeyQualifier is what a foreign-key column says beyond naming its
// target: "home" out of home_team_id pointing at team, and nothing at all out
// of a plain team_id.
func foreignKeyQualifier(column, target string) string {
	base, ok := trimIDSuffix(column)
	if !ok {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSuffix(base, target), "_")
}

func trimIDSuffix(column string) (string, bool) {
	if len(column) > 3 && column[len(column)-3:] == "_id" {
		return column[:len(column)-3], true
	}
	return column, false
}

// dedupeRelationNames disambiguates two relations that landed on the same
// accessor — two foreign keys to the same table, say — by appending the target
// to the later one rather than dropping it.
func dedupeRelationNames(rels []ir.Relation) []ir.Relation {
	seen := make(map[string]int, len(rels))
	for i := range rels {
		name := rels[i].Name
		if n, dup := seen[name]; dup {
			rels[i].Name = name + rels[i].Target
			seen[rels[i].Name] = n + 1
			continue
		}
		seen[name] = 1
	}
	return rels
}

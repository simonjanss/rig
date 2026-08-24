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
	// RevisionHeader is the header the API revision travels in. The revision
	// itself is not here: it is decided from the finished document's hash, so it
	// is stamped on afterwards.
	RevisionHeader string
	Namer          *naming.Namer
	// Auth is the authentication foundation this API is served with, already
	// resolved by the project configuration, or nil for a project with none.
	Auth *ir.Auth
	// Files is the resolved file handling, or nil for a project that accepts no
	// uploads.
	Files *ir.Files
	// Notifications is the resolved inbox, or nil for a project with none.
	Notifications *ir.Notifications
	// Presence is the resolved presence block, or nil for a project that does
	// not track who is here.
	Presence *ir.Presence
	// Throttle is the resolved throttle block, or nil for a project that does
	// not limit API calls.
	Throttle *ir.Throttle
	// Monitoring is the resolved monitoring block, or nil for a project with no
	// page. Never set without Tracing; the config check is what guarantees it.
	Monitoring *ir.Monitoring

	// Tracing is the resolved tracing block, or nil for a project that emits no
	// spans.
	Tracing *ir.Tracing
	// EmbeddedFoundation is `migrations.foundation: embedded` — rig's own
	// migrations stay in the modules that own them.
	EmbeddedFoundation bool
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
		Name:           opt.Name,
		Version:        opt.Version,
		Description:    opt.Description,
		BasePath:       opt.BasePath,
		RevisionHeader: opt.RevisionHeader,
		Auth:           opt.Auth,
		Files:          opt.Files,
		Notifications:  opt.Notifications,
		Presence:       opt.Presence,
		Throttle:       opt.Throttle,
		Tracing:        opt.Tracing,
		Monitoring:     opt.Monitoring,

		// Who keeps rig's own migrations. Carried from here to the generators
		// untouched: it is a fact about the project, not about the schema.
		EmbeddedFoundation: opt.EmbeddedFoundation,
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

	res.Files = projectFileColumns(t, n)
	res.Storage = projectStorage(t, schema, life, resourceForTable, n)

	res.Notifiable = projectNotifiable(t, schema)
	res.Parents = projectParents(t, resourceForTable, n)
	children, d := projectChildren(t, schema, resourceForTable, n)
	diags.Append(d)
	res.Children = children

	return res, diags
}

// projectNotifiable reports whether notifications can be about this table's
// rows, which is to say whether a link table joins it to rig_notification.
//
// Found by scanning link tables rather than by parsing names. Any link table one
// side of which is rig_notification makes the other side notifiable, so
// `blog_post_notification` is a recommendation in the documentation and nothing
// depends on it — the same position the file convention takes, minus the part
// where the name has to carry a role, because here there is nothing for a name
// to say that the foreign key does not.
//
// This costs almost no new code, and that is the reason the join table is the
// declaration. [classifyLinkTable] already accepts a table whose primary key is
// exactly two foreign-key columns and whose only other columns are rig's own
// managed ones; tenant_id is one of those, and the composite tenant-carrying
// form denormalizes onto its other column — so the tenant-safe shape rig
// recommends is the shape that classifies. Everything else follows for free:
// ManyToMany in both directions, the filter, the embed option, and no CRUD
// surface over a join row.
func projectNotifiable(t *ir.Table, schema ir.Schema) bool {
	if t.Name == NotificationTable {
		return false
	}
	for i := range schema.Tables {
		lt := schema.Tables[i].LinkTable
		if lt == nil {
			continue
		}
		if lt.LeftTable == NotificationTable && lt.RightTable == t.Name {
			return true
		}
		if lt.RightTable == NotificationTable && lt.LeftTable == t.Name {
			return true
		}
	}
	return false
}

// projectParents resolves the foreign keys this table holds to tables rig
// generates a service for.
//
// The boundary is the one M5.9's reference check already draws: a table with no
// resource has no hooks to declare and no service to declare them in, so
// rig_file, rig_account and the audit actor columns are all on the other side of
// it and their foreign keys go on behaving exactly as the schema says. The rule
// is one sentence — rig calls the tables it generates a service for.
func projectParents(t *ir.Table, resourceForTable map[string]string, n *naming.Namer) []ir.ParentLink {
	var out []ir.ParentLink
	seen := make(map[string]bool)

	for i := range t.Columns {
		c := &t.Columns[i]
		if c.ForeignKey == nil || c.ForeignKey.Table == t.Name {
			continue
		}
		target, ok := resourceForTable[c.ForeignKey.Table]
		if !ok {
			continue
		}

		// The same accessor the BelongsTo relation gets, so the hook a service
		// implements is named after the relation a reader already knows.
		name := relationName(n, c.Name, target)
		if seen[name] {
			name += n.Go(c.Name)
		}
		seen[name] = true

		out = append(out, ir.ParentLink{
			Name:   name,
			Parent: target,
			Table:  c.ForeignKey.Table,
			Column: c.Name,
		})
	}
	return out
}

// projectChildren resolves the tables pointing at this one, in the order they
// are told about a delete.
//
// The order does not matter for correctness — everything is in one transaction,
// so any hook returning an error unwinds every hook before it — and it matters
// for two things that are worth naming, because they are the reason somebody
// will file a bug: what one sibling can see of another, and which error the
// caller gets when two of them would both refuse. Whatever the order is, it has
// to be the same on every request and in every process.
//
// So it is derived rather than alphabetical: referencing tables before
// referenced ones, which is the order the rows themselves would have to go in,
// and the graph is the same one the HasMany relations are built from.
// Alphabetical would settle determinism and nothing else, and "your hooks run in
// alphabetical order" is the kind of rule that is technically documented and
// never once anticipated.
func projectChildren(
	t *ir.Table,
	schema ir.Schema,
	resourceForTable map[string]string,
	n *naming.Namer,
) ([]ir.ChildLink, diag.List) {
	var (
		diags diag.List
		links []ir.ChildLink
	)

	parent, isResource := resourceForTable[t.Name]
	if !isResource {
		return nil, diags
	}

	for i := range schema.Tables {
		other := &schema.Tables[i]
		if other.Name == t.Name || !projectable(other) {
			continue
		}
		child, ok := resourceForTable[other.Name]
		if !ok {
			continue
		}

		for j := range other.Columns {
			c := &other.Columns[j]
			if c.ForeignKey == nil || c.ForeignKey.Table != t.Name {
				continue
			}
			name := n.Go(n.Plural(other.Name))
			if q := foreignKeyQualifier(c.Name, t.Name); q != "" {
				name = n.Go(q) + name
			}
			links = append(links, ir.ChildLink{
				Name:   name,
				Child:  child,
				Table:  other.Name,
				Column: c.Name,
				Hook:   relationName(n, c.Name, parent),
			})
		}
	}

	if len(links) < 2 {
		return links, diags
	}

	ordered, cyclic := orderChildren(links, schema)
	if cyclic {
		diags.Add(diag.CodeDeleteOrderCycle, diag.At(t.Name),
			"the tables referencing %q reference each other in a cycle, so there is no order "+
				"that tells each one after the tables pointing at it; they are told in schema "+
				"order instead, and `on_delete.order` in %q's configuration is how to settle it",
			t.Name, t.Name)
	}
	return ordered, diags
}

// orderChildren topologically sorts child links by their own foreign keys, ties
// broken by the order the tables appear in the schema so the result is stable.
//
// The second return reports a cycle, which has no topological order at all: the
// remaining tables keep schema order and the caller says so in a diagnostic
// rather than silently picking one.
func orderChildren(links []ir.ChildLink, schema ir.Schema) ([]ir.ChildLink, bool) {
	// Distinct tables, in first-appearance order. Two foreign keys from one
	// table — home_team_id and away_team_id — are one node with two links
	// hanging off it: the table hears about the delete once, in one place.
	var tables []string
	byTable := make(map[string][]ir.ChildLink, len(links))
	for _, l := range links {
		if _, seen := byTable[l.Table]; !seen {
			tables = append(tables, l.Table)
		}
		byTable[l.Table] = append(byTable[l.Table], l)
	}

	member := make(map[string]bool, len(tables))
	for _, name := range tables {
		member[name] = true
	}

	// An edge from a table to the sibling it references. Deleting is told to
	// the referencing table first, so the referenced one is the dependency.
	after := make(map[string]map[string]bool, len(tables))
	indegree := make(map[string]int, len(tables))
	for i := range schema.Tables {
		tbl := &schema.Tables[i]
		if !member[tbl.Name] {
			continue
		}
		for j := range tbl.Columns {
			c := &tbl.Columns[j]
			if c.ForeignKey == nil || c.ForeignKey.Table == tbl.Name || !member[c.ForeignKey.Table] {
				continue
			}
			if after[tbl.Name] == nil {
				after[tbl.Name] = make(map[string]bool)
			}
			if after[tbl.Name][c.ForeignKey.Table] {
				continue
			}
			after[tbl.Name][c.ForeignKey.Table] = true
			indegree[c.ForeignKey.Table]++
		}
	}

	var (
		out     = make([]ir.ChildLink, 0, len(links))
		emitted = make(map[string]bool, len(tables))
	)
	for len(emitted) < len(tables) {
		next := ""
		for _, name := range tables {
			if !emitted[name] && indegree[name] == 0 {
				next = name
				break
			}
		}
		if next == "" {
			break // a cycle: nothing is free.
		}
		emitted[next] = true
		out = append(out, byTable[next]...)
		for dep := range after[next] {
			indegree[dep]--
		}
	}

	if len(emitted) == len(tables) {
		return out, false
	}
	for _, name := range tables {
		if !emitted[name] {
			out = append(out, byTable[name]...)
		}
	}
	return out, true
}

// projectFileColumns resolves the table's `<role>_file_id` columns, in the order
// they appear on the table.
//
// Here rather than in [Expand] because recognizing one means reading the table's
// foreign keys, and Expand is handed an API with no schema behind it. Everything
// a generator needs is derived once, in the one place that can still see where
// the column points.
func projectFileColumns(t *ir.Table, n *naming.Namer) []ir.FileColumn {
	var out []ir.FileColumn
	for i := range t.Columns {
		c := &t.Columns[i]
		if !isFileColumn(t, c) {
			continue
		}
		role, _ := FileRole(c.Name)
		out = append(out, ir.FileColumn{
			Role:     n.JSON(n.Go(role)),
			Column:   c.Name,
			Field:    n.Go(c.Name),
			Part:     n.JSON(n.Go(role) + "File"),
			Segment:  n.PathSegment(n.Go(role)) + "-file",
			Required: !c.Nullable,
		})
	}
	return out
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
	managed := IsManagedColumn(t.Name, c.Name)
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

			CreatedByAPIKey: columnRef(t, life.CreatedByKey),
			UpdatedByAPIKey: columnRef(t, life.UpdatedByKey),
			DeletedByAPIKey: columnRef(t, life.DeletedByKey),
		}
	}

	if life.SoftDeletable() {
		s.SoftDelete = &ir.SoftDelete{
			Column:   columnRef(t, life.DeletedAt),
			Actor:    columnRef(t, life.DeletedBy),
			ActorKey: columnRef(t, life.DeletedByKey),
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

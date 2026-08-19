package compile

import (
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// Validate checks the frozen document against rig's rules.
//
// There are two kinds. Structural rules are always errors: the generators
// cannot produce correct code without them, so there is nothing to configure.
// Convention rules take their severity from the project's validate block,
// because "every column must have a comment" is a policy a team adopts rather
// than a fact about the code.
//
// Every rule reports at the position in the table's configuration that a reader
// would go to in order to fix it.
func Validate(doc *ir.Document, set *tableconf.Set, p *project.Project) diag.List {
	var diags diag.List

	v := p.Config.Validate
	sev := func(configured string, code diag.Code) diag.Severity {
		return p.Severity(configured, code)
	}

	for i := range doc.Schema.Tables {
		t := &doc.Schema.Tables[i]
		loaded := set.Get(t.Name)
		res := doc.ResourceForTable(t.Name)

		// A table whose configuration could not be read has an unknown intent.
		// Rules that ask what it says would all fire at once and bury the one
		// diagnostic that matters, so they are skipped until it parses.
		unreadable := set.Failed(t.Name)

		// Structural.
		diags.Append(checkPrimaryKey(t, res, loaded))
		diags.Append(checkTenantColumn(t, loaded))
		diags.Append(checkAuditColumns(t, loaded))
		diags.Append(checkSnapshotColumns(doc, t, loaded))
		if !unreadable {
			diags.Append(checkRestoreWindow(t, res, loaded, fileRestoreWindowDays(p)))
			diags.Append(checkSnapshotIgnore(t, res, loaded))
		}

		// Convention.
		if !unreadable {
			diags.Append(checkComments(t, loaded, sev(v.MissingComment, diag.CodeMissingTableComment)))
		}
		diags.Append(checkIndexes(t, loaded,
			sev(v.ForeignKeyNotIndexed, diag.CodeForeignKeyNotIndexed),
			sev(v.TenantIndexLeading, diag.CodeTenantIndexNotLeading)))
		diags.Append(checkColumnNaming(t, loaded,
			sev(v.BooleanPrefix, diag.CodeBooleanPrefix),
			sev(v.TimestampSuffix, diag.CodeTimestampSuffix),
			sev(v.DateSuffix, diag.CodeDateSuffix),
			sev(v.ForeignKeyNaming, diag.CodeForeignKeyNaming)))
		diags.Append(checkCascades(t, loaded, sev(v.CascadeDelete, diag.CodeCascadeDelete)))
	}

	diags.Append(checkEnumConsistency(doc))
	diags.Append(checkCustomEndpoints(doc, set))

	return diags
}

// checkPrimaryKey insists on a single uuid column named id.
//
// Every generated Get, Update, Delete, and Restore addresses a row by one
// identifier; a composite or non-uuid key would make each of those a different
// shape, and the uniformity is most of what makes the generated code readable.
func checkPrimaryKey(t *ir.Table, res *ir.Resource, loaded *tableconf.Loaded) diag.List {
	var diags diag.List

	// A join table's key is its two foreign keys, which is checked separately.
	if res == nil || t.LinkTable != nil {
		return diags
	}

	if len(t.PrimaryKey) == 0 {
		diags.Add(diag.CodeMissingPrimaryKey, at(loaded, t, "table"),
			"table %q has no primary key; rig needs `id uuid primary key`", t.Name)
		return diags
	}
	if len(t.PrimaryKey) > 1 {
		diags.Add(diag.CodePrimaryKeyShape, at(loaded, t, "table"),
			"table %q has a composite primary key (%s); rig addresses rows by a single `id uuid`",
			t.Name, strings.Join(t.PrimaryKey, ", "))
		return diags
	}

	name := t.PrimaryKey[0]
	if name != ColID {
		diags.Add(diag.CodePrimaryKeyShape, at(loaded, t, "table"),
			"table %q has primary key %q; rig expects it to be named %q", t.Name, name, ColID)
		return diags
	}
	if c := t.Column(name); c != nil && !isUUIDType(c) {
		diags.Add(diag.CodePrimaryKeyShape, at(loaded, t, "table"),
			"column %s.id is %s; rig expects uuid", t.Name, c.SQLType)
	}

	return diags
}

func checkTenantColumn(t *ir.Table, loaded *tableconf.Loaded) diag.List {
	var diags diag.List

	c := t.Column(ColTenantID)
	if c == nil {
		return diags
	}
	if !isUUIDType(c) || c.Nullable {
		diags.Add(diag.CodeTenantColumnShape, at(loaded, t, "table"),
			"column %s.%s is %s (nullable %t); the tenant column must be `uuid not null` "+
				"because every generated query filters by it",
			t.Name, ColTenantID, c.SQLType, c.Nullable)
	}
	return diags
}

func checkAuditColumns(t *ir.Table, loaded *tableconf.Loaded) diag.List {
	var diags diag.List

	for _, name := range auditActorColumns() {
		c := t.Column(name)
		if c == nil {
			continue
		}
		// The actor is nullable because rig itself acts without one during
		// migrations and background work.
		if !isUUIDType(c) || !c.Nullable {
			diags.Add(diag.CodeAuditColumnShape, at(loaded, t, "table"),
				"column %s.%s is %s (nullable %t); audit actor columns must be `uuid` and nullable",
				t.Name, name, c.SQLType, c.Nullable)
		}
	}

	for _, name := range []string{ColCreatedAt, ColUpdatedAt, ColDeletedAt} {
		c := t.Column(name)
		if c == nil {
			continue
		}
		if !isTimestampType(c) {
			diags.Add(diag.CodeAuditColumnShape, at(loaded, t, "table"),
				"column %s.%s is %s; audit timestamps must be `timestamptz`", t.Name, name, c.SQLType)
		}
	}

	if c := t.Column(ColCreatedAt); c != nil && c.Nullable {
		diags.Add(diag.CodeAuditColumnShape, at(loaded, t, "table"),
			"column %s.%s is nullable; every row has a creation time", t.Name, ColCreatedAt)
	}
	if c := t.Column(ColDeletedAt); c != nil && !c.Nullable {
		diags.Add(diag.CodeAuditColumnShape, at(loaded, t, "table"),
			"column %s.%s is NOT NULL, so no row could ever be live", t.Name, ColDeletedAt)
	}

	return diags
}

// checkSnapshotColumns enforces the all-or-none rule on the snapshot triple.
//
// A partial triple is far more likely to be a half-finished migration than a
// deliberate choice, and treating it as "not snapshotable" would silently drop
// the versioning the author was in the middle of adding.
func checkSnapshotColumns(doc *ir.Document, t *ir.Table, loaded *tableconf.Loaded) diag.List {
	var diags diag.List

	life := scanLifecycle(t)
	found := life.SnapshotColumnsFound()
	if found == 0 {
		return diags
	}

	if found < 3 {
		var missing []string
		if life.VersionType == nil {
			missing = append(missing, ColVersionType)
		}
		if life.SnapshotID == nil {
			missing = append(missing, SnapshotFromIDColumn(t.Name))
		}
		if life.SnapshotAt == nil {
			missing = append(missing, SnapshotFromAtColumn(t.Name))
		}
		diags.Add(diag.CodeSnapshotTriplePartial, at(loaded, t, "table"),
			"table %q has some snapshot columns but is missing %s; a table keeps versions or it does not",
			t.Name, strings.Join(missing, " and "))
		return diags
	}

	if life.VersionType.EnumType == "" {
		diags.Add(diag.CodeSnapshotColumnType, at(loaded, t, "table"),
			"column %s.%s is %s; it must be an enum containing %q and %q",
			t.Name, ColVersionType, life.VersionType.SQLType, VersionOriginal, VersionSnapshot)
	} else if e := doc.PgEnum(life.VersionType.EnumType); e != nil {
		for _, want := range []string{VersionOriginal, VersionSnapshot} {
			if !e.HasValue(want) {
				diags.Add(diag.CodeSnapshotEnumValues, at(loaded, t, "table"),
					"enum %q is missing the value %q", e.Name, want)
			}
		}
	}

	if !isUUIDType(life.SnapshotID) || !life.SnapshotID.Nullable {
		diags.Add(diag.CodeSnapshotColumnType, at(loaded, t, "table"),
			"column %s.%s is %s (nullable %t); it must be a nullable uuid referencing %s.id",
			t.Name, life.SnapshotID.Name, life.SnapshotID.SQLType, life.SnapshotID.Nullable, t.Name)
	} else if fk := life.SnapshotID.ForeignKey; fk == nil || fk.Table != t.Name {
		diags.Add(diag.CodeSnapshotColumnType, at(loaded, t, "table"),
			"column %s.%s must be a foreign key back to %s.id", t.Name, life.SnapshotID.Name, t.Name)
	}

	if !isTimestampType(life.SnapshotAt) || !life.SnapshotAt.Nullable {
		diags.Add(diag.CodeSnapshotColumnType, at(loaded, t, "table"),
			"column %s.%s is %s (nullable %t); it must be a nullable timestamptz",
			t.Name, life.SnapshotAt.Name, life.SnapshotAt.SQLType, life.SnapshotAt.Nullable)
	}

	return diags
}

// checkRestoreWindow ties the retention setting to the schema that needs it.
//
// fileWindow is rig_file's window resolved from `files.restore_window`, or zero
// when no files block governs it. Where it applies it replaces the key rather
// than defaulting it: one number decides how long the bytes are kept, and a
// second one in services/rig_file could only ever disagree with it.
func checkRestoreWindow(t *ir.Table, res *ir.Resource, loaded *tableconf.Loaded, fileWindow int) diag.List {
	var diags diag.List
	if res == nil {
		return diags
	}

	soft := res.Storage.IsSoftDeletable()

	var configured *int
	if loaded != nil {
		configured = loaded.File.RestoreWindowDays
	}

	if t.Name == FileTable && fileWindow > 0 {
		if configured != nil {
			diags.Add(diag.CodeRestoreWindowForbidden, at(loaded, t, "restore_window_days"),
				"restore_window_days is set on %q, whose window is files.restore_window in "+
					"rig.yaml: it is how long the bytes are kept as well as how long the row "+
					"is restorable, so there is one number and this is not where it lives",
				t.Name)
		}
		return diags
	}

	switch {
	case soft && configured == nil:
		diags.Add(diag.CodeRestoreWindowRequired, at(loaded, t, "table"),
			"table %q is soft-deletable, so it must declare how long a deleted row stays "+
				"restorable: add `restore_window_days:`", t.Name)

	case !soft && configured != nil:
		diags.Add(diag.CodeRestoreWindowForbidden, at(loaded, t, "restore_window_days"),
			"table %q has no %s column, so there is nothing to restore", t.Name, ColDeletedAt)

	case soft && *configured <= 0:
		diags.Add(diag.CodeRestoreWindowInvalid, at(loaded, t, "restore_window_days"),
			"restore_window_days is %d; it must be a positive number of days", *configured)
	}

	return diags
}

func checkSnapshotIgnore(t *ir.Table, res *ir.Resource, loaded *tableconf.Loaded) diag.List {
	var diags diag.List
	if loaded == nil || res == nil {
		return diags
	}

	snapshotable := res.Storage.IsSnapshotable()

	for name, cc := range loaded.File.Columns {
		if !cc.SnapshotIgnore {
			continue
		}
		if !snapshotable {
			diags.Add(diag.CodeSnapshotIgnoreUnavailable, loaded.At("columns", name, "snapshot_ignore"),
				"table %q keeps no snapshots, so there is nothing for %q to be excluded from",
				t.Name, name)
			continue
		}
		if isSnapshotColumn(t.Name, name) {
			diags.Add(diag.CodeSnapshotIgnoreReserved, loaded.At("columns", name, "snapshot_ignore"),
				"column %q is snapshot bookkeeping and is always managed by rig", name)
		}
	}

	return diags
}

func checkComments(t *ir.Table, loaded *tableconf.Loaded, sev diag.Severity) diag.List {
	var diags diag.List
	if sev == "" {
		return diags
	}

	// A join table is a relation, not a resource, so it has no configuration
	// file of its own. Demanding documentation with nowhere to write it is a
	// rule nobody can satisfy.
	if t.LinkTable != nil {
		return diags
	}

	if !documented(t.Comment) {
		diags.AddSeverity(diag.CodeMissingTableComment, sev, at(loaded, t, "comment"),
			"table %q has no comment; describe what it is for in its `comment:` key", t.Name)
	}

	for i := range t.Columns {
		c := &t.Columns[i]
		// Columns rig manages carry generated documentation, which is enough.
		if c.CommentSource == ir.CommentSourceAuto {
			continue
		}
		if !documented(c.Comment) {
			diags.AddSeverity(diag.CodeMissingColumnComment, sev, at(loaded, t, "columns", c.Name, "comment"),
				"column %s.%s has no comment", t.Name, c.Name)
		}
	}

	return diags
}

// documented reports whether a comment says anything.
//
// A placeholder left over from a scaffold is not documentation, and treating it
// as though it were would make the rule satisfiable by ignoring it — which is
// the same as not having the rule.
func documented(comment string) bool {
	c := strings.TrimSpace(comment)
	if c == "" {
		return false
	}
	upper := strings.ToUpper(c)
	for _, placeholder := range []string{"TODO", "FIXME", "XXX"} {
		if strings.HasPrefix(upper, placeholder) {
			return false
		}
	}
	return true
}

// checkIndexes catches the two index mistakes that hurt most in practice.
func checkIndexes(t *ir.Table, loaded *tableconf.Loaded, fkSev, tenantSev diag.Severity) diag.List {
	var diags diag.List

	if fkSev != "" {
		for i := range t.Columns {
			c := &t.Columns[i]
			if c.ForeignKey == nil {
				continue
			}
			if indexCovers(t, c.Name) {
				continue
			}
			diags.AddSeverity(diag.CodeForeignKeyNotIndexed, fkSev, at(loaded, t, "columns", c.Name),
				"foreign key %s.%s is not covered by an index", t.Name, c.Name)
		}
	}

	if tenantSev != "" && t.Column(ColTenantID) != nil {
		leads := false
		for _, idx := range t.Indexes {
			if idx.LeadsWith(ColTenantID) {
				leads = true
				break
			}
		}
		if !leads {
			diags.AddSeverity(diag.CodeTenantIndexNotLeading, tenantSev, at(loaded, t, "table"),
				"no index on %q leads with %s; every generated query filters by it, so this is "+
					"the difference between a seek and a full scan", t.Name, ColTenantID)
		}
	}

	return diags
}

// indexCovers reports whether a column is the first column of some index. Being
// buried in the middle of a composite index does not help a lookup by that
// column alone, which is what a foreign key needs.
//
// An index leading with the tenant and then this column counts, but only where
// the key itself carries the tenant. `(tenant_id, todo_id) references todo
// (tenant_id, id)` is enforced on both columns, so `(tenant_id, todo_id)` is the
// index Postgres uses to check the child rows when a parent is deleted, as well
// as the one every generated query wants — and demanding a second index leading
// with the column alone would be rig warning about the shape it asked for.
//
// A single-column key gets no such credit. `references todo (id)` is checked
// with `WHERE todo_id = $1` and nothing else, which a `(tenant_id, todo_id)`
// index cannot serve, so deleting a parent would scan the child table however
// well the generated reads are covered.
func indexCovers(t *ir.Table, column string) bool {
	tenantCarrying := carriesTenantInKey(t, column)

	for _, idx := range t.Indexes {
		if idx.LeadsWith(column) {
			return true
		}
		if tenantCarrying && idx.Partial == "" && len(idx.Columns) > 1 &&
			idx.Columns[0] == ColTenantID && idx.Columns[1] == column {
			return true
		}
	}
	// A single-column primary key or unique constraint is an index too.
	return len(t.PrimaryKey) == 1 && t.PrimaryKey[0] == column
}

// carriesTenantInKey reports whether the foreign key denormalized onto this
// column is the two-column form pairing it with the tenant.
//
// It reads the table's keys rather than the column's [ir.FKRef], because the
// FKRef is the denormalized view and by design says nothing about how many
// columns the constraint spans — which is the whole question here.
func carriesTenantInKey(t *ir.Table, column string) bool {
	for _, fk := range t.ForeignKeys {
		i, ok := denormalizableColumn(fk)
		if !ok || fk.Columns[i] != column {
			continue
		}
		return len(fk.Columns) == 2
	}
	return false
}

func checkColumnNaming(t *ir.Table, loaded *tableconf.Loaded, boolSev, tsSev, dateSev, fkSev diag.Severity) diag.List {
	var diags diag.List

	for i := range t.Columns {
		c := &t.Columns[i]
		at := at(loaded, t, "columns", c.Name)

		if boolSev != "" && isBoolType(c) && !hasBooleanPrefix(c.Name) {
			diags.AddSeverity(diag.CodeBooleanPrefix, boolSev, at,
				"boolean column %s.%s does not read as a question; prefix it with %s",
				t.Name, c.Name, strings.Join(booleanPrefixes, ", "))
		}

		if tsSev != "" && !IsManagedColumn(t.Name, c.Name) {
			endsAt := strings.HasSuffix(c.Name, "_at")
			switch {
			case isTimestampType(c) && !endsAt:
				diags.AddSeverity(diag.CodeTimestampSuffix, tsSev, at,
					"timestamp column %s.%s should end in _at", t.Name, c.Name)
			case endsAt && !isTimestampType(c):
				diags.AddSeverity(diag.CodeTimestampSuffix, tsSev, at,
					"column %s.%s ends in _at but is %s, not a timestamp", t.Name, c.Name, c.SQLType)
			case endsAt && !isInstantType(c):
				// A name ending in _at claims the column records when something
				// happened, and a bare timestamp cannot: it is a clock reading
				// with no anchor, so two of them cannot be ordered across a
				// daylight-saving change and one means different moments to two
				// servers. A wall-clock value is a fine thing to store — a
				// birthday, opening hours — under a name that does not promise an
				// instant.
				diags.AddSeverity(diag.CodeTimestampSuffix, tsSev, at,
					"column %s.%s ends in _at but is %s; an instant must be `timestamptz`, "+
						"which stores UTC and no zone", t.Name, c.Name, c.SQLType)
			}
		}

		if dateSev != "" {
			endsDate := strings.HasSuffix(c.Name, "_date")
			switch {
			case isDateType(c) && !endsDate:
				diags.AddSeverity(diag.CodeDateSuffix, dateSev, at,
					"date column %s.%s should end in _date", t.Name, c.Name)
			case endsDate && !isDateType(c):
				diags.AddSeverity(diag.CodeDateSuffix, dateSev, at,
					"column %s.%s ends in _date but is %s, not a date", t.Name, c.Name, c.SQLType)
			}
		}

		// A self-referencing key is exempt. There is only one table it could
		// point at, so parent_id and root_token_id say more than
		// parent_account_token_id would, and the convention exists to make the
		// target obvious rather than to make names long.
		selfReference := c.ForeignKey != nil && c.ForeignKey.Table == t.Name

		// A file column is exempt for the same reason, and it is the third of
		// these. `<role>_file_id` is the declaration: there is only one table it
		// could point at, so naming the role says more than naming the target,
		// and the alternative the rule would demand is either rig_file_id — one
		// file per table, forever — or profile_image_rig_file_id.
		if fkSev != "" && c.ForeignKey != nil && !isAuditActorColumn(c.Name) &&
			!selfReference && !isFileColumn(c) {
			want := c.ForeignKey.Table + "_id"
			if c.Name != want && !strings.HasSuffix(c.Name, "_"+want) {
				diags.AddSeverity(diag.CodeForeignKeyNaming, fkSev, at,
					"column %s.%s references %s, so it should be named %s or <qualifier>_%s",
					t.Name, c.Name, c.ForeignKey.Table, want, want)
			}
		}
	}

	return diags
}

// checkCascades rejects ON DELETE CASCADE.
//
// A cascade deletes rows behind the service layer's back: no hooks run, no
// audit entries are written, and soft-deletable children are hard-deleted
// regardless. Deleting through the service is slower and correct.
func checkCascades(t *ir.Table, loaded *tableconf.Loaded, sev diag.Severity) diag.List {
	var diags diag.List
	if sev == "" {
		return diags
	}

	for _, fk := range t.ForeignKeys {
		if !strings.EqualFold(fk.OnDelete, "CASCADE") {
			continue
		}
		diags.AddSeverity(diag.CodeCascadeDelete, sev, at(loaded, t, "table"),
			"foreign key %q on %s declares ON DELETE CASCADE; delete through the service layer "+
				"instead, so hooks, audit entries and snapshots are not bypassed", fk.Name, t.Name)
	}

	return diags
}

// checkEnumConsistency insists that an enum type is nullable everywhere or
// nowhere, since one generated Go type has to serve every column that uses it.
func checkEnumConsistency(doc *ir.Document) diag.List {
	var diags diag.List

	type usage struct {
		table, column string
		nullable      bool
	}
	seen := make(map[string]usage)

	for i := range doc.Schema.Tables {
		t := &doc.Schema.Tables[i]
		for j := range t.Columns {
			c := &t.Columns[j]
			if c.EnumType == "" {
				continue
			}
			prev, ok := seen[c.EnumType]
			if !ok {
				seen[c.EnumType] = usage{t.Name, c.Name, c.Nullable}
				continue
			}
			if prev.nullable != c.Nullable {
				diags.Add(diag.CodeEnumNullabilityMixed, diag.At(t.Name+"."+c.Name),
					"enum %q is nullable on %s.%s but not on %s.%s; one generated type has to "+
						"serve both, so it must be one or the other",
					c.EnumType, prev.table, prev.column, t.Name, c.Name)
			}
		}
	}

	return diags
}

// checkCustomEndpoints resolves the shapes a hand-written endpoint refers to.
func checkCustomEndpoints(doc *ir.Document, set *tableconf.Set) diag.List {
	var diags diag.List

	for i := range doc.API.Resources {
		r := &doc.API.Resources[i]
		var loaded *tableconf.Loaded
		if r.Storage != nil {
			loaded = set.Get(r.Storage.Table)
		}

		// A table that asked not to be exposed and then declared an endpoint
		// has said two contradictory things, and the quiet resolution — the
		// endpoint simply never being generated — is the worst one.
		if r.Unexposed {
			if len(r.Endpoints) > 0 {
				diags.Add(diag.CodeUnexposedConflict, anchorForTable(loaded, tableOf(r), "expose"),
					"%s is not exposed, so its %d endpoint(s) would never be served",
					r.Name, len(r.Endpoints))
			}
			if r.Electric != nil {
				diags.Add(diag.CodeUnexposedConflict, anchorForTable(loaded, tableOf(r), "expose"),
					"%s is not exposed, so its live-sync endpoint would never be served", r.Name)
			}
			continue
		}

		for j := range r.Endpoints {
			e := &r.Endpoints[j]
			if e.Impl.Kind != ir.EndpointCustom {
				continue
			}
			where := endpointAnchor(loaded, e.Name)

			if obj := e.Request.BodyObject; obj != "" && !known(doc, obj) {
				diags.Add(diag.CodeUnknownBodyObject, where,
					"endpoint %s.%s takes a body of type %q, which is not declared anywhere",
					r.Name, e.Name, obj)
			}
			for _, resp := range e.Responses {
				if resp.BodyObject != "" && !known(doc, resp.BodyObject) {
					diags.Add(diag.CodeUnknownBodyObject, where,
						"endpoint %s.%s returns %q for status %d, which is not declared anywhere",
						r.Name, e.Name, resp.BodyObject, resp.StatusCode)
				}
			}
			for _, p := range e.Request.PathParams {
				if !strings.Contains(e.Path, "{"+strings.ToLower(p.Name)+"}") &&
					!strings.Contains(e.Path, "{"+p.Wire+"}") {
					diags.Add(diag.CodeInvalidEndpoint, where,
						"endpoint %s.%s declares path parameter %q, but its path %q does not use it",
						r.Name, e.Name, p.Name, e.Path)
				}
			}
		}
	}

	return diags
}

func known(doc *ir.Document, name string) bool {
	_, ok := doc.TypeKindOf(name)
	return ok
}

// endpointAnchor finds the configured endpoint by name so a diagnostic lands on
// it, rather than on the top of the file.
func endpointAnchor(loaded *tableconf.Loaded, name string) diag.Anchor {
	if loaded == nil {
		return diag.Anchor{}
	}
	for i, ec := range loaded.File.Endpoints {
		if ec.Name == name {
			return loaded.At("endpoints", strconv.Itoa(i))
		}
	}
	return loaded.At("endpoints")
}

// at points a diagnostic at a key in the table's configuration. With no
// configuration file there is nothing to point at, and the message alone has to
// carry the table's name — which is why every message here names it.
func at(loaded *tableconf.Loaded, t *ir.Table, segments ...string) diag.Anchor {
	if loaded == nil {
		return diag.At(t.Name)
	}
	return loaded.At(segments...)
}

// tableOf names a resource's table, or the resource itself when there is none.
func tableOf(r *ir.Resource) string {
	if r.Storage != nil {
		return r.Storage.Table
	}
	return r.Name
}

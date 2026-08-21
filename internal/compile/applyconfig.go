package compile

import (
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/internal/pgtypes"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// ConfigOptions tune how configuration is merged.
type ConfigOptions struct {
	Namer *naming.Namer
	// UnmentionedColumn is the severity for a column the database has and the
	// configuration does not mention. Empty reports nothing.
	UnmentionedColumn diag.Severity
	// Ignored are tables the database has and this document deliberately does
	// not, so a configuration file naming one can be told apart from a file
	// naming a table that was renamed or dropped. The two need different advice,
	// and "no such table" would send somebody looking for a migration that is
	// not the problem.
	Ignored []string
	// Notifications is the resolved notifications block, or nil for a project
	// with no inbox.
	//
	// It arrives here for the reason FileRestoreWindowDays does: rig's two
	// notification tables have no table configuration in an ordinary project,
	// and the two things that have to be true of them — that an inbox read
	// narrows to the caller and that the inbox is the only one of them with a
	// live-sync shape — are the same decision in every project. A copy of them
	// in a YAML file could only ever disagree with this one, and the way it
	// would disagree is by streaming somebody else's inbox.
	Notifications *ir.Notifications

	// FileRestoreWindowDays is how long a deleted rig_file row stays
	// restorable, resolved from `files.restore_window` in rig.yaml.
	//
	// It arrives here rather than through a table configuration because
	// rig_file has no `restore_window_days` key: the sweeper reads one number
	// for every file, and a second copy of it in services/rig_file would be a
	// number that could disagree with the one the bytes are kept for. Zero
	// means the project has no files block, and rig_file — if it is projected
	// at all — is an ordinary table subject to the ordinary rule.
	FileRestoreWindowDays int
}

// ApplyConfig merges the per-table YAML onto the projected API.
//
// Configuration wins wherever it speaks, and speaks about nothing physical:
// there is no key for a column's type or nullability, so a file can never claim
// something the database contradicts.
//
// Drift is handled asymmetrically on purpose. A column the configuration does
// not mention is a warning, because a migration must never stop `rig generate`
// from running. A configuration entry naming a column that no longer exists is
// an error, because dropping it silently is how a rename quietly loses the
// comment and the immutability that went with it.
func ApplyConfig(api ir.API, schema ir.Schema, set *tableconf.Set, opt ConfigOptions) (ir.API, ir.Schema, diag.List) {
	var diags diag.List

	n := namerOrDefault(opt.Namer)

	outSchema := ir.Schema{
		Name:   schema.Name,
		Tables: slices.Clone(schema.Tables),
		Enums:  slices.Clone(schema.Enums),
	}
	outAPI := ir.API{
		Name:           api.Name,
		Version:        api.Version,
		Description:    api.Description,
		BasePath:       api.BasePath,
		RevisionHeader: api.RevisionHeader,
		Enums:          slices.Clone(api.Enums),
		Objects:        slices.Clone(api.Objects),
		Resources:      slices.Clone(api.Resources),
		// Table configuration has nothing to say about authentication or about
		// file handling, so both are carried through untouched. They are named
		// rather than left out because this copy is field by field, and a field
		// nobody listed is a field silently dropped.
		Auth:               api.Auth,
		Files:              api.Files,
		Notifications:      api.Notifications,
		Tracing:            api.Tracing,
		Monitoring:         api.Monitoring,
		EmbeddedFoundation: api.EmbeddedFoundation,
	}

	tableIndex := make(map[string]int, len(outSchema.Tables))
	for i := range outSchema.Tables {
		tableIndex[outSchema.Tables[i].Name] = i
	}
	resourceForTable := make(map[string]int, len(outAPI.Resources))
	for i := range outAPI.Resources {
		if s := outAPI.Resources[i].Storage; s != nil {
			resourceForTable[s.Table] = i
		}
	}
	enumIndex := make(map[string]int, len(outAPI.Enums))
	for i := range outAPI.Enums {
		if pg := outAPI.Enums[i].PgType; pg != "" {
			enumIndex[pg] = i
		}
	}

	// A configuration file for a table that no longer exists is always an
	// error: it is the strongest signal available that a migration renamed or
	// dropped something and the configuration was left behind.
	for _, table := range set.Tables() {
		loaded := set.Get(table)
		if _, ok := tableIndex[table]; ok {
			continue
		}
		if slices.Contains(opt.Ignored, table) {
			// rig_file has a switch of its own, and sending somebody to
			// auth.expose for it would be sending them to a key that happens to
			// work while leaving files.expose — the one the rest of rig reads —
			// saying the opposite.
			if table == FileTable {
				diags.Add(diag.CodeForeignTable, loaded.At("table"),
					"%q is rig's own file table, so rig generates nothing from this file; "+
						"set `files.expose: true` in rig.yaml to project it", table)
				continue
			}
			diags.Add(diag.CodeForeignTable, loaded.At("table"),
				"%q belongs to the rig/auth module, so rig generates nothing from "+
					"this file", table)
			continue
		}
		diags.Add(diag.CodeUnknownTable, loaded.At("table"),
			"no table named %q exists in the database", table)
	}

	// An enum's Go name can be changed in configuration, and every field that
	// referred to the derived one has to move with it.
	renamed := map[string]string{}

	for i := range outSchema.Tables {
		t := &outSchema.Tables[i]
		loaded := set.Get(t.Name)

		diags.Append(applyEnumConfig(loaded, outAPI.Enums, enumIndex, renamed))

		ri, hasResource := resourceForTable[t.Name]
		if !hasResource {
			// A join table has no resource of its own; it is configured, if at
			// all, from the resources it links.
			continue
		}

		res := outAPI.Resources[ri]
		updated, d := applyTableConfig(loaded, t, res, n, opt, set.Failed(t.Name))
		diags.Append(d)
		outAPI.Resources[ri] = updated
	}

	retypeEnums(&outAPI, renamed)
	pruneCascade(&outAPI)

	return outAPI, outSchema, diags
}

// pruneCascade drops every delete-propagation edge that has nowhere to land.
//
// It runs after configuration rather than during projection because `expose` is
// a configuration key: at projection time nothing is unexposed yet. An unexposed
// resource has a model and a repository and no service — service-go skips it —
// so there is no rules interface to declare a hook in and no writer to run one
// from. rig calls the tables it generates a service for, and this is where that
// sentence is enforced rather than restated.
//
// A parent also has to offer Delete, which is a second question from being
// exposed and settles the case that would otherwise be confusing: rig_file is a
// resource in a project with `files.expose`, and it is `operations: [Get, List]`
// — the write path is the upload endpoint on the row that owns the file, not a
// POST to the file table. A `<RigFile>Deleting` field on every table with a file
// column would be a hook nothing can ever reach, and a field that can never fire
// is worse than no field: somebody implements it and waits.
// It also refreshes the resource name on every surviving edge. The links were
// derived before configuration had its say, and `resource:` renames a resource —
// rig_notification_recipient arrives as NotificationRecipient — so a generator
// handed the projected name would emit a type that does not exist. Tables are
// what the edges are keyed by, because a physical name is the one thing
// configuration cannot move.
func pruneCascade(api *ir.API) {
	var (
		// serviced is a table rig writes a service layer for, so a hook has
		// somewhere to be declared.
		serviced = make(map[string]string, len(api.Resources))
		// deletable is a table whose delete a hook could actually be reached by.
		deletable = make(map[string]bool, len(api.Resources))
	)
	for i := range api.Resources {
		res := &api.Resources[i]
		if res.Unexposed || res.Storage == nil {
			continue
		}
		serviced[res.Storage.Table] = res.Name
		deletable[res.Storage.Table] = res.Supports(ir.OpDelete)
	}

	for i := range api.Resources {
		res := &api.Resources[i]
		if res.Storage == nil || serviced[res.Storage.Table] == "" {
			res.Parents, res.Children = nil, nil
			continue
		}
		res.Parents = slices.DeleteFunc(res.Parents, func(p ir.ParentLink) bool {
			return !deletable[p.Table]
		})
		for j := range res.Parents {
			res.Parents[j].Parent = serviced[res.Parents[j].Table]
		}
		if !res.Supports(ir.OpDelete) {
			res.Children = nil
			continue
		}
		res.Children = slices.DeleteFunc(res.Children, func(c ir.ChildLink) bool {
			return serviced[c.Table] == ""
		})
		for j := range res.Children {
			res.Children[j].Child = serviced[res.Children[j].Table]
		}
	}
}

// retypeEnums points every field at an enum's configured name.
//
// Renaming an enum and leaving its references behind produces a document whose
// fields have a type nothing declares, which surfaces two stages later as an
// unresolvable type rather than as the rename it was. Doing it here, once, over
// the whole API, is what keeps a rename to one line of configuration.
func retypeEnums(api *ir.API, renamed map[string]string) {
	if len(renamed) == 0 {
		return
	}

	retype := func(f *ir.Field) {
		to, ok := renamed[f.Type]
		if !ok {
			return
		}
		f.Type = to
		// The Go type is the same name behind whatever pointer or slice the
		// column's nullability added, so only the name part moves.
		if name := typeNameOf(f.GoType); name != "" {
			f.GoType = strings.Replace(f.GoType, name, to, 1)
		}
	}

	for i := range api.Objects {
		for j := range api.Objects[i].Fields {
			retype(&api.Objects[i].Fields[j])
		}
	}
	for i := range api.Resources {
		r := &api.Resources[i]
		for j := range r.Fields {
			retype(&r.Fields[j].Field)
		}
		for j := range r.Endpoints {
			e := &r.Endpoints[j]
			for k := range e.Request.PathParams {
				retype(&e.Request.PathParams[k])
			}
			for k := range e.Request.QueryParams {
				retype(&e.Request.QueryParams[k])
			}
			for k := range e.Request.BodyParams {
				retype(&e.Request.BodyParams[k])
			}
		}
	}
}

// typeNameOf strips the pointer and slice markers from a Go type.
func typeNameOf(goType string) string {
	return strings.TrimLeft(goType, "*[]")
}

func applyTableConfig(
	loaded *tableconf.Loaded,
	t *ir.Table,
	res ir.Resource,
	n *naming.Namer,
	opt ConfigOptions,
	unreadable bool,
) (ir.Resource, diag.List) {
	var diags diag.List

	// An unconfigured table still needs its columns checked against the
	// unmentioned-column rule, so the nil case runs the same path with an empty
	// configuration.
	var cfg tableconf.File
	if loaded != nil {
		cfg = *loaded.File
	}

	// Except rig's own notification tables, which are projected so that link
	// tables can find them and which almost no project writes a configuration
	// for. The rule exists to catch a migration that added a column nobody
	// documented; here the migration is rig's, every column is already
	// commented in it, and the warning is one a project cannot act on. A
	// warning nobody can act on is one everybody learns to skip, and the ones
	// worth reading go with it.
	if loaded == nil && isNotificationTable(t.Name) && opt.Notifications != nil {
		opt.UnmentionedColumn = ""
	}

	out := res
	out.Fields = slices.Clone(res.Fields)
	out.Endpoints = slices.Clone(res.Endpoints)
	if res.Storage != nil {
		storage := *res.Storage
		storage.Relations = slices.Clone(res.Storage.Relations)
		out.Storage = &storage
	}

	if cfg.Resource != "" {
		out.Name = cfg.Resource
		out.Plural = n.Go(n.Plural(cfg.Resource))
		out.PathSegment = n.PathSegment(out.Plural)
	}
	if cfg.Plural != "" {
		out.Plural = cfg.Plural
		out.PathSegment = n.PathSegment(out.Plural)
	}
	if cfg.PathSegment != "" {
		out.PathSegment = cfg.PathSegment
	}
	if cfg.Comment != "" {
		out.Description = cfg.Comment
		t.Comment = cfg.Comment
	}
	if len(cfg.Operations) > 0 {
		out.Operations = slices.Clone(cfg.Operations)
	}
	if len(cfg.Public) > 0 {
		out.Public = slices.Clone(cfg.Public)
	}
	if cfg.Expose != nil && !*cfg.Expose {
		// The model and the repository still come out; only the API stays
		// quiet. Clearing the operations too is what makes that true for every
		// generator without each of them having to remember.
		out.Unexposed = true
		out.Operations = nil
	}

	if out.Storage != nil {
		if cfg.RestoreWindowDays != nil && out.Storage.SoftDelete != nil {
			out.Storage.SoftDelete.RestoreWindowDays = *cfg.RestoreWindowDays
		}
		// rig_file's window comes from rig.yaml, and it wins: a project that
		// wrote the key anyway has been told to remove it by checkRestoreWindow,
		// and until it does the number the bytes are actually kept for is the
		// one to generate against.
		if t.Name == FileTable && opt.FileRestoreWindowDays > 0 && out.Storage.SoftDelete != nil {
			out.Storage.SoftDelete.RestoreWindowDays = opt.FileRestoreWindowDays
		}

		if len(cfg.OrderBy) > 0 {
			order, d := parseOrderBy(loaded, t, cfg.OrderBy)
			diags.Append(d)
			out.Storage.DefaultOrder = order
		}
	}

	if unreadable {
		// Its intent is unknown, so there is nothing to merge and nothing
		// useful to say about what it failed to mention.
		return out, diags
	}

	diags.Append(applyAccessConfig(loaded, &out, cfg))
	applyNotificationTable(&out, t, opt)
	diags.Append(applyOnDeleteConfig(loaded, &out, cfg))
	diags.Append(applyColumnConfig(loaded, t, &out, cfg, n, opt))
	diags.Append(applyRelationConfig(loaded, &out, cfg))
	diags.Append(applyElectricConfig(loaded, &out, cfg, n))
	diags.Append(applyEndpointConfig(loaded, &out, cfg, n))

	return out, diags
}

func applyColumnConfig(
	loaded *tableconf.Loaded,
	t *ir.Table,
	res *ir.Resource,
	cfg tableconf.File,
	n *naming.Namer,
	opt ConfigOptions,
) diag.List {
	var diags diag.List

	// Every configured column must exist.
	for name := range cfg.Columns {
		if t.Column(name) == nil {
			diags.Add(diag.CodeUnknownColumn, loaded.At("columns", name),
				"table %q has no column named %q", t.Name, name)
		}
	}

	fieldForColumn := make(map[string]int, len(res.Fields))
	for i := range res.Fields {
		if c := res.Fields[i].Column; c != nil {
			fieldForColumn[c.Name] = i
		}
	}

	var (
		excluded []string
		snapIgn  []string
	)

	// Iterate the schema, not the configuration, so column order stays the
	// database's and an unconfigured column is still visited.
	for ci := range t.Columns {
		col := &t.Columns[ci]
		cc, configured := cfg.Columns[col.Name]

		if !configured {
			if !IsManagedColumn(t.Name, col.Name) {
				diags.AddSeverity(diag.CodeUnmentionedColumn, opt.UnmentionedColumn,
					anchorForTable(loaded, t.Name, "columns"),
					"column %s.%s is not mentioned in its table configuration", t.Name, col.Name)
			}
			continue
		}

		if cc.Comment != "" {
			col.Comment = cc.Comment
			col.CommentSource = ir.CommentSourceConfig
		}
		if cc.SnapshotIgnore {
			snapIgn = append(snapIgn, col.Name)
		}

		fi, exposed := fieldForColumn[col.Name]
		if !exposed {
			// The column failed to project — an unmappable type, already
			// reported. Nothing further to say about it.
			continue
		}

		if cc.Exclude {
			excluded = append(excluded, col.Name)
			if requiredForCreate(col) && res.Supports(ir.OpCreate) {
				diags.Add(diag.CodeExcludeBreaksCreate, loaded.At("columns", col.Name, "exclude"),
					"column %s.%s is NOT NULL with no default, so excluding it makes %s impossible to create",
					t.Name, col.Name, res.Name)
			}
			continue
		}

		f := &res.Fields[fi]

		if cc.Field != "" {
			f.Name = cc.Field
			f.Wire = n.JSON(cc.Field)
		}
		if cc.Comment != "" {
			f.Description = cc.Comment
		}
		if cc.Example != "" {
			f.Example = cc.Example
		}

		if cc.Format != "" {
			if f.Type != ir.TypeString {
				diags.Add(diag.CodeInvalidFormat, loaded.At("columns", col.Name, "format"),
					"format %q applies to text columns, but %s.%s is %s",
					cc.Format, t.Name, col.Name, col.SQLType)
			} else {
				f.Format = cc.Format
			}
		}

		if cc.GoType != "" {
			f.GoType = cc.GoType
			if f.IsNullable() {
				f.GoType = pointerTo(cc.GoType)
			}
		}

		if cc.Immutable {
			if !col.Writable() {
				diags.Add(diag.CodeImmutableUnwritable, loaded.At("columns", col.Name, "immutable"),
					"column %s.%s is computed by the database, so it is already read-only",
					t.Name, col.Name)
			}
			f.Immutable = true
		}
		if cc.ReadOnly {
			f.ReadOnly = true
		}

		switch {
		case len(cc.Operations) > 0:
			ops, d := checkFieldOperations(loaded, t, col, cc.Operations, f.ReadOnly)
			diags.Append(d)
			res.Fields[fi].Operations = ops
		case f.ReadOnly:
			res.Fields[fi].Operations = []string{ir.FieldOpRead}
		case f.Immutable:
			res.Fields[fi].Operations = []string{ir.FieldOpRead, ir.FieldOpCreate}
		}
	}

	if len(excluded) > 0 {
		res.Fields = slices.DeleteFunc(res.Fields, func(f ir.ResourceField) bool {
			return f.Column != nil && slices.Contains(excluded, f.Column.Name)
		})
	}

	if len(snapIgn) > 0 {
		if res.Storage != nil && res.Storage.Snapshot != nil {
			snapshot := *res.Storage.Snapshot
			snapshot.IgnoreColumns = snapIgn
			res.Storage.Snapshot = &snapshot
		}
		// A snapshot_ignore on a table that keeps no snapshots is reported by
		// the validation rules, which see the whole document.
	}

	return diags
}

// requiredForCreate reports whether a client must supply a value for a column.
func requiredForCreate(c *ir.Column) bool {
	return !c.Nullable && !c.HasDefault && c.Writable()
}

func checkFieldOperations(loaded *tableconf.Loaded, t *ir.Table, col *ir.Column, ops []string, readOnly bool) ([]string, diag.List) {
	var diags diag.List

	out := slices.Clone(ops)
	// Read is implied: a field you can write but never see is not something the
	// configuration should be able to express by accident.
	if !slices.Contains(out, ir.FieldOpRead) {
		out = append([]string{ir.FieldOpRead}, out...)
	}

	if readOnly || !col.Writable() {
		for _, op := range out {
			if op == ir.FieldOpCreate || op == ir.FieldOpUpdate {
				diags.Add(diag.CodeOperationUnsupported, loaded.At("columns", col.Name, "operations"),
					"column %s.%s cannot be written, so it cannot take part in %s",
					t.Name, col.Name, op)
			}
		}
		return []string{ir.FieldOpRead}, diags
	}

	return out, diags
}

func parseOrderBy(loaded *tableconf.Loaded, t *ir.Table, order []string) ([]ir.OrderTerm, diag.List) {
	var diags diag.List
	out := make([]ir.OrderTerm, 0, len(order))

	for i, entry := range order {
		name, desc := strings.TrimPrefix(entry, "-"), strings.HasPrefix(entry, "-")
		if t.Column(name) == nil {
			diags.Add(diag.CodeOrderByUnknown, loaded.At("order_by", strconv.Itoa(i)),
				"order_by names %q, which is not a column of %s", name, t.Name)
			continue
		}
		out = append(out, ir.OrderTerm{Column: name, Desc: desc})
	}
	return out, diags
}

func applyEnumConfig(loaded *tableconf.Loaded, enums []ir.Enum, enumIndex map[string]int, renamed map[string]string) diag.List {
	var diags diag.List
	if loaded == nil {
		return diags
	}

	for pgType, ec := range loaded.File.Enums {
		ei, ok := enumIndex[pgType]
		if !ok {
			diags.Add(diag.CodeUnknownEnum, loaded.At("enums", pgType),
				"no enum type named %q exists in the database", pgType)
			continue
		}

		e := &enums[ei]
		if ec.Name != "" && ec.Name != e.Name {
			renamed[e.Name] = ec.Name
			e.Name = ec.Name
		}
		if ec.Description != "" {
			e.Description = ec.Description
		}

		for label := range ec.Values {
			if !hasEnumValue(e, label) {
				diags.Add(diag.CodeUnknownEnumValue, loaded.At("enums", pgType, "values", label),
					"enum %q has no value %q", pgType, label)
			}
		}

		for vi := range e.Values {
			v := &e.Values[vi]
			vc, configured := ec.Values[v.Wire]
			if !configured {
				continue
			}
			if vc.Name != "" {
				v.Name = vc.Name
			}
			if vc.Description != "" {
				v.Description = vc.Description
			}
		}
	}

	return diags
}

func hasEnumValue(e *ir.Enum, wire string) bool {
	for _, v := range e.Values {
		if v.Wire == wire {
			return true
		}
	}
	return false
}

func applyRelationConfig(loaded *tableconf.Loaded, res *ir.Resource, cfg tableconf.File) diag.List {
	var diags diag.List
	if res.Storage == nil {
		return diags
	}

	byTable := make(map[string]int, len(res.Storage.Relations))
	for i, r := range res.Storage.Relations {
		// A belongs-to or has-many relation is keyed by the table at the far
		// end; a many-to-many by the join table that carries it.
		key := r.ForeignTable
		if r.LinkTable != nil {
			key = r.LinkTable.Table
		}
		if key != "" {
			byTable[key] = i
		}
	}

	for table, rc := range cfg.Relations {
		ri, ok := byTable[table]
		if !ok {
			diags.Add(diag.CodeUnknownRelation, loaded.At("relations", table),
				"%s has no relation through %q", res.Name, table)
			continue
		}
		if rc.Name != "" {
			res.Storage.Relations[ri].Name = rc.Name
		}
		res.Storage.Relations[ri].Embed = rc.Embed
	}

	return diags
}

// applyAccessConfig resolves the column an owner-scoped read filters on.
//
// It defaults to the created-by audit column, which every generated write
// already stamps — so what a read narrows to and what a write records are the
// same fact. That used to be the only answer, on the argument that there was no
// way to point the filter at a column nothing maintains.
//
// `access.owner` breaks that premise honestly rather than quietly. Nothing
// audits an inbox line's account_id — it is not who acted — but it is NOT NULL,
// it is written by the engine and by nothing else, and it is therefore not a
// column nothing maintains. The premise was about columns a caller can leave
// empty, and the checks below are what keep it to that: a named column must be a
// uuid referencing rig_account, and it must not be nullable. The nullability the
// audit column is allowed to have — "a row created by a migration or by a
// service has no account behind it… invisible to a narrow read, which is the
// correct answer and a surprising one" — is exactly what a column like this must
// not have, and here it can be checked rather than tolerated.
func applyAccessConfig(loaded *tableconf.Loaded, res *ir.Resource, cfg tableconf.File) diag.List {
	var diags diag.List
	if cfg.Access == nil {
		return diags
	}

	at := loaded.At("access", "scope")
	switch cfg.Access.Scope {
	case "", accessScopeTenant:
		return diags
	case accessScopeOwn:
	default:
		diags.Add(diag.CodeConfigInvalid, at,
			"access.scope must be %q or %q, not %q",
			accessScopeTenant, accessScopeOwn, cfg.Access.Scope)
		return diags
	}

	if res.Storage == nil {
		diags.Add(diag.CodeConfigInvalid, at,
			"access.scope: own needs a table to filter, and %s is not stored as one", res.Name)
		return diags
	}
	if named := cfg.Access.Owner; named != "" {
		owner, d := ownerColumn(loaded, res, named)
		diags.Append(d)
		if owner != nil {
			res.Storage.Owner = owner
		}
		return diags
	}

	if res.Storage.Audit == nil || res.Storage.Audit.CreatedBy == nil {
		diags.Add(diag.CodeConfigInvalid, at,
			"access.scope: own filters on who created a row, so %s needs a created_by_account_id "+
				"column — or an `access.owner` naming the column that says who the row is for",
			res.Storage.Table)
		return diags
	}
	// The column is nullable by convention, and has to be: a row created by a
	// migration or by a service has no account behind it. Such a row is then
	// invisible to a narrow read, which is the correct answer and a surprising
	// one. It is not a diagnostic, because it would fire on every owner-scoped
	// table and a warning nobody can act on is one everybody learns to skip; the
	// generated repository says it where somebody reading the query will see it.
	owner := *res.Storage.Audit.CreatedBy
	res.Storage.Owner = &owner
	return diags
}

// applyOnDeleteConfig moves the named children to the front of the derived
// order.
//
// Listing some of them rather than all is the ordinary case: the reason to write
// this key at all is one pair whose derived order is wrong, and demanding the
// whole sequence would mean a list that has to be edited every time a table is
// added — and, worse, a list that silently stops mentioning one.
func applyOnDeleteConfig(loaded *tableconf.Loaded, res *ir.Resource, cfg tableconf.File) diag.List {
	var diags diag.List
	if cfg.OnDelete == nil || len(cfg.OnDelete.Order) == 0 {
		return diags
	}

	known := make(map[string]bool, len(res.Children))
	for _, c := range res.Children {
		known[c.Table] = true
	}

	var (
		front []ir.ChildLink
		taken = make(map[string]bool, len(cfg.OnDelete.Order))
	)
	for i, table := range cfg.OnDelete.Order {
		at := loaded.At("on_delete", "order")
		if !known[table] {
			diags.Add(diag.CodeDeleteOrderUnknown, at,
				"on_delete.order names %q, which has no foreign key to %s; the order is over "+
					"the tables that reference this one", table, res.Storage.Table)
			continue
		}
		if taken[table] {
			diags.Add(diag.CodeDeleteOrderUnknown, at,
				"on_delete.order names %q twice; position %d is the one that would win", table, i+1)
			continue
		}
		taken[table] = true
		for _, c := range res.Children {
			if c.Table == table {
				front = append(front, c)
			}
		}
	}

	rest := make([]ir.ChildLink, 0, len(res.Children))
	for _, c := range res.Children {
		if !taken[c.Table] {
			rest = append(rest, c)
		}
	}
	res.Children = append(front, rest...)
	return diags
}

// applyNotificationTable settles what is true of rig's own two notification
// tables in every project, from the one block that configures them.
//
// Three things, and each is somewhere a table configuration would be the wrong
// home. **The tables stay in the schema whenever notifications are enabled**,
// and `expose` marks them unexposed instead of dropping them — a departure from
// how `files.expose` works, and the one trap in this milestone: link tables are
// classified against a map built after the ignored tables have been removed, so
// a dropped rig_notification would make every project's link tables silently
// stop being link tables and every notifiable resource silently stop being one.
//
// **The inbox narrows to the caller**, on account_id rather than on the audit
// column, because an inbox line belongs to the person it is addressed to.
//
// **The inbox is the only one with a live-sync shape.** That is a security
// statement rather than a convenience: rig_notification holds rows that are
// pending for people who are not recipients yet and may never be, so a
// tenant-scoped shape over it would stream Friday's unpublished announcement to
// the whole tenant on Monday. The recipient row carries its own copy of `kind`
// so a subscriber never needs the join.
func applyNotificationTable(res *ir.Resource, t *ir.Table, opt ConfigOptions) {
	cfg := opt.Notifications
	if cfg == nil || !cfg.Enabled || res.Storage == nil {
		return
	}
	if !isNotificationTable(t.Name) {
		return
	}

	if !cfg.Expose {
		res.Unexposed = true
		res.Operations = nil
	}

	if t.Name != NotificationRecipientTable {
		return
	}

	if res.Storage.Owner == nil {
		for i := range res.Fields {
			c := res.Fields[i].Column
			if c == nil || c.Name != NotificationRecipientOwner {
				continue
			}
			owner := *c
			res.Storage.Owner = &owner
			break
		}
	}

	if res.Electric == nil {
		res.Electric = &ir.ElectricEndpoint{
			Auth: ir.ElectricAuthTenant,
			Path: "/electric/" + res.Storage.Table,
		}
	}
}

// ownerColumn resolves and checks a named `access.owner`.
//
// Three checks, and each of them is a way the read would otherwise fail quietly.
// A column that is not on the table is a typo that would narrow to nothing. One
// that is not a uuid referencing rig_account is a filter comparing an account
// identifier against something else, which matches no rows and says so nowhere.
// And a nullable one is a row every narrow read is blind to — which is tolerable
// for an audit column, because "nobody created this" is a real state, and is not
// tolerable for a column whose whole job is to say whose row this is.
func ownerColumn(loaded *tableconf.Loaded, res *ir.Resource, name string) (*ir.ColumnRef, diag.List) {
	var diags diag.List
	at := loaded.At("access", "owner")

	var owner *ir.ColumnRef
	for i := range res.Fields {
		if c := res.Fields[i].Column; c != nil && c.Name == name {
			ref := *c
			ref.Table = res.Storage.Table
			owner = &ref
			break
		}
	}
	if owner == nil {
		diags.Add(diag.CodeUnknownColumn, at,
			"access.owner names %q, and %s has no such column", name, res.Storage.Table)
		return nil, diags
	}

	if owner.SQLType != "uuid" {
		diags.Add(diag.CodeConfigInvalid, at,
			"access.owner names %s.%s, which is %s; an owner column holds an account "+
				"identifier and has to be uuid", res.Storage.Table, name, owner.SQLType)
		return nil, diags
	}

	if !referencesAccounts(res, name) {
		diags.Add(diag.CodeConfigInvalid, at,
			"access.owner names %s.%s, which does not reference rig_account; the filter compares "+
				"it against the caller's account, so a column pointing somewhere else matches "+
				"nothing and reports nothing", res.Storage.Table, name)
		return nil, diags
	}

	if owner.Nullable {
		diags.Add(diag.CodeConfigInvalid, at,
			"access.owner names %s.%s, which is nullable; a row with no owner is invisible to "+
				"every narrow read and nothing reports it, so the column has to be NOT NULL",
			res.Storage.Table, name)
		return nil, diags
	}

	return owner, diags
}

// referencesAccounts reports whether a column is a foreign key to rig_account.
//
// Read off the resource's own relations rather than the table's constraints,
// because the relations are where the composite tenant-carrying form has already
// been denormalized onto the column that carries the meaning.
func referencesAccounts(res *ir.Resource, column string) bool {
	for _, rel := range res.Storage.Relations {
		if rel.Kind == ir.RelationBelongsTo && rel.LocalColumn == column {
			return rel.ForeignTable == AccountTable
		}
	}
	return false
}

func applyElectricConfig(loaded *tableconf.Loaded, res *ir.Resource, cfg tableconf.File, n *naming.Namer) diag.List {
	var diags diag.List
	if cfg.Electric == nil || !cfg.Electric.Enabled || res.Storage == nil {
		return diags
	}

	auth := ir.ElectricAuth(cfg.Electric.Auth)
	if auth == "" {
		auth = ir.ElectricAuthTenant
	}

	e := &ir.ElectricEndpoint{
		Auth: auth,
		Path: "/electric/" + res.Storage.Table,
	}

	names := make([]string, 0, len(cfg.Electric.Params))
	for name := range cfg.Electric.Params {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		p := cfg.Electric.Params[name]
		if reservedElectricParam(name) {
			diags.Add(diag.CodeInvalidEndpoint, loaded.At("electric", "params", name),
				"%q is reserved by the sync protocol and cannot be a parameter name", name)
			continue
		}
		e.Params = append(e.Params, ir.ElectricParam{
			Name:        name,
			Field:       n.Go(name),
			Type:        p.Type,
			Optional:    p.Optional,
			Description: p.Description,
		})
	}

	res.Electric = e
	return diags
}

// reservedElectricParam names the query parameters the sync protocol owns.
// Accepting one would silently break streaming rather than fail loudly.
func reservedElectricParam(name string) bool {
	switch strings.ToLower(name) {
	case "offset", "handle", "live", "cursor", "table", "where", "columns", "replica":
		return true
	default:
		return false
	}
}

func applyEndpointConfig(loaded *tableconf.Loaded, res *ir.Resource, cfg tableconf.File, n *naming.Namer) diag.List {
	var diags diag.List

	seen := make(map[string]bool, len(cfg.Endpoints))
	for i, ec := range cfg.Endpoints {
		at := loaded.At("endpoints", strconv.Itoa(i), "name")
		if seen[ec.Name] {
			diags.Add(diag.CodeInvalidEndpoint, at,
				"%s already declares an endpoint named %q", res.Name, ec.Name)
			continue
		}
		seen[ec.Name] = true

		e := ir.Endpoint{
			Name:        ec.Name,
			Method:      ec.Method,
			Path:        ec.Path,
			Summary:     ec.Summary,
			Description: ec.Description,
			Permission:  ec.Permission,
			// The table's public list is the other way to say this, and it is
			// resolved in one place during expansion rather than here as well.
			Public: ec.Public,
			Impl: ir.EndpointImpl{
				Kind:          ir.EndpointCustom,
				ServiceMethod: ec.Name,
				HandlerName:   ec.Name + res.Name,
			},
			// Even a hand-written endpoint can fail in the standard ways, and
			// leaving these out would make its documentation claim otherwise.
			Errors: []int{400, 401, 403, 429, 500},
		}

		req := ec.Req()
		e.Request = ir.EndpointRequest{
			PathParams:  convertParams(req.PathParams, n),
			QueryParams: convertParams(req.QueryParams, n),
			BodyParams:  convertParams(req.Body, n),
			BodyObject:  req.BodyObject,
		}
		if len(e.Request.BodyParams) > 0 || e.Request.BodyObject != "" {
			e.Request.ContentTypes = []string{MediaJSON}
		}
		if len(e.Request.BodyParams) > 0 && e.Request.BodyObject != "" {
			diags.Add(diag.CodeInvalidEndpoint, loaded.At("endpoints", strconv.Itoa(i), "request"),
				"endpoint %q declares both body fields and a body object; pick one", ec.Name)
		}
		if len(e.Request.PathParams) > 0 {
			e.Errors = append(e.Errors, 404)
		}
		for j, p := range e.Request.QueryParams {
			if p.Wire == ir.ScopeParam {
				diags.Add(diag.CodeReservedName,
					loaded.At("endpoints", strconv.Itoa(i), "request", "query_params", strconv.Itoa(j), "name"),
					"%q is reserved: it is how a caller widens a read past its own rows, so an endpoint cannot mean something else by it",
					ir.ScopeParam)
			}
		}

		for _, rc := range ec.Responses {
			e.Responses = append(e.Responses, ir.EndpointResponse{
				StatusCode:   rc.Status,
				Description:  rc.Description,
				BodyObject:   rc.BodyObject,
				BodyFields:   convertParams(rc.BodyFields, n),
				ContentTypes: responseContentTypes(rc),
			})
		}
		if len(e.Responses) == 0 {
			diags.Add(diag.CodeInvalidEndpoint, loaded.At("endpoints", strconv.Itoa(i)),
				"endpoint %q declares no responses", ec.Name)
		}

		slices.Sort(e.Errors)
		e.Errors = slices.Compact(e.Errors)

		res.Endpoints = append(res.Endpoints, e)
	}

	return diags
}

// responseContentTypes is JSON or nothing.
//
// A custom endpoint cannot declare a content type of its own — M5.9 reads the
// field for the endpoints rig synthesizes and does not let a table configuration
// write it, because a declared binary body means the service method stops
// receiving a decoded body in the general case. That is a milestone of its own.
func responseContentTypes(rc tableconf.EndpointResponse) []string {
	if rc.BodyObject == "" && len(rc.BodyFields) == 0 {
		return nil
	}
	return []string{MediaJSON}
}

func convertParams(params []tableconf.Param, n *naming.Namer) []ir.Field {
	if len(params) == 0 {
		return nil
	}
	out := make([]ir.Field, 0, len(params))
	for _, p := range params {
		f := ir.Field{
			Name:        p.Name,
			Wire:        n.JSON(p.Name),
			Type:        p.Type,
			Description: p.Description,
			Default:     p.Default,
		}
		if p.Array {
			f.Modifiers = append(f.Modifiers, ir.ModifierArray)
		}
		if p.Optional {
			f.Modifiers = append(f.Modifiers, ir.ModifierNullable)
		}
		f.GoType = paramGoType(f)
		out = append(out, f)
	}
	return out
}

// paramGoType renders a declared parameter's Go type.
//
// A name that is not a primitive is an enum or object declared elsewhere, and
// its Go type is the name itself. Resolution of whether that name exists at all
// happens at freeze; guessing here would report the same mistake twice.
func paramGoType(f ir.Field) string {
	base := f.Type
	if m, ok := pgtypes.Primitive(f.Type); ok {
		base = m.GoType
	}
	if f.IsArray() {
		return "[]" + base
	}
	if f.IsNullable() {
		return pointerTo(base)
	}
	return base
}

// anchorForTable points at a key in a table's configuration.
//
// A table with no configuration file at all has no position to offer, so the
// anchor falls back to a dotted path naming the table. That keeps the rendered
// diagnostic readable — "article.columns: ..." rather than a bare colon.
func anchorForTable(loaded *tableconf.Loaded, table string, segments ...string) diag.Anchor {
	if loaded == nil {
		return diag.At(strings.Join(append([]string{table}, segments...), "."))
	}
	return loaded.At(segments...)
}

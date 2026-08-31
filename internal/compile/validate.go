package compile

import (
	"slices"
	"strconv"
	"strings"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/scaffold"
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
		diags.Append(checkElectricReplication(doc, t, res, loaded))
		if !unreadable {
			diags.Append(checkRestoreWindow(t, res, loaded, fileRestoreWindowDays(p),
				p.Config.Notifications.Enabled))
			diags.Append(checkSnapshotIgnore(t, res, loaded))
		}

		// Convention.
		if !unreadable {
			diags.Append(checkComments(t, loaded, sev(v.MissingComment, diag.CodeMissingTableComment)))
		}
		diags.Append(checkIndexes(t, loaded,
			sev(v.ForeignKeyNotIndexed, diag.CodeForeignKeyNotIndexed),
			sev(v.TenantIndexLeading, diag.CodeTenantIndexNotLeading)))
		diags.Append(checkColumnNaming(t, loaded, res != nil && res.Foundation,
			sev(v.BooleanPrefix, diag.CodeBooleanPrefix),
			sev(v.TimestampSuffix, diag.CodeTimestampSuffix),
			sev(v.DateSuffix, diag.CodeDateSuffix),
			sev(v.ForeignKeyNaming, diag.CodeForeignKeyNaming)))
		diags.Append(checkCascades(t, loaded, sev(v.CascadeDelete, diag.CodeCascadeDelete)))
	}

	diags.Append(checkEnumConsistency(doc))
	diags.Append(checkCustomEndpoints(doc, set))
	diags.Append(checkFoundationJSONCase(doc, p))
	diags.Append(checkCacheHasReaders(doc, p))
	diags.Append(checkElectricWALLevel(doc, p))

	return diags
}

// checkElectricReplication refuses live sync on a table Postgres will not
// publish.
//
// A shape is not a query rig runs. It is a filter in front of a stream the sync
// service follows over logical replication, and logical replication carries only
// what a publication names. So everything about the endpoint can be right — the
// route mounted, the caller authenticated, the tenant predicate built — and
// there still be nothing behind it.
//
// The three halves are refused for different reasons, and only one of them is
// absolute. An UNLOGGED table can never be followed by anything. An unpublished
// table usually can be, because the sync service publishes it on the first
// subscription — but only from a role that owns it, since Postgres wants
// ownership both to add a table to a publication and to set REPLICA IDENTITY
// FULL. A least-privilege deployment fails there, on the subscription, as an
// error about access rather than about replication. Saying it in a migration is
// what makes the answer the same in every environment.
//
// Which is why Electric's own publication is not evidence. It maintains
// `electric_publication_default` itself and adds a table to it on the first
// subscription, so on any machine where `rig db up` has served one shape the
// table is published — by exactly the mechanism this rule exists to stop a
// project depending on. Counting it would leave the rule silent on every
// developer's machine and loud only where nobody has run the app, which is the
// wrong way round for a rule about what happens somewhere else.
//
// The replica identity is the second half of the publication's job and is gated on
// exactly the same privilege, which is why it is checked here rather than
// somewhere of its own: Postgres wants ownership to publish a table and to set its
// replica identity, so a migration that does one and not the other has moved half
// of what a least-privilege deployment cannot do for itself. It is read off the
// table rather than the server, so it has a nil check of its own — empty means
// nobody looked, and a rule reported on evidence nobody collected is the failure
// the paragraph below is about.
//
// This is the only rule here that reads the schema's replication block, and the
// nil check is the whole reason it can be an error. Nil means nobody looked: a
// dump written before rig read these facts, or a hand-written fixture. Silence
// is the only honest answer then, because reporting a table as unpublished on
// evidence nobody collected would fail `rig validate --schema` on a correct
// project.
func checkElectricReplication(doc *ir.Document, t *ir.Table, res *ir.Resource,
	loaded *tableconf.Loaded,
) diag.List {
	var diags diag.List

	if res == nil || res.Electric == nil || doc.Schema.Replication == nil {
		return diags
	}
	where := at(loaded, t, "electric", "enabled")

	// An UNLOGGED table can be published — Postgres accepts the ALTER — and it
	// still emits nothing, so this is reported even when the publication is
	// there. Reporting only the membership would send somebody to fix the half
	// that was already right.
	if t.Unlogged {
		diags.Add(diag.CodeElectricUnlogged, where,
			"table %q asks for live sync and is UNLOGGED: it writes no WAL, so its shape "+
				"would stream nothing", t.Name)
	}

	// Reported whatever the publication says, and before it, because the two are one
	// job: publishing a table and giving it an identity are both gated on ownership,
	// so a project that did one is a project that could have done the other. Silent on
	// an empty value, which is a schema nobody read this off — see above.
	if t.ReplicaIdentity != "" && t.ReplicaIdentity != ir.ReplicaIdentityFull {
		diags.Add(diag.CodeElectricReplicaIdentity, where,
			"table %q asks for live sync and its replica identity is %s: %s, so a subscriber "+
				"has nothing to match an updated or deleted row against",
			t.Name, t.ReplicaIdentity, replicaIdentityCost(t.ReplicaIdentity))
	}

	if slices.ContainsFunc(t.Publications, func(name string) bool {
		return migrationOwned(doc.Schema.Replication, name)
	}) {
		return diags
	}

	// The three ways of being unpublished want different next steps — write the
	// migration, add one table to the publication that is already there, or stop
	// relying on the one Electric wrote — so the message says which one this is.
	// None of them claims the stream is empty: the sync service may well publish
	// the table on the first subscription, and what this rule is about is not
	// depending on that.
	switch names := publicationNames(doc.Schema.Replication); {
	case len(t.Publications) > 0:
		diags.Add(diag.CodeElectricNotPublished, where,
			"table %q asks for live sync, and the only publication carrying it (%s) is one the "+
				"sync service created and owns: it added the table there itself, which it can "+
				"only do where its database role owns the table",
			t.Name, strings.Join(t.Publications, ", "))
	case len(names) == 0:
		diags.Add(diag.CodeElectricNotPublished, where,
			"table %q asks for live sync, and this database has no publication at all: "+
				"nothing has told Postgres to replicate it", t.Name)
	default:
		diags.Add(diag.CodeElectricNotPublished, where,
			"table %q asks for live sync, but no publication carries it (this database has %s): "+
				"nothing has told Postgres to replicate it",
			t.Name, strings.Join(names, ", "))
	}

	return diags
}

// replicaIdentityCost says what this identity puts in the WAL for the row as it
// was, which is the fact the three non-FULL values differ on.
//
// Three sentences rather than one, because the next step differs: `default` is
// what a table has when nobody has done anything, `index` is somebody's
// deliberate choice that live sync cannot use, and `nothing` is a table that
// cannot report a change at all.
func replicaIdentityCost(id ir.ReplicaIdentity) string {
	switch id {
	case ir.ReplicaIdentityDefault:
		return "the default carries the primary key of the old row and no other column"
	case ir.ReplicaIdentityIndex:
		return "an index identity carries only that index's columns, which live sync cannot use"
	case ir.ReplicaIdentityNothing:
		return "nothing carries no old row at all"
	default:
		return "live sync needs the whole old row"
	}
}

// SyncServicePublication reports whether a publication carries the name the sync
// service uses for its own.
//
// The name is `electric_publication_` followed by the replication stream id, which
// is "default" unless a deployment sets one. Matching the prefix rather than the
// whole name is what keeps this true under a deployment that does.
//
// The name alone does not say who made it, which is the whole of
// [migrationOwned]: the sync service is not the only thing that may create this
// publication, and a migration that creates it first is the arrangement that makes
// live sync work for a role owning no tables.
//
// Exported because two things have to agree about it and they are in different
// packages: this package reports a table as unpublished, and `rig migration new
// --publish-shapes` writes the migration that publishes it. A copy of the rule
// beside the fix is a way for the fix to write nothing where the rule fires.
func SyncServicePublication(name string) bool {
	return strings.HasPrefix(name, "electric_publication_")
}

// migrationOwned reports whether a publication is one this project's migrations
// wrote, which is the only kind that answers RIG5090.
//
// Two ways to qualify, and the second is the one that took a real server to work
// out. A publication under a name of the project's own — `rig_publication` — was
// obviously written by a migration. And a publication under the sync service's own
// name qualifies too, **if the role that runs migrations owns it**, because that
// means a migration created it before the sync service ever connected.
//
// That second case is not a loophole. The sync service reads only its own
// publication: a `rig_publication` is never consulted, so a table published there
// and nowhere else streams nothing for a role that owns no tables — the very
// deployment this rule exists for. Creating that publication in a migration, ahead
// of the service, and running the service with
// ELECTRIC_MANUAL_TABLE_PUBLISHING=true, is what actually works. Refusing it would
// mean refusing the answer.
//
// What stays refused is the publication the sync service made for itself. Owned by
// its own role, created on first connection, and a table in it got there because
// the service had privileges it will not have elsewhere — which is the dependency
// this rule is about, and the reason the owner rather than the name is what
// decides.
//
// A publication under a third role — some other tool's — is not evidence either,
// on the same reasoning: whatever maintains it is not this project's migrations.
func migrationOwned(rep *ir.Replication, name string) bool {
	if !SyncServicePublication(name) {
		return true
	}
	if rep == nil {
		return false
	}
	for _, p := range rep.Publications {
		if p.Name == name {
			return p.Owned
		}
	}
	return false
}

// checkElectricWALLevel refuses live sync on a server that cannot decode a
// publication at all.
//
// Once per document rather than once per table: the setting is the server's, and
// a project with a dozen shapes has one thing to fix, not a dozen.
func checkElectricWALLevel(doc *ir.Document, p *project.Project) diag.List {
	var diags diag.List

	rep := doc.Schema.Replication
	if rep == nil || rep.WALLevel == "" || rep.WALLevel == "logical" {
		return diags
	}
	if !slices.ContainsFunc(doc.API.Resources, func(r ir.Resource) bool { return r.Electric != nil }) {
		return diags
	}

	diags.Add(diag.CodeElectricWALLevel, p.At("database", "electric", "enabled"),
		"some table asks for live sync and this database runs with wal_level=%s: "+
			"no publication on it can be decoded", rep.WALLevel)

	return diags
}

// publicationNames lists the publications a database has, for a message that
// has to tell "you published this somewhere else" from "you have published
// nothing yet".
func publicationNames(rep *ir.Replication) []string {
	out := make([]string, 0, len(rep.Publications))
	for _, p := range rep.Publications {
		out = append(out, p.Name)
	}
	return out
}

// checkCacheHasReaders refuses a `cache:` block nothing would read.
//
// It is the same rule every other block carries — numbers somebody set and
// believed in, which nothing consults — and it is here rather than in
// `internal/project` because there are now two ways to satisfy it and that
// package can only see one of them. The reads rig makes for itself come with the
// `auth:` block; the reads it makes for a table come with `cache: true` in that
// table's own file. Either is enough. Neither, and the block is four numbers and
// a goroutine holding a Postgres connection open for nothing.
func checkCacheHasReaders(doc *ir.Document, p *project.Project) diag.List {
	var diags diag.List

	if doc.API.Cache == nil || !doc.API.Cache.Enabled {
		return diags
	}
	if p.Config.Auth.Enabled {
		return diags
	}
	if slices.ContainsFunc(doc.API.Resources, func(r ir.Resource) bool { return r.Cached }) {
		return diags
	}

	diags.Add(diag.CodeConfigInvalid, p.At("cache", "enabled"),
		"cache.enabled is true but nothing reads it: this project has no `auth:` block and no "+
			"table sets `cache: true`. Add one of those, or remove the block")

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
func checkRestoreWindow(t *ir.Table, res *ir.Resource, loaded *tableconf.Loaded, fileWindow int, notifications bool) diag.List {
	var diags diag.List
	if res == nil {
		return diags
	}

	soft := res.Storage.IsSoftDeletable()

	var configured *int
	if loaded != nil {
		configured = loaded.File.RestoreWindowDays
	}

	// The inbox is soft-deletable — dismissing a line is a soft delete against
	// the recipient row, so that one person clearing their inbox changes nothing
	// anybody else sees — and it has no table configuration in an ordinary
	// project to declare a window in. How long a dismissed line is kept is
	// `notifications.retention`, one number for every project, and a second copy
	// of it in a YAML file could only ever disagree.
	if t.Name == NotificationRecipientTable && notifications {
		if configured != nil {
			diags.Add(diag.CodeRestoreWindowForbidden, at(loaded, t, "restore_window_days"),
				"restore_window_days is set on %q, whose retention is notifications.retention "+
					"in rig.yaml: it is how long a dismissed line is kept as well as how long "+
					"it could be brought back, so there is one number and this is not where it "+
					"lives", t.Name)
		}
		return diags
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

func checkColumnNaming(t *ir.Table, loaded *tableconf.Loaded, rigsOwn bool, boolSev, tsSev, dateSev, fkSev diag.Severity) diag.List {
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

		// A file column no longer needs an exemption of its own. The rule used
		// to demand rig_file_id of profile_image_file_id — one file per table,
		// forever — or profile_image_rig_file_id; with the prefix optional,
		// `<role>_file_id` is the ordinary <qualifier>_file_id spelling for
		// rig_file, and the rule reaches it without one.
		//
		// Because a reference to one of rig's own tables is not exempt but is
		// allowed two spellings — see [foreignKeyNames], which is where that
		// carve-out went, along with notification_id and the inbox's account_id.

		// And every column of every table rig created, which a project cannot
		// rename and did not write. The rule is advice about a schema somebody
		// is writing, and this is not one of those — the advice here would be to
		// rename rig_account.tenant_id, in a migration the project does not own,
		// to satisfy a convention rig chose not to follow in its own DDL.
		//
		// It covered the notification tables only, because those were the ones a
		// project projected without a table configuration. Now every one of
		// rig's is projected that way.

		if fkSev != "" && c.ForeignKey != nil && !isAuditActorColumn(c.Name) &&
			!selfReference && !rigsOwn {
			want := foreignKeyNames(c.ForeignKey.Table)
			if !namedAfter(c.Name, want) {
				diags.AddSeverity(diag.CodeForeignKeyNaming, fkSev, at,
					"column %s.%s references %s, so it should be named %s",
					t.Name, c.Name, c.ForeignKey.Table, foreignKeyAdvice(want))
			}
		}
	}

	return diags
}

// foreignKeyNames are the column names the convention accepts for a foreign key
// to target.
//
// `<target>_id` always, and — when the target is one of rig's own tables — the
// same without the rig_ prefix. The prefix is there so a project can tell rig's
// tables from its own in psql, not so every foreign key to one has to repeat
// it: tenant_id names what it points at as plainly as rig_tenant_id does, and
// it is what the project would have written had the table been its own.
//
// rig's own DDL already agrees — every foundation table scoped to a tenant
// calls the column tenant_id — and so does the projection, where
// [relationName] takes the column stem and yields the accessor Tenant rather
// than RigTenant. This rule was the last thing asking for the prefix back.
//
// It was three carve-outs before it was a rule: the notification link table's
// notification_id, the inbox's account_id, and a project's own tenant_id, which
// never got one and is what sent the other two looking for a shared reason. The
// file column's went the same way, by a different argument arriving in the same
// place: profile_image_file_id names the role rather than the target, and once
// the prefix is optional that is <qualifier>_file_id against rig_file rather
// than an exception to the rule. [isFileColumn] still decides what a file column
// is everywhere else; it just no longer has to be consulted here.
func foreignKeyNames(target string) []string {
	if bare := strings.TrimPrefix(target, scaffold.TablePrefix); bare != target && bare != "" {
		return []string{bare + "_id", target + "_id"}
	}
	return []string{target + "_id"}
}

// namedAfter reports whether a column is named after one of the accepted
// targets, bare or behind a <qualifier>_ prefix.
func namedAfter(column string, names []string) bool {
	return slices.ContainsFunc(names, func(want string) bool {
		return column == want || strings.HasSuffix(column, "_"+want)
	})
}

// foreignKeyAdvice is the "should be named ..." half of the RIG6030 message.
//
// One accepted name reads as the rule always did. Two have to say that the
// qualifier applies to both, or the message would look like a choice between a
// bare name and a qualified one.
func foreignKeyAdvice(names []string) string {
	if len(names) == 1 {
		return names[0] + " or <qualifier>_" + names[0]
	}
	return strings.Join(names, " or ") + ", either with a <qualifier>_ prefix"
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

// checkFoundationJSONCase reports an exposed table of rig's whose JSON keys will
// not follow this project's `naming.json_case`.
//
// The Go for rig's notification tables and for rig_account is compiled once, in
// a module that ships rather than in this project, so its struct tags are fixed
// — and Go struct tags cannot be parameterised. A project that asked for
// snake_case therefore gets it on its own tables and camelCase on rig's.
//
// A warning rather than a refusal, because the project has done nothing wrong and
// the mixture is not new: rig's hand-written routes have answered camelCase since
// they existed, so /auth/login already disagrees with a snake_case API. What is
// new is that the disagreement now reaches a resource with a filter grammar and a
// typed client, which is worth saying out loud rather than leaving to be found in
// a response body.
func checkFoundationJSONCase(doc *ir.Document, p *project.Project) diag.List {
	var diags diag.List

	if c := p.Config.Naming.JSONCase; c == "" || c == "camel" {
		return diags
	}

	for i := range doc.API.Resources {
		r := &doc.API.Resources[i]
		if r.Unexposed || r.Storage == nil || !isShippedModel(r.Storage.Table) {
			continue
		}
		diags.Add(diag.CodeFoundationJSONCase, diag.At(r.Storage.Table),
			"%s is rig's own table and its Go is compiled with camelCase keys, so it will not "+
				"follow naming.json_case: %s", r.Storage.Table, p.Config.Naming.JSONCase)
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
			// A live-sync shape is deliberately not one of these, and the
			// rule used to say it was. A shape route is mounted from
			// Handlers.Shapes and has never consulted `expose`, so it is served
			// either way — and the combination is not a contradiction but a
			// shape somebody wants: the inbox has no CRUD surface at all and
			// goes live the moment a row commits. What `expose` decides is
			// whether there are REST endpoints; what `electric` decides is
			// whether there is a stream, and those are different questions.
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

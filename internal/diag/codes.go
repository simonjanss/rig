package diag

import (
	"cmp"
	"slices"
)

// Code identifies a class of problem. Codes are stable: a reader who has seen
// RIG3101 once should recognize it forever, and CI can suppress or promote a
// specific one without pattern-matching on message text.
//
// Every code carries its own documentation, so there is no separate table to
// fall out of sync with the constants.
type Code struct {
	// ID is the stable identifier, for example "RIG3101".
	ID string
	// Severity is what the code reports at unless a rule overrides it. Rules
	// whose severity comes from the project configuration pass it explicitly.
	Severity Severity
	// Summary is a one-line description of the problem class, used by the
	// generated documentation and by `rig codes`.
	Summary string
	// Hint, when set, is attached to every diagnostic with this code and tells
	// the reader what to do about it.
	Hint string
}

var registry = map[string]Code{}

// newCode registers a code. Registering the same ID twice panics at init, which
// makes a copy-pasted code a build-time failure rather than a confusing report.
func newCode(id string, sev Severity, summary, hint string) Code {
	if _, dup := registry[id]; dup {
		panic("diag: duplicate diagnostic code " + id)
	}
	c := Code{ID: id, Severity: sev, Summary: summary, Hint: hint}
	registry[id] = c
	return c
}

// Codes returns every registered code, ordered by ID.
func Codes() []Code {
	out := make([]Code, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	slices.SortFunc(out, func(a, b Code) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// LookupCode returns the code with the given ID.
func LookupCode(id string) (Code, bool) {
	c, ok := registry[id]
	return c, ok
}

// Reading the database: RIG1xxx.
var (
	CodeUnmappableType = newCode("RIG1001", SeverityError,
		"A column's Postgres type has no mapping to a Go type.",
		"Use a supported type, or add a domain over one.")

	CodeEnumWithoutValues = newCode("RIG1002", SeverityError,
		"A Postgres enum type has no labels.",
		"")

	CodeUnsupportedRelation = newCode("RIG1003", SeverityWarning,
		"A relation was skipped because rig cannot project it.",
		"")
)

// Projecting the API surface: RIG2xxx.
var (
	CodeNameCollision = newCode("RIG2001", SeverityError,
		"Two tables or columns project to the same API name.",
		"Rename one of them with the `resource:` or `field:` key.")

	CodeUnpluralizable = newCode("RIG2002", SeverityError,
		"A table name has no derivable plural.",
		"Set `plural:` on the table, or add an entry to `naming.plurals`.")

	CodeReservedName = newCode("RIG2003", SeverityError,
		"An endpoint's parameter uses a name rig reserves.",
		"Rename it with the `name:` key.")

	CodeReservedResource = newCode("RIG2004", SeverityError,
		"A table projects to a resource name rig's own tables take.",
		"Rename the table, or give it a `resource:` of its own. The names are "+
			"reserved whether or not this project exposes rig's tables, so that "+
			"turning on `auth.expose` later never forces the rename then.")

	CodeReservedTablePrefix = newCode("RIG2005", SeverityError,
		"A table name uses the `rig_` prefix, which rig keeps for its own tables.",
		"Rename the table. The prefix is what tells a reader which tables arrived "+
			"with the foundation, and the next part rig adds may create one by that "+
			"name outright.")
)

// Reading the table configuration: RIG3xxx.
var (
	CodeConfigSyntax = newCode("RIG3001", SeverityError,
		"A configuration file is not valid YAML.",
		"")

	CodeConfigInvalid = newCode("RIG3002", SeverityError,
		"A configuration file does not match its schema.",
		"Run `rig schema` to write the schema files, then point your editor at them for completion and inline errors.")

	CodeConfigFile = newCode("RIG3003", SeverityError,
		"A configuration file is misplaced, unreadable, or duplicated.",
		"")

	CodeFoundationMode = newCode("RIG3004", SeverityError,
		"`migrations.foundation` contradicts the migrations on disk, or `auth.own`.",
		"Pick the mode when the project is set up and leave it alone. The two modes "+
			"record what they applied in different tables, so switching after there is "+
			"a database would re-apply a schema that is already there — which fails "+
			"partway through `rig db up` rather than at the top. To change it, start "+
			"from an empty database. `auth.own` is the same contradiction stated the "+
			"other way: a project maintaining rig's tables itself cannot also leave "+
			"them to the modules.")

	CodeMonitoringWithoutTracing = newCode("RIG3005", SeverityError,
		"`monitoring:` is enabled and `tracing:` is not.",
		"The monitoring page is a reader over the span file that `tracing:` writes; "+
			"it stores nothing of its own. Without spans it would be empty forever, "+
			"which is a page that looks broken rather than one that is off. Set "+
			"`tracing: {enabled: true}`, or remove the `monitoring:` block.")

	CodeMonitoringPasswordInFile = newCode("RIG3006", SeverityWarning,
		"`monitoring.password` puts a secret in rig.yaml.",
		"rig.yaml is checked in, and the page it guards lists every path, request id "+
			"and error cause this server has seen. Leave the key out and the page "+
			"reads `monitoring.password_env` — $RIG_MONITOR_PASSWORD by default — "+
			"instead. Whether the trade is worth making is the project's call, so "+
			"this is a warning and not a refusal.")

	CodeTableCacheUnavailable = newCode("RIG3007", SeverityError,
		"`cache: true` was set on a table in a project with no `cache:` block.",
		"Add `cache: enabled: true` to rig.yaml, or remove the key: with no channel to publish on, a held row could never be withdrawn.")

	CodeTableCacheNotYours = newCode("RIG3008", SeverityError,
		"`cache: true` was set on one of rig's own tables.",
		"Remove the key. Holding a row is a promise that every write to it goes through the generated repository, and rig's own modules write these tables with their own SQL — so a held row could go stale with nothing to withdraw it.")

	CodeUnmentionedColumn = newCode("RIG3100", SeverityWarning,
		"A column exists in the database but is not mentioned in the table configuration.",
		"Run `rig sync` to add it, then replace the placeholder comment.")

	CodeUnknownColumn = newCode("RIG3101", SeverityError,
		"The configuration names a column that no longer exists.",
		"Remove the entry, or run `rig sync --prune`.")

	CodeUnknownTable = newCode("RIG3102", SeverityError,
		"A configuration file names a table that no longer exists.",
		"Delete the file. `rig sync` reports it but will not remove it: it may hold endpoint definitions worth keeping.")

	CodeForeignTable = newCode("RIG3107", SeverityError,
		"A configuration file names a table rig manages rather than generates from.",
		"Delete the file, or turn on the switch the message names — `auth.expose` for the "+
			"authentication foundation, `files.expose` for rig_file — to have rig generate a "+
			"model and a repository for it after all.")

	CodeUnknownEnumValue = newCode("RIG3103", SeverityError,
		"The configuration names an enum label that no longer exists.",
		"Remove the entry, or run `rig sync --prune`.")

	CodeUnmentionedEnumValue = newCode("RIG3104", SeverityWarning,
		"An enum label is not described in the table configuration.",
		"Run `rig sync` to add it.")

	CodeUnknownEnum = newCode("RIG3105", SeverityError,
		"The configuration names an enum type that no longer exists.",
		"")

	CodeUnknownRelation = newCode("RIG3106", SeverityError,
		"The configuration names a relation that does not exist.",
		"")

	CodeDuplicateFieldName = newCode("RIG3201", SeverityError,
		"Two columns are exposed under the same API field name.",
		"Change one of the `field:` keys.")

	CodeExcludeBreaksCreate = newCode("RIG3210", SeverityError,
		"Excluding this column would make the resource impossible to create.",
		"Give the column a default, make it nullable, or drop the Create operation.")

	CodeImmutableUnwritable = newCode("RIG3211", SeverityError,
		"`immutable` was set on a column that can never be written anyway.",
		"Remove the key: generated and identity columns are already read-only.")

	CodeUnknownBodyObject = newCode("RIG3220", SeverityError,
		"A custom endpoint references an object that does not exist.",
		"")

	CodeInvalidEndpoint = newCode("RIG3221", SeverityError,
		"A custom endpoint is malformed.",
		"")

	CodeInvalidFormat = newCode("RIG3230", SeverityError,
		"A column declares a format its type cannot carry.",
		"Formats apply to String columns only.")

	CodeInvalidOperation = newCode("RIG3240", SeverityError,
		"An operation is not valid here.",
		"")

	CodeOperationUnsupported = newCode("RIG3241", SeverityError,
		"An operation was requested that the table cannot support.",
		"")

	CodeUnexposedConflict = newCode("RIG3250", SeverityError,
		"A table that is not exposed also asks for something only the API layer provides.",
		"Remove `expose: false`, or remove the endpoints and live-sync configuration it cannot serve.")
)

// Expanding CRUD, filters, and defaults: RIG4xxx.
var (
	CodeEndpointShadowed = newCode("RIG4001", SeverityInfo,
		"A hand-written endpoint replaces the generated one of the same name.",
		"")
)

// Structural validation: RIG5xxx. These are always errors — the generators
// cannot produce correct code without them.
var (
	CodeMissingPrimaryKey = newCode("RIG5001", SeverityError,
		"A table has no primary key.",
		"Every table rig exposes needs `id uuid primary key`.")

	CodePrimaryKeyShape = newCode("RIG5002", SeverityError,
		"A table's primary key is not a single `id uuid` column.",
		"Rename the column to `id`, or exclude the table from the API.")

	CodeSnapshotTriplePartial = newCode("RIG5010", SeverityError,
		"A table has some but not all of the snapshot columns.",
		"A snapshotable table needs version_type, snapshot_from_<table>_id and snapshot_from_<table>_at, or none of them.")

	CodeSnapshotColumnType = newCode("RIG5011", SeverityError,
		"A snapshot column has the wrong type.",
		"")

	CodeSnapshotEnumValues = newCode("RIG5012", SeverityError,
		"The version_type enum is missing a required label.",
		"It must contain both 'Original' and 'Snapshot'.")

	CodeSnapshotIgnoreUnavailable = newCode("RIG5013", SeverityError,
		"`snapshot_ignore` was set on a table that keeps no snapshots.",
		"Remove the key, or add the snapshot columns to the table.")

	CodeSnapshotIgnoreReserved = newCode("RIG5014", SeverityError,
		"`snapshot_ignore` was set on a snapshot bookkeeping column.",
		"")

	CodeAuditColumnShape = newCode("RIG5020", SeverityError,
		"An audit column has the wrong type.",
		"Audit actor columns must be `uuid` and nullable; audit timestamps must be `timestamptz`.")

	CodeRestoreWindowRequired = newCode("RIG5030", SeverityError,
		"A soft-deletable table does not declare `restore_window_days`.",
		"Add `restore_window_days:` with the number of days a deleted row stays restorable.")

	CodeRestoreWindowForbidden = newCode("RIG5031", SeverityError,
		"`restore_window_days` was set on a table that is not soft-deletable.",
		"Remove the key, or add a nullable `deleted_at timestamptz` column.")

	CodeRestoreWindowInvalid = newCode("RIG5032", SeverityError,
		"`restore_window_days` must be a positive number of days.",
		"")

	CodeEnumNullabilityMixed = newCode("RIG5040", SeverityError,
		"An enum type is nullable in one column and not in another.",
		"Generated enum handling needs one answer per type.")

	CodeOrderByUnknown = newCode("RIG5050", SeverityError,
		"`order_by` names a column that does not exist.",
		"")

	CodeDeleteOrderCycle = newCode("RIG5060", SeverityWarning,
		"The tables referencing one table reference each other in a cycle.",
		"There is no order that tells each child after the tables pointing at it, "+
			"so they are told in schema order. Settle it with `on_delete.order`.")

	CodeDeleteOrderUnknown = newCode("RIG5061", SeverityError,
		"`on_delete.order` names a table that does not reference this one.",
		"")

	CodeTenantColumnShape = newCode("RIG5070", SeverityError,
		"The tenant column has the wrong type.",
		"`tenant_id` must be `uuid not null`.")

	CodeUnresolvedType = newCode("RIG5080", SeverityError,
		"A field references a type that is not declared anywhere.",
		"")
)

// Convention validation: RIG6xxx. Severity comes from the `validate` block of
// the project configuration; the value here is the default.
var (
	CodeMissingTableComment = newCode("RIG6001", SeverityError,
		"A table has no comment.",
		"Describe what the table is for in its configuration's `comment:` key.")

	CodeMissingColumnComment = newCode("RIG6002", SeverityError,
		"A column has no comment.",
		"Describe the column in its `comment:` key.")

	CodeForeignKeyNotIndexed = newCode("RIG6010", SeverityError,
		"A foreign-key column is not covered by an index.",
		"Add an index; unindexed foreign keys make joins and cascading lookups slow.")

	CodeTenantIndexNotLeading = newCode("RIG6011", SeverityError,
		"No index leads with the tenant column.",
		"Every generated query filters by tenant, so an index leading with it is the difference between a seek and a scan.")

	CodeBooleanPrefix = newCode("RIG6020", SeverityWarning,
		"A boolean column does not read as a predicate.",
		"Prefix it with is_, has_, can_, should_, was_ or allow_.")

	CodeTimestampSuffix = newCode("RIG6021", SeverityWarning,
		"A timestamp column's name does not end in _at, or an _at column is not a timestamp.",
		"")

	CodeDateSuffix = newCode("RIG6022", SeverityWarning,
		"A date column's name does not end in _date, or a _date column is not a date.",
		"")

	CodeForeignKeyNaming = newCode("RIG6030", SeverityWarning,
		"A foreign-key column is not named after the table it references.",
		"Name it <target>_id, or <qualifier>_<target>_id.")

	CodeCascadeDelete = newCode("RIG6040", SeverityError,
		"A foreign key declares ON DELETE CASCADE.",
		"Delete through the service layer instead, so hooks and snapshots are not bypassed.")

	CodeMigrationFilename = newCode("RIG6050", SeverityError,
		"A migration file is not named NNNNN_snake_case.sql.",
		"")

	CodeMigrationDuplicate = newCode("RIG6051", SeverityError,
		"Two migrations share the same numeric prefix.",
		"")
)

// Internal consistency: RIG9xxx. These report a bug in rig, not in the project.
var (
	CodeColumnRefMismatch = newCode("RIG9001", SeverityError,
		"A field's column reference disagrees with the schema it points at.",
		"This is a bug in rig. Please report it with the schema that triggered it.")

	CodeInternal = newCode("RIG9002", SeverityError,
		"An internal invariant was violated.",
		"This is a bug in rig. Please report it with the schema that triggered it.")
)

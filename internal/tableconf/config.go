// Package tableconf reads the per-table YAML that annotates an introspected
// schema.
//
// The database says what exists; these files say what it means. A file can add
// documentation, rename what a column is called on the wire, forbid an update,
// exclude a column from the API, or declare an endpoint beyond CRUD. It can
// never contradict the database: there is no key for a column's type,
// nullability, or keys, because those are facts rather than choices.
//
// Files are keyed by physical names — table, column, and enum label as they are
// spelled in Postgres — because API names are derived and can change under you.
//
// Every diagnostic this package produces carries the exact line and column of
// the key that caused it. That is why the loader keeps the YAML syntax tree
// around instead of decoding straight into structs.
//
// Descriptions live in `jsonschema_description` tags rather than inside the
// `jsonschema` tag, because that tag splits on commas and prose contains them.
package tableconf

// File is one table's configuration.
type File struct {
	// Table is the physical table name. It must match the file's location, so
	// a renamed table is caught rather than silently ignored.
	Table string `yaml:"table" json:"table" jsonschema_description:"The physical table name this file configures."`

	// Resource overrides the derived API resource name, for example Lesson.
	Resource string `yaml:"resource,omitempty" json:"resource,omitempty" jsonschema_description:"API resource name. Defaults to the table name in PascalCase."`
	// Plural overrides the derived plural, for example Lessons.
	Plural string `yaml:"plural,omitempty" json:"plural,omitempty" jsonschema_description:"Plural resource name. Defaults to an inflection of the resource name."`
	// PathSegment overrides the derived URL segment, for example lessons.
	PathSegment string `yaml:"path_segment,omitempty" json:"path_segment,omitempty" jsonschema_description:"URL path segment. Defaults to the kebab-case plural."`

	// Comment documents what the table is for. It becomes the Go doc comment on
	// the model and the description in the OpenAPI document.
	Comment string `yaml:"comment,omitempty" json:"comment,omitempty" jsonschema_description:"What this table is for and why it exists."`

	// Operations replaces the default operation set. It is a replacement rather
	// than a merge: additive semantics could not express removing one.
	Operations []string `yaml:"operations,omitempty" json:"operations,omitempty" jsonschema:"enum=Create,enum=Get,enum=List,enum=Search,enum=Update,enum=Delete" jsonschema_description:"Operations to expose. Replaces the default set entirely."`

	// Public names the operations that answer without a credential.
	//
	// Both generated operations and custom endpoints, by name, because they
	// share one namespace: a custom endpoint called Get replaces the generated
	// one. A custom endpoint can also say so in its own block, which is where
	// everything else about it is declared.
	Public []string `yaml:"public,omitempty" json:"public,omitempty" jsonschema_description:"Operations and endpoints that answer without a credential."`

	// Expose controls whether the table appears in the API at all. Nil means
	// yes. False keeps the model and the repository and generates no endpoints,
	// which is what a table like rig_account_token wants: the data layer is worth
	// generating and a REST interface for it is not.
	Expose *bool `yaml:"expose,omitempty" json:"expose,omitempty" jsonschema_description:"Expose this table through the API. Defaults to true. False still generates the model and repository."`

	// OrderBy is the default ordering, most significant first. A leading minus
	// sorts descending, for example "-starts_at".
	OrderBy []string `yaml:"order_by,omitempty" json:"order_by,omitempty" jsonschema_description:"Default ordering by column name. Prefix with - for descending."`

	// RestoreWindowDays bounds how long a soft-deleted row stays restorable.
	// Required for a soft-deletable table and rejected on any other.
	RestoreWindowDays *int `yaml:"restore_window_days,omitempty" json:"restore_window_days,omitempty" jsonschema_description:"Days a soft-deleted row stays restorable. Required when the table has deleted_at."`

	// Access is how wide a read reaches by default.
	Access *Access `yaml:"access,omitempty" json:"access,omitempty" jsonschema_description:"How wide a read of this table reaches by default."`

	// Cache holds this table's Get in memory, withdrawn on the writes rig makes
	// to it. It needs the project's `cache:` block, and it is a promise as much
	// as a setting: every write to this table has to go through the generated
	// repository, because that is the only place rig can publish the withdrawal
	// from. A write through Store.Pool, or raw SQL from inside a dbhook, serves a
	// stale row until the entry expires.
	Cache *bool `yaml:"cache,omitempty" json:"cache,omitempty" jsonschema_description:"Hold this table's Get in memory. Requires the project cache block, and promises every write goes through the generated repository."`

	// OnDelete states the order this table's children hear about a delete.
	OnDelete *OnDelete `yaml:"on_delete,omitempty" json:"on_delete,omitempty" jsonschema_description:"The order tables referencing this one are told about a delete."`

	Columns   map[string]Column   `yaml:"columns,omitempty" json:"columns,omitempty" jsonschema_description:"Per-column configuration keyed by physical column name."`
	Enums     map[string]Enum     `yaml:"enums,omitempty" json:"enums,omitempty" jsonschema_description:"Per-enum configuration keyed by Postgres enum type name."`
	Relations map[string]Relation `yaml:"relations,omitempty" json:"relations,omitempty" jsonschema_description:"Per-relation configuration keyed by the related table name."`

	Electric  *Electric  `yaml:"electric,omitempty" json:"electric,omitempty" jsonschema_description:"Live-sync shape endpoint for this table."`
	Endpoints []Endpoint `yaml:"endpoints,omitempty" json:"endpoints,omitempty" jsonschema_description:"Endpoints beyond the generated CRUD set."`
}

// Column configures one column's appearance in the API.
type Column struct {
	// Comment documents the column. It wins over a Postgres COMMENT ON and over
	// any comment rig would generate for a well-known column.
	Comment string `yaml:"comment,omitempty" json:"comment,omitempty" jsonschema_description:"What this column holds."`

	// Field overrides the derived API field name.
	Field string `yaml:"field,omitempty" json:"field,omitempty" jsonschema_description:"API field name. Defaults to the column name in PascalCase."`

	// Format narrows a string for documentation and client validation.
	Format string `yaml:"format,omitempty" json:"format,omitempty" jsonschema:"enum=EmailAddress,enum=URL,enum=PhoneNumber,enum=Color,enum=CountryCode,enum=LanguageCode,enum=TimeZone,enum=RichText" jsonschema_description:"Semantic format of a string column."`

	// GoType names a concrete Go type for a jsonb column, replacing the generic
	// raw-message type.
	GoType string `yaml:"go_type,omitempty" json:"go_type,omitempty" jsonschema_description:"Named Go type for a jsonb column."`

	// Operations replaces the derived set of Create, Read, Update.
	Operations []string `yaml:"operations,omitempty" json:"operations,omitempty" jsonschema:"enum=Create,enum=Read,enum=Update" jsonschema_description:"Operations this column takes part in. Replaces the default set entirely."`

	// Immutable allows the column on create but never on update, so it is
	// absent from the update input entirely.
	Immutable bool `yaml:"immutable,omitempty" json:"immutable,omitempty" jsonschema_description:"Settable on create, never on update."`

	// ReadOnly keeps the column out of every write path.
	ReadOnly bool `yaml:"read_only,omitempty" json:"read_only,omitempty" jsonschema_description:"Never writable through the API."`

	// Exclude removes the column from the API entirely.
	Exclude bool `yaml:"exclude,omitempty" json:"exclude,omitempty" jsonschema_description:"Do not expose this column at all."`

	// SnapshotIgnore keeps the live row's value when a snapshot is restored.
	// The value is still copied into the snapshot itself.
	SnapshotIgnore bool `yaml:"snapshot_ignore,omitempty" json:"snapshot_ignore,omitempty" jsonschema_description:"Keep the live value when restoring a snapshot."`

	// Example is shown in generated documentation.
	Example string `yaml:"example,omitempty" json:"example,omitempty" jsonschema_description:"Example value for documentation."`
}

// Enum configures a Postgres enum type's appearance in the API.
type Enum struct {
	// Name overrides the derived Go and TypeScript type name.
	Name string `yaml:"name,omitempty" json:"name,omitempty" jsonschema_description:"Generated type name. Defaults to the enum type name in PascalCase."`

	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"What this enumeration represents."`

	// Values is keyed by the Postgres label, which stays the wire value.
	Values map[string]EnumValue `yaml:"values,omitempty" json:"values,omitempty" jsonschema_description:"Per-label configuration keyed by the Postgres label."`
}

// EnumValue configures one label.
type EnumValue struct {
	// Name overrides the derived identifier. The Postgres label remains the
	// value on the wire regardless.
	Name        string `yaml:"name,omitempty" json:"name,omitempty" jsonschema_description:"Generated identifier. Defaults to the label in PascalCase."`
	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"What this value means."`
}

// Relation configures a link derived from a foreign key or a join table.
type Relation struct {
	// Name overrides the derived accessor name.
	Name string `yaml:"name,omitempty" json:"name,omitempty" jsonschema_description:"Name of the relation on the resource."`
	// Embed includes the related rows in read responses.
	Embed bool `yaml:"embed,omitempty" json:"embed,omitempty" jsonschema_description:"Include the related rows in read responses."`
}

// Access is how wide a read of a table reaches by default.
//
// Only the default. What a caller may ask for beyond it is a permission, and how
// far the answer actually reaches is in the response — this is the floor, not the
// ceiling.
type Access struct {
	// Scope is "tenant" (every row the tenant owns, the default) or "own" (the
	// rows the caller created).
	//
	// "own" needs a created-by audit column, which is what it filters on. It
	// narrows reads only: an owner-scoped table refuses to change somebody else's
	// row outright, with no parameter to widen that, because a write is a
	// different kind of decision from a read and one flag for both would be a bad
	// answer to two questions.
	Scope string `yaml:"scope,omitempty" json:"scope,omitempty" jsonschema:"enum=tenant,enum=own" jsonschema_description:"Default read width: tenant (every row) or own (rows the caller created)."`

	// Owner names the column "own" filters on. It defaults to
	// created_by_account_id, the audit column every generated write stamps.
	//
	// Naming another one is for the table whose owner is not whoever created the
	// row: an inbox line belongs to the person it is addressed to, and an
	// assigned task belongs to its assignee. The column has to be a uuid
	// referencing rig_account and it has to be NOT NULL — the nullability the
	// audit column is allowed is exactly what a column like this must not have,
	// because a null there is a row nobody can see and nothing reports.
	Owner string `yaml:"owner,omitempty" json:"owner,omitempty" jsonschema_description:"Column an owner-scoped read filters on. Defaults to created_by_account_id. Must be a NOT NULL uuid referencing rig_account."`
}

// OnDelete states the order the tables referencing this one are told that a row
// is going.
//
// Note what is and is not here. The parent states the *sequence*, which is a
// fact about the relationships between its children and belongs to no single one
// of them — and the parent is the only place that can see all of them at once.
// It does not state the *action*: that is a function on the child, and the
// parent has no opinion about it.
//
// rig derives an order already — referencing tables before referenced ones — and
// that default is right for the case that recurs. This is for when it is wrong.
type OnDelete struct {
	// Order names child tables, by their physical names, in the order they
	// should hear about a delete. Anything left out runs after, in the derived
	// order.
	Order []string `yaml:"order,omitempty" json:"order,omitempty" jsonschema_description:"Child tables in the order they are told about a delete. Anything unlisted runs after."`
}

// Electric configures a live-sync shape endpoint.
//
// The tenant and lifecycle predicates are built by rig. Declared params are
// handed to the application's own scoping function, which can only narrow the
// shape further.
type Electric struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" jsonschema_description:"Expose a live-sync shape endpoint for this table."`

	// Auth is the session kind required. Defaults to tenant.
	Auth string `yaml:"auth,omitempty" json:"auth,omitempty" jsonschema:"enum=tenant,enum=admin" jsonschema_description:"Session kind required to subscribe."`

	// Params are extra query parameters, keyed by the query-string key.
	Params map[string]ElectricParam `yaml:"params,omitempty" json:"params,omitempty" jsonschema_description:"Extra query parameters, keyed by query-string key."`
}

// ElectricParam is one declared query parameter of a live-sync endpoint.
type ElectricParam struct {
	Type        string `yaml:"type" json:"type" jsonschema:"enum=Bool,enum=Int,enum=Int64,enum=Float64,enum=String,enum=UUID,enum=Date,enum=Time,enum=Timestamp" jsonschema_description:"Parameter type."`
	Optional    bool   `yaml:"optional,omitempty" json:"optional,omitempty" jsonschema_description:"Whether the parameter may be omitted."`
	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"What this parameter narrows."`
}

// Endpoint declares an operation beyond the generated CRUD set.
//
// An endpoint named after a generated one replaces it. That is reported as a
// note, so the shadowing is visible rather than mysterious.
type Endpoint struct {
	Name   string `yaml:"name" json:"name" jsonschema_description:"Method name on the generated service interface."`
	Method string `yaml:"method" json:"method" jsonschema:"enum=GET,enum=POST,enum=PUT,enum=PATCH,enum=DELETE,enum=QUERY" jsonschema_description:"HTTP method."`
	// Path is relative to the resource, for example "/{id}/_publish".
	Path string `yaml:"path,omitempty" json:"path,omitempty" jsonschema_description:"Path relative to the resource, for example /{id}/_publish."`

	Summary     string `yaml:"summary,omitempty" json:"summary,omitempty" jsonschema_description:"One-line description."`
	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"Longer description."`

	// Permission is the RBAC key required. Empty means a valid session is enough.
	Permission string `yaml:"permission,omitempty" json:"permission,omitempty" jsonschema_description:"RBAC permission key required to call this endpoint."`

	// Public answers without a credential. The table's `public:` list says the
	// same thing from the other end; this is here so an endpoint declared in
	// one block does not have half of itself in another.
	Public bool `yaml:"public,omitempty" json:"public,omitempty" jsonschema_description:"Answer this endpoint without requiring a credential."`

	Request   *EndpointRequest   `yaml:"request,omitempty" json:"request,omitempty" jsonschema_description:"What the client sends."`
	Responses []EndpointResponse `yaml:"responses,omitempty" json:"responses,omitempty" jsonschema_description:"Every status this endpoint can return."`
}

// Req returns the endpoint's request, or an empty one when it declares none, so
// callers do not have to nil-check before reading parameter lists.
func (e *Endpoint) Req() EndpointRequest {
	if e.Request == nil {
		return EndpointRequest{}
	}
	return *e.Request
}

// EndpointRequest is what a client sends to a custom endpoint.
type EndpointRequest struct {
	PathParams  []Param `yaml:"path_params,omitempty" json:"path_params,omitempty" jsonschema_description:"Parameters interpolated into the path."`
	QueryParams []Param `yaml:"query_params,omitempty" json:"query_params,omitempty" jsonschema_description:"Query-string parameters."`
	Body        []Param `yaml:"body,omitempty" json:"body,omitempty" jsonschema_description:"Body fields. Mutually exclusive with body_object."`
	// BodyObject names a whole object as the body instead of listing fields.
	BodyObject string `yaml:"body_object,omitempty" json:"body_object,omitempty" jsonschema_description:"Name of an object to use as the whole body."`
}

// Param is one request parameter.
type Param struct {
	Name        string `yaml:"name" json:"name" jsonschema_description:"Field name in PascalCase."`
	Type        string `yaml:"type" json:"type" jsonschema_description:"Primitive type, or the name of an enum or object."`
	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"What this parameter does."`
	Optional    bool   `yaml:"optional,omitempty" json:"optional,omitempty" jsonschema_description:"Whether the parameter may be omitted."`
	Array       bool   `yaml:"array,omitempty" json:"array,omitempty" jsonschema_description:"Whether the parameter is a list."`
	Default     string `yaml:"default,omitempty" json:"default,omitempty" jsonschema_description:"Default value when omitted."`
}

// EndpointResponse is one outcome of a custom endpoint.
type EndpointResponse struct {
	Status      int    `yaml:"status" json:"status" jsonschema:"minimum=100,maximum=599" jsonschema_description:"HTTP status code."`
	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"When this status is returned."`
	// BodyObject names the object returned, for example Lesson or Error.
	BodyObject string  `yaml:"body_object,omitempty" json:"body_object,omitempty" jsonschema_description:"Name of the object returned."`
	BodyFields []Param `yaml:"body_fields,omitempty" json:"body_fields,omitempty" jsonschema_description:"Inline body fields. Mutually exclusive with body_object."`
}

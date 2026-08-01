package ir

import "slices"

// API is the projected layer: what clients see.
type API struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// BasePath is the prefix every route sits under, for example "/api/v1".
	BasePath string `json:"base_path"`

	Enums     []Enum     `json:"enums"`
	Objects   []Object   `json:"objects"`
	Resources []Resource `json:"resources"`
}

// Origin records why an object exists, so a generator can treat hand-declared
// shapes differently from ones rig synthesized.
type Origin string

const (
	// OriginProjected is an object derived from a table's readable columns.
	OriginProjected Origin = "Projected"
	// OriginFilter is one of the generated filter shapes for Search.
	OriginFilter Origin = "Filter"
	// OriginBuiltin is a framework shape such as Error or Pagination.
	OriginBuiltin Origin = "Builtin"
	// OriginConfig is an object declared in a table's YAML.
	OriginConfig Origin = "Config"
)

// TypeKind is the resolution of [Field.Type], computed once when the document is
// frozen so no generator has to resolve names itself.
type TypeKind string

const (
	TypeKindPrimitive TypeKind = "Primitive"
	TypeKindEnum      TypeKind = "Enum"
	TypeKindObject    TypeKind = "Object"
	TypeKindResource  TypeKind = "Resource"
)

// The primitive type names a [Field.Type] may take. Anything else must resolve
// to an enum, object, or resource declared in the same [API].
const (
	TypeBool      = "Bool"
	TypeBytes     = "Bytes"
	TypeDate      = "Date"
	TypeDecimal   = "Decimal"
	TypeFloat64   = "Float64"
	TypeInt       = "Int"
	TypeInt64     = "Int64"
	TypeJSON      = "JSON"
	TypeString    = "String"
	TypeTime      = "Time"
	TypeTimestamp = "Timestamp"
	TypeUUID      = "UUID"
)

// Modifiers a field may carry.
const (
	ModifierNullable = "Nullable"
	ModifierArray    = "Array"
)

// Operations a resource may support.
const (
	OpCreate = "Create"
	OpGet    = "Get"
	OpList   = "List"
	OpSearch = "Search"
	OpUpdate = "Update"
	OpDelete = "Delete"
)

// Operations a field may participate in. Read is implied by any of Get, List,
// or Search on the owning resource.
const (
	FieldOpCreate = "Create"
	FieldOpRead   = "Read"
	FieldOpUpdate = "Update"
)

// Enum is a closed set of values exposed to clients.
type Enum struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	Values      []EnumValue `json:"values"`
	// PgType binds this enum to the Postgres type it came from. Empty for enums
	// that exist only in the API, such as ErrorCode.
	PgType string `json:"pg_type,omitempty"`
}

// EnumValue separates the identifier from the literal deliberately: Postgres
// labels are frequently snake_case while Go and TypeScript want PascalCase, and
// conflating the two makes one of the three wrong.
type EnumValue struct {
	// Name is the Go and TypeScript identifier, for example "InProgress".
	Name string `json:"name"`
	// Wire is the literal on the wire and in Postgres, for example "in_progress".
	Wire        string `json:"wire"`
	Description string `json:"description,omitempty"`
}

// Object is a named shape referenced by fields.
type Object struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Fields      []Field `json:"fields"`
	Origin      Origin  `json:"origin"`
}

// Field is one member of an object, request, or response.
type Field struct {
	// Name is the Go and TypeScript identifier, for example "ManagerEmailAddress".
	Name string `json:"name"`
	// Wire is the JSON key, for example "managerEmailAddress".
	Wire        string `json:"wire"`
	Description string `json:"description,omitempty"`

	// Type is a primitive name or the name of an enum, object, or resource.
	Type string `json:"type"`
	// TypeKind resolves Type. Set when the document is frozen.
	TypeKind TypeKind `json:"type_kind"`
	// GoType is the concrete Go type for this field, including a leading * when
	// the field is nullable, for example "*string" or "time.Time".
	GoType string `json:"go_type,omitempty"`

	// Format narrows a primitive for documentation and client-side validation:
	// EmailAddress, URL, PhoneNumber, Color, CountryCode, RichText.
	Format string `json:"format,omitempty"`

	Modifiers []string `json:"modifiers,omitempty"`
	Default   string   `json:"default,omitempty"`
	Example   string   `json:"example,omitempty"`

	// ReadOnly fields never appear in a create or update body.
	ReadOnly bool `json:"read_only,omitempty"`
	// Immutable fields may be set on create but never updated, so they are
	// absent from the update body entirely.
	Immutable bool `json:"immutable,omitempty"`

	// Column binds this field to storage. Nil for computed or virtual fields.
	Column *ColumnRef `json:"column,omitempty"`
}

// IsNullable reports whether the field carries the Nullable modifier.
func (f Field) IsNullable() bool { return slices.Contains(f.Modifiers, ModifierNullable) }

// IsArray reports whether the field carries the Array modifier.
func (f Field) IsArray() bool { return slices.Contains(f.Modifiers, ModifierArray) }

// ScanStrategy tells the persistence generator how to move a value between Go
// and pgx. It is derived from the SQL type once, in one place, rather than
// re-derived by every generator that touches the column.
type ScanStrategy string

const (
	ScanDirect      ScanStrategy = "Direct"
	ScanUUID        ScanStrategy = "UUID"
	ScanEnumText    ScanStrategy = "EnumText"
	ScanTimestamptz ScanStrategy = "Timestamptz"
	ScanJSONB       ScanStrategy = "JSONB"
	ScanArray       ScanStrategy = "Array"
	ScanNumeric     ScanStrategy = "Numeric"
)

// ColumnRef binds an API field to the column that stores it. The fields
// duplicated from [Column] are the ones needed on nearly every emitted line;
// [Document.Column] reaches the full column when more is needed.
type ColumnRef struct {
	Table    string       `json:"table"`
	Name     string       `json:"name"`
	SQLType  string       `json:"sql_type"`
	Nullable bool         `json:"nullable"`
	Scan     ScanStrategy `json:"scan"`
}

// ResourceField is a field of a resource, with the operations it takes part in.
type ResourceField struct {
	Field `json:",inline"`
	// Operations is some subset of Create, Read, Update.
	Operations []string `json:"operations"`
}

// In reports whether the field participates in the given field operation.
func (f ResourceField) In(op string) bool { return slices.Contains(f.Operations, op) }

// Resource is one table exposed as an API resource.
type Resource struct {
	Name        string `json:"name"`         // Lesson
	Plural      string `json:"plural"`       // Lessons
	PathSegment string `json:"path_segment"` // lessons
	Description string `json:"description,omitempty"`

	Operations []string        `json:"operations"`
	Fields     []ResourceField `json:"fields"`
	Endpoints  []Endpoint      `json:"endpoints"`

	// Storage is nil for a virtual resource with no table behind it.
	Storage *ResourceStorage `json:"storage,omitempty"`
	// Electric is set when the resource exposes a live-sync shape endpoint.
	Electric *ElectricEndpoint `json:"electric,omitempty"`
}

// Supports reports whether the resource has the given operation.
func (r *Resource) Supports(op string) bool { return slices.Contains(r.Operations, op) }

// Endpoint returns the named endpoint, or nil.
func (r *Resource) Endpoint(name string) *Endpoint {
	for i := range r.Endpoints {
		if r.Endpoints[i].Name == name {
			return &r.Endpoints[i]
		}
	}
	return nil
}

// HasEndpoint reports whether the resource already declares the named endpoint.
// Expansion consults this before synthesizing a CRUD endpoint, so a hand-written
// endpoint always shadows the generated one of the same name.
func (r *Resource) HasEndpoint(name string) bool { return r.Endpoint(name) != nil }

// Field returns the named field, or nil.
func (r *Resource) Field(name string) *ResourceField {
	for i := range r.Fields {
		if r.Fields[i].Name == name {
			return &r.Fields[i]
		}
	}
	return nil
}

// OrderTerm is one clause of a default ordering.
type OrderTerm struct {
	Column string `json:"column"`
	Desc   bool   `json:"desc,omitempty"`
}

// AuditColumns names the columns rig stamps automatically. Any of them may be
// nil: a table opts in by having the column.
type AuditColumns struct {
	CreatedAt *ColumnRef `json:"created_at,omitempty"`
	CreatedBy *ColumnRef `json:"created_by,omitempty"`
	UpdatedAt *ColumnRef `json:"updated_at,omitempty"`
	UpdatedBy *ColumnRef `json:"updated_by,omitempty"`
	DeletedAt *ColumnRef `json:"deleted_at,omitempty"`
	DeletedBy *ColumnRef `json:"deleted_by,omitempty"`
}

// SoftDelete marks a table whose rows are retired by stamping a timestamp
// rather than being removed.
type SoftDelete struct {
	Column *ColumnRef `json:"column"`
	Actor  *ColumnRef `json:"actor,omitempty"`
	// RestoreWindowDays bounds how long a deleted row stays restorable.
	RestoreWindowDays int `json:"restore_window_days"`
}

// Snapshot marks a table that keeps immutable copies of prior versions.
type Snapshot struct {
	VersionType *ColumnRef `json:"version_type"`
	FromID      *ColumnRef `json:"from_id"`
	FromAt      *ColumnRef `json:"from_at"`
	// IgnoreColumns are not replayed when a snapshot is restored: the live
	// row's values win. They are still copied into the snapshot itself.
	IgnoreColumns []string `json:"ignore_columns,omitempty"`
}

// RelationKind is the cardinality of a [Relation].
type RelationKind string

const (
	RelationBelongsTo  RelationKind = "BelongsTo"
	RelationHasMany    RelationKind = "HasMany"
	RelationManyToMany RelationKind = "ManyToMany"
)

// Relation is a link from one resource to another, derived from a foreign key
// or from a join table.
type Relation struct {
	Name   string       `json:"name"`
	Kind   RelationKind `json:"kind"`
	Target string       `json:"target"` // resource name

	LocalColumn   string     `json:"local_column,omitempty"`
	ForeignTable  string     `json:"foreign_table,omitempty"`
	ForeignColumn string     `json:"foreign_column,omitempty"`
	LinkTable     *LinkTable `json:"link_table,omitempty"`

	// Embed includes the related rows in read responses.
	Embed bool `json:"embed,omitempty"`
}

// ResourceStorage is everything the persistence layer needs that the API layer
// does not care about.
type ResourceStorage struct {
	Table      string   `json:"table"`
	PrimaryKey []string `json:"primary_key"`

	// Tenant is the column every generated query is scoped by.
	Tenant *ColumnRef `json:"tenant,omitempty"`

	Audit      *AuditColumns `json:"audit,omitempty"`
	SoftDelete *SoftDelete   `json:"soft_delete,omitempty"`
	Snapshot   *Snapshot     `json:"snapshot,omitempty"`

	// AuditLog enables per-column change logging for this table.
	AuditLog bool `json:"audit_log"`

	DefaultOrder []OrderTerm `json:"default_order,omitempty"`
	Relations    []Relation  `json:"relations,omitempty"`

	Sortable   []string `json:"sortable,omitempty"`
	Filterable []string `json:"filterable,omitempty"`
}

// IsSoftDeletable reports whether rows of this resource are retired rather than
// removed.
func (s *ResourceStorage) IsSoftDeletable() bool { return s != nil && s.SoftDelete != nil }

// IsSnapshotable reports whether updates to this resource keep prior versions.
func (s *ResourceStorage) IsSnapshotable() bool { return s != nil && s.Snapshot != nil }

// ElectricAuth is the authentication level a live-sync endpoint requires.
type ElectricAuth string

const (
	// ElectricAuthTenant requires a session scoped to a tenant, and the shape is
	// filtered to that tenant.
	ElectricAuthTenant ElectricAuth = "tenant"
	// ElectricAuthAdmin requires an administrative session.
	ElectricAuthAdmin ElectricAuth = "admin"
)

// ElectricEndpoint describes a live-sync shape endpoint for a resource. The
// tenant and lifecycle predicates are built by rig; the declared params are
// passed to the application's own scoping function.
type ElectricEndpoint struct {
	Auth ElectricAuth `json:"auth"`
	// Path is the route, for example "/electric/lesson".
	Path   string          `json:"path"`
	Params []ElectricParam `json:"params,omitempty"`
}

// ElectricParam is one query parameter a live-sync endpoint accepts.
type ElectricParam struct {
	// Name is the query-string key exactly as clients send it.
	Name string `json:"name"`
	// Field is the Go identifier in the generated request struct.
	Field       string `json:"field"`
	Type        string `json:"type"`
	Optional    bool   `json:"optional,omitempty"`
	Description string `json:"description,omitempty"`
}

// EndpointKind distinguishes an endpoint rig synthesized from one declared in
// configuration.
type EndpointKind string

const (
	EndpointGenerated EndpointKind = "Generated"
	EndpointCustom    EndpointKind = "Custom"
)

// Endpoint is one operation on a resource.
type Endpoint struct {
	Name        string `json:"name"`
	Title       string `json:"title,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`

	Method string `json:"method"`
	// Path is relative to the resource, for example "/{id}" or "".
	Path string `json:"path"`
	// Pattern is the full route in net/http form, for example
	// "GET /api/v1/lessons/{id}". Computed once when the document is frozen so
	// the router, the OpenAPI document, and the TypeScript client cannot
	// disagree about path shape.
	Pattern string `json:"pattern"`
	// AliasPatterns are additional routes dispatching to the same handler. Search
	// uses this to expose a POST fallback alongside the QUERY route, for
	// intermediaries that reject unfamiliar methods.
	AliasPatterns []string `json:"alias_patterns,omitempty"`

	OperationID string `json:"operation_id"`
	// Permission is the RBAC key required to call this endpoint. Empty means a
	// valid session is enough.
	Permission string `json:"permission,omitempty"`

	Request   EndpointRequest    `json:"request"`
	Responses []EndpointResponse `json:"responses"`
	// Errors are the standard failure statuses this endpoint can return. They
	// are listed as bare codes rather than as full responses because every one
	// of them has the same body — the Error object — and spelling that out on
	// every endpoint would triple the size of the document without adding a
	// fact. Generators pair each code with the matching ErrorCode value.
	Errors []int        `json:"errors,omitempty"`
	Impl   EndpointImpl `json:"impl"`
}

// EndpointRequest is everything a client sends.
type EndpointRequest struct {
	ContentType string  `json:"content_type,omitempty"`
	Headers     []Field `json:"headers,omitempty"`
	PathParams  []Field `json:"path_params,omitempty"`
	QueryParams []Field `json:"query_params,omitempty"`
	BodyParams  []Field `json:"body_params,omitempty"`
	// BodyObject names a whole object as the body, used instead of BodyParams.
	BodyObject string `json:"body_object,omitempty"`
}

// EndpointResponse is one possible outcome. An endpoint always lists every
// status it can return, so 200, 404, and 409 are all first-class.
type EndpointResponse struct {
	StatusCode  int     `json:"status_code"`
	Description string  `json:"description,omitempty"`
	ContentType string  `json:"content_type,omitempty"`
	Headers     []Field `json:"headers,omitempty"`
	BodyFields  []Field `json:"body_fields,omitempty"`
	BodyObject  string  `json:"body_object,omitempty"`
}

// EndpointImpl tells the service and server generators how this endpoint is
// wired up.
type EndpointImpl struct {
	Kind EndpointKind `json:"kind"`
	// RepoMethod is the repository call the default implementation makes. Empty
	// for custom endpoints, which have no default.
	RepoMethod string `json:"repo_method,omitempty"`
	// ServiceMethod is the method name on the generated service interface.
	ServiceMethod string `json:"service_method"`
	// HandlerName is the generated HTTP handler's name.
	HandlerName string `json:"handler_name"`
}

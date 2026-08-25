package ir

import (
	"slices"
	"strings"
)

// API is the projected layer: what clients see.
type API struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	// BasePath is the prefix every route sits under, for example "/api/v1".
	BasePath string `json:"base_path"`

	// Revision is the date this API surface last changed, as YYYY-MM-DD.
	//
	// It is not a build stamp and not [Version]: Version is the path segment,
	// and a build stamp would move on every regeneration, so two identical
	// clients built a month apart would look a month apart. This moves only
	// when the document's own hash does, which is what makes "how old is the
	// oldest client still calling" a question with an answer.
	//
	// Empty for a document nobody stamped — [Document.Hash] deliberately does
	// not see this field, so stamping one never changes the other.
	Revision string `json:"revision,omitempty"`

	// RevisionHeader is the header Revision travels in, both ways.
	//
	// Configurable because it is a name in somebody else's namespace: a proxy
	// or a gateway may already have claimed it. Empty in a document that
	// predates the setting; a generator reading it falls back to the default.
	RevisionHeader string `json:"revision_header,omitempty"`

	Enums     []Enum     `json:"enums"`
	Objects   []Object   `json:"objects"`
	Resources []Resource `json:"resources"`

	// Auth is the authentication foundation this API is served with, or nil for
	// a project that has none.
	//
	// It is here rather than beside the schema because it is part of the surface:
	// the endpoints it mounts, the permissions a caller has to hold, and how long
	// a token a client is holding stays valid are all things a specification and a
	// client library have to say. A generator reads it instead of asking the
	// project what was configured, which is what keeps the wiring, the document
	// and the client from each carrying their own copy of the answer.
	Auth *Auth `json:"auth,omitempty"`

	// Files is the file handling this API is served with, or nil for a project
	// that accepts no uploads.
	//
	// Here for the reason [API.Auth] is: a byte cap, a sweep interval and the
	// list of types served inline are all part of the surface, and a generator
	// reads them out of the document rather than asking the project what was
	// configured. That is what keeps the wiring, the documentation and a
	// client's expectations from each carrying their own copy of the answer.
	Files *Files `json:"files,omitempty"`

	// Notifications is the inbox this API serves, or nil for a project with
	// none. Here for the reason [API.Auth] and [API.Files] are.
	Notifications *Notifications `json:"notifications,omitempty"`

	// Presence is the resolved `presence:` block, or nil for a project that does
	// not track it.
	Presence *Presence `json:"presence,omitempty"`

	// Throttle is the resolved `throttle:` block, or nil for a project that
	// does not limit API calls.
	//
	// It is part of the surface rather than a deployment detail, which is why it
	// is here and not beside the database settings: the numbers travel to
	// clients in the RateLimit-* headers and into the generated documentation,
	// so a specification that quoted anything else would be quoting a number
	// nobody enforces.
	Throttle *Throttle `json:"throttle,omitempty"`

	// Cache is the resolved `cache:` block, or nil for a project that caches
	// nothing.
	//
	// It is the one part of this document no client can observe, which is why
	// [Document.Hash] clears it: whether a replica answered a request out of
	// memory or out of the database is invisible over HTTP, and a project that
	// spent a revision turning it on would be telling every client it was built
	// against something older than the server.
	Cache *Cache `json:"cache,omitempty"`

	// Tracing is the spans this API's generated code opens, or nil for a
	// project that asked for none.
	//
	// Nil is the question every generator asks, and asking it is the whole
	// mechanism: a nil here means no import of rig/observe, no span, and no
	// OpenTelemetry in the application's go.mod. Not a span that calls a no-op
	// — optional, in rig, means absent.
	Tracing *Tracing `json:"tracing,omitempty"`

	// Monitoring is rig's own page over the spans, or nil for a project that
	// asked for none. Never set without Tracing.
	Monitoring *Monitoring `json:"monitoring,omitempty"`

	// EmbeddedFoundation says rig's own migrations are carried by the modules
	// that own them rather than vendored into this project's migrations
	// directory.
	//
	// It is here because a generator has to know. The application applies its
	// own schema, so a project whose modules carry theirs needs the code that
	// hands those sets to rig/migrate in the right order — and a project that
	// vendored them must not get it, because then the same DDL would be applied
	// twice under two histories.
	//
	// A bool rather than the mode's spelling: vendored is the default and the
	// absence, so false is what a document written before the key existed means.
	EmbeddedFoundation bool `json:"embedded_foundation,omitempty"`

	// Permissions is every permission this API's endpoints require, computed once
	// at Freeze from the endpoints themselves.
	//
	// Derived once rather than by each consumer, for the same reason
	// [Endpoint.Pattern] is: the handler's check, the catalogue written to the
	// database, an administration screen and the grant an owner receives all have
	// to mean the same thing, and three places deriving it is three places to
	// drift.
	Permissions []Permission `json:"permissions"`
}

// Permission is one thing a caller may be allowed to do.
//
// The key is what code checks and what a role grants. The name and description
// are for the interface where somebody hands it out — they come from the
// resource, so nobody writes them twice.
type Permission struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`

	// Resource is the resource it was derived from, so a rename can be reported
	// against something more useful than a string.
	Resource string `json:"resource,omitempty"`
	// Action is read, write, delete, or the name of a custom endpoint.
	Action string `json:"action,omitempty"`
}

// Files is the resolved `files:` block: everything about uploads that is a
// number or a name rather than a function.
//
// Every duration and size here is resolved when the configuration is read, so a
// zero means somebody wrote a zero rather than meaning "use the default".
type Files struct {
	// Enabled says this project accepts uploads at all. It is what makes
	// server-go write the wiring.
	Enabled bool `json:"enabled"`
	// Expose says rig_file is projected as a read-only resource, so its url can
	// be read and synced.
	Expose bool `json:"expose,omitempty"`
	// Backend is where the bytes go: memory or s3.
	Backend string `json:"backend"`

	// MaxBytes caps one upload. A hard per-file cap and not a quota.
	MaxBytes int64 `json:"max_bytes"`
	// InlineTypes are the sniffed types served without an attachment
	// disposition. Everything else downloads.
	InlineTypes []string `json:"inline_types,omitempty"`

	// AbandonedAfterSeconds and RestoreWindowSeconds are the sweeper's two
	// rules, in seconds because a document is JSON and a duration is not.
	AbandonedAfterSeconds int64 `json:"abandoned_after_seconds"`
	RestoreWindowSeconds  int64 `json:"restore_window_seconds"`

	// CookieDownloads accepts the session cookie on file GET routes, so a
	// stored URL works in an img or a download link.
	CookieDownloads bool `json:"cookie_downloads,omitempty"`
}

// Notifications is the resolved notifications block: whether this project has an
// inbox, and the numbers the engine that fills it runs on.
type Notifications struct {
	// Enabled says this project has an inbox. It is what makes server-go write
	// the wiring, and what keeps rig's notification tables in the schema.
	Enabled bool `json:"enabled"`
	// Expose says the inbox is projected as a resource as well, so it gets the
	// filter grammar and a generated client. The hand-written routes exist
	// either way.
	Expose bool `json:"expose,omitempty"`

	// DefaultDigest is what an account with no setting for a channel gets.
	DefaultDigest string `json:"default_digest"`

	// ClaimTTLSeconds is how long a dispatcher's claim is honoured, and the rest
	// of the retry arithmetic beside it. Seconds because a document is JSON and
	// a Go duration in one is either unreadable or has to be parsed by everybody.
	ClaimTTLSeconds int64 `json:"claim_ttl_seconds"`
	// SendTimeoutSeconds bounds one call into a channel, and is necessarily
	// below ClaimTTLSeconds: a send that may outlive its own lease is a send
	// whose row another dispatcher has already taken.
	SendTimeoutSeconds int64 `json:"send_timeout_seconds"`
	MaxAttempts        int   `json:"max_attempts"`
	BackoffBaseSeconds int64 `json:"backoff_base_seconds"`
	// BackoffCapSeconds is the longest one wait may be. It is what makes a long
	// attempt count a schedule rather than a sleep: doubling from a minute
	// fourteen times is five days, and capped at an hour it is eight.
	BackoffCapSeconds int64 `json:"backoff_cap_seconds"`
	// RetentionSeconds is how long a read and deleted inbox line is kept.
	RetentionSeconds int64 `json:"retention_seconds"`
}

// Presence is the resolved presence block: whether this project tracks who is
// here, and the four numbers that decide what that costs.
//
// Two of them are answered to the browser on every heartbeat, which is why they
// are in the revision hash and the other two are not — see the presence
// milestone in NEXT.md, and internal/revision for the clearing.
type Presence struct {
	// Enabled says this project tracks presence. It is what makes server-go
	// write the wiring, and what keeps rig_presence in the schema.
	Enabled bool `json:"enabled"`
	// Expose says presence is projected as a read-only resource as well, so it
	// gets the filter grammar and a generated client. The hand-written routes and
	// the live shape exist either way.
	Expose bool `json:"expose,omitempty"`

	// TTLSeconds is how long a session stays present after its last heartbeat.
	// Seconds because a document is JSON and a Go duration in one is either a
	// number nobody can read or a string every consumer has to parse.
	TTLSeconds int64 `json:"ttl_seconds"`
	// HeartbeatSeconds is how often a browser should confirm it is still there.
	// It is carried so the heartbeat route can answer with it, which is what puts
	// the interval on the server's side of the wire.
	HeartbeatSeconds int64 `json:"heartbeat_seconds"`
	// SweepSeconds is how often the in-process sweeper runs. Negative leaves the
	// generated task as the only sweeper.
	SweepSeconds int64 `json:"sweep_seconds"`
	// GraceSeconds is how long past the TTL a row survives, which is what keeps
	// the reader's arithmetic and the sweeper's from disagreeing.
	GraceSeconds int64 `json:"grace_seconds"`
}

// Tracing is the resolved `tracing:` block: whether the generated code opens
// spans, and what this service is called when it does.
//
// The service name is here rather than left to the application because the
// application already said it once, in `project.name`. Nothing about where the
// spans go is here at all: that is a property of the deployment, not of the
// generated code, and the same binary runs where there is a collector and where
// there is not.
type Tracing struct {
	// Enabled says the generated server, repositories and client open spans.
	// A document carrying this block always has it set, since a block that is
	// off resolves to no block at all.
	Enabled bool `json:"enabled"`
	// ServiceName is what this application is called in a collector, taken from
	// the project's own name.
	ServiceName string `json:"service_name"`
}

// Monitoring is the resolved `monitoring:` block: rig's own page over the spans
// this server wrote and the lines it logged.
//
// A document carries it only when the project asked for the page, and only ever
// alongside [Tracing] — the page is a reader over the span file and has nothing
// to read without it, which the compiler refuses rather than leaves to be
// discovered as a page that never fills up.
//
// The password is here and the span file is not, which looks inconsistent and
// is not. Where the spans go is a property of the deployment: the same binary
// runs on a laptop with no collector and in production with one. Which password
// guards the page is a property of the deployment too — which is why
// PasswordEnv is the ordinary way to say it — and Password is the project that
// decided otherwise, warned about once when rig.yaml is read.
type Monitoring struct {
	// Enabled says the generated server mounts the page. A document carrying
	// this block always has it set, since a block that is off resolves to no
	// block at all.
	Enabled bool `json:"enabled"`
	// ServiceName is what the page calls this application, taken from the
	// project's own name.
	ServiceName string `json:"service_name"`
	// BasePath is where the page is mounted, resolved and absolute.
	BasePath string `json:"base_path"`
	// MaxTraces is how many requests the page lists, newest first.
	MaxTraces int `json:"max_traces"`
	// MaxLogs is how many log lines the page reads, newest first.
	MaxLogs int `json:"max_logs"`
	// PasswordEnv names the variable the page reads its password from.
	PasswordEnv string `json:"password_env"`
	// Password is the literal from rig.yaml, and empty in every project that
	// took the warning.
	Password string `json:"password,omitempty"`
	// Allow is the addresses that may reach the page, as CIDR ranges or single
	// addresses. Empty allows any, and it narrows the password rather than
	// standing in for it.
	Allow []string `json:"allow,omitempty"`
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

// ScopeParam is the query-string key that widens a read past the caller's own
// rows.
//
// Here rather than in the compiler because both ends need it and neither should
// have to know the other exists: the compiler reserves the name and projects the
// parameter, and a generator has to recognise it to emit the check that
// authorizes it. It matches
// [github.com/simonjanss/rig/runtime/tenancy.ScopeParam], which is what a client
// reads.
const ScopeParam = "scope"

// Operations a resource may support.
const (
	OpCreate = "Create"
	OpGet    = "Get"
	OpList   = "List"
	OpSearch = "Search"
	OpUpdate = "Update"
	OpDelete = "Delete"
)

// Operations a soft-deletable resource gets on top of the CRUD set.
//
// A deletion that stamps the row rather than removing it has somewhere the row
// went and a way back from it, and neither is expressible as CRUD. Each follows
// the operation it is a variety of: listing the trash is a listing, and
// bringing a row back is a write.
const (
	OpListDeleted = "ListDeleted"
	OpRestore     = "Restore"
)

// Operations a snapshotable resource gets on top of the CRUD set.
//
// They are not listed in a table's `operations:` — that is the CRUD set, and
// these follow from the schema rather than from a choice. A table that keeps
// its previous versions can be asked for them, and one whose rows can be
// updated can be put back to one. Both are dropped along with the operation
// they depend on: no Get, no history; no Update, nothing to revert with.
const (
	OpVersions = "Versions"
	OpRevert   = "Revert"
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
	Wire string `json:"wire"`

	// Description is the human-readable text for this field, and the only copy
	// of it.
	//
	// Every generator that can carry a description must emit this one: a Go doc
	// comment, an OpenAPI `description`, a TypeScript doc comment. It is written
	// once — by the compiler, from the column comment or from the shape of the
	// thing — precisely so that a rule explained on a filter field is the same
	// sentence in the struct, in the schema, and in the client. A generator that
	// paraphrases it, or leaves it out, is how three descriptions of one field
	// start disagreeing.
	//
	// internal/gen/gentest.DescriptionsSurvive is the guard, and every generator
	// with a place to put a description calls it.
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

// IsTextual reports whether the field is text a pattern can be matched against.
//
// Not every String field is. An inet or cidr column is a string on the wire and
// is not text in Postgres, so LIKE against one is a type error rather than a
// filter — and a list of addresses is not a pattern either.
//
// It lives here, on the IR, because the API layer and the persistence layer
// both have to answer it the same way. The last time they each decided for
// themselves, one generated a filter field the other had nowhere to put.
func (f Field) IsTextual() bool {
	return f.Type == TypeString && !f.IsArray() && baseGoTypeName(f.GoType) == "string"
}

// baseGoTypeName strips the pointer and slice markers from a Go type.
func baseGoTypeName(goType string) string { return strings.TrimLeft(goType, "*[]") }

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

	// Unexposed keeps a table out of the API while keeping its model and
	// repository.
	//
	// The authentication foundation is why this exists. Sessions and API keys
	// need generated persistence and must not have generated CRUD endpoints:
	// nothing good comes of a REST interface for the token table. Saying so on
	// the resource rather than deleting it is what lets the data layer stay
	// generated while the API layer stays silent.
	//
	// It is negative so that the zero value is the ordinary case.
	Unexposed bool `json:"unexposed,omitempty"`

	Operations []string `json:"operations"`
	// Public names the operations that answer without a credential, generated
	// and custom alike. An endpoint carries the resolved flag; this is the
	// declaration the endpoints are built from.
	Public    []string        `json:"public,omitempty"`
	Fields    []ResourceField `json:"fields"`
	Endpoints []Endpoint      `json:"endpoints"`

	// Files are this resource's file columns, in the order they appear on the
	// table. Each one yields an upload, a download and a delete endpoint, two
	// permission keys, and a part on the multipart form the create also accepts.
	//
	// Resolved once, where the schema is still in scope: recognizing one means
	// reading the table's foreign keys, and by the time a generator holds a
	// resource the constraints are gone. A generator that re-derived it from a
	// field name would be re-deriving the convention the compiler exists to have
	// already settled.
	Files []FileColumn `json:"files,omitempty"`

	// Notifiable says this table is joined to rig_notification by a link table,
	// so notifications can be about its rows.
	//
	// It is derived from the schema rather than declared: any link table one
	// side of which is rig_notification makes the other side notifiable, so the
	// recommended name — blog_post_notification — is a recommendation and
	// nothing depends on it. There is nothing for a name to say here that the
	// foreign key does not.
	//
	// A notifiable resource's rules interface grows two required methods: when
	// notifications about a row are due, and — at the moment of sending — who
	// should hear about it.
	Notifiable bool `json:"notifiable,omitempty"`

	// Cached says this resource's Get is held in memory between requests, keyed
	// by the row and the scope the read was made under, and withdrawn by a
	// Postgres NOTIFY published on the transaction of every write the generated
	// repository makes to the row.
	//
	// Declared per table rather than derived, because it is a promise about the
	// application as much as a fact about the schema: rig can only publish the
	// withdrawal from writes it makes, so a project that writes this table
	// through Store.Pool or with raw SQL from inside a dbhook is a project where
	// this serves a stale row until the entry expires. Nothing in the schema says
	// whether that is true, so nothing but a person can turn this on.
	//
	// It needs [API.Cache], which is what owns the channel the withdrawals
	// travel on.
	Cached bool `json:"cached,omitempty"`

	// Parents are the resources this one points at, one per foreign key, in the
	// order the columns appear on the table. Each becomes a pair of hook fields
	// the service layer may fill in.
	Parents []ParentLink `json:"parents,omitempty"`

	// Children are the resources that point at this one, in the order they are
	// told about a delete.
	//
	// The order is derived — referencing tables before referenced ones — and
	// resolved here rather than in a generator, because it is a fact about the
	// whole schema and a generator holds one resource at a time.
	Children []ChildLink `json:"children,omitempty"`

	// Storage is nil for a virtual resource with no table behind it.
	Storage *ResourceStorage `json:"storage,omitempty"`
	// Electric is set when the resource exposes a live-sync shape endpoint.
	Electric *ElectricEndpoint `json:"electric,omitempty"`
}

// ParentLink is one foreign key from a resource to another resource.
//
// Only a foreign key to a table rig generates a service for is here. A column
// pointing at rig_file or at an audit actor has no service to declare a hook in,
// and rig should not pretend it can call one.
type ParentLink struct {
	// Name is the accessor, matching the BelongsTo relation: HomeTeam out of
	// home_team_id. It is what the two hook fields are named after.
	Name string `json:"name"`
	// Parent is the resource name, Table the physical table behind it.
	Parent string `json:"parent"`
	Table  string `json:"table"`
	// Column is the foreign key on this resource's own table.
	Column string `json:"column"`
}

// ChildLink is one foreign key pointing at a resource, from the parent's side.
type ChildLink struct {
	// Name is the HasMany accessor: HomeFixtures out of fixture.home_team_id.
	Name string `json:"name"`
	// Child is the resource name, Table the physical table behind it.
	Child string `json:"child"`
	Table string `json:"table"`
	// Column is the foreign key on the child's table.
	Column string `json:"column"`
	// Hook is the field on the child's parent-hooks struct this edge fills:
	// HomeTeam, matching the child's own [ParentLink.Name].
	Hook string `json:"hook"`
}

// Supports reports whether the resource has the given operation.
func (r *Resource) Supports(op string) bool { return slices.Contains(r.Operations, op) }

// IsPublic reports whether an operation answers without a credential.
func (r *Resource) IsPublic(op string) bool { return slices.Contains(r.Public, op) }

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

	// The key columns record which API key a change came through, where the
	// account columns record whose account it was. An integration's service
	// account may be shared between several keys, so the pair together say both
	// "the nightly import did this" and "through this credential".
	CreatedByAPIKey *ColumnRef `json:"created_by_api_key,omitempty"`
	UpdatedByAPIKey *ColumnRef `json:"updated_by_api_key,omitempty"`
	DeletedByAPIKey *ColumnRef `json:"deleted_by_api_key,omitempty"`
}

// SoftDelete marks a table whose rows are retired by stamping a timestamp
// rather than being removed.
type SoftDelete struct {
	Column *ColumnRef `json:"column"`
	Actor  *ColumnRef `json:"actor,omitempty"`
	// ActorKey is the API key the deletion came through, when the table has a
	// column for it. It is stamped beside Actor rather than instead of it.
	ActorKey *ColumnRef `json:"actor_key,omitempty"`
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

	// Owner is the column a read is narrowed by when the table asked for it —
	// the account that created the row.
	//
	// Set only by `access: { scope: own }`, because it changes what a request
	// answers with and that is not something to infer from a column being
	// present. Plenty of tables record who created a row without meaning that
	// nobody else may read it.
	Owner *ColumnRef `json:"owner,omitempty"`

	Audit      *AuditColumns `json:"audit,omitempty"`
	SoftDelete *SoftDelete   `json:"soft_delete,omitempty"`
	Snapshot   *Snapshot     `json:"snapshot,omitempty"`

	DefaultOrder []OrderTerm `json:"default_order,omitempty"`
	Relations    []Relation  `json:"relations,omitempty"`

	Sortable   []string `json:"sortable,omitempty"`
	Filterable []string `json:"filterable,omitempty"`
}

// IsOwnerScoped reports whether a read of this resource defaults to the rows the
// caller created.
func (s *ResourceStorage) IsOwnerScoped() bool { return s != nil && s.Owner != nil }

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

// ElectricStreamSuffix marks a route as a live-sync stream.
//
// It is the last segment of every shape's route, so it reads as something done
// to whatever precedes it — subscribe to this — and a shape's route is the read
// surface it streams plus one marker. That is why the trash and history shapes
// are composed by inserting before it rather than appending after it.
const ElectricStreamSuffix = "/_stream"

// ElectricEndpoint describes a live-sync shape endpoint for a resource. The
// tenant and lifecycle predicates are built by rig; the declared params are
// passed to the application's own scoping function.
type ElectricEndpoint struct {
	Auth ElectricAuth `json:"auth"`
	// Path is the live shape's route, for example "/api/v1/lesson/_stream".
	// It is written relative to the API's base path and expanded to the full
	// route when the document is frozen, the same way an endpoint's is.
	Path   string          `json:"path"`
	Params []ElectricParam `json:"params,omitempty"`
}

// DeletedPath is the route of the trash shape, and VersionsPath the route of the
// history one.
//
// Both are the live shape's route with a segment spliced in ahead of the stream
// marker. They live here rather than in the generator that mounts them so that
// there is one answer to where the marker goes: a second place composing these
// by hand is a second place that could put it somewhere else.
func (e ElectricEndpoint) DeletedPath() string {
	return e.stem() + "/_deleted" + ElectricStreamSuffix
}

// VersionsPath is the route of the history shape. See DeletedPath.
func (e ElectricEndpoint) VersionsPath() string {
	return e.stem() + "/{id}/_versions" + ElectricStreamSuffix
}

// stem is the route without its stream marker: what the shapes are shapes of.
func (e ElectricEndpoint) stem() string {
	return strings.TrimSuffix(e.Path, ElectricStreamSuffix)
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
	// Public answers without a credential.
	//
	// It means the endpoint does not require the claims lookup to succeed, not
	// that the lookup is skipped: an application that resolves a tenant from
	// the host rather than from a token still gets one, and a caller who does
	// present a credential is still identified by it. What changes is that a
	// caller who presents nothing is served instead of refused.
	Public bool `json:"public,omitempty"`

	// Permission is the RBAC key required to call this endpoint. Empty means a
	// valid session is enough.
	Permission string `json:"permission,omitempty"`

	// WidePermission is the key required to pass ?scope=all, on a read of an
	// owner-scoped resource. Empty everywhere else.
	//
	// Separate from Permission because they answer different questions and a
	// caller can hold one without the other. Reading your own notes and reading
	// everybody's are two grants, which is the whole point of the parameter.
	WidePermission string `json:"wide_permission,omitempty"`

	// File is the file column this endpoint acts on, set on the three rig
	// synthesizes per file column and nil on everything else.
	//
	// The upload's own [EndpointRequest.FileParts] says what the form carries;
	// this says which column the bytes end up on, which the download and the
	// delete need just as much and neither of them has a form.
	File *FileColumn `json:"file,omitempty"`

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
	// ContentTypes are the media types this endpoint accepts, most preferred
	// first.
	//
	// A list rather than one string because an endpoint can honestly accept two:
	// a create on a table with a file column takes either a JSON body or a
	// multipart form carrying the same JSON in one part and the bytes in
	// another. Everything else has exactly one, and a generator that wants "the"
	// content type takes the first.
	ContentTypes []string `json:"content_types,omitempty"`

	// FileParts are the file parts a multipart request carries, in the order the
	// server reads them. Empty on every endpoint that is JSON only.
	//
	// It is here because [EndpointRequest.ContentTypes] alone is not enough to
	// render a multipart request: it says the endpoint takes a form, and says
	// nothing about what the parts are called. A generator left to work that out
	// would re-derive the <role>_file_id convention from a field's name — which
	// is the convention the compiler exists to have already resolved, and which
	// three generators deriving separately would eventually disagree about.
	FileParts []FilePart `json:"file_parts,omitempty"`

	Headers     []Field `json:"headers,omitempty"`
	PathParams  []Field `json:"path_params,omitempty"`
	QueryParams []Field `json:"query_params,omitempty"`
	BodyParams  []Field `json:"body_params,omitempty"`
	// BodyObject names a whole object as the body, used instead of BodyParams.
	BodyObject string `json:"body_object,omitempty"`
}

// FileColumn is one `<role>_file_id` column on a resource: a single file
// attached to each row, in a named role.
//
// The column is the declaration, so everything here is derived from its name
// and nothing is configurable. It is spelled out rather than left to each
// generator because five things follow from one column — three endpoints, a
// form part, two permission keys, a Go field and a path segment — and five
// derivations of one convention is five places for it to drift.
type FileColumn struct {
	// Role is the `<role>` from `<role>_file_id`, in the API's own casing, for
	// example "profileImage".
	Role string `json:"role"`
	// Column is the column itself, for example "profile_image_file_id". It is
	// what the upload writes and the download reads, and it is the one member
	// [FilePart] deliberately does not carry.
	Column string `json:"column"`
	// Field is the Go field on the row, for example "ProfileImageFileID".
	Field string `json:"field"`
	// Part is the multipart part's name, for example "profileImageFile".
	Part string `json:"part"`
	// Segment is the path segment the file endpoints sit under, for example
	// "profile-image-file". It is derived here so the router, the client and a
	// specification cannot spell it three ways.
	Segment string `json:"segment"`
	// Required says the column cannot be null.
	Required bool `json:"required,omitempty"`
}

// GoName is the column's Go name without the identifier suffix, for example
// "ProfileImageFile" out of "ProfileImageFileID".
//
// It is the stem every generated identifier for this column is built from —
// UploadProfileImageFile, DownloadProfileImageFile, the member on the create's
// files struct — so that four generators do not each decide where to cut.
func (f FileColumn) GoName() string { return strings.TrimSuffix(f.Field, "ID") }

// FilePart is one file a multipart request carries.
type FilePart struct {
	// Name is the part's name on the wire, for example "profileImageFile". It
	// is what the server binds the bytes to, so a client that spells it
	// differently has uploaded a part nobody claimed.
	Name string `json:"name"`
	// Field is the Go field on the owning row, for example
	// "ProfileImageFileID". A generator naming a member after the part rather
	// than after the column would produce a shape that reads nothing like the
	// row it belongs to.
	Field string `json:"field"`
	// Role is the <role> from <role>_file_id, for example "profileImage".
	Role string `json:"role"`
	// Required says the column cannot be null, so the part has to be present.
	// It is the whole reason a multipart create exists: a not-null file column
	// is unreachable when the row and its bytes are two requests.
	Required bool `json:"required,omitempty"`
}

// The media types rig's own endpoints speak.
//
// They live here rather than in the compiler because they are written into the
// document by one package and read back out of it by every generator that
// renders a request — an OpenAPI document must not claim application/json for
// an endpoint whose document says otherwise, and a client must not send JSON to
// one that takes a form. Until files, every endpoint said JSON, so nothing had
// ever been asked.
const (
	MediaJSON = "application/json"
	// MediaMultipart is what an upload arrives as, and what a create accepts in
	// addition to JSON on a table with a file column.
	MediaMultipart = "multipart/form-data"
	// MediaOctet is the fallback a download announces when the file's own
	// sniffed type is not known at generation time — which it never is.
	MediaOctet = "application/octet-stream"
)

// EndpointResponse is one possible outcome. An endpoint always lists every
// status it can return, so 200, 404, and 409 are all first-class.
type EndpointResponse struct {
	StatusCode  int    `json:"status_code"`
	Description string `json:"description,omitempty"`
	// ContentTypes are what this response can be. A list for symmetry with the
	// request, and because a download answers with whatever the file turned out
	// to be rather than with one type known at generation time.
	ContentTypes []string `json:"content_types,omitempty"`
	Headers      []Field  `json:"headers,omitempty"`
	BodyFields   []Field  `json:"body_fields,omitempty"`
	BodyObject   string   `json:"body_object,omitempty"`
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

// Throttle is the resolved throttle block: how many calls one caller may make.
//
// Fair-use limiting rather than a defence against a flood. What reaches this is
// a request that already cost a connection, a handshake and a goroutine, so the
// arithmetic here bounds abuse and cost rather than volume — see docs/api.md.
type Throttle struct {
	Enabled bool `json:"enabled"`

	// APIKey, Account and Tenant apply to a caller who said who they were; IP
	// applies only to one who did not. A zero limit is one nobody configured,
	// and the runtime fills it from its own defaults.
	APIKey  ThrottleLimit `json:"api_key"`
	Account ThrottleLimit `json:"account"`
	Tenant  ThrottleLimit `json:"tenant"`
	IP      ThrottleLimit `json:"ip"`

	// IntervalSeconds is how long a replica may count locally before publishing.
	// It is the accuracy of the limit stated as time, so it belongs in the
	// document beside the numbers it qualifies.
	//
	// Fractional, unlike every other duration here: sub-second is a reasonable
	// thing to ask for from something that runs on every request, where a
	// sub-second *window* is not.
	IntervalSeconds float64 `json:"interval_seconds,omitempty"`

	// Routes are extra limits on particular patterns, on top of the per-caller
	// ones.
	Routes []ThrottleRoute `json:"routes,omitempty"`

	// Exempt are patterns nothing applies to.
	Exempt []string `json:"exempt,omitempty"`
}

// ThrottleLimit is how many, and over how long.
type ThrottleLimit struct {
	Max           int   `json:"max,omitempty"`
	WindowSeconds int64 `json:"window_seconds,omitempty"`
}

// ThrottleRoute is one route pattern's own limit.
type ThrottleRoute struct {
	// Pattern is the route as the router registers it, matched against what
	// net/http reports rather than against a path.
	Pattern       string `json:"pattern"`
	Max           int    `json:"max"`
	WindowSeconds int64  `json:"window_seconds"`
}

// Cache is the resolved cache block: whether rig holds its own per-request reads
// in memory, and the channel that withdraws them.
//
// The three numbers here describe a mechanism rather than a surface. Nothing a
// client sends or receives changes when they do, which is what makes this the
// one field [Document.Hash] clears outright rather than in part.
type Cache struct {
	// Enabled says rig caches the reads it owns end to end — session and API key
	// verification. It is what makes server-go write the wiring.
	Enabled bool `json:"enabled"`

	// TTLSeconds is how long an answer may be reused with no invalidation
	// arriving. It is the backstop rather than the guarantee: a revocation is
	// published on the transaction that made it, and reaches every replica when
	// that transaction commits.
	//
	// Fractional for the reason [Throttle.IntervalSeconds] is: this is a number
	// somebody may reasonably want below a second.
	TTLSeconds float64 `json:"ttl_seconds"`

	// Channel is the Postgres NOTIFY channel invalidations travel on. Validated
	// as an identifier when the configuration was read, because it reaches
	// Postgres both quoted in a LISTEN and as a parameter to pg_notify.
	Channel string `json:"channel"`

	// MaxEntries bounds one cache. Past it the whole map is dropped rather than
	// swept — see runtime/cache.
	MaxEntries int `json:"max_entries,omitempty"`
}

// Package project reads rig.yaml, the file that marks the root of a rig
// project and configures everything that is not per-table.
//
// Discovery walks up from the working directory, so rig commands work from
// anywhere inside a project. Every path in the file resolves relative to the
// file's own directory rather than the working directory, so the same
// configuration means the same thing regardless of where it is run from.
package project

// Config is the contents of rig.yaml.
type Config struct {
	Version int `yaml:"version,omitempty" json:"version,omitempty" jsonschema_description:"Configuration format version."`

	Project    ProjectInfo `yaml:"project" json:"project" jsonschema_description:"Identity of the application being generated."`
	Layout     Layout      `yaml:"layout,omitempty" json:"layout,omitempty" jsonschema_description:"Where table configuration and generated code live."`
	API        API         `yaml:"api,omitempty" json:"api,omitempty" jsonschema_description:"Shape of the generated HTTP API."`
	OpenAPI    OpenAPI     `yaml:"openapi,omitempty" json:"openapi,omitempty" jsonschema_description:"OpenAPI document settings."`
	Database   Database    `yaml:"database,omitempty" json:"database,omitempty" jsonschema_description:"Where to run migrations and read the schema from."`
	Migrations Migrations  `yaml:"migrations,omitempty" json:"migrations,omitempty" jsonschema_description:"Migration file location."`
	Naming     Naming      `yaml:"naming,omitempty" json:"naming,omitempty" jsonschema_description:"How database names become Go and JSON names."`
	Validate   Validate    `yaml:"validate,omitempty" json:"validate,omitempty" jsonschema_description:"Severity of each configurable convention rule."`

	Generators []Generator `yaml:"generators,omitempty" json:"generators,omitempty" jsonschema_description:"Generators to run, in order."`
}

// ProjectInfo identifies the application.
type ProjectInfo struct {
	Name string `yaml:"name" json:"name" jsonschema_description:"Short name of the application."`
	// Module is the Go module path of the generated application. Generated
	// imports are built from it.
	Module string `yaml:"module" json:"module" jsonschema_description:"Go module path of the application, used to build generated import paths."`
}

// Layout says where a table's configuration and code live.
//
// The templates take {table} for the snake_case name, {Table} for PascalCase,
// and {tables} for the plural. Putting a table's configuration next to the
// service code that implements it keeps everything about one table in one
// directory; a flat layout is a one-line change.
type Layout struct {
	TableDir   string `yaml:"table_dir,omitempty" json:"table_dir,omitempty" jsonschema_description:"Directory for a table's configuration and service code. Templates: {table} {Table} {tables}."`
	ConfigFile string `yaml:"config_file,omitempty" json:"config_file,omitempty" jsonschema_description:"Path to a table's configuration file. May reference {table_dir}."`
}

// SearchMethod selects how the Search operation is exposed.
type SearchMethod string

const (
	// SearchQuery exposes Search as HTTP QUERY only.
	SearchQuery SearchMethod = "query"
	// SearchPost exposes Search as POST to a /_search sub-path only.
	SearchPost SearchMethod = "post"
	// SearchBoth exposes QUERY with the POST sub-path as an alias, which is the
	// default: QUERY is the correct method for a read with a body, but some
	// intermediaries still reject methods they do not recognize.
	SearchBoth SearchMethod = "both"
)

// API configures the generated HTTP surface.
type API struct {
	Name        string `yaml:"name,omitempty" json:"name,omitempty" jsonschema_description:"Name of the API, used in documentation and generated type prefixes."`
	Version     string `yaml:"version,omitempty" json:"version,omitempty" jsonschema_description:"API version segment, for example v1."`
	BasePath    string `yaml:"base_path,omitempty" json:"base_path,omitempty" jsonschema_description:"Prefix every route sits under, for example /api/v1."`
	Description string `yaml:"description,omitempty" json:"description,omitempty" jsonschema_description:"What this API is for."`

	SearchMethod SearchMethod `yaml:"search_method,omitempty" json:"search_method,omitempty" jsonschema:"enum=query,enum=post,enum=both" jsonschema_description:"How the Search operation is exposed. Defaults to both: QUERY with a POST alias."`
}

// OpenAPI configures the emitted specification.
type OpenAPI struct {
	// Version selects the specification version. 3.1 has no way to express an
	// operation on the QUERY method, so under 3.1 the POST alias is what gets
	// documented; 3.2 emits the QUERY operation directly.
	Version string `yaml:"version,omitempty" json:"version,omitempty" jsonschema:"enum=3.1,enum=3.2" jsonschema_description:"OpenAPI specification version. 3.1 documents the POST alias for Search; 3.2 emits QUERY natively."`
}

// Database says where rig runs migrations and reads the schema.
//
// With no URL, rig starts a throwaway container, applies the migrations, reads
// the schema back, and leaves the container running for the next command. Set
// URL to point at a database you manage instead, which is what CI does.
type Database struct {
	Image         string `yaml:"image,omitempty" json:"image,omitempty" jsonschema_description:"Container image for the throwaway database."`
	ContainerName string `yaml:"container_name,omitempty" json:"container_name,omitempty" jsonschema_description:"Name of the throwaway container."`
	Port          int    `yaml:"port,omitempty" json:"port,omitempty" jsonschema_description:"Host port the throwaway database listens on."`
	Name          string `yaml:"name,omitempty" json:"name,omitempty" jsonschema_description:"Database name."`
	User          string `yaml:"user,omitempty" json:"user,omitempty" jsonschema_description:"Database user."`
	Password      string `yaml:"password,omitempty" json:"password,omitempty" jsonschema_description:"Database password. This is a local throwaway database; do not put a real secret here."`
	Schema        string `yaml:"schema,omitempty" json:"schema,omitempty" jsonschema_description:"Postgres schema to read. Defaults to public."`

	// URL, when set, is used as-is and no container is started.
	URL string `yaml:"url,omitempty" json:"url,omitempty" jsonschema_description:"Connection URL of an existing database. Set this to skip the throwaway container entirely."`
}

// Migrations says where migration files live.
type Migrations struct {
	Dir string `yaml:"dir,omitempty" json:"dir,omitempty" jsonschema_description:"Directory holding goose migration files."`
	// Table is the goose bookkeeping table.
	Table string `yaml:"table,omitempty" json:"table,omitempty" jsonschema_description:"Name of the migration bookkeeping table."`
}

// Naming tunes the conversion from database names to code and wire names.
type Naming struct {
	JSONCase string `yaml:"json_case,omitempty" json:"json_case,omitempty" jsonschema:"enum=camel,enum=pascal,enum=snake" jsonschema_description:"Shape of generated JSON keys. Defaults to camel."`
	// Initialisms are added to the built-in list of acronyms that stay
	// uppercase in Go identifiers.
	Initialisms []string `yaml:"initialisms,omitempty" json:"initialisms,omitempty" jsonschema_description:"Extra acronyms that stay uppercase in Go identifiers, for example SCB."`
	// Plurals overrides derived plurals, keyed by the singular table name.
	Plurals map[string]string `yaml:"plurals,omitempty" json:"plurals,omitempty" jsonschema_description:"Plural overrides keyed by table name, for the words English inflection gets wrong."`
}

// Validate sets the severity of each configurable convention rule. Every value
// is one of off, warn, or error. Structural rules are not listed here: a
// schema that breaks them cannot be generated from at all.
type Validate struct {
	UnmentionedColumn    string `yaml:"unmentioned_column,omitempty" json:"unmentioned_column,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A column exists in the database but is not mentioned in its table configuration."`
	MissingComment       string `yaml:"missing_comment,omitempty" json:"missing_comment,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A table or column has no comment."`
	ForeignKeyNotIndexed string `yaml:"fk_needs_index,omitempty" json:"fk_needs_index,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A foreign-key column is not covered by an index."`
	TenantIndexLeading   string `yaml:"tenant_id_leading_index,omitempty" json:"tenant_id_leading_index,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"No index leads with the tenant column."`
	BooleanPrefix        string `yaml:"boolean_prefix,omitempty" json:"boolean_prefix,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A boolean column does not read as a predicate."`
	TimestampSuffix      string `yaml:"timestamp_suffix,omitempty" json:"timestamp_suffix,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A timestamp column is not named _at, or an _at column is not a timestamp."`
	DateSuffix           string `yaml:"date_suffix,omitempty" json:"date_suffix,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A date column is not named _date, or a _date column is not a date."`
	ForeignKeyNaming     string `yaml:"fk_naming,omitempty" json:"fk_naming,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A foreign-key column is not named after the table it references."`
	CascadeDelete        string `yaml:"cascade_delete,omitempty" json:"cascade_delete,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A foreign key declares ON DELETE CASCADE."`
	MigrationFilename    string `yaml:"migration_filename,omitempty" json:"migration_filename,omitempty" jsonschema:"enum=off,enum=warn,enum=error" jsonschema_description:"A migration file is not named NNNNN_snake_case.sql."`
}

// Generator selects one generator to run and configures it.
//
// Selection is by name rather than by output path, so adding a generator adds
// no fields to this struct.
type Generator struct {
	Name string `yaml:"name" json:"name" jsonschema_description:"Registered generator name, for example persist-go."`
	// OutDir overrides the generator's default output directory, relative to
	// the project root.
	OutDir string `yaml:"out_dir,omitempty" json:"out_dir,omitempty" jsonschema_description:"Output directory, relative to the project root."`
	// Options is validated against the generator's own schema, not this one.
	Options map[string]any `yaml:"options,omitempty" json:"options,omitempty" jsonschema_description:"Generator-specific options. Each generator publishes its own schema for this block."`
}

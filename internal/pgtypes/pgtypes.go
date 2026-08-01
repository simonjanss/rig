// Package pgtypes maps Postgres types onto Go types and scan strategies.
//
// The mapping lives in one table so that the persistence generator, the API
// generator, and the TypeScript generator all derive their view of a column
// from the same decision. When a type is not in the table, rig refuses to
// generate rather than guessing: a silently wrong scan is far worse to debug
// than a message naming the column.
package pgtypes

import (
	"strings"

	"github.com/simonjanss/rig/pkg/ir"
)

// Import paths for the Go types the mapping table refers to.
const (
	ImportTime    = "time"
	ImportUUID    = "github.com/google/uuid"
	ImportJSON    = "encoding/json"
	ImportNetip   = "net/netip"
	ImportPgtype  = "github.com/jackc/pgx/v5/pgtype"
	ImportDecimal = ImportPgtype
)

// Mapping is everything rig needs to know about a column's type.
type Mapping struct {
	// IRType is the API-facing primitive name, one of the ir.Type* constants.
	IRType string
	// GoType is the Go type for a NOT NULL column. Nullable columns take a
	// pointer to it, except for types that are already nilable.
	GoType string
	// Import is the package GoType comes from, empty for builtins.
	Import string
	// Scan tells the persistence layer how to move the value in and out of pgx.
	Scan ir.ScanStrategy
	// TSType is the TypeScript type for the value.
	TSType string
	// NilableGoType marks Go types that already carry a nil state, such as
	// slices. Making them pointers would add a second, meaningless nil.
	NilableGoType bool
}

// table maps a normalized Postgres type name to its mapping. Keys are the
// canonical pg_type names plus the SQL spellings people actually write.
var table = map[string]Mapping{
	// Text.
	"text":              {IRType: ir.TypeString, GoType: "string", Scan: ir.ScanDirect, TSType: "string"},
	"varchar":           {IRType: ir.TypeString, GoType: "string", Scan: ir.ScanDirect, TSType: "string"},
	"character varying": {IRType: ir.TypeString, GoType: "string", Scan: ir.ScanDirect, TSType: "string"},
	"char":              {IRType: ir.TypeString, GoType: "string", Scan: ir.ScanDirect, TSType: "string"},
	"character":         {IRType: ir.TypeString, GoType: "string", Scan: ir.ScanDirect, TSType: "string"},
	"bpchar":            {IRType: ir.TypeString, GoType: "string", Scan: ir.ScanDirect, TSType: "string"},
	"citext":            {IRType: ir.TypeString, GoType: "string", Scan: ir.ScanDirect, TSType: "string"},
	"name":              {IRType: ir.TypeString, GoType: "string", Scan: ir.ScanDirect, TSType: "string"},

	// Identity.
	"uuid": {IRType: ir.TypeUUID, GoType: "uuid.UUID", Import: ImportUUID, Scan: ir.ScanUUID, TSType: "string"},

	// Boolean.
	"bool":    {IRType: ir.TypeBool, GoType: "bool", Scan: ir.ScanDirect, TSType: "boolean"},
	"boolean": {IRType: ir.TypeBool, GoType: "bool", Scan: ir.ScanDirect, TSType: "boolean"},

	// Integers. int4 maps to Go's int: it is 64-bit on every platform rig
	// supports, so nothing is truncated, and it is what an idiomatic Go API
	// would use.
	"int2":     {IRType: ir.TypeInt, GoType: "int16", Scan: ir.ScanDirect, TSType: "number"},
	"smallint": {IRType: ir.TypeInt, GoType: "int16", Scan: ir.ScanDirect, TSType: "number"},
	"int4":     {IRType: ir.TypeInt, GoType: "int", Scan: ir.ScanDirect, TSType: "number"},
	"integer":  {IRType: ir.TypeInt, GoType: "int", Scan: ir.ScanDirect, TSType: "number"},
	"int8":     {IRType: ir.TypeInt64, GoType: "int64", Scan: ir.ScanDirect, TSType: "number"},
	"bigint":   {IRType: ir.TypeInt64, GoType: "int64", Scan: ir.ScanDirect, TSType: "number"},

	// Floating point.
	"float4":           {IRType: ir.TypeFloat64, GoType: "float32", Scan: ir.ScanDirect, TSType: "number"},
	"real":             {IRType: ir.TypeFloat64, GoType: "float32", Scan: ir.ScanDirect, TSType: "number"},
	"float8":           {IRType: ir.TypeFloat64, GoType: "float64", Scan: ir.ScanDirect, TSType: "number"},
	"double precision": {IRType: ir.TypeFloat64, GoType: "float64", Scan: ir.ScanDirect, TSType: "number"},

	// Exact numerics travel as strings on the wire: JSON numbers are IEEE
	// doubles, and rounding a monetary amount in transit is not acceptable.
	"numeric": {IRType: ir.TypeDecimal, GoType: "pgtype.Numeric", Import: ImportDecimal, Scan: ir.ScanNumeric, TSType: "string"},
	"decimal": {IRType: ir.TypeDecimal, GoType: "pgtype.Numeric", Import: ImportDecimal, Scan: ir.ScanNumeric, TSType: "string"},
	"money":   {IRType: ir.TypeDecimal, GoType: "pgtype.Numeric", Import: ImportDecimal, Scan: ir.ScanNumeric, TSType: "string"},

	// Dates and times.
	"date":                        {IRType: ir.TypeDate, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanDirect, TSType: "string"},
	"time":                        {IRType: ir.TypeTime, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanDirect, TSType: "string"},
	"time without time zone":      {IRType: ir.TypeTime, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanDirect, TSType: "string"},
	"timetz":                      {IRType: ir.TypeTime, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanDirect, TSType: "string"},
	"time with time zone":         {IRType: ir.TypeTime, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanDirect, TSType: "string"},
	"timestamp":                   {IRType: ir.TypeTimestamp, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanTimestamptz, TSType: "string"},
	"timestamp without time zone": {IRType: ir.TypeTimestamp, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanTimestamptz, TSType: "string"},
	"timestamptz":                 {IRType: ir.TypeTimestamp, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanTimestamptz, TSType: "string"},
	"timestamp with time zone":    {IRType: ir.TypeTimestamp, GoType: "time.Time", Import: ImportTime, Scan: ir.ScanTimestamptz, TSType: "string"},

	// Structured.
	"json":  {IRType: ir.TypeJSON, GoType: "json.RawMessage", Import: ImportJSON, Scan: ir.ScanJSONB, TSType: "unknown", NilableGoType: true},
	"jsonb": {IRType: ir.TypeJSON, GoType: "json.RawMessage", Import: ImportJSON, Scan: ir.ScanJSONB, TSType: "unknown", NilableGoType: true},
	"bytea": {IRType: ir.TypeBytes, GoType: "[]byte", Scan: ir.ScanDirect, TSType: "string", NilableGoType: true},

	// Network types, used by the authentication foundation.
	"inet": {IRType: ir.TypeString, GoType: "netip.Addr", Import: ImportNetip, Scan: ir.ScanDirect, TSType: "string"},
	"cidr": {IRType: ir.TypeString, GoType: "netip.Prefix", Import: ImportNetip, Scan: ir.ScanDirect, TSType: "string"},
}

// Lookup resolves a column's type.
//
// sqlType is the type as written in the schema and udtName is the underlying
// pg_type name; they differ for arrays, where udtName is the element type
// prefixed with an underscore. An array yields the element's mapping with a
// slice Go type and the Array scan strategy, so callers see one mapping rather
// than having to unwrap element types themselves.
func Lookup(sqlType, udtName string) (m Mapping, isArray bool, ok bool) {
	norm := normalize(sqlType)

	// Arrays present as "text[]" in sqlType, or as "ARRAY" with the element in
	// udtName as "_text". Either spelling reaches the element the same way.
	elem, arr := arrayElement(norm, udtName)
	if arr {
		em, ok := table[normalize(elem)]
		if !ok {
			return Mapping{}, true, false
		}
		em.GoType = "[]" + em.GoType
		em.TSType = em.TSType + "[]"
		em.Scan = ir.ScanArray
		em.NilableGoType = true
		return em, true, true
	}

	if m, ok := table[norm]; ok {
		return m, false, true
	}
	// Fall back to the underlying type name, which is how domains and some
	// spellings reach their base type.
	if udtName != "" {
		if m, ok := table[normalize(udtName)]; ok {
			return m, false, true
		}
	}
	return Mapping{}, false, false
}

// EnumMapping is the mapping for a column whose type is a Postgres enum. The
// Go type is the generated enum, which the caller names.
func EnumMapping(goType string) Mapping {
	return Mapping{
		IRType: goType,
		GoType: goType,
		Scan:   ir.ScanEnumText,
		TSType: goType,
	}
}

// ArrayElementName returns the element type of an array type, and whether the
// type is an array at all.
func ArrayElementName(sqlType, udtName string) (string, bool) {
	elem, ok := arrayElement(normalize(sqlType), udtName)
	return elem, ok
}

func arrayElement(normSQLType, udtName string) (string, bool) {
	if elem, ok := strings.CutSuffix(normSQLType, "[]"); ok {
		return elem, true
	}
	// Introspection reports an array as the type "ARRAY" with the element in
	// udtName, prefixed with an underscore.
	if normSQLType == "array" || normSQLType == "" {
		if elem, ok := strings.CutPrefix(udtName, "_"); ok {
			return elem, true
		}
	}
	return "", false
}

func normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	// Drop a length or precision qualifier: "varchar(255)" and
	// "numeric(10, 2)" map exactly as their unqualified forms do.
	if head, rest, found := strings.Cut(s, "("); found {
		if _, tail, closed := strings.Cut(rest, ")"); closed {
			s = strings.TrimSpace(head) + strings.TrimSpace(tail)
		}
	}
	return strings.TrimSpace(s)
}

// GoTypeFor renders the Go type for a column, adding a pointer for nullable
// columns whose type does not already carry a nil state.
func GoTypeFor(m Mapping, nullable bool) string {
	if nullable && !m.NilableGoType {
		return "*" + m.GoType
	}
	return m.GoType
}

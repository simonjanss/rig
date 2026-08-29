package pgtypes_test

import (
	"testing"

	"github.com/simonjanss/rig/internal/pgtypes"
	"github.com/simonjanss/rig/pkg/ir"
)

func TestLookupScalars(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		sqlType string
		udtName string
		irType  string
		goType  string
		scan    ir.ScanStrategy
	}{
		{"text", "text", ir.TypeString, "string", ir.ScanDirect},
		{"uuid", "uuid", ir.TypeUUID, "uuid.UUID", ir.ScanUUID},
		{"boolean", "bool", ir.TypeBool, "bool", ir.ScanDirect},
		{"integer", "int4", ir.TypeInt, "int", ir.ScanDirect},
		{"bigint", "int8", ir.TypeInt64, "int64", ir.ScanDirect},
		{"smallint", "int2", ir.TypeInt, "int16", ir.ScanDirect},
		{"double precision", "float8", ir.TypeFloat64, "float64", ir.ScanDirect},
		{"numeric", "numeric", ir.TypeDecimal, "pgtype.Numeric", ir.ScanNumeric},
		{"date", "date", ir.TypeDate, "time.Time", ir.ScanDirect},
		{"timestamp with time zone", "timestamptz", ir.TypeTimestamp, "time.Time", ir.ScanTimestamptz},
		{"jsonb", "jsonb", ir.TypeJSON, "json.RawMessage", ir.ScanJSONB},
		{"bytea", "bytea", ir.TypeBytes, "[]byte", ir.ScanDirect},
		{"inet", "inet", ir.TypeString, "netip.Addr", ir.ScanDirect},
	} {
		m, isArray, ok := pgtypes.Lookup(tc.sqlType, tc.udtName)
		if !ok {
			t.Errorf("Lookup(%q) failed", tc.sqlType)
			continue
		}
		if isArray {
			t.Errorf("Lookup(%q) reported an array", tc.sqlType)
		}
		if m.IRType != tc.irType || m.GoType != tc.goType || m.Scan != tc.scan {
			t.Errorf("Lookup(%q) = {%s %s %s}, want {%s %s %s}",
				tc.sqlType, m.IRType, m.GoType, m.Scan, tc.irType, tc.goType, tc.scan)
		}
	}
}

func TestLookupIgnoresLengthAndPrecision(t *testing.T) {
	t.Parallel()

	for _, sqlType := range []string{"varchar(255)", "character varying(64)", "numeric(10, 2)", "  TEXT  "} {
		if _, _, ok := pgtypes.Lookup(sqlType, ""); !ok {
			t.Errorf("Lookup(%q) failed; qualifiers and case should not matter", sqlType)
		}
	}
}

func TestLookupArrays(t *testing.T) {
	t.Parallel()

	// Both spellings introspection produces must reach the same mapping.
	for _, tc := range []struct{ sqlType, udtName string }{
		{"text[]", "_text"},
		{"ARRAY", "_text"},
	} {
		m, isArray, ok := pgtypes.Lookup(tc.sqlType, tc.udtName)
		if !ok || !isArray {
			t.Fatalf("Lookup(%q, %q) = ok %v, isArray %v", tc.sqlType, tc.udtName, ok, isArray)
		}
		if m.GoType != "[]string" {
			t.Errorf("Lookup(%q, %q) GoType = %q, want []string", tc.sqlType, tc.udtName, m.GoType)
		}
		if m.TSType != "string[]" {
			t.Errorf("Lookup(%q, %q) TSType = %q, want string[]", tc.sqlType, tc.udtName, m.TSType)
		}
		if m.Scan != ir.ScanArray {
			t.Errorf("Lookup(%q, %q) Scan = %q, want %q", tc.sqlType, tc.udtName, m.Scan, ir.ScanArray)
		}
		if !m.NilableGoType {
			t.Errorf("a slice already carries nil; it should not be marked as needing a pointer")
		}
	}
}

func TestLookupUnknownTypeFails(t *testing.T) {
	t.Parallel()

	// Refusing is the point: guessing a scan for an unmapped type produces
	// code that compiles and then misbehaves at runtime.
	if _, _, ok := pgtypes.Lookup("tsvector", "tsvector"); ok {
		t.Fatal("Lookup of an unmapped type should fail")
	}
	if _, isArray, ok := pgtypes.Lookup("tsvector[]", "_tsvector"); ok || !isArray {
		t.Fatalf("an array of an unmapped element should fail but still report as an array: ok=%v isArray=%v", ok, isArray)
	}
}

func TestLookupFallsBackToUDTName(t *testing.T) {
	t.Parallel()

	// A domain reports its own name as the SQL type and its base type as the
	// UDT name.
	m, _, ok := pgtypes.Lookup("email_address_domain", "text")
	if !ok {
		t.Fatal("a domain should resolve through its base type")
	}
	if m.GoType != "string" {
		t.Errorf("GoType = %q, want string", m.GoType)
	}
}

func TestArrayElementName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		sqlType, udtName, want string
		ok                     bool
	}{
		{"text[]", "_text", "text", true},
		{"ARRAY", "_cidr", "cidr", true},
		{"text", "text", "", false},
	} {
		got, ok := pgtypes.ArrayElementName(tc.sqlType, tc.udtName)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ArrayElementName(%q, %q) = %q, %v; want %q, %v",
				tc.sqlType, tc.udtName, got, ok, tc.want, tc.ok)
		}
	}
}

func TestGoTypeForNullable(t *testing.T) {
	t.Parallel()

	str, _, _ := pgtypes.Lookup("text", "text")
	if got := pgtypes.GoTypeFor(str, false); got != "string" {
		t.Errorf("not-null text = %q, want string", got)
	}
	if got := pgtypes.GoTypeFor(str, true); got != "*string" {
		t.Errorf("nullable text = %q, want *string", got)
	}

	// A slice is already nilable; a pointer to it would add a second,
	// meaningless nil for callers to check.
	arr, _, _ := pgtypes.Lookup("text[]", "_text")
	if got := pgtypes.GoTypeFor(arr, true); got != "[]string" {
		t.Errorf("nullable text[] = %q, want []string", got)
	}

	raw, _, _ := pgtypes.Lookup("jsonb", "jsonb")
	if got := pgtypes.GoTypeFor(raw, true); got != "json.RawMessage" {
		t.Errorf("nullable jsonb = %q, want json.RawMessage", got)
	}
}

func TestEnumMapping(t *testing.T) {
	t.Parallel()

	m := pgtypes.EnumMapping("LessonStatus")
	if m.Scan != ir.ScanEnumText {
		t.Errorf("Scan = %q, want %q", m.Scan, ir.ScanEnumText)
	}
	if m.GoType != "LessonStatus" || m.IRType != "LessonStatus" || m.TSType != "LessonStatus" {
		t.Errorf("enum mapping should use the generated type throughout: %+v", m)
	}
	// An enum is a value type, so a nullable enum column takes a pointer.
	if got := pgtypes.GoTypeFor(m, true); got != "*LessonStatus" {
		t.Errorf("nullable enum = %q, want *LessonStatus", got)
	}
}

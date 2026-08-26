package electric

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Value renders one column of a [Row] the way the sync service renders it.
//
// Every value in a shape response is the text Postgres prints for it, or JSON
// null, and a subscriber decides how to read it from the column's type in
// [Snapshot.Schema] rather than from the JSON. So this returns a string or nil,
// never a number or a bool — a value that arrived typed would be decoded twice
// and disagree with the same column read over the stream.
//
// It takes any because it is called on whatever a row's field happens to be,
// including a nil pointer, which is the null.
//
// Two callers. A hand-written [Fallback] renders its rows with this, because Go
// values are what it has. The read [Config.DB] answers with takes most columns as
// the text Postgres prints and needs no renderer for them, and calls this for the
// four it decodes instead — the unexported binaryFormats says why those four are
// not read as text.
func Value(v any) any {
	switch v := v.(type) {
	case nil:
		return nil

	// Before the general case for a struct, because time.Time has a String of
	// its own and it prints Go's layout rather than Postgres's.
	//
	// RFC 3339 with microseconds, in UTC. The sync service writes a space and a
	// two-digit offset — "2026-08-25 08:06:07.123456+00" — and this writes
	// "2026-08-25T08:06:07.123456Z", because both are the same instant to the
	// parser a generated client installs and only one of them is also what the
	// same column looks like over the API. Microseconds because that is all
	// Postgres stores, so a nanosecond here would be one this row never had.
	case time.Time:
		return v.UTC().Format("2006-01-02T15:04:05.999999Z07:00")

	// Before []byte, which it is underneath. A jsonb column's text is the JSON
	// itself, and quoting it as a byte string would send a subscriber the
	// characters of a document instead of the document.
	case json.RawMessage:
		if v == nil {
			return nil
		}
		return string(v)

	case []byte:
		if v == nil {
			return nil
		}
		return `\x` + fmt.Sprintf("%x", v)

	// Postgres prints a boolean as t or f, and the sync service normalises it to
	// true or false before it reaches a client. This is the normalised form,
	// because a client is what reads it.
	case bool:
		return strconv.FormatBool(v)

	case string:
		return v
	}

	// A pgtype wrapper says whether it holds anything, and an invalid one is a
	// NULL that a codec would otherwise render as an empty string — which is a
	// value, and a different one. A nil slice or map is the same mistake in the
	// other direction: an absent array is not an empty array.
	if isNull(v) {
		return nil
	}

	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface:
		return Value(rv.Elem().Interface())

	// A named string type, which is every enum a generated model carries. The
	// label is the value, and it is already the one the database holds.
	case reflect.String:
		return rv.String()
	case reflect.Bool:
		return strconv.FormatBool(rv.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(rv.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(rv.Uint(), 10)
	case reflect.Float32:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 32)
	case reflect.Float64:
		return strconv.FormatFloat(rv.Float(), 'g', -1, 64)
	}

	// A uuid, and anything else whose String is its Postgres form. Ahead of the
	// codecs below because pgx has no type for a uuid.UUID and would fall
	// through to JSON, which would quote it.
	if s, ok := v.(fmt.Stringer); ok {
		return s.String()
	}

	// An array, a numeric, and the pgtype wrappers a generated model uses for
	// the columns Go has no type for. This is pgx's own text encoder, which is
	// the same code that read the value out of the database in the first place,
	// so an array's quoting and a numeric's scale are not this package's opinion.
	if typ, ok := codecs.TypeForValue(v); ok {
		if out, err := codecs.Encode(typ.OID, pgtype.TextFormatCode, v, nil); err == nil {
			return string(out)
		}
	}

	// A struct or a map somebody put in a jsonb column and scanned into
	// something of their own.
	if out, err := json.Marshal(v); err == nil {
		return string(out)
	}
	return fmt.Sprint(v)
}

// codecs is pgx's text encoder, held once because building one is not free and
// nothing here mutates it.
var codecs = pgtype.NewMap()

// isNull reports whether a value is absent rather than empty.
//
// Three ways it can be, and all three were found by comparing a snapshot
// against what the sync service sent for the same row. A nil pointer is the
// obvious one. A pgtype wrapper — pgtype.Numeric on a numeric column — carries
// its own validity, and an invalid one encodes to "" rather than to nothing. And
// a nil slice is a NULL array, where an empty slice is an array with no
// elements; Postgres tells those apart and so does anything reading them.
func isNull(v any) bool {
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func:
		if rv.IsNil() {
			return true
		}
	}

	if valuer, ok := v.(driver.Valuer); ok {
		if dv, err := valuer.Value(); err == nil && dv == nil {
			return true
		}
	}
	return false
}

// DateOnly renders a date column: the day, with no time and no zone, which is
// what Postgres prints and what the same column looks like over the stream.
//
// It exists because a date and a timestamp are both a time.Time by the time they
// reach here, and [Value] cannot tell them apart. Something upstream can: the
// read [Config.DB] answers with asks the column's own type, and a hand-written
// [Fallback] knows which of its fields is which.
func DateOnly(v any) any { return dayOrClock(v, time.DateOnly) }

// TimeOnly renders a time-of-day column, for the reason [DateOnly] exists — for a
// hand-written [Fallback] only, since Postgres prints a time in one form whatever
// the session says and the read [Config.DB] answers with takes it as it comes.
//
// With the fraction, because a time column keeps microseconds and Postgres
// prints them: the sync service sends "10:06:07.5" for a column holding half a
// second past the minute, and time.TimeOnly would have sent "10:06:07". The
// layout trims trailing zeros, which is what Postgres does too, so a whole
// second is still "10:06:07".
func TimeOnly(v any) any { return dayOrClock(v, clockLayout) }

// clockLayout is time.TimeOnly with the fraction Postgres keeps.
const clockLayout = "15:04:05.999999"

func dayOrClock(v any, layout string) any {
	switch v := v.(type) {
	case nil:
		return nil
	case time.Time:
		return v.UTC().Format(layout)
	}

	if isNull(v) {
		return nil
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		return dayOrClock(rv.Elem().Interface(), layout)
	}
	return Value(v)
}

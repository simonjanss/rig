package electric_test

import (
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/simonjanss/rig/runtime/electric"
)

// priority stands in for a generated enum: a named string type, whose value is
// the label the database holds.
type priority string

// The strings on the right are what a real sync service put on the wire for the
// same values, read off an ElectricSQL 1.6.9 container. Two of them differ on
// purpose and say why.
func TestValueRendersWhatTheSyncServiceRenders(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 25, 10, 6, 7, 123456000, time.FixedZone("+02", 2*60*60))
	notes := "hello"
	count := int64(9007199254740993)

	for _, c := range []struct {
		name string
		in   any
		want any
	}{
		{"text", "a \"quoted\" text", "a \"quoted\" text"},
		{"a text pointer", &notes, "hello"},
		{"a nil text pointer", (*string)(nil), nil},
		{"an enum", priority("Snapshot"), "Snapshot"},
		{"a uuid", uuid.MustParse("a89fb26d-f50a-4411-a578-5dc3ffa2e755"), "a89fb26d-f50a-4411-a578-5dc3ffa2e755"},
		{"int2", int16(32000), "32000"},
		{"int4", int32(2000000000), "2000000000"},
		{"int8", count, "9007199254740993"},
		{"an int8 pointer", &count, "9007199254740993"},
		{"float8", 1.5, "1.5"},
		{"float4", float32(2.5), "2.5"},
		{"numeric", pgtype.Numeric{Int: big.NewInt(123456789), Exp: -4, Valid: true}, "12345.6789"},
		{"a text array", []string{"one", "two, with comma", `has "quote"`}, `{one,"two, with comma","has \"quote\""}`},
		{"an int array", []int32{1, 2, 3}, "{1,2,3}"},
		{"bytea", []byte{0, 255, 16}, `\x00ff10`},
		{"a nil bytea", []byte(nil), nil},
		{"jsonb", json.RawMessage(`{"a":1}`), `{"a":1}`},
		{"a nil jsonb", json.RawMessage(nil), nil},

		// A null numeric and a null array both used to render as "", which is a
		// value and not the absence of one. Both were found by comparing a
		// snapshot against what the sync service sent for the same row.
		{"a null numeric", pgtype.Numeric{}, nil},
		{"a null array", []string(nil), nil},
		{"an empty array", []string{}, "{}"},
		{"nothing at all", nil, nil},

		// A boolean is t or f in Postgres and true or false by the time the sync
		// service has finished with it. This is a client's answer, so it is the
		// second one — "t" would decode as false and nobody would notice.
		{"true", true, "true"},
		{"false", false, "false"},

		// The sync service writes "2026-08-25 08:06:07.123456+00": a space, and
		// an offset Postgres shortens to two digits. This is the same instant in
		// the form the API sends, which is the one a client is already parsing.
		{"timestamptz", at, "2026-08-25T08:06:07.123456Z"},
		{"a timestamptz pointer", &at, "2026-08-25T08:06:07.123456Z"},
		{"a nil timestamptz", (*time.Time)(nil), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := electric.Value(c.in); got != c.want {
				t.Errorf("Value(%#v) = %#v, want %#v", c.in, got, c.want)
			}
		})
	}
}

// A date and a time of day are both a time.Time by the time they reach the
// encoder, so the generated code says which it has.
func TestDateAndTimeOfDayKeepTheirOwnShape(t *testing.T) {
	t.Parallel()

	at := time.Date(2026, 8, 25, 10, 6, 7, 0, time.UTC)

	if got := electric.DateOnly(at); got != "2026-08-25" {
		t.Errorf("DateOnly = %#v", got)
	}
	if got := electric.TimeOnly(at); got != "10:06:07" {
		t.Errorf("TimeOnly = %#v", got)
	}

	// A time column keeps microseconds and Postgres prints them, so the sync
	// service sends "10:06:07.5" for this one. Read off a 1.6.9 container, along
	// with the fact that a whole second above has no fraction at all.
	half := at.Add(500 * time.Millisecond)
	if got := electric.TimeOnly(half); got != "10:06:07.5" {
		t.Errorf("TimeOnly with a fraction = %#v", got)
	}
	// And a date has no time to keep any of.
	if got := electric.DateOnly(half); got != "2026-08-25" {
		t.Errorf("DateOnly = %#v", got)
	}
	if got := electric.DateOnly(&at); got != "2026-08-25" {
		t.Errorf("DateOnly through a pointer = %#v", got)
	}
	if got := electric.DateOnly((*time.Time)(nil)); got != nil {
		t.Errorf("a null date = %#v", got)
	}
	if got := electric.TimeOnly((*time.Time)(nil)); got != nil {
		t.Errorf("a null time = %#v", got)
	}
}

// Nothing reaches a subscriber as a JSON number or a JSON bool, because the
// column's type in the schema header is what says how to read it and a value
// that arrived already typed would be read twice.
func TestEveryValueIsAStringOrNull(t *testing.T) {
	t.Parallel()

	for _, v := range []any{
		true, 1, int64(2), 3.5, "four", uuid.New(), time.Now(),
		[]int32{1}, json.RawMessage(`1`), pgtype.Numeric{Int: big.NewInt(1), Valid: true},
	} {
		switch got := electric.Value(v).(type) {
		case string:
		default:
			t.Errorf("Value(%#v) is a %T", v, got)
		}
	}
}

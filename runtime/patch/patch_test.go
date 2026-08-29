package patch_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/simonjanss/rig/runtime/patch"
)

// body is the shape a generated update input has: one field of each kind.
type body struct {
	Title patch.Optional[string] `json:"title"` // a NOT NULL column
	Notes patch.Nullable[string] `json:"notes"` // a nullable column
}

// The zero value is absent, which is what makes this work with encoding/json at
// all: UnmarshalJSON is only called for keys that are present, so a field
// nobody mentioned is never touched.
func TestAnOmittedFieldIsAbsent(t *testing.T) {
	t.Parallel()

	var b body
	if err := json.Unmarshal([]byte(`{}`), &b); err != nil {
		t.Fatal(err)
	}

	if !b.Title.IsAbsent() {
		t.Error("an omitted Optional should be absent")
	}
	if !b.Notes.IsAbsent() {
		t.Error("an omitted Nullable should be absent")
	}
	if b.Notes.Touched() {
		t.Error("an omitted field was not mentioned")
	}
}

func TestAValueIsSet(t *testing.T) {
	t.Parallel()

	var b body
	if err := json.Unmarshal([]byte(`{"title":"Write it","notes":"soon"}`), &b); err != nil {
		t.Fatal(err)
	}

	if v, ok := b.Title.Get(); !ok || v != "Write it" {
		t.Errorf("title = %q, %v", v, ok)
	}
	if v, ok := b.Notes.Get(); !ok || v != "soon" {
		t.Errorf("notes = %q, %v", v, ok)
	}
	if b.Notes.IsNull() {
		t.Error("a value is not null")
	}
}

// The distinction the whole package exists for.
func TestNullClearsANullableField(t *testing.T) {
	t.Parallel()

	var b body
	if err := json.Unmarshal([]byte(`{"notes":null}`), &b); err != nil {
		t.Fatal(err)
	}

	if !b.Notes.IsNull() {
		t.Fatal("an explicit null should be null, not absent")
	}
	if b.Notes.IsAbsent() {
		t.Error("an explicit null was mentioned")
	}
	if !b.Notes.Touched() {
		t.Error("a repository needs to know this column was mentioned")
	}
	if _, ok := b.Notes.Get(); ok {
		t.Error("there is no value to get")
	}
	if b.Notes.Ptr() != nil {
		t.Error("a null should reach the driver as a nil pointer")
	}
}

// A NOT NULL column cannot be cleared, so a client that tries is told rather
// than quietly ignored. This is the one null that has to be caught at runtime,
// because it arrives over the wire; the Go API cannot express it at all.
func TestNullIsRefusedForAnOptionalField(t *testing.T) {
	t.Parallel()

	var b body
	err := json.Unmarshal([]byte(`{"title":null}`), &b)
	if err == nil {
		t.Fatal("null for a NOT NULL column should be refused")
	}

	var refused patch.ErrNullNotAllowed
	if !errors.As(err, &refused) {
		t.Errorf("err = %#v, want ErrNullNotAllowed so the decoder can name the field", err)
	}
	// And nothing was recorded, so a caller cannot act on a half-decoded value.
	if !b.Title.IsAbsent() {
		t.Error("a refused field should not have been set")
	}
}

func TestConstructors(t *testing.T) {
	t.Parallel()

	if v, ok := patch.NewOptional("x").Get(); !ok || v != "x" {
		t.Errorf("NewOptional = %q, %v", v, ok)
	}
	if !patch.Absent[string]().IsAbsent() {
		t.Error("Absent should be absent")
	}

	if v, ok := patch.NewNullable("x").Get(); !ok || v != "x" {
		t.Errorf("NewNullable = %q, %v", v, ok)
	}
	if !patch.Null[string]().IsNull() {
		t.Error("Null should be null")
	}
	if !patch.Unspecified[string]().IsAbsent() {
		t.Error("Unspecified should be absent")
	}
}

// FromPtr is how the previous row's value — which the model holds as *T —
// becomes an input, which is what merging a partial update needs.
func TestFromPtrRoundTrips(t *testing.T) {
	t.Parallel()

	value := "kept"
	set := patch.FromPtr(&value)
	if v, ok := set.Get(); !ok || v != "kept" {
		t.Errorf("FromPtr(&v) = %q, %v", v, ok)
	}
	if got := set.Ptr(); got == nil || *got != "kept" {
		t.Errorf("Ptr() = %v", got)
	}

	cleared := patch.FromPtr[string](nil)
	if !cleared.IsNull() {
		t.Error("FromPtr(nil) should clear")
	}
	if cleared.Ptr() != nil {
		t.Error("Ptr() of a cleared value should be nil")
	}
}

func TestOr(t *testing.T) {
	t.Parallel()

	if got := patch.Absent[string]().Or("fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
	if got := patch.NewOptional("given").Or("fallback"); got != "given" {
		t.Errorf("got %q", got)
	}
	// Both absent and null fall back: neither carries a value.
	if got := patch.Null[string]().Or("fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
	if got := patch.NewNullable("given").Or("fallback"); got != "given" {
		t.Errorf("got %q", got)
	}
}

func TestMarshal(t *testing.T) {
	t.Parallel()

	b := body{Title: patch.NewOptional("Write it"), Notes: patch.Null[string]()}

	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"title":"Write it","notes":null}`; string(out) != want {
		t.Errorf("got %s\nwant %s", out, want)
	}
}

// A round trip has to preserve which of the three states a nullable field was
// in, or an echoed request means something different from the one sent.
func TestNullableRoundTrips(t *testing.T) {
	t.Parallel()

	for _, in := range []string{`{"notes":"y"}`, `{"notes":null}`, `{}`} {
		var first body
		if err := json.Unmarshal([]byte(in), &first); err != nil {
			t.Fatalf("%s: %v", in, err)
		}

		encoded, err := json.Marshal(struct {
			Notes patch.Nullable[string] `json:"notes"`
		}{first.Notes})
		if err != nil {
			t.Fatal(err)
		}

		var second struct {
			Notes patch.Nullable[string] `json:"notes"`
		}
		if err := json.Unmarshal(encoded, &second); err != nil {
			t.Fatal(err)
		}

		// Absent encodes as null, because a struct field cannot be omitted
		// from the outside — so absent becomes null on the way back. Anything
		// carrying a value has to survive exactly.
		if first.Notes.IsSet() && first.Notes.String() != second.Notes.String() {
			t.Errorf("%s: notes became %s", in, second.Notes.String())
		}
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ got, want string }{
		{patch.Absent[int]().String(), "absent"},
		{patch.NewOptional(7).String(), "7"},
		{patch.Unspecified[int]().String(), "absent"},
		{patch.Null[int]().String(), "null"},
		{patch.NewNullable(7).String(), "7"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// A malformed value is a decoding error, not a silent absence.
func TestABadValueIsAnError(t *testing.T) {
	t.Parallel()

	var b body
	if err := json.Unmarshal([]byte(`{"title":42}`), &b); err == nil {
		t.Error("a number for a string field should fail")
	}
	if err := json.Unmarshal([]byte(`{"notes":42}`), &b); err == nil {
		t.Error("a number for a string field should fail")
	}
}

// A nullable array column is held as a plain slice rather than a pointer to
// one, so the pointer form has nothing to take the address of. This is the
// version of FromPtr for that case, and it is what turns a previous row's value
// back into an input when a snapshot is replayed.
func TestFromSlice(t *testing.T) {
	t.Parallel()

	var absent []string
	if got := patch.FromSlice(absent); !got.IsNull() {
		t.Errorf("a nil slice should clear the column, got %+v", got)
	}

	// An empty slice is a value. A column set to {} is not a column set to
	// NULL, and confusing the two would quietly change the row.
	empty := patch.FromSlice([]string{})
	if empty.IsNull() {
		t.Error("an empty slice is a value, not a clear")
	}
	if v, ok := empty.Get(); !ok || len(v) != 0 {
		t.Errorf("Get = %v, %v", v, ok)
	}

	full := patch.FromSlice([]string{"a", "b"})
	v, ok := full.Get()
	if !ok || len(v) != 2 || v[0] != "a" {
		t.Errorf("Get = %v, %v", v, ok)
	}

	// Same three states as every other Nullable, so a repository writing one
	// needs no special case.
	if !full.Touched() || !empty.Touched() || !patch.FromSlice(absent).Touched() {
		t.Error("all three are things the caller asked for")
	}
}

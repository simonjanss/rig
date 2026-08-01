package patch_test

import (
	"encoding/json"
	"testing"

	"github.com/simonjanss/rig/runtime/patch"
)

// body is what an update request decodes into: a field the client omitted must
// be distinguishable from one they set to null.
type body struct {
	Title patch.Patch[string] `json:"title"`
	Notes patch.Patch[string] `json:"notes"`
	Count patch.Patch[int]    `json:"count"`
}

func TestThreeStates(t *testing.T) {
	t.Parallel()

	var b body
	if err := json.Unmarshal([]byte(`{"title":"hello","notes":null}`), &b); err != nil {
		t.Fatal(err)
	}

	// Set.
	if v, ok := b.Title.Get(); !ok || v != "hello" {
		t.Errorf("Title.Get() = %q, %v", v, ok)
	}
	if !b.Title.IsSet() || b.Title.IsNull() || b.Title.IsAbsent() {
		t.Error("Title should read as set")
	}

	// Null.
	if _, ok := b.Notes.Get(); ok {
		t.Error("Notes is null, so there is no value")
	}
	if !b.Notes.IsNull() || b.Notes.IsAbsent() {
		t.Error("Notes should read as null, not absent")
	}

	// Absent. This is the one a pointer cannot express, and the reason the
	// type exists.
	if !b.Count.IsAbsent() || b.Count.IsNull() || b.Count.IsSet() {
		t.Error("Count should read as absent")
	}
}

// Touched is what a repository asks when deciding which columns to write.
func TestTouched(t *testing.T) {
	t.Parallel()

	var b body
	if err := json.Unmarshal([]byte(`{"title":"x","notes":null}`), &b); err != nil {
		t.Fatal(err)
	}

	if !b.Title.Touched() {
		t.Error("a set field was touched")
	}
	if !b.Notes.Touched() {
		t.Error("clearing a field is still touching it")
	}
	if b.Count.Touched() {
		t.Error("an omitted field was not touched")
	}
}

func TestConstructors(t *testing.T) {
	t.Parallel()

	if v, ok := patch.Set("x").Get(); !ok || v != "x" {
		t.Errorf("Set = %q, %v", v, ok)
	}
	if !patch.Null[string]().IsNull() {
		t.Error("Null should be null")
	}
	if !patch.Absent[string]().IsAbsent() {
		t.Error("Absent should be absent")
	}

	// The zero value is absent, which is what makes the JSON behavior work
	// without any cooperation from the surrounding struct.
	var zero patch.Patch[string]
	if !zero.IsAbsent() {
		t.Error("the zero value should be absent")
	}
}

func TestOr(t *testing.T) {
	t.Parallel()

	if got := patch.Set(7).Or(3); got != 7 {
		t.Errorf("Or on a set value = %d", got)
	}
	if got := patch.Null[int]().Or(3); got != 3 {
		t.Errorf("Or on null = %d", got)
	}
	if got := patch.Absent[int]().Or(3); got != 3 {
		t.Errorf("Or on absent = %d", got)
	}
}

func TestMarshal(t *testing.T) {
	t.Parallel()

	out, err := json.Marshal(struct {
		A patch.Patch[string] `json:"a"`
		B patch.Patch[string] `json:"b"`
		C patch.Patch[string] `json:"c"`
	}{
		A: patch.Set("x"),
		B: patch.Null[string](),
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":"x","b":null,"c":null}` {
		t.Errorf("marshal = %s", out)
	}
}

func TestNestedTypes(t *testing.T) {
	t.Parallel()

	type inner struct {
		N int `json:"n"`
	}
	var b struct {
		V patch.Patch[inner]    `json:"v"`
		S patch.Patch[[]string] `json:"s"`
	}

	if err := json.Unmarshal([]byte(`{"v":{"n":3},"s":["a","b"]}`), &b); err != nil {
		t.Fatal(err)
	}
	if v, ok := b.V.Get(); !ok || v.N != 3 {
		t.Errorf("nested struct = %+v, %v", v, ok)
	}
	if s, ok := b.S.Get(); !ok || len(s) != 2 {
		t.Errorf("slice = %v, %v", s, ok)
	}
}

func TestUnmarshalRejectsTheWrongType(t *testing.T) {
	t.Parallel()

	var b body
	if err := json.Unmarshal([]byte(`{"count":"not a number"}`), &b); err == nil {
		t.Fatal("a type mismatch should be an error")
	}
}

func TestString(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		p    patch.Patch[int]
		want string
	}{
		{patch.Set(3), "3"},
		{patch.Null[int](), "null"},
		{patch.Absent[int](), "absent"},
	} {
		if got := tc.p.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
	}
}

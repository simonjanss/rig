package compile_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/ir"
	"github.com/simonjanss/rig/runtime/authwire"
)

// The one thing that makes declaring a shape nobody reads yet safe.
//
// `AuthLogEntry` is injected into every document that has an `auth:` block, so
// an OpenAPI document and a TypeScript client can describe the authentication
// trail the day they can describe any /auth/* route. But the endpoint answering
// it today is hand-written, in a module that cannot import a project's generated
// model package, so the shape exists twice: as this object and as
// [authwire.AuthLogEntryView].
//
// Two spellings of one shape drift. A field added to the wire struct and not to
// the object would leave the document quietly describing a response that has one
// more member than it says — and nothing about a shape nobody reads would ever
// notice. So the wire struct is the source and this test is the check: same
// members, same JSON names, same order.
func TestTheAuthLogEntryObjectMatchesTheWire(t *testing.T) {
	t.Parallel()

	obj := builtinObject(t, compile.ObjectAuthLogEntry)

	var wire []string
	view := reflect.TypeFor[authwire.AuthLogEntryView]()
	for i := range view.NumField() {
		tag := view.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if name == "" {
			t.Fatalf("%s has no json tag, so the wire name is the Go name and nothing here can check it",
				view.Field(i).Name)
		}
		wire = append(wire, name)
	}

	declared := make([]string, 0, len(obj.Fields))
	for _, f := range obj.Fields {
		declared = append(declared, f.Wire)
	}

	if len(declared) != len(wire) {
		t.Fatalf("the object declares %d members and the wire struct has %d:\n object: %v\n wire:   %v",
			len(declared), len(wire), declared, wire)
	}
	for i := range wire {
		if declared[i] != wire[i] {
			t.Errorf("member %d: the object says %q and the wire struct says %q",
				i, declared[i], wire[i])
		}
	}
}

// The names are literal, and that is deliberate: `api.json_case` shapes the keys
// rig generates, and these are not generated. The auth module is hand-written
// and shared, so it answers camelCase whatever a project sets — and an object
// rendered through the namer would describe a response nobody receives.
func TestTheAuthLogEntryObjectIgnoresJSONCase(t *testing.T) {
	t.Parallel()

	for _, jsonCase := range []naming.Case{naming.CaseSnake, naming.CasePascal} {
		obj := builtinObjectWith(t, compile.ObjectAuthLogEntry, jsonCase)
		for _, f := range obj.Fields {
			if f.Name == "ID" && f.Wire != "id" {
				t.Errorf("json_case %s rendered the wire name as %q; the auth module still answers %q",
					jsonCase, f.Wire, "id")
			}
			if f.Name == "EmailAddress" && f.Wire != "emailAddress" {
				t.Errorf("json_case %s rendered the wire name as %q; the auth module still answers %q",
					jsonCase, f.Wire, "emailAddress")
			}
		}
	}
}

// A project without authentication gets no object. A shape nothing references
// and nobody can obtain is a type every client carries for nothing — the same
// rule the file shape follows.
func TestTheAuthLogEntryObjectIsAbsentWithoutAuth(t *testing.T) {
	t.Parallel()

	out, diags := compile.Expand(ir.API{Version: "v1"}, compile.ExpandOptions{})
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	for _, o := range out.Objects {
		if o.Name == compile.ObjectAuthLogEntry {
			t.Fatal("a project with no auth: block should get no authentication shapes")
		}
	}
}

// builtinObject expands an API with authentication on and returns one injected
// object.
func builtinObject(t *testing.T, name string) ir.Object {
	t.Helper()
	return builtinObjectWith(t, name, naming.CaseCamel)
}

func builtinObjectWith(t *testing.T, name string, jsonCase naming.Case) ir.Object {
	t.Helper()

	out, diags := compile.Expand(
		ir.API{Version: "v1", Auth: &ir.Auth{BasePath: "/auth"}},
		compile.ExpandOptions{Namer: naming.New(naming.Config{JSONCase: jsonCase})},
	)
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	for _, o := range out.Objects {
		if o.Name == name {
			return o
		}
	}
	t.Fatalf("no %s object was injected", name)
	return ir.Object{}
}

package authwire_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The TypeScript mirror of this package.
//
// It is hand-written, like this file's subject: the authentication endpoints
// are rig's own, so their shapes are not generated for either end. That is the
// whole reason this test exists — a member added on one side and forgotten on
// the other is a field a client quietly stops sending, and nothing fails to
// compile in either language.
const mirror = "../../ts/packages/client/src/authwire.ts"

// Where the two spellings differ, and why.
var renamed = map[string]string{
	// `Page` is taken in the TypeScript package by the shape `paginate` walks,
	// which is about iterating a collection rather than about what one response
	// body looks like.
	"Page": "AuthPage",
}

// A type declared on one side and deliberately absent from the other.
var notMirrored = map[string]bool{}

func TestTheTypeScriptMirrorMatchesTheWire(t *testing.T) {
	t.Parallel()

	if _, err := os.Stat(mirror); os.IsNotExist(err) {
		t.Skip("the TypeScript package is not in this checkout")
	}

	wire := parseGo(t, "authwire.go")
	ts := parseTS(t, mirror)

	for name, want := range wire {
		if notMirrored[name] {
			continue
		}
		spelled := name
		if alias, ok := renamed[name]; ok {
			spelled = alias
		}

		got, ok := ts[spelled]
		if !ok {
			t.Errorf("authwire.go declares %s and %s does not; add it, or say "+
				"why not in notMirrored", name, filepath.Base(mirror))
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s disagrees about %s:\n  go: %v\n  ts: %v",
				filepath.Base(mirror), name, want, got)
		}
	}

	// The other direction, which catches the subtler drift: a shape the server
	// stopped answering with, still described to every browser caller.
	spelled := make(map[string]bool, len(wire))
	for name := range wire {
		if alias, ok := renamed[name]; ok {
			name = alias
		}
		spelled[name] = true
	}
	for name := range ts {
		if !spelled[name] {
			t.Errorf("%s declares %s and authwire.go does not",
				filepath.Base(mirror), name)
		}
	}
}

// parseGo reads every exported struct's wire names, in order.
//
// The source rather than reflection, because reflection can only be asked about
// a type somebody already named — and the drift worth catching most is a type
// added here that nobody mirrored, which no list of names would contain.
func parseGo(t *testing.T, path string) map[string][]string {
	t.Helper()

	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}

	structs := map[string]*ast.StructType{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				continue
			}
			if st, ok := ts.Type.(*ast.StructType); ok {
				structs[ts.Name.Name] = st
			}
		}
	}

	out := make(map[string][]string, len(structs))
	for name, st := range structs {
		out[name] = wireNames(t, name, st, structs)
	}
	return out
}

func wireNames(
	t *testing.T, owner string, st *ast.StructType,
	structs map[string]*ast.StructType,
) []string {
	t.Helper()

	var out []string
	for _, field := range st.Fields.List {
		tag := ""
		if field.Tag != nil {
			tag = reflect.StructTag(strings.Trim(field.Tag.Value, "`")).Get("json")
		}
		key, _, _ := strings.Cut(tag, ",")

		// An embedded struct with no tag of its own is flattened onto the wire,
		// which is how a sign-in answers with a token pair's members at the top
		// level rather than under a nested key.
		if len(field.Names) == 0 && key == "" {
			ident, ok := field.Type.(*ast.Ident)
			if !ok {
				t.Fatalf("%s embeds something this test cannot follow", owner)
			}
			embedded, ok := structs[ident.Name]
			if !ok {
				t.Fatalf("%s embeds %s, which is not declared here", owner, ident.Name)
			}
			out = append(out, wireNames(t, ident.Name, embedded, structs)...)
			continue
		}

		if key == "-" {
			continue
		}
		if key == "" {
			t.Fatalf("%s has a member with no json tag, which the wire cannot describe", owner)
		}
		out = append(out, key)
	}
	return out
}

var (
	// `export type X = {`, `export type X<T> = {`, `export type X = Y & {`.
	tsOpen   = regexp.MustCompile(`^export type (\w+)(?:<[^>]*>)? = (?:(\w+) & )?\{$`)
	tsMember = regexp.MustCompile(`^ {4}(\w+)\??:`)
)

// parseTS reads the mirror's members, in order, flattening an intersection the
// same way Go flattens an embedded struct.
func parseTS(t *testing.T, path string) map[string][]string {
	t.Helper()

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	members := map[string][]string{}
	extends := map[string]string{}
	var order []string

	current := ""
	for _, line := range strings.Split(string(body), "\n") {
		if open := tsOpen.FindStringSubmatch(line); open != nil {
			current = open[1]
			order = append(order, current)
			members[current] = nil
			if open[2] != "" {
				extends[current] = open[2]
			}
			continue
		}
		if current == "" {
			continue
		}
		if line == "};" {
			current = ""
			continue
		}
		if m := tsMember.FindStringSubmatch(line); m != nil {
			members[current] = append(members[current], m[1])
		}
	}

	out := make(map[string][]string, len(order))
	for _, name := range order {
		var fields []string
		if base, ok := extends[name]; ok {
			fields = append(fields, members[base]...)
		}
		out[name] = append(fields, members[name]...)
	}
	return out
}

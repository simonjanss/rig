package yamlconf_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goccy/go-yaml/parser"

	"github.com/simonjanss/rig/internal/yamlconf"
)

// sample is a small format exercising the shapes real configuration uses:
// scalars, a nested struct, a map of structs, and a list of structs.
type sample struct {
	Name    string            `yaml:"name" json:"name"`
	Count   *int              `yaml:"count,omitempty" json:"count,omitempty"`
	Mode    string            `yaml:"mode,omitempty" json:"mode,omitempty" jsonschema:"enum=fast,enum=slow"`
	Nested  *nested           `yaml:"nested,omitempty" json:"nested,omitempty"`
	Entries map[string]entry  `yaml:"entries,omitempty" json:"entries,omitempty"`
	Items   []item            `yaml:"items,omitempty" json:"items,omitempty"`
	Tags    map[string]string `yaml:"tags,omitempty" json:"tags,omitempty"`
}

type nested struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

type entry struct {
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
}

type item struct {
	Kind string `yaml:"kind" json:"kind"`
	Size int    `yaml:"size,omitempty" json:"size,omitempty" jsonschema:"minimum=1"`
}

func newFormat() *yamlconf.Format {
	return &yamlconf.Format{
		ID:          "https://rig.dev/schema/test.v1.json",
		Title:       "test format",
		Description: "A format used only by tests.",
		New:         func() any { return &sample{} },
	}
}

func TestDecodeValid(t *testing.T) {
	t.Parallel()

	src := "name: demo\ncount: 3\nmode: fast\nnested:\n  enabled: true\n" +
		"entries:\n  a:\n    note: hello\nitems:\n  - kind: box\n    size: 2\n"

	var got sample
	ix, ok, diags := newFormat().Decode("t.yaml", []byte(src), &got)
	if !ok || diags.HasErrors() {
		t.Fatalf("valid input rejected:\n%s", diags.String())
	}
	if ix.File() != "t.yaml" {
		t.Errorf("index file = %q", ix.File())
	}
	if got.Name != "demo" || got.Count == nil || *got.Count != 3 || got.Mode != "fast" {
		t.Errorf("scalars decoded wrong: %+v", got)
	}
	if got.Nested == nil || !got.Nested.Enabled {
		t.Errorf("nested decoded wrong: %+v", got.Nested)
	}
	if got.Entries["a"].Note != "hello" {
		t.Errorf("map decoded wrong: %+v", got.Entries)
	}
	if len(got.Items) != 1 || got.Items[0].Kind != "box" {
		t.Errorf("list decoded wrong: %+v", got.Items)
	}
}

func TestDecodeReportsExactPositions(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		yaml       string
		wantLine   int
		wantColumn int
		wantMsg    string
	}{
		{
			name:       "unknown key lands on the key",
			yaml:       "name: demo\nnested:\n  enabld: true\n",
			wantLine:   3,
			wantColumn: 3,
			wantMsg:    "additional properties 'enabld' not allowed",
		},
		{
			name:       "bad enum lands on the value",
			yaml:       "name: demo\nmode: medium\n",
			wantLine:   2,
			wantColumn: 1,
			wantMsg:    "value must be one of",
		},
		{
			name:       "missing key lands on the enclosing item",
			yaml:       "name: demo\nitems:\n  - size: 2\n",
			wantLine:   3,
			wantColumn: 5,
			wantMsg:    "missing property 'kind'",
		},
		{
			name:       "constraint lands on the key",
			yaml:       "name: demo\nitems:\n  - kind: box\n    size: 0\n",
			wantLine:   4,
			wantColumn: 5,
			wantMsg:    "got 0, want 1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var got sample
			_, ok, diags := newFormat().Decode("t.yaml", []byte(tc.yaml), &got)
			if ok {
				t.Fatal("invalid input should not decode")
			}
			all := diags.All()
			if len(all) != 1 {
				t.Fatalf("got %d diagnostics, want 1:\n%s", len(all), diags.String())
			}
			d := all[0]
			if d.Anchor.Line != tc.wantLine || d.Anchor.Column != tc.wantColumn {
				t.Errorf("anchor = %d:%d, want %d:%d (message %q)",
					d.Anchor.Line, d.Anchor.Column, tc.wantLine, tc.wantColumn, d.Message)
			}
			if !strings.Contains(d.Message, tc.wantMsg) {
				t.Errorf("message = %q, want it to contain %q", d.Message, tc.wantMsg)
			}
		})
	}
}

func TestDecodeReturnsIndexEvenWhenInvalid(t *testing.T) {
	t.Parallel()

	// A caller that finds its own problems still needs positions for them, so
	// the index survives a failed validation.
	var got sample
	ix, ok, _ := newFormat().Decode("t.yaml", []byte("name: demo\nbogus: 1\n"), &got)
	if ok {
		t.Fatal("invalid input should not decode")
	}
	if ix == nil {
		t.Fatal("the index should be returned even when validation fails")
	}
	if a := ix.At("name"); a.Line != 1 {
		t.Errorf("index unusable after a failed validation: %+v", a)
	}
}

func TestDecodeSyntaxError(t *testing.T) {
	t.Parallel()

	var got sample
	_, ok, diags := newFormat().Decode("t.yaml", []byte("name: demo\n  oops:\n"), &got)
	if ok {
		t.Fatal("unparseable YAML should not decode")
	}
	all := diags.All()
	if len(all) != 1 || all[0].Code.ID != "RIG3001" {
		t.Fatalf("want one RIG3001:\n%s", diags.String())
	}
	if strings.HasPrefix(all[0].Message, "[") {
		t.Errorf("the position belongs in the anchor, not the message: %q", all[0].Message)
	}
}

func TestDecodeEmpty(t *testing.T) {
	t.Parallel()

	var got sample
	_, ok, diags := newFormat().Decode("t.yaml", nil, &got)
	if ok {
		t.Fatal("an empty file should not decode")
	}
	if !strings.Contains(diags.String(), "empty") {
		t.Fatalf("want an 'empty' diagnostic:\n%s", diags.String())
	}
}

func TestSchemaClosesEveryObject(t *testing.T) {
	t.Parallel()

	raw, err := newFormat().Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}

	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("emitted schema is not valid JSON: %v", err)
	}
	if s["$id"] != "https://rig.dev/schema/test.v1.json" {
		t.Errorf("$id = %v", s["$id"])
	}
	if s["additionalProperties"] != false {
		t.Error("the root should reject unknown keys")
	}
	for name, def := range s["$defs"].(map[string]any) {
		if d, ok := def.(map[string]any); ok && d["type"] == "object" {
			if d["additionalProperties"] != false {
				t.Errorf("%s should reject unknown keys", name)
			}
		}
	}
}

func TestSchemaIsCachedAndStable(t *testing.T) {
	t.Parallel()

	f := newFormat()
	first, err := f.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	for range 3 {
		again, err := f.Schema()
		if err != nil {
			t.Fatalf("Schema: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("the schema written for editors must be stable across calls")
		}
	}
}

func index(t *testing.T, src string) *yamlconf.Index {
	t.Helper()
	f, err := parser.ParseBytes([]byte(src), parser.ParseComments)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return yamlconf.BuildIndex("t.yaml", f.Docs[0].Body)
}

func TestIndexAtFallsBackToTheNearestAncestor(t *testing.T) {
	t.Parallel()

	ix := index(t, "table: lesson\ncolumns:\n  title:\n    comment: hi\n")

	if a := ix.At("columns.title.comment"); a.Line != 4 || a.Column != 5 {
		t.Errorf("exact path = %d:%d, want 4:5", a.Line, a.Column)
	}
	if a := ix.At("columns.title"); a.Line != 3 || a.Column != 3 {
		t.Errorf("parent path = %d:%d, want 3:3", a.Line, a.Column)
	}
	if a := ix.At("table"); a.Line != 1 || a.Column != 1 {
		t.Errorf("top-level path = %d:%d, want 1:1", a.Line, a.Column)
	}

	// A path not in the file reports at the nearest key that is, which beats
	// defaulting to line 1.
	a := ix.At("columns.title.immutable")
	if a.Line != 3 {
		t.Errorf("missing key anchored at line %d, want the enclosing column at line 3", a.Line)
	}
	if a.Path != "columns.title.immutable" {
		t.Errorf("the reported path should stay the one asked about, got %q", a.Path)
	}

	if a := ix.At("nonexistent"); a.File == "" || a.Line != 1 {
		t.Errorf("unknown top-level path = %+v, want the file at line 1", a)
	}
}

func TestIndexHas(t *testing.T) {
	t.Parallel()

	ix := index(t, "name: demo\nnested:\n  enabled: false\n")
	if !ix.Has("nested.enabled") {
		t.Error("Has should be true for a key that is present, even set to its zero value")
	}
	if ix.Has("nested.missing") {
		t.Error("Has should be false for an absent key")
	}
}

func TestIndexSequencePaths(t *testing.T) {
	t.Parallel()

	ix := index(t, "items:\n  - kind: a\n  - kind: b\n")
	if a := ix.At("items.1.kind"); a.Line != 3 {
		t.Errorf("items.1.kind anchored at line %d, want 3", a.Line)
	}
}

func TestNilIndexIsUsable(t *testing.T) {
	t.Parallel()

	var ix *yamlconf.Index
	if a := ix.At("columns.title"); a.Path != "columns.title" {
		t.Errorf("a nil index should still produce a path-only anchor, got %+v", a)
	}
	if ix.File() != "" || ix.Has("x") {
		t.Error("a nil index has no file and knows no paths")
	}
}

func TestJoin(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"columns", "title", "comment"}, "columns.title.comment"},
		{[]string{"", "table"}, "table"},
		{[]string{"endpoints", "0", "name"}, "endpoints.0.name"},
		{nil, ""},
	} {
		if got := yamlconf.Join(tc.in...); got != tc.want {
			t.Errorf("Join(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

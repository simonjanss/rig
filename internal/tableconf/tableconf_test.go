package tableconf_test

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/tableconf"
)

func TestLoadValidFile(t *testing.T) {
	t.Parallel()

	loaded, diags := tableconf.Load(filepath.Join("testdata", "valid", "lesson.yaml"))
	if diags.HasErrors() {
		t.Fatalf("a valid file reported errors:\n%s", diags.String())
	}
	if loaded == nil {
		t.Fatal("no file returned")
	}

	f := loaded.File
	if f.Table != "lesson" || f.Resource != "Lesson" || f.PathSegment != "lessons" {
		t.Errorf("identity decoded wrong: table=%q resource=%q segment=%q", f.Table, f.Resource, f.PathSegment)
	}
	if len(f.Operations) != 6 {
		t.Errorf("operations = %v", f.Operations)
	}
	if f.RestoreWindowDays == nil || *f.RestoreWindowDays != 30 {
		t.Errorf("restore_window_days = %v", f.RestoreWindowDays)
	}
	if !f.AuditEnabled() {
		t.Error("audit should be enabled")
	}

	fixture, ok := f.Columns["fixture_id"]
	if !ok {
		t.Fatal("fixture_id column missing")
	}
	if fixture.Field != "FixtureID" || !fixture.Immutable {
		t.Errorf("fixture_id decoded wrong: %+v", fixture)
	}
	if !f.Columns["internal_revision"].Exclude {
		t.Error("internal_revision should be excluded")
	}
	if !f.Columns["notes"].SnapshotIgnore {
		t.Error("notes should be snapshot_ignore")
	}
	if f.Columns["manager_email"].Format != "EmailAddress" {
		t.Errorf("manager_email format = %q", f.Columns["manager_email"].Format)
	}

	status := f.Enums["lesson_status"]
	if status.Name != "LessonStatus" || len(status.Values) != 2 {
		t.Errorf("enum decoded wrong: %+v", status)
	}
	if status.Values["in_progress"].Name != "InProgress" {
		t.Errorf("enum value decoded wrong: %+v", status.Values["in_progress"])
	}

	if !f.Relations["lesson_player"].Embed {
		t.Error("lesson_player should be embedded")
	}

	if f.Electric == nil || !f.Electric.Enabled || f.Electric.Auth != "tenant" {
		t.Fatalf("electric decoded wrong: %+v", f.Electric)
	}
	if p := f.Electric.Params["matchday"]; p.Type != "Int" || !p.Optional {
		t.Errorf("electric param decoded wrong: %+v", p)
	}

	if len(f.Endpoints) != 1 {
		t.Fatalf("got %d endpoints, want 1", len(f.Endpoints))
	}
	e := f.Endpoints[0]
	if e.Name != "Publish" || e.Method != "POST" || e.Permission != "lesson.publish" {
		t.Errorf("endpoint decoded wrong: %+v", e)
	}
	if got := e.Req(); len(got.PathParams) != 1 || len(got.Body) != 1 {
		t.Errorf("endpoint request decoded wrong: %+v", got)
	}
	if len(e.Responses) != 2 || e.Responses[1].Status != 409 {
		t.Errorf("endpoint responses decoded wrong: %+v", e.Responses)
	}
}

func TestAuditDefaultsOn(t *testing.T) {
	t.Parallel()

	loaded, diags := tableconf.Parse("t.yaml", []byte("table: t\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}
	if !loaded.File.AuditEnabled() {
		t.Error("audit should default to on when unset")
	}

	off, _ := tableconf.Parse("t.yaml", []byte("table: t\naudit: false\n"))
	if off.File.AuditEnabled() {
		t.Error("audit: false should turn the log off")
	}
}

// The whole point of keeping the syntax tree is that a reader can jump straight
// to the mistake. These are exact positions, not just "reported something".
func TestDiagnosticsAnchorExactly(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		yaml        string
		wantLine    int
		wantColumn  int
		wantMessage string
		wantCode    string
	}{
		{
			name:        "misspelled key anchors on the key, not the block",
			yaml:        "table: lesson\ncolumns:\n  title:\n    commnet: hi\n",
			wantLine:    4,
			wantColumn:  5,
			wantMessage: "additional properties 'commnet' not allowed",
			wantCode:    "RIG3002",
		},
		{
			name:        "bad enum value anchors on the value",
			yaml:        "table: lesson\noperations: [Create, Fetch]\n",
			wantLine:    2,
			wantColumn:  22,
			wantMessage: "value must be one of",
			wantCode:    "RIG3002",
		},
		{
			name:        "missing key anchors on the enclosing item",
			yaml:        "table: lesson\nendpoints:\n  - name: Publish\n    path: /x\n",
			wantLine:    3,
			wantColumn:  9,
			wantMessage: "missing property 'method'",
			wantCode:    "RIG3002",
		},
		{
			name:        "wrong type anchors on the key",
			yaml:        "table: lesson\nrestore_window_days: thirty\n",
			wantLine:    2,
			wantColumn:  1,
			wantMessage: "got string, want integer",
			wantCode:    "RIG3002",
		},
		{
			name:        "out-of-range status",
			yaml:        "table: lesson\nendpoints:\n  - name: P\n    method: POST\n    responses:\n      - status: 99\n",
			wantLine:    6,
			wantColumn:  9,
			wantMessage: "got 99, want 100",
			wantCode:    "RIG3002",
		},
		{
			name:        "unknown top-level key",
			yaml:        "table: lesson\nsoft_delete: true\n",
			wantLine:    2,
			wantColumn:  1,
			wantMessage: "additional properties 'soft_delete' not allowed",
			wantCode:    "RIG3002",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			loaded, diags := tableconf.Parse("lesson.yaml", []byte(tc.yaml))
			if loaded != nil {
				t.Fatal("an invalid file should not decode")
			}
			all := diags.All()
			if len(all) != 1 {
				t.Fatalf("got %d diagnostics, want exactly 1:\n%s", len(all), diags.String())
			}

			d := all[0]
			if d.Code.ID != tc.wantCode {
				t.Errorf("code = %s, want %s", d.Code.ID, tc.wantCode)
			}
			if d.Anchor.Line != tc.wantLine || d.Anchor.Column != tc.wantColumn {
				t.Errorf("anchor = %d:%d, want %d:%d (message was %q)",
					d.Anchor.Line, d.Anchor.Column, tc.wantLine, tc.wantColumn, d.Message)
			}
			if !strings.Contains(d.Message, tc.wantMessage) {
				t.Errorf("message = %q, want it to contain %q", d.Message, tc.wantMessage)
			}
		})
	}
}

func TestSyntaxErrorReportsPosition(t *testing.T) {
	t.Parallel()

	loaded, diags := tableconf.Parse("lesson.yaml", []byte("table: lesson\n  bad indent:\n"))
	if loaded != nil {
		t.Fatal("unparseable YAML should not decode")
	}
	all := diags.All()
	if len(all) != 1 || all[0].Code.ID != "RIG3001" {
		t.Fatalf("want one RIG3001, got:\n%s", diags.String())
	}
	if all[0].Anchor.Line != 1 {
		t.Errorf("anchor line = %d, want 1", all[0].Anchor.Line)
	}
	if strings.Contains(all[0].Message, "[1:8]") {
		t.Errorf("the position should live in the anchor, not be repeated in the message: %q", all[0].Message)
	}
}

func TestEmptyFile(t *testing.T) {
	t.Parallel()

	loaded, diags := tableconf.Parse("lesson.yaml", nil)
	if loaded != nil {
		t.Fatal("an empty file should not decode")
	}
	if !strings.Contains(diags.String(), "empty") {
		t.Fatalf("want an 'empty' diagnostic, got:\n%s", diags.String())
	}
}

func TestMultipleProblemsAllReported(t *testing.T) {
	t.Parallel()

	// A developer who made three mistakes should learn about all three now.
	src := "table: lesson\n" +
		"columns:\n" +
		"  a:\n" +
		"    commnet: x\n" +
		"  b:\n" +
		"    formt: y\n" +
		"  c:\n" +
		"    format: NotAFormat\n"

	_, diags := tableconf.Parse("lesson.yaml", []byte(src))
	if n := diags.Len(); n != 3 {
		t.Fatalf("got %d diagnostics, want 3:\n%s", n, diags.String())
	}

	lines := make([]int, 0, 3)
	for _, d := range diags.All() {
		lines = append(lines, d.Anchor.Line)
	}
	want := []int{4, 6, 8}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("anchored at lines %v, want %v:\n%s", lines, want, diags.String())
		}
	}
}

func TestIndexAtFallsBackToTheNearestAncestor(t *testing.T) {
	t.Parallel()

	loaded, diags := tableconf.Parse("lesson.yaml", []byte(
		"table: lesson\ncolumns:\n  title:\n    comment: hi\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}
	ix := loaded.Index

	if a := ix.At("columns.title.comment"); a.Line != 4 || a.Column != 5 {
		t.Errorf("exact path = %d:%d, want 4:5", a.Line, a.Column)
	}
	if a := ix.At("columns.title"); a.Line != 3 || a.Column != 3 {
		t.Errorf("parent path = %d:%d, want 3:3", a.Line, a.Column)
	}
	if a := ix.At("table"); a.Line != 1 || a.Column != 1 {
		t.Errorf("top-level path = %d:%d, want 1:1", a.Line, a.Column)
	}

	// A path that is not in the file at all reports at the nearest key that is,
	// which is far more useful than defaulting to line 1.
	a := ix.At("columns.title.immutable")
	if a.Line != 3 {
		t.Errorf("missing key anchored at line %d, want the enclosing column at line 3", a.Line)
	}
	if a.Path != "columns.title.immutable" {
		t.Errorf("the reported path should stay the one asked about, got %q", a.Path)
	}

	// Nothing to fall back to at all still yields a usable anchor.
	if a := ix.At("nonexistent"); a.File == "" || a.Line != 1 {
		t.Errorf("unknown top-level path = %+v, want the file at line 1", a)
	}
}

func TestNilIndexIsUsable(t *testing.T) {
	t.Parallel()

	var ix *tableconf.Index
	if a := ix.At("columns.title"); a.Path != "columns.title" {
		t.Errorf("a nil index should still produce a path-only anchor, got %+v", a)
	}
	if ix.File() != "" {
		t.Error("a nil index has no file")
	}
}

func TestLoadDir(t *testing.T) {
	t.Parallel()

	set, diags := tableconf.LoadDir([]string{
		filepath.Join("testdata", "valid", "lesson.yaml"),
		filepath.Join("testdata", "valid", "fixture.yaml"),
	})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}
	if set.Len() != 2 {
		t.Fatalf("loaded %d tables, want 2", set.Len())
	}
	if set.Get("lesson") == nil || set.Get("fixture") == nil {
		t.Error("both tables should be reachable by name")
	}
	if set.Get("nope") != nil {
		t.Error("an unconfigured table should be nil")
	}
	if got := set.Tables(); len(got) != 2 || got[0] != "lesson" {
		t.Errorf("Tables() = %v, want load order", got)
	}
}

func TestLoadDirRejectsFilenameMismatch(t *testing.T) {
	t.Parallel()

	// The filename is how `rig sync` finds a table's file. A mismatch means one
	// of the two names is being silently ignored from then on.
	set, diags := tableconf.LoadDir([]string{filepath.Join("testdata", "mismatch", "player.yaml")})
	if !diags.HasErrors() {
		t.Fatal("a filename that disagrees with the table should be an error")
	}
	if set.Len() != 0 {
		t.Error("the mismatched file should not enter the set")
	}
	if !strings.Contains(diags.String(), "by filename") {
		t.Errorf("the message should explain why it matters:\n%s", diags.String())
	}
}

func TestLoadDirRejectsDuplicateTables(t *testing.T) {
	t.Parallel()

	p := filepath.Join("testdata", "valid", "lesson.yaml")
	_, diags := tableconf.LoadDir([]string{p, p})
	if !diags.HasErrors() {
		t.Fatal("configuring the same table twice should be an error")
	}
	if !strings.Contains(diags.String(), "already configured") {
		t.Errorf("unexpected message:\n%s", diags.String())
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Parallel()

	loaded, diags := tableconf.Load(filepath.Join("testdata", "does-not-exist.yaml"))
	if loaded != nil {
		t.Fatal("a missing file should not decode")
	}
	all := diags.All()
	if len(all) != 1 || all[0].Code.ID != "RIG3003" {
		t.Fatalf("want one RIG3003, got:\n%s", diags.String())
	}
}

func TestSchemaIsWellFormed(t *testing.T) {
	t.Parallel()

	raw, err := tableconf.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}

	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("the emitted schema is not valid JSON: %v", err)
	}

	if s["$id"] != tableconf.SchemaID {
		t.Errorf("$id = %v, want %s", s["$id"], tableconf.SchemaID)
	}
	// A typo in a key must be an error, not a silent no-op, so the schema has
	// to close every object.
	if s["additionalProperties"] != false {
		t.Error("the root object should reject unknown keys")
	}
	req, _ := s["required"].([]any)
	if len(req) != 1 || req[0] != "table" {
		t.Errorf("required = %v, want exactly [table]", req)
	}

	defs, _ := s["$defs"].(map[string]any)
	for _, name := range []string{"Column", "Enum", "Endpoint", "Electric", "Param"} {
		d, ok := defs[name].(map[string]any)
		if !ok {
			t.Errorf("$defs is missing %s", name)
			continue
		}
		if d["additionalProperties"] != false {
			t.Errorf("%s should reject unknown keys", name)
		}
	}

	// Descriptions carry commas; they must survive tag parsing intact.
	col := defs["Column"].(map[string]any)["properties"].(map[string]any)
	imm := col["immutable"].(map[string]any)
	if !strings.Contains(imm["description"].(string), "never on update") {
		t.Errorf("description was truncated at a comma: %q", imm["description"])
	}
}

func TestSchemaIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := tableconf.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	for range 3 {
		again, err := tableconf.Schema()
		if err != nil {
			t.Fatalf("Schema: %v", err)
		}
		if string(again) != string(first) {
			t.Fatal("the schema written for editors must be stable across runs")
		}
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
		if got := tableconf.Join(tc.in...); got != tc.want {
			t.Errorf("Join(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDiagnosticsCarryTheirHint(t *testing.T) {
	t.Parallel()

	_, diags := tableconf.Parse("lesson.yaml", []byte("table: lesson\nbogus: 1\n"))
	all := diags.All()
	if len(all) != 1 {
		t.Fatalf("want 1 diagnostic, got %d", len(all))
	}
	if all[0].Hint != diag.CodeConfigInvalid.Hint || all[0].Hint == "" {
		t.Errorf("hint = %q, want the code's hint", all[0].Hint)
	}
}

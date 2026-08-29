package tableconf_test

import (
	"encoding/json"
	"os"
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
			wantColumn:  5,
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

// A project's configuration is a directory of files, and each of these is a
// mistake somebody makes on the way to a working one.
func TestLoadDirRefusesTheWaysAProjectCanBeAmbiguous(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	good := filepath.Join(dir, "lesson.yaml")
	writeConfig(t, good, "table: lesson\ncomment: A lesson.\n")

	// Named for one table, configuring another. The filename is how `rig sync`
	// finds the file to update, so one of the two names would be ignored from
	// then on.
	misnamed := filepath.Join(dir, "player.yaml")
	writeConfig(t, misnamed, "table: team\ncomment: A team.\n")

	// A file that names no table at all.
	anonymous := filepath.Join(dir, "anonymous.yaml")
	writeConfig(t, anonymous, "comment: Something.\n")

	set, diags := tableconf.LoadDir([]string{good, misnamed, anonymous})

	if !diags.HasErrors() {
		t.Fatal("two of those three files are mistakes")
	}
	if set.Get("lesson") == nil {
		t.Error("the good file should still have loaded")
	}
	if set.Get("team") != nil {
		t.Error("a misnamed file should not be used under the table it claims")
	}

	rendered := diags.String()
	for _, want := range []string{"player.yaml", "anonymous.yaml"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the diagnostics should name %s:\n%s", want, rendered)
		}
	}
}

// Two files claiming one table is a project where half the configuration is
// silently ignored, and which half depends on directory order.
func TestTwoFilesCannotConfigureOneTable(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	first := filepath.Join(dir, "lesson.yaml")
	second := filepath.Join(dir, "copy.yaml")
	writeConfig(t, first, "table: lesson\ncomment: A lesson.\n")
	writeConfig(t, second, "table: lesson\ncomment: The same lesson.\n")

	_, diags := tableconf.LoadDir([]string{first, second})

	if !diags.HasErrors() {
		t.Fatal("a duplicate table should be refused")
	}
	if !strings.Contains(diags.String(), "already configured") {
		t.Errorf("the diagnostic should say why:\n%s", diags.String())
	}
}

// A file that cannot be read leaves the table's intent unknown, which is not
// the same as unconfigured. Rules that ask what the configuration says have to
// stay quiet, or one mistyped key buries itself under a diagnostic per column.
func TestAnUnreadableFileMarksTheTableFailedRatherThanAbsent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	broken := filepath.Join(dir, "lesson.yaml")
	writeConfig(t, broken, "table: lesson\ncolumns: [this is not a mapping\n")

	set, diags := tableconf.LoadDir([]string{broken})

	if !diags.HasErrors() {
		t.Fatal("a file that does not parse is an error")
	}
	if !set.Failed("lesson") {
		t.Error("the table should be marked failed, so downstream rules stay quiet")
	}
	if set.Get("lesson") != nil {
		t.Error("and it has no configuration to read")
	}
	if set.Failed("something-else") {
		t.Error("only the table whose file failed")
	}
}

// The zero set is what a project with no configuration at all has, and every
// accessor is reached before anything has been loaded.
func TestTheAccessorsTolerateNothingLoaded(t *testing.T) {
	t.Parallel()

	var none *tableconf.Set

	if none.Len() != 0 || none.Tables() != nil || none.Get("lesson") != nil || none.Failed("lesson") {
		t.Error("a nil set answers empty rather than panicking")
	}

	empty := tableconf.NewSet()
	if empty.Len() != 0 {
		t.Errorf("Len = %d", empty.Len())
	}
}

// Load order is what the reporting follows, so it has to be the order the
// files were given rather than a map's.
func TestTablesKeepsLoadOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	var paths []string
	for _, table := range []string{"zebra", "apple", "mango"} {
		path := filepath.Join(dir, table+".yaml")
		writeConfig(t, path, "table: "+table+"\ncomment: A thing.\n")
		paths = append(paths, path)
	}

	set, diags := tableconf.LoadDir(paths)
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}

	got := set.Tables()
	want := []string{"zebra", "apple", "mango"}
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("Tables = %v, want %v", got, want)
	}
	if set.Len() != 3 {
		t.Errorf("Len = %d", set.Len())
	}
}

// An anchor for a key that is not in the file still has to point somewhere: a
// diagnostic with no position is one nobody can act on.
func TestAnAnchorForAMissingKeyStillNamesTheFile(t *testing.T) {
	t.Parallel()

	loaded, diags := tableconf.Parse("services/lesson/lesson.yaml",
		[]byte("table: lesson\ncomment: A lesson.\n"))
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}

	at := loaded.At("columns", "nothing", "comment")
	if at.File == "" {
		t.Errorf("anchor = %+v, want it to name the file", at)
	}

	// And on a file that could not be loaded at all, the path is all there is.
	var missing *tableconf.Loaded
	if got := missing.At("table"); got.Path == "" {
		t.Errorf("anchor = %+v, want it to name the key", got)
	}
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

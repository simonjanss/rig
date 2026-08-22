package project_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
)

const minimal = "project:\n  name: fantasyfootball\n  module: github.com/simonjanss/fantasyfootball\n"

func TestParseAppliesDefaults(t *testing.T) {
	t.Parallel()

	p, diags := project.Parse("rig.yaml", []byte(minimal))
	if diags.HasErrors() {
		t.Fatalf("a minimal configuration should be valid:\n%s", diags.String())
	}
	c := p.Config

	if c.Version != 1 {
		t.Errorf("version = %d, want 1", c.Version)
	}
	if c.Layout.TableDir != project.DefaultTableDir || c.Layout.ConfigFile != project.DefaultConfigFile {
		t.Errorf("layout defaults wrong: %+v", c.Layout)
	}
	if c.API.Name != "fantasyfootball" {
		t.Errorf("api.name should default to the project name, got %q", c.API.Name)
	}
	if c.API.Version != "v1" || c.API.BasePath != "/api/v1" {
		t.Errorf("api defaults wrong: %+v", c.API)
	}
	if c.API.SearchMethod != project.SearchBoth {
		t.Errorf("search_method = %q, want both", c.API.SearchMethod)
	}
	if c.Database.ContainerName != "fantasyfootball-db" {
		t.Errorf("container name = %q", c.Database.ContainerName)
	}
	if c.Database.Port != project.DefaultPort || c.Database.Schema != "public" {
		t.Errorf("database defaults wrong: %+v", c.Database)
	}
	if c.Migrations.Dir != "migrations" {
		t.Errorf("migrations.dir = %q", c.Migrations.Dir)
	}
	if c.Migrations.Foundation != project.FoundationVendored {
		t.Errorf("migrations.foundation = %q, want vendored", c.Migrations.Foundation)
	}
	if c.Naming.JSONCase != "camel" {
		t.Errorf("json_case = %q", c.Naming.JSONCase)
	}
}

func TestBasePathIsNormalized(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"api/v2", "/api/v2"},
		{"/api/v2/", "/api/v2"},
		{"/api/v2", "/api/v2"},
	} {
		src := minimal + "api:\n  base_path: " + tc.in + "\n"
		p, diags := project.Parse("rig.yaml", []byte(src))
		if diags.HasErrors() {
			t.Fatalf("%s:\n%s", tc.in, diags.String())
		}
		if p.Config.API.BasePath != tc.want {
			t.Errorf("base_path %q normalized to %q, want %q", tc.in, p.Config.API.BasePath, tc.want)
		}
	}
}

func TestBasePathFollowsAPIVersion(t *testing.T) {
	t.Parallel()

	p, _ := project.Parse("rig.yaml", []byte(minimal+"api:\n  version: v3\n"))
	if p.Config.API.BasePath != "/api/v3" {
		t.Errorf("base_path = %q, want it to follow the api version", p.Config.API.BasePath)
	}
}

func TestRequiredFields(t *testing.T) {
	t.Parallel()

	// name and module are required by the schema, so the schema reports them
	// and there is no duplicate check to drift out of sync with it.
	_, diags := project.Parse("rig.yaml", []byte("project:\n  name: x\n"))
	if !strings.Contains(diags.String(), "missing property 'module'") {
		t.Errorf("a missing module should be reported:\n%s", diags.String())
	}

	_, diags = project.Parse("rig.yaml", []byte("project:\n  module: example.com/x\n"))
	if !strings.Contains(diags.String(), "missing property 'name'") {
		t.Errorf("a missing name should be reported:\n%s", diags.String())
	}
}

func TestLayoutMustNameATable(t *testing.T) {
	t.Parallel()

	// Without a placeholder every table would share one configuration file.
	src := minimal + "layout:\n  table_dir: services\n  config_file: services/all.yaml\n"
	_, diags := project.Parse("rig.yaml", []byte(src))
	if !strings.Contains(diags.String(), "must name a table") {
		t.Fatalf("a layout with no table placeholder should be rejected:\n%s", diags.String())
	}
}

func TestDuplicateGeneratorRejected(t *testing.T) {
	t.Parallel()

	src := minimal + "generators:\n  - name: persist-go\n  - name: persist-go\n"
	_, diags := project.Parse("rig.yaml", []byte(src))
	if !strings.Contains(diags.String(), "already configured") {
		t.Fatalf("a duplicate generator should be reported:\n%s", diags.String())
	}
}

func TestUnknownKeyAnchorsExactly(t *testing.T) {
	t.Parallel()

	src := minimal + "databse:\n  port: 5432\n"
	_, diags := project.Parse("rig.yaml", []byte(src))
	all := diags.All()
	if len(all) != 1 {
		t.Fatalf("got %d diagnostics, want 1:\n%s", len(all), diags.String())
	}
	if all[0].Anchor.Line != 4 || all[0].Anchor.Column != 1 {
		t.Errorf("anchor = %d:%d, want 4:1", all[0].Anchor.Line, all[0].Anchor.Column)
	}
	if !strings.Contains(all[0].Message, "'databse' not allowed") {
		t.Errorf("message = %q", all[0].Message)
	}
}

func TestLayoutTemplates(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		layout    string
		table     string
		wantDir   string
		wantConfi string
	}{
		{
			name:      "nested default keeps a table's files together",
			layout:    "",
			table:     "lesson_time",
			wantDir:   "services/lesson_time",
			wantConfi: "services/lesson_time/lesson_time.yaml",
		},
		{
			name:      "flat layout",
			layout:    "layout:\n  table_dir: services/{table}\n  config_file: config/{table}.yaml\n",
			table:     "lesson",
			wantDir:   "services/lesson",
			wantConfi: "config/lesson.yaml",
		},
		{
			name:      "pascal and plural placeholders",
			layout:    "layout:\n  table_dir: internal/{Table}\n  config_file: schema/{tables}.yaml\n",
			table:     "lesson_time",
			wantDir:   "internal/LessonTime",
			wantConfi: "schema/lesson_times.yaml",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, diags := project.Parse("/proj/rig.yaml", []byte(minimal+tc.layout))
			if diags.HasErrors() {
				t.Fatalf("unexpected errors:\n%s", diags.String())
			}
			if got := p.Rel(p.TableDir(tc.table)); got != tc.wantDir {
				t.Errorf("TableDir(%q) = %q, want %q", tc.table, got, tc.wantDir)
			}
			if got := p.Rel(p.TableConfigPath(tc.table)); got != tc.wantConfi {
				t.Errorf("TableConfigPath(%q) = %q, want %q", tc.table, got, tc.wantConfi)
			}
		})
	}
}

func TestPluralOverrideReachesTheLayout(t *testing.T) {
	t.Parallel()

	src := minimal +
		"layout:\n  table_dir: services/{tables}\n  config_file: services/{tables}/{table}.yaml\n" +
		"naming:\n  plurals:\n    person: people\n"

	p, diags := project.Parse("/proj/rig.yaml", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}
	if got := p.Rel(p.TableDir("person")); got != "services/people" {
		t.Errorf("TableDir(person) = %q, want services/people", got)
	}
}

// Who keeps rig's own migrations. Vendored is the default and the unset value,
// because a project that never heard of the key has the migrations in its
// directory and must keep having them.
func TestTheFoundationModeIsReadAndDefaulted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		yaml     string
		want     project.FoundationMode
		vendored bool
	}{
		{"unset", minimal, project.FoundationVendored, true},
		{
			"vendored spelled out",
			minimal + "\nmigrations:\n  foundation: vendored\n",
			project.FoundationVendored, true,
		},
		{
			"embedded",
			minimal + "\nmigrations:\n  foundation: embedded\n",
			project.FoundationEmbedded, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, diags := project.Parse("/proj/rig.yaml", []byte(tc.yaml))
			if diags.HasErrors() {
				t.Fatal(diags.String())
			}
			if got := p.Config.Migrations.Foundation; got != tc.want {
				t.Errorf("foundation = %q, want %q", got, tc.want)
			}
			if got := p.Config.Migrations.Vendored(); got != tc.vendored {
				t.Errorf("Vendored() = %v, want %v", got, tc.vendored)
			}
		})
	}

	// A mode rig does not know is refused by the schema rather than silently read
	// as vendored — the two modes apply different migrations, so guessing is the
	// one thing that must not happen.
	_, diags := project.Parse("/proj/rig.yaml", []byte(minimal+"\nmigrations:\n  foundation: whatever\n"))
	if !diags.HasErrors() {
		t.Error("an unknown foundation mode should be refused")
	}
}

func TestPathsResolveAgainstTheConfigFile(t *testing.T) {
	t.Parallel()

	// A command run from a subdirectory must mean the same thing as one run
	// from the root, so paths resolve against the file, not the process.
	p, _ := project.Parse("/proj/rig.yaml", []byte(minimal))
	if p.Root != "/proj" {
		t.Fatalf("root = %q, want /proj", p.Root)
	}
	if got := p.MigrationsDir(); got != filepath.Join("/proj", "migrations") {
		t.Errorf("MigrationsDir = %q", got)
	}
	if got := p.Path("a/b"); got != filepath.Join("/proj", "a", "b") {
		t.Errorf("Path(a/b) = %q", got)
	}
	if got := p.Path("/abs/path"); got != "/abs/path" {
		t.Errorf("an absolute path should pass through, got %q", got)
	}
	if got := p.Rel("/elsewhere/x"); got != "/elsewhere/x" {
		t.Errorf("a path outside the project should pass through, got %q", got)
	}
}

func TestDatabaseURL(t *testing.T) {
	t.Parallel()

	p, _ := project.Parse("rig.yaml", []byte(minimal))
	if !p.UsesContainer() {
		t.Error("with no url set, rig should manage the container")
	}
	// TimeZone is pinned so that ::date and date_trunc mean the same thing on
	// every machine. A timestamptz stores an instant and no zone, so nothing
	// about the data depends on it — only on how SQL reads a clock off it.
	want := "postgres://rig:rig@localhost:55432/rig?sslmode=disable&TimeZone=UTC"
	if got := p.DatabaseURL(); got != want {
		t.Errorf("DatabaseURL = %q, want %q", got, want)
	}

	// An explicit URL is used as-is, TimeZone and all: appending a parameter to
	// a connection string somebody wrote is a good way to break one that already
	// carries its own. That is how CI points at a service container rather than
	// starting one.
	explicit := minimal + "database:\n  url: postgres://ci@db:5432/app\n"
	p2, _ := project.Parse("rig.yaml", []byte(explicit))
	if p2.UsesContainer() {
		t.Error("an explicit url should skip the container")
	}
	if got := p2.DatabaseURL(); got != "postgres://ci@db:5432/app" {
		t.Errorf("DatabaseURL = %q", got)
	}
}

func TestDatabasePasswordIsEscaped(t *testing.T) {
	t.Parallel()

	src := minimal + "database:\n  password: \"p@ss word/1\"\n"
	p, _ := project.Parse("rig.yaml", []byte(src))
	if strings.Contains(p.DatabaseURL(), "p@ss word/1") {
		t.Errorf("credentials must be escaped into the URL, got %q", p.DatabaseURL())
	}
}

func TestFind(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	deep := filepath.Join(root, "services", "lesson")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(root, "rig.yaml")
	if err := os.WriteFile(cfg, []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := project.Find(deep)
	if err != nil {
		t.Fatalf("Find from a subdirectory: %v", err)
	}
	if resolved, _ := filepath.EvalSymlinks(got); resolved != mustResolve(t, cfg) {
		t.Errorf("Find = %q, want %q", got, cfg)
	}
}

func TestFindStopsAtRepositoryBoundary(t *testing.T) {
	t.Parallel()

	// A stray rig.yaml above a repository must not capture it.
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "rig.yaml"), []byte(minimal), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(outer, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := project.Find(repo); !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("Find should stop at the repository boundary, got %v", err)
	}
}

func TestFindNotFound(t *testing.T) {
	t.Parallel()

	if _, err := project.Find(t.TempDir()); !errors.Is(err, project.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestLoadReportsMissingConfig(t *testing.T) {
	t.Parallel()

	p, diags := project.Load(t.TempDir())
	if p != nil {
		t.Fatal("no project should be returned")
	}
	if !strings.Contains(diags.String(), "no rig.yaml") {
		t.Errorf("unhelpful message:\n%s", diags.String())
	}
}

func TestTableConfigPaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "rig.yaml"), minimal)
	writeFile(t, filepath.Join(root, "services", "lesson", "lesson.yaml"), "table: lesson\n")
	writeFile(t, filepath.Join(root, "services", "fixture", "fixture.yaml"), "table: fixture\n")
	// A YAML file that is not a table configuration must not be picked up.
	writeFile(t, filepath.Join(root, "services", "lesson", "notes.yaml"), "hello: world\n")
	writeFile(t, filepath.Join(root, "docker-compose.yaml"), "services: {}\n")

	p, diags := project.LoadFile(filepath.Join(root, "rig.yaml"))
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}

	paths, err := p.TableConfigPaths()
	if err != nil {
		t.Fatalf("TableConfigPaths: %v", err)
	}

	var rel []string
	for _, path := range paths {
		rel = append(rel, p.Rel(path))
	}
	want := "services/fixture/fixture.yaml,services/lesson/lesson.yaml"
	if strings.Join(rel, ",") != want {
		t.Errorf("TableConfigPaths = %v, want %s", rel, want)
	}
}

func TestSeverity(t *testing.T) {
	t.Parallel()

	src := minimal + "validate:\n  missing_comment: warn\n  boolean_prefix: off\n"
	p, diags := project.Parse("rig.yaml", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}
	v := p.Config.Validate

	if got := p.Severity(v.MissingComment, diagCodeError()); got != "warning" {
		t.Errorf("configured warn = %q", got)
	}
	if got := p.Severity(v.BooleanPrefix, diagCodeError()); got != "" {
		t.Errorf("off should report nothing, got %q", got)
	}
	if got := p.Severity(v.CascadeDelete, diagCodeError()); got != "error" {
		t.Errorf("unset should fall back to the code's default, got %q", got)
	}
}

func TestNamerUsesProjectSettings(t *testing.T) {
	t.Parallel()

	src := minimal + "naming:\n  json_case: snake\n  initialisms: [SCB]\n"
	p, _ := project.Parse("rig.yaml", []byte(src))
	n := p.Namer()

	if got := n.Go("scb_code"); got != "SCBCode" {
		t.Errorf("Go(scb_code) = %q, want SCBCode", got)
	}
	if got := n.JSON("EmailAddress"); got != "email_address" {
		t.Errorf("JSON case not applied, got %q", got)
	}
}

func TestSchemaIsWellFormed(t *testing.T) {
	t.Parallel()

	raw, err := project.Schema()
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	var s map[string]any
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if s["$id"] != project.SchemaID {
		t.Errorf("$id = %v", s["$id"])
	}
	if s["additionalProperties"] != false {
		t.Error("the root should reject unknown keys")
	}
	req, _ := s["required"].([]any)
	if len(req) != 1 || req[0] != "project" {
		t.Errorf("required = %v, want [project]", req)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustResolve(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// diagCodeError is a stand-in code whose default severity is error, used to
// check that an unset rule falls back to its code rather than to silence.
func diagCodeError() diag.Code { return diag.CodeCascadeDelete }

func TestTableForConfigPath(t *testing.T) {
	t.Parallel()

	p, _ := project.Parse("/proj/rig.yaml", []byte(minimal))

	for _, tc := range []struct {
		path  string
		table string
		ok    bool
	}{
		{"/proj/services/lesson/lesson.yaml", "lesson", true},
		{"/proj/services/lesson_time/lesson_time.yaml", "lesson_time", true},
		// The directory and the filename disagree, so the layout could never
		// have produced this path.
		{"/proj/services/lesson/notes.yaml", "", false},
		{"/proj/services/lesson/lesson.yml", "", false},
		{"/proj/elsewhere/lesson.yaml", "", false},
	} {
		got, ok := p.TableForConfigPath(tc.path)
		if got != tc.table || ok != tc.ok {
			t.Errorf("TableForConfigPath(%q) = %q, %v; want %q, %v", tc.path, got, ok, tc.table, tc.ok)
		}
	}
}

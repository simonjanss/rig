package scaffold_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/scaffold"
)

func TestWriteSkipsExistingFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	existing := filepath.Join(root, "rig.yaml")
	if err := os.WriteFile(existing, []byte("hand written\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	written, skipped, err := scaffold.Write([]scaffold.File{
		{Path: existing, Content: "generated"},
		{Path: filepath.Join(root, "sub", "new.txt"), Content: "generated"},
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(skipped) != 1 || skipped[0] != existing {
		t.Errorf("skipped = %v, want the existing file", skipped)
	}
	if len(written) != 1 {
		t.Errorf("written = %v, want one new file", written)
	}

	// A scaffold that overwrites your work on the second run is worse than no
	// scaffold at all.
	got, _ := os.ReadFile(existing)
	if string(got) != "hand written\n" {
		t.Errorf("an existing file was overwritten: %q", got)
	}
}

func TestProjectFiles(t *testing.T) {
	t.Parallel()

	files := scaffold.Project(scaffold.ProjectOptions{
		Name:   "fantasyfootball",
		Module: "github.com/you/ff",
	})

	byPath := map[string]string{}
	for _, f := range files {
		byPath[f.Path] = f.Content
	}

	for _, want := range []string{"rig.yaml", ".gitignore", "AGENTS.md", "migrations/.keep"} {
		if _, ok := byPath[want]; !ok {
			t.Errorf("%s was not scaffolded", want)
		}
	}

	cfg := byPath["rig.yaml"]
	for _, want := range []string{
		// Quoted, so a project whose name is all digits — a directory named
		// "2026" is where the name usually comes from — is still a string.
		`name: "fantasyfootball"`,
		`module: "github.com/you/ff"`,
		"$schema=.rig/rig.schema.json",
		"table_dir: services/{table}",
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("rig.yaml is missing %q:\n%s", want, cfg)
		}
	}

	// Generated files are rewritten on every run, so committing them invites a
	// merge conflict on code nobody wrote.
	if !strings.Contains(byPath[".gitignore"], "*.gen.go") {
		t.Error(".gitignore should exclude generated code")
	}

	// An agent joining the repository has to know which files it may edit
	// before it can do anything useful.
	agents := byPath["AGENTS.md"]
	for _, want := range []string{"rig sync", "never edit", "tenant_id", "service layer"} {
		if !strings.Contains(agents, want) {
			t.Errorf("AGENTS.md is missing %q", want)
		}
	}
}

func TestNextMigrationNumber(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if n, err := scaffold.NextMigrationNumber(dir); err != nil || n != 1 {
		t.Fatalf("empty directory: got %d, %v; want 1", n, err)
	}

	for _, name := range []string{
		"00001_init.sql",
		"00007_add_team.sql",
		"00003_add_player.sql",
		"notes.md",           // not a migration
		"0008_bad_width.sql", // not rig's naming, but version 8 to goose
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The next number follows the highest present, not the count: a gap left by
	// a deleted migration must not cause a collision.
	//
	// And "highest present" is goose's reading rather than rig's. 0008 is four
	// digits, so RIG6050 refuses the name — but goose applies the file as
	// version 8 regardless, and handing 8 back here would answer a badly named
	// migration with a duplicate one. A file rig would not have written is still
	// a number that is taken.
	n, err := scaffold.NextMigrationNumber(dir)
	if err != nil {
		t.Fatal(err)
	}
	if n != 9 {
		t.Errorf("next = %d, want 9", n)
	}
}

func TestMigrationFilename(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ in, want string }{
		{"create_team", "00003_create_team.sql"},
		{"Create Team", "00003_create_team.sql"},
		{"add-team-tier", "00003_add_team_tier.sql"},
		{"  Add   Team  ", "00003_add_team.sql"},
		{"add/team!", "00003_add_team.sql"},
	} {
		got := filepath.Base(scaffold.MigrationFilename("m", 3, tc.in))
		if got != tc.want {
			t.Errorf("MigrationFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMigrationWithoutTable(t *testing.T) {
	t.Parallel()

	sql := scaffold.Migration(scaffold.MigrationOptions{Name: "backfill"})
	for _, want := range []string{"-- +goose Up", "-- +goose Down", "-- +goose StatementBegin", "-- +goose StatementEnd"} {
		if !strings.Contains(sql, want) {
			t.Errorf("missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "CREATE TABLE") {
		t.Error("no table was asked for")
	}
}

// TestScaffoldedSQLIsWellFormed checks the shape that decides whether the file
// applies at all. A scaffold that fails on the first `rig db up` with a syntax
// error nobody expected to be theirs is worse than none.
func TestScaffoldedSQLIsWellFormed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		opt  scaffold.MigrationOptions
	}{
		{"plain", scaffold.MigrationOptions{Table: "team"}},
		{"soft delete", scaffold.MigrationOptions{Table: "team", SoftDelete: true}},
		{"snapshot", scaffold.MigrationOptions{Table: "team", Snapshot: true}},
		{"both", scaffold.MigrationOptions{Table: "team", SoftDelete: true, Snapshot: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql := scaffold.Migration(tc.opt)

			body, ok := between(sql, "CREATE TABLE team (", "\n);")
			if !ok {
				t.Fatalf("no CREATE TABLE body found:\n%s", sql)
			}

			// The last element must not carry a comma, or Postgres rejects the
			// whole statement.
			lines := nonEmptyLines(body)
			last := lines[len(lines)-1]
			if strings.HasSuffix(strings.TrimSpace(last), ",") {
				t.Errorf("trailing comma before ');':\n%s", body)
			}

			// Balanced parentheses catch a malformed CHECK constraint.
			if opened, closed := strings.Count(body, "("), strings.Count(body, ")"); opened != closed {
				t.Errorf("unbalanced parentheses (%d open, %d close):\n%s", opened, closed, body)
			}

			for _, want := range []string{"id", "tenant_id", "created_at"} {
				if !strings.Contains(body, want) {
					t.Errorf("missing the %s column:\n%s", want, body)
				}
			}
			if tc.opt.SoftDelete != strings.Contains(body, "deleted_at") {
				t.Errorf("soft delete = %v but deleted_at present = %v", tc.opt.SoftDelete, !tc.opt.SoftDelete)
			}
			if tc.opt.Snapshot {
				for _, want := range []string{"version_type", "snapshot_from_team_id", "snapshot_from_team_at", "CHECK"} {
					if !strings.Contains(body, want) {
						t.Errorf("snapshot scaffold is missing %q:\n%s", want, body)
					}
				}
				if !strings.Contains(sql, "CREATE TYPE team_version_type") {
					t.Error("the version enum was not created")
				}
				if !strings.Contains(sql, "DROP TYPE team_version_type") {
					t.Error("the down migration should drop the enum it created")
				}
				// A partial index cannot serve the foreign-key check Postgres
				// runs on delete, so scaffolding one would trip rig's own
				// index rule.
				if strings.Contains(sql, "team_snapshot_idx ON team (snapshot_from_team_id)\n    WHERE") {
					t.Error("the snapshot index must not be partial")
				}
			}

			// Every generated query filters by tenant.
			if !strings.Contains(sql, "CREATE INDEX team_tenant_created_idx") {
				t.Error("no index leading with tenant_id was scaffolded")
			}
			// The placeholder marks where your own columns belong.
			if !strings.Contains(body, "-- Add your columns here.") {
				t.Error("no placeholder for the developer's own columns")
			}
		})
	}
}

func between(s, start, end string) (string, bool) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		out = append(out, l)
	}
	return out
}

// Every commented suggestion in the scaffolded rig.yaml has to decode when it is
// uncommented, which is the whole point of putting it there.
//
// The `servers:` block used to be scaffolded as `# servers: ["https://…"]`, a
// list of strings where the loader wanted a list of objects — so following rig's
// own suggestion produced an error from rig. Nothing noticed, because nothing
// had ever parsed what the scaffold suggests.
func TestTheCommentedSuggestionsInRigYAMLDecode(t *testing.T) {
	t.Parallel()

	var cfg string
	for _, f := range scaffold.Project(scaffold.ProjectOptions{
		Name:   "fantasyfootball",
		Module: "github.com/you/ff",
	}) {
		if f.Path == "rig.yaml" {
			cfg = f.Content
		}
	}

	uncommented := uncomment(cfg)
	if !strings.Contains(uncommented, "servers:") {
		t.Fatalf("no servers block was suggested:\n%s", cfg)
	}

	if _, diags := project.Parse("rig.yaml", []byte(uncommented)); diags.HasErrors() {
		t.Errorf("uncommenting what rig suggested does not decode:\n%s\n\n%s",
			diags.String(), uncommented)
	}
}

// uncomment strips the leading "# " from every commented configuration line,
// leaving prose comments — which are followed by a blank line rather than by
// more configuration — alone by way of the indentation rig writes.
func uncomment(cfg string) string {
	var b strings.Builder
	for _, line := range strings.Split(cfg, "\n") {
		switch {
		case strings.HasPrefix(line, "# servers:"), strings.HasPrefix(line, "#   "):
			b.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "#"), " "))
		default:
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

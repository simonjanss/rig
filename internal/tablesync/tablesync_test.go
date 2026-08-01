package tablesync_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/internal/tablesync"
	"github.com/simonjanss/rig/pkg/ir"
)

// teamSchema is a table with the conventional columns, an enum, and a join
// table, normalized the way the sync command normalizes before planning.
func teamSchema(t *testing.T) ir.Schema {
	t.Helper()

	raw := ir.Schema{
		Name: "public",
		Enums: []ir.PgEnum{{
			Name: "team_tier",
			Values: []ir.PgEnumValue{
				{Value: "amateur"}, {Value: "professional"},
			},
		}},
		Tables: []ir.Table{
			{
				Name: "team", Kind: ir.TableKindBase,
				Columns: []ir.Column{
					{Name: "id", SQLType: "uuid", Ordinal: 1},
					{Name: "tenant_id", SQLType: "uuid", Ordinal: 2},
					{Name: "created_at", SQLType: "timestamptz", Ordinal: 3},
					{Name: "deleted_at", SQLType: "timestamptz", Nullable: true, Ordinal: 4},
					{Name: "name", SQLType: "text", Ordinal: 5},
					{Name: "tier", SQLType: "team_tier", UDTName: "team_tier", Ordinal: 6},
				},
				PrimaryKey: []string{"id"},
			},
			{
				Name: "player", Kind: ir.TableKindBase,
				Columns:    []ir.Column{{Name: "id", SQLType: "uuid", Ordinal: 1}},
				PrimaryKey: []string{"id"},
			},
			{
				Name: "team_player", Kind: ir.TableKindBase,
				Columns: []ir.Column{
					{Name: "team_id", SQLType: "uuid", Ordinal: 1, ForeignKey: &ir.FKRef{Table: "team", Column: "id"}},
					{Name: "player_id", SQLType: "uuid", Ordinal: 2, ForeignKey: &ir.FKRef{Table: "player", Column: "id"}},
				},
				PrimaryKey: []string{"team_id", "player_id"},
			},
		},
	}

	schema, diags := compile.Normalize(raw, compile.NormalizeOptions{})
	if diags.HasErrors() {
		t.Fatalf("fixture does not normalize:\n%s", diags.String())
	}
	return schema
}

func pathFor(root string) func(string) string {
	return func(table string) string {
		return filepath.Join(root, "services", table, table+".yaml")
	}
}

func TestPlanCreatesMissingFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	changes, err := tablesync.Plan(teamSchema(t), tableconf.NewSet(), pathFor(root), tablesync.Options{})
	if err != nil {
		t.Fatal(err)
	}

	kinds := map[string]tablesync.ChangeKind{}
	for _, c := range changes {
		kinds[c.Table] = c.Kind
	}

	if kinds["team"] != tablesync.ChangeCreate || kinds["player"] != tablesync.ChangeCreate {
		t.Errorf("expected both tables to be created, got %v", kinds)
	}
	// A join table is a relation on the resources it links, so it has nothing
	// of its own to configure.
	if _, planned := kinds["team_player"]; planned {
		t.Error("a join table should not get a configuration file")
	}
}

func TestCreatedFileShape(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	changes, err := tablesync.Plan(teamSchema(t), tableconf.NewSet(), pathFor(root), tablesync.Options{})
	if err != nil {
		t.Fatal(err)
	}

	content := changeFor(t, changes, "team").Content

	for _, want := range []string{
		"table: team",
		"restore_window_days: 30", // deleted_at is present
		"columns:",
		"  name:",
		"  tier:",
		"enums:",
		"  team_tier:",
		"    name: TeamTier",
		"      amateur:",
		"$schema=",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("generated file is missing %q:\n%s", want, content)
		}
	}

	// Columns rig manages take no configuration, so listing them would be
	// noise in every file.
	for _, unwanted := range []string{"  id:", "  tenant_id:", "  created_at:", "  deleted_at:"} {
		if strings.Contains(content, unwanted) {
			t.Errorf("managed column %q should not be written:\n%s", unwanted, content)
		}
	}

	// A placeholder must not read as documentation, or the missing-comment
	// rule would pass on a file nobody has filled in.
	if !strings.Contains(content, "TODO") {
		t.Error("new entries should carry a TODO placeholder")
	}
}

func TestGeneratedFileIsValid(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	changes, err := tablesync.Plan(teamSchema(t), tableconf.NewSet(), pathFor(root), tablesync.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tablesync.Apply(changes); err != nil {
		t.Fatal(err)
	}

	// What sync writes must be something rig can read back. A generator that
	// emits configuration its own loader rejects is worse than useless.
	for _, table := range []string{"team", "player"} {
		path := pathFor(root)(table)
		loaded, diags := tableconf.Load(path)
		if diags.HasErrors() {
			t.Errorf("%s does not load:\n%s", table, diags.String())
			continue
		}
		if loaded.File.Table != table {
			t.Errorf("%s declares table %q", path, loaded.File.Table)
		}
	}
}

// TestUpdatePreservesHandWrittenContent is the property the whole package
// exists for. A tool that reformats your file is a tool you stop running.
func TestUpdatePreservesHandWrittenContent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := pathFor(root)("team")
	original := `# yaml-language-server: $schema=../../.rig/table.schema.json
table: team
comment: A group of players that competes as a unit.

# Deleted teams stay recoverable for a month.
restore_window_days: 30

columns:
  name:
    comment: Display name shown in league tables.

endpoints:
  - name: Archive
    method: POST
    path: /{id}/_archive
    summary: Retire a team at the end of a season.
    responses:
      - status: 200
        body_object: Team
`
	writeFile(t, path, original)

	set, diags := tableconf.LoadDir([]string{path})
	if diags.HasErrors() {
		t.Fatalf("fixture does not load:\n%s", diags.String())
	}

	changes, err := tablesync.Plan(teamSchema(t), set, pathFor(root), tablesync.Options{})
	if err != nil {
		t.Fatal(err)
	}

	updated := changeFor(t, changes, "team")
	if updated.Kind != tablesync.ChangeUpdate {
		t.Fatalf("kind = %s, want update", updated.Kind)
	}
	got := updated.Content

	// Everything the developer wrote survives, byte for byte.
	for _, want := range []string{
		"# yaml-language-server: $schema=../../.rig/table.schema.json",
		"comment: A group of players that competes as a unit.",
		"# Deleted teams stay recoverable for a month.",
		"    comment: Display name shown in league tables.",
		"  - name: Archive",
		"    summary: Retire a team at the end of a season.",
		"        body_object: Team",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("sync lost %q:\n%s", want, got)
		}
	}

	// And the new column arrives in the right place.
	if !strings.Contains(got, "  tier:\n    comment: 'TODO: describe this.'") {
		t.Errorf("the new column was not added correctly:\n%s", got)
	}
	if !strings.Contains(got, "  team_tier:") {
		t.Errorf("the enum was not added:\n%s", got)
	}

	// The blank line before the endpoints block is still there: losing
	// whitespace is how a tool turns one line of change into a whole-file diff.
	if !strings.Contains(got, "\n\nendpoints:") {
		t.Errorf("blank lines were not preserved:\n%s", got)
	}
}

func TestUpdateIsANoOpWhenNothingChanged(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Create the files, then plan again against the same schema.
	changes, err := tablesync.Plan(teamSchema(t), tableconf.NewSet(), pathFor(root), tablesync.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tablesync.Apply(changes); err != nil {
		t.Fatal(err)
	}

	var paths []string
	for _, c := range changes {
		paths = append(paths, c.Path)
	}
	set, _ := tableconf.LoadDir(paths)

	again, err := tablesync.Plan(teamSchema(t), set, pathFor(root), tablesync.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("a second sync should have nothing to do, got %d changes: %+v", len(again), again)
	}
}

func TestPrune(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := pathFor(root)("team")
	writeFile(t, path, `table: team
comment: A group of players.
restore_window_days: 30
columns:
  name:
    comment: Display name.
  tier:
    comment: Competitive level.
  nickname:
    comment: This column was dropped by a migration.
`)

	set, _ := tableconf.LoadDir([]string{path})

	// Without --prune the stale entry is left alone: removing it is the kind of
	// edit someone should see coming.
	changes, err := tablesync.Plan(teamSchema(t), set, pathFor(root), tablesync.Options{})
	if err != nil {
		t.Fatal(err)
	}
	kept := changeFor(t, changes, "team")
	if !strings.Contains(kept.Content, "nickname") {
		t.Errorf("the stale entry should survive without --prune:\n%s", kept.Content)
	}
	if strings.Contains(strings.Join(kept.Notes, " "), "pruned") {
		t.Errorf("nothing should be pruned without --prune: %v", kept.Notes)
	}

	changes, err = tablesync.Plan(teamSchema(t), set, pathFor(root), tablesync.Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}

	pruned := changeFor(t, changes, "team")
	if !strings.Contains(strings.Join(pruned.Notes, " "), "pruned nickname") {
		t.Errorf("notes = %v, want the pruned column named", pruned.Notes)
	}
	if strings.Contains(pruned.Content, "nickname") {
		t.Errorf("the stale entry survived:\n%s", pruned.Content)
	}
	// Pruning one entry must not disturb its neighbours.
	for _, keep := range []string{"  name:", "  tier:", "comment: Display name."} {
		if !strings.Contains(pruned.Content, keep) {
			t.Errorf("pruning removed %q as well:\n%s", keep, pruned.Content)
		}
	}
}

func TestOrphanIsReportedNotDeleted(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := pathFor(root)("stadium")
	writeFile(t, path, "table: stadium\ncomment: This table was dropped.\n")

	set, _ := tableconf.LoadDir([]string{path})
	changes, err := tablesync.Plan(teamSchema(t), set, pathFor(root), tablesync.Options{Prune: true})
	if err != nil {
		t.Fatal(err)
	}

	orphan := changeFor(t, changes, "stadium")
	if orphan.Kind != tablesync.ChangeOrphan {
		t.Fatalf("kind = %s, want orphan", orphan.Kind)
	}

	if err := tablesync.Apply(changes); err != nil {
		t.Fatal(err)
	}
	// Even with --prune the file stays: it may hold endpoint definitions that
	// took real work, and a flag is not enough authority to discard them.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("an orphaned file should be reported, not deleted: %v", err)
	}
}

func TestOnlyLimitsToOneTable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	changes, err := tablesync.Plan(teamSchema(t), tableconf.NewSet(), pathFor(root),
		tablesync.Options{Only: "team"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Table != "team" {
		t.Fatalf("--table should plan one table, got %+v", changes)
	}
}

func changeFor(t *testing.T, changes []tablesync.Change, table string) tablesync.Change {
	t.Helper()
	for _, c := range changes {
		if c.Table == table {
			return c
		}
	}
	t.Fatalf("no change planned for %q", table)
	return tablesync.Change{}
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

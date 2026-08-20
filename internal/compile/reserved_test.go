// What rig keeps for itself, and what it lets through.
//
// The reservedtables fixture covers the three refusals. These cover the
// exemptions, which are the half that cannot be a fixture: whether a rig_ table
// is rig's own depends on the project's migrations, and a fixture directory has
// none.
package compile_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// compileReserved runs the pipeline over a schema and returns the codes it
// reported, so a case can name what it expects instead of matching messages.
func compileReserved(t *testing.T, schema ir.Schema, set *tableconf.Set, projectSrc string, foundation []string) []string {
	t.Helper()

	p, pdiags := project.Parse("rig.yaml", []byte(projectSrc))
	if pdiags.HasErrors() {
		t.Fatal(pdiags.String())
	}

	_, diags := compile.Compile(schema, set, compile.Options{
		Project:    p,
		Tool:       "rig (test)",
		Foundation: foundation,
	})

	var out []string
	for _, d := range diags.All() {
		if strings.HasPrefix(d.Code.ID, "RIG20") {
			out = append(out, d.Code.ID)
		}
	}
	return out
}

// renameTable is how these cases are built: the fixture's shape with one name
// changed, so a case differs from the fixture in exactly the thing under test.
func renameTable(t *testing.T, schema ir.Schema, from, to string) ir.Schema {
	t.Helper()

	found := false
	for i := range schema.Tables {
		if schema.Tables[i].Name == from {
			schema.Tables[i].Name = to
			found = true
		}
	}
	if !found {
		t.Fatalf("the fixture no longer has a table named %q", from)
	}
	return schema
}

func reservedSchema(t *testing.T) ir.Schema {
	t.Helper()
	return readSchema(t, filepath.Join("testdata", "reservedtables", "schema.json"))
}

// The prefix is rig's, and what makes a rig_ table rig's is a migration that
// created it — not the name. Both halves matter: without the first a project
// could take the prefix, and without the second every scaffolded project would
// be refused its own foundation.
func TestTheRigPrefixIsRigsUnlessRigCreatedTheTable(t *testing.T) {
	t.Parallel()

	schema := readSchema(t, filepath.Join("testdata", "files", "schema.json"))

	if got := compileReserved(t, schema, tableconf.NewSet(), defaultProject, []string{"rig_file"}); len(got) != 0 {
		t.Errorf("a scaffolded rig_file was refused: %v", got)
	}

	got := compileReserved(t, schema, tableconf.NewSet(), defaultProject, nil)
	if !slices.Contains(got, diag.CodeReservedTablePrefix.ID) {
		t.Errorf("codes = %v, want a %s for a rig_file nobody scaffolded",
			got, diag.CodeReservedTablePrefix.ID)
	}
}

// The resource name a foundation table takes is its own to take. Anything else
// under that name is refused, which is the point of reserving it.
func TestAFoundationTableMayHaveTheNameItReserves(t *testing.T) {
	t.Parallel()

	// account becomes rig_account, and says it projects to Account — exactly
	// what `rig setup-project --expose rig_account` writes.
	schema := renameTable(t, reservedSchema(t), "account", "rig_account")

	loaded, ldiags := tableconf.Parse("rig_account.yaml", []byte("table: rig_account\nresource: Account\n"))
	if ldiags.HasErrors() {
		t.Fatal(ldiags.String())
	}
	set := tableconf.NewSet()
	set.Add(loaded)

	got := compileReserved(t, schema, set, defaultProject, []string{"rig_account"})
	if slices.Contains(got, diag.CodeReservedResource.ID) {
		t.Errorf("rig's own table was refused the name it reserves: %v", got)
	}
}

// `auth.own` is the off switch, and it is off for both rules at once. A project
// that has forked the schema owns rig_account and owns Account with it, so
// leaving either rule on would refuse it its own tables.
func TestAuthOwnTurnsBothRulesOff(t *testing.T) {
	t.Parallel()

	const own = `project:
  name: demo
  module: example.com/demo
auth:
  own: true
`

	if got := compileReserved(t, reservedSchema(t), tableconf.NewSet(), own, nil); len(got) != 0 {
		t.Errorf("codes = %v, want none — auth.own owns all of it", got)
	}
}

// What the commands that write a file ask before they write it.
//
// [compile.Reserved] answers from a name alone, which is all `rig migration new
// --table` and `rig sync` have. It has to agree with the diagnostic — a command
// that wrote a file `rig validate` then refused would be worse than no check —
// and it has to stay quiet for everything else, because it is standing between
// a person and their own schema.
func TestReservedAnswersFromANameAlone(t *testing.T) {
	t.Parallel()

	p, pdiags := project.Parse("rig.yaml", []byte(defaultProject))
	if pdiags.HasErrors() {
		t.Fatal(pdiags.String())
	}

	for _, tc := range []struct {
		table string
		want  string
	}{
		{"account", "rig_account"},
		{"file", "rig_file"},
		{"api_key", "rig_api_key"},
		// The notification part arrived after this rule did and reserved its
		// names without anybody editing a list. These are the proof.
		{"notification", "rig_notification"},
		{"notification_recipient", "rig_notification_recipient"},
		{"notification_device", "rig_notification_device"},
		{"notification_setting", "rig_notification_setting"},
		{"notification_delivery", "rig_notification_delivery"},
		{"rig_leaderboard", "prefix"},
		{"rig_api_key", "setup-project"},
		// Everything else, which is the case that matters most.
		{"todo", ""},
		{"accounts", ""},
		{"account_role", ""},
		{"bookmark", ""},
	} {
		got := compile.Reserved(p, nil, tc.table)
		switch {
		case tc.want == "" && got != "":
			t.Errorf("%s was refused: %s", tc.table, got)
		case tc.want != "" && got == "":
			t.Errorf("%s was allowed, want a refusal mentioning %q", tc.table, tc.want)
		case tc.want != "" && !strings.Contains(got, tc.want):
			t.Errorf("%s: %q, want it to mention %q", tc.table, got, tc.want)
		}
	}
}

// The bookkeeping table is rig's and arrives through a different door. A project
// that renamed migrations.table and left the old one behind should not be told
// its migration history is illegal.
func TestTheMigrationsTableIsNotRefused(t *testing.T) {
	t.Parallel()

	schema := renameTable(t, reservedSchema(t), "rig_leaderboard", project.DefaultMigrationsTable)

	got := compileReserved(t, schema, tableconf.NewSet(), defaultProject, nil)
	if slices.Contains(got, diag.CodeReservedTablePrefix.ID) {
		t.Errorf("codes = %v, want no %s for %s",
			got, diag.CodeReservedTablePrefix.ID, project.DefaultMigrationsTable)
	}
}

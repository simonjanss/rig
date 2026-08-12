package scaffold_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/internal/tableconf"
)

func options() scaffold.FoundationOptions {
	return scaffold.FoundationOptions{
		MigrationsDir: "migrations",
		ConfigPath: func(table string) string {
			return filepath.Join("services", table, table+".yaml")
		},
	}
}

// Migrations and nothing else. The foundation's tables belong to the rig/auth
// module — their Go types, stores and endpoints are imported from there — so a
// table configuration for one would ask rig to generate a few thousand lines the
// project never calls.
func TestTheFoundationWritesOnlyMigrations(t *testing.T) {
	t.Parallel()

	files := scaffold.Foundation(options())
	if len(files) == 0 {
		t.Fatal("no files")
	}

	var migrations, configs int
	for _, f := range files {
		switch {
		case strings.HasPrefix(f.Path, "migrations"+string(filepath.Separator)):
			migrations++
			if !strings.Contains(f.Content, "-- +goose Up") ||
				!strings.Contains(f.Content, "-- +goose Down") {
				t.Errorf("%s is not a reversible goose migration", f.Path)
			}
		case strings.HasPrefix(f.Path, "services"+string(filepath.Separator)):
			configs++
		default:
			t.Errorf("%s is neither a migration nor a configuration", f.Path)
		}
	}

	if migrations != len(scaffold.Parts()) {
		t.Errorf("%d migrations for %d parts", migrations, len(scaffold.Parts()))
	}
	if configs != 0 {
		t.Errorf("%d table configurations written; the foundation is not generated from", configs)
	}
}

// Exposing a table is how an application gets a model and a repository for one
// after all — an administration screen listing the people in a tenant, most
// often. Every table has to be exposable, or `--expose` works for some of the
// foundation and silently writes nothing for the rest.
func TestEveryCreatedTableCanBeExposed(t *testing.T) {
	t.Parallel()

	var created []string
	for _, f := range scaffold.Foundation(options()) {
		if strings.HasSuffix(f.Path, ".sql") {
			created = append(created, tablesIn(f.Content)...)
		}
	}

	for _, table := range created {
		if slices.Contains(joinTables, table) {
			// A pure join table becomes a relation on the two resources it
			// links, not a resource of its own, so it has nothing to configure.
			continue
		}

		opt := options()
		opt.Expose = []string{table}

		var configured []string
		for _, f := range scaffold.Foundation(opt) {
			if strings.HasSuffix(f.Path, ".yaml") {
				configured = append(configured, strings.TrimSuffix(filepath.Base(f.Path), ".yaml"))
			}
		}

		if !slices.Equal(configured, []string{table}) {
			t.Errorf("exposing %s wrote %v", table, configured)
		}
	}
}

// The tables that are links rather than resources. Named here rather than
// inferred, so that a new one has to be thought about instead of quietly
// skipped by a heuristic.
var joinTables = []string{"role_permission", "account_role"}

// missing_comment is an error by default, and the foundation has to satisfy
// the rules it ships with — a scaffold whose own tables fail `rig validate` on
// the first run teaches the wrong thing about the tool.
func TestEveryCreatedTableIsCommented(t *testing.T) {
	t.Parallel()

	for _, f := range scaffold.Foundation(options()) {
		if !strings.HasSuffix(f.Path, ".sql") {
			continue
		}

		for _, table := range tablesIn(f.Content) {
			if !strings.Contains(f.Content, "COMMENT ON TABLE  "+table) &&
				!strings.Contains(f.Content, "COMMENT ON TABLE "+table) {
				t.Errorf("%s creates %s and never says what it is for", f.Path, table)
			}
		}
	}
}

// Every configuration file has to be one the loader accepts, at the exact shape
// the schema validates against. A scaffold that writes a file rig then rejects
// is worse than writing nothing.
func TestEveryConfigurationParses(t *testing.T) {
	t.Parallel()

	for _, f := range scaffold.Foundation(options()) {
		if !strings.HasSuffix(f.Path, ".yaml") {
			continue
		}

		loaded, diags := tableconf.Parse(f.Path, []byte(f.Content))
		if diags.HasErrors() {
			t.Errorf("%s does not parse:\n%s", f.Path, diags.String())
			continue
		}
		if loaded.File.Table == "" {
			t.Errorf("%s names no table", f.Path)
		}
	}
}

// Numbers are assigned in order from where the project already is, because
// goose runs them in that order and a foundation applied out of order would
// reference a table that does not exist yet.
func TestMigrationsAreNumberedInDependencyOrder(t *testing.T) {
	t.Parallel()

	opt := options()
	opt.FirstNumber = 5

	var names []string
	for _, f := range scaffold.Foundation(opt) {
		if strings.HasSuffix(f.Path, ".sql") {
			names = append(names, filepath.Base(f.Path))
		}
	}

	if len(names) == 0 {
		t.Fatal("no migrations")
	}
	if !strings.HasPrefix(names[0], "00005_") {
		t.Errorf("the first is %s, want it to continue from 5", names[0])
	}
	// Tenancy first: everything else references the tenant.
	if !strings.Contains(names[0], "tenancy") {
		t.Errorf("the first migration is %s, want tenancy", names[0])
	}

	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("%s does not sort before %s", names[i-1], names[i])
		}
	}
}

// Skipping is how a project says it does not want API keys. Skipping what
// everything else references is a schema that will not apply, and finding that
// out from psql is a poor way to learn it.
func TestSkippingLeavesTheRestCoherent(t *testing.T) {
	t.Parallel()

	opt := options()
	// Sessions goes with the keys: a token and a log entry name the key a
	// request arrived with, and they declare that column where the table is
	// created rather than altering it afterwards.
	opt.Skip = []string{"apikeys", "sessions", "oauth"}

	for _, f := range scaffold.Foundation(opt) {
		for _, skipped := range opt.Skip {
			if strings.Contains(f.Path, skipped) {
				t.Errorf("%s was skipped and written anyway", f.Path)
			}
		}
	}

	// And what everything depends on is declared, so a skip list that would
	// break the schema can be refused before anything is written.
	for _, part := range []string{"apikeys", "oauth"} {
		if got := scaffold.Requires(part); len(got) != 1 || got[0] != "tenancy" {
			t.Errorf("Requires(%q) = %v, want [tenancy]", part, got)
		}
	}
	if got := scaffold.Requires("sessions"); len(got) != 2 ||
		got[0] != "tenancy" || got[1] != "apikeys" {
		t.Errorf("Requires(\"sessions\") = %v, want [tenancy apikeys]", got)
	}
	if got := scaffold.Requires("tenancy"); len(got) != 0 {
		t.Errorf("tenancy depends on nothing, got %v", got)
	}
}

// Running setup twice must not write the same tables again under fresh
// numbers: `rig db up` would fail on the first CREATE TABLE, which is a poor
// way to learn the command was not idempotent.
func TestAPartAlreadyAppliedIsNotWrittenAgain(t *testing.T) {
	t.Parallel()

	opt := options()
	opt.Existing = []string{"00001_rig_tenancy.sql", "00002_rig_sessions.sql"}
	opt.FirstNumber = 3

	for _, f := range scaffold.Foundation(opt) {
		if strings.Contains(f.Path, "tenancy") || strings.Contains(f.Path, "sessions") {
			t.Errorf("%s is already applied and was written again", f.Path)
		}
	}

	// The number a part was written under is not remembered, so matching has
	// to be on the name.
	renumbered := options()
	renumbered.Existing = []string{"00042_rig_tenancy.sql"}

	for _, f := range scaffold.Foundation(renumbered) {
		if strings.Contains(f.Path, "tenancy") {
			t.Errorf("%s should have been recognised at any number", f.Path)
		}
	}
}

// tablesIn reads the table names a migration creates.
func tablesIn(sql string) []string {
	var out []string
	for line := range strings.SplitSeq(sql, "\n") {
		const prefix = "CREATE TABLE "
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(line, prefix), " ")
		out = append(out, strings.TrimSpace(name))
	}
	return out
}

// PartTables is written out by hand, because which tables belong to the auth
// module decides whether rig generates code for them — so it must not drift from
// the SQL that creates them.
func TestPartTablesMatchTheMigrations(t *testing.T) {
	t.Parallel()

	for _, part := range scaffold.Parts() {
		opt := options()
		opt.Skip = skipAllBut(part)

		var created []string
		for _, f := range scaffold.Foundation(opt) {
			if strings.HasSuffix(f.Path, ".sql") {
				created = append(created, tablesIn(f.Content)...)
			}
		}

		declared := scaffold.PartTables(part)
		slices.Sort(created)
		slices.Sort(declared)

		if !slices.Equal(created, declared) {
			t.Errorf("%s creates %v but declares %v", part, created, declared)
		}
	}
}

// A part is only managed once its migration is there, so a project with an
// rig_account table nobody scaffolded keeps getting a model and a repository for it.
func TestOnlyAppliedPartsAreManaged(t *testing.T) {
	t.Parallel()

	if got := scaffold.Managed(nil); len(got) != 0 {
		t.Errorf("nothing applied, yet %v is claimed", got)
	}

	got := scaffold.Managed([]string{"00001_rig_tenancy.sql"})
	if !slices.Equal(got, scaffold.PartTables("tenancy")) {
		t.Errorf("Managed = %v, want just the tenancy tables", got)
	}
	if slices.Contains(got, "rig_api_key") {
		t.Error("a part that was skipped should not be claimed")
	}

	// And every part applied is every table claimed.
	var all []string
	for _, part := range scaffold.Parts() {
		all = append(all, "00001_rig_"+part+".sql")
	}
	if got, want := len(scaffold.Managed(all)), len(scaffold.Tables()); got != want {
		t.Errorf("%d tables managed, want all %d", got, want)
	}
}

// skipAllBut leaves one part in, so its migration can be read on its own.
func skipAllBut(keep string) []string {
	var out []string
	for _, part := range scaffold.Parts() {
		if part != keep {
			out = append(out, part)
		}
	}
	return out
}

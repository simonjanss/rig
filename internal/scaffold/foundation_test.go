package scaffold_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/project"
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

// A part is only managed once its migration is there, which is what separates a
// rig_account rig created from one somebody wrote by hand — the second is
// refused for using a prefix rig keeps, not quietly generated for.
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

// Under `embedded` the modules carry the schema, so there is nothing for the
// project to keep — and the migrations are the only thing that changes. A table
// configuration is about rig.yaml rather than DDL, so exposing one is still a
// question a project in this mode can answer.
func TestConfigsOnlyWritesNoMigrations(t *testing.T) {
	t.Parallel()

	opt := options()
	opt.ConfigsOnly = true
	opt.Expose = []string{"rig_account"}

	files := scaffold.Foundation(opt)
	if len(files) != 1 {
		t.Fatalf("want just the one configuration, got %d files", len(files))
	}
	if !strings.HasSuffix(files[0].Path, ".yaml") {
		t.Errorf("wrote %s, want only a table configuration", files[0].Path)
	}

	// And with nothing exposed it writes nothing at all, which is the ordinary
	// case for this mode rather than a failure to do anything.
	opt.Expose = nil
	if files := scaffold.Foundation(opt); len(files) != 0 {
		t.Errorf("want no files, got %v", files)
	}
}

// The upgrade path, which is what the append-only sets are for. A module that
// has gained a migration since this project was set up has a part nothing in the
// directory matches, so it is written at the next free number — and what is
// already applied is left exactly as it was.
func TestAnUnseenPartIsWrittenAtTheNextNumber(t *testing.T) {
	t.Parallel()

	// A project that vendored everything except the last part, as though that
	// part had not existed when it was set up.
	parts := scaffold.Parts()
	old, latest := parts[:len(parts)-1], parts[len(parts)-1]

	var existing []string
	for i, part := range old {
		existing = append(existing, fmt.Sprintf("%05d_rig_%s.sql", i+1, part))
	}

	opt := options()
	opt.Existing = existing
	opt.FirstNumber = len(existing) + 1

	files := scaffold.Foundation(opt)
	if len(files) != 1 {
		t.Fatalf("want only the part this project has never seen, got %d files", len(files))
	}

	want := fmt.Sprintf("%05d_rig_%s.sql", len(existing)+1, latest)
	if filepath.Base(files[0].Path) != want {
		t.Errorf("wrote %s, want %s", filepath.Base(files[0].Path), want)
	}

	// Nothing already applied is rewritten, which is the half that matters:
	// somebody's database has run those, and a second copy under a fresh number
	// would fail on the first CREATE TABLE.
	for _, name := range existing {
		for _, f := range files {
			if strings.HasSuffix(f.Path, name) {
				t.Errorf("%s was written again", name)
			}
		}
	}
}

// Every set the foundation is assembled from has to be coherent on its own terms.
// Each owning module asserts this too; here it is asserted about the three
// together, which is the arrangement rig actually uses.
func TestEverySetIsCoherent(t *testing.T) {
	t.Parallel()

	for _, s := range scaffold.Sets() {
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", s.Module, err)
		}
	}
}

// A part name has to identify one migration across all three sets, because that
// is what a vendored `_rig_<name>.sql` file claims and what [scaffold.SetOf]
// answers from. Two modules shipping a `tenancy` would make both ambiguous, and
// the ambiguity would land in somebody's migrations directory.
func TestPartNamesAreUniqueAcrossSets(t *testing.T) {
	t.Parallel()

	owner := map[string]string{}
	for _, s := range scaffold.Sets() {
		for _, m := range s.Migrations {
			if prev, taken := owner[m.Name]; taken {
				t.Errorf("both %s and %s ship a migration named %q", prev, s.Module, m.Name)
			}
			owner[m.Name] = s.Module
		}
	}

	for _, part := range scaffold.Parts() {
		s, ok := scaffold.SetOf(part)
		if !ok {
			t.Errorf("part %q belongs to no set", part)
			continue
		}
		if s.Module != owner[part] {
			t.Errorf("SetOf(%q) = %s, want %s", part, s.Module, owner[part])
		}
	}
}

// Each set records itself somewhere of its own, and nowhere near the project's.
// Two sets sharing a table would share a numbering sequence, which is the
// collision the separation exists to prevent; a set writing into the project's
// table would make rig's history and the project's indistinguishable.
func TestEverySetRecordsItselfSeparately(t *testing.T) {
	t.Parallel()

	seen := map[string]string{}
	for _, s := range scaffold.Sets() {
		if s.Table == project.DefaultMigrationsTable {
			t.Errorf("%s records itself in the project's own bookkeeping table", s.Module)
		}
		if prev, taken := seen[s.Table]; taken {
			t.Errorf("%s and %s both record themselves in %s", prev, s.Module, s.Table)
		}
		seen[s.Table] = s.Module
	}
}

// A part cannot require one that is applied after it. [scaffold.Requires] is
// hand-written — a dependency between two CREATE TABLEs is not something to
// derive by parsing — so this is what keeps it agreeing with the order the sets
// are applied in.
func TestRequirementsComeFirst(t *testing.T) {
	t.Parallel()

	parts := scaffold.Parts()
	for i, part := range parts {
		for _, needs := range scaffold.Requires(part) {
			j := slices.Index(parts, needs)
			if j < 0 {
				t.Errorf("%s requires %q, which is not a part", part, needs)
				continue
			}
			if j >= i {
				t.Errorf("%s requires %s, which is applied later", part, needs)
			}
		}
	}
}

// The parts a configuration brings, which is how a project that did not vendor
// the foundation says which of it it has.
//
// Brings and not asks for: a set is applied whole, so every part of every set a
// feature reaches comes with it. `auth:` with no provider configured still brings
// oauth, and an inbox in a project with no authentication brings the whole of
// auth's set on its way to rig_account.
func TestWantedExpandsToPartsInOrder(t *testing.T) {
	t.Parallel()

	everything := []string{"tenancy", "apikeys", "sessions", "oauth", "notifications", "idempotency"}

	cases := []struct {
		name  string
		want  scaffold.Wanted
		parts []string
	}{
		// idempotency is on every row below, including this one. It is the part
		// no feature asks for and every project brings, because what it does is
		// engaged by a request header rather than by a configuration.
		{"nothing", scaffold.Wanted{}, []string{"idempotency"}},
		{
			// files' set is one migration, so this is the one case where what a
			// project asks for and what it gets are the same list.
			"files alone",
			scaffold.Wanted{Files: true},
			[]string{"files", "idempotency"},
		},
		{
			// oauth is not a question a project in this mode gets to answer: it is
			// a migration in auth's set, and goose reads a directory.
			"auth without oauth brings oauth anyway",
			scaffold.Wanted{Auth: true},
			[]string{"tenancy", "apikeys", "sessions", "oauth", "idempotency"},
		},
		{
			"auth with oauth is the same list",
			scaffold.Wanted{Auth: true, OAuth: true},
			[]string{"tenancy", "apikeys", "sessions", "oauth", "idempotency"},
		},
		{
			// The cross-module edge: an inbox line names an account, so auth's set
			// comes along even though nothing asked for authentication — and all of
			// it, not just the tenancy migration that creates rig_account.
			"notifications alone",
			scaffold.Wanted{Notifications: true},
			everything,
		},
		{
			// Every field set, so this case is what fails when a part is added and
			// Wanted grows a flag nobody wired into Parts.
			"everything",
			scaffold.Wanted{
				Auth: true, OAuth: true, Files: true, Notifications: true, Presence: true,
			},
			scaffold.Parts(),
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.want.Parts()
			if !slices.Equal(got, c.parts) {
				t.Errorf("Parts() = %v, want %v", got, c.parts)
			}
		})
	}
}

// The invariant the case above is one reading of, asserted directly: the tables a
// configuration is told it has are exactly the tables the sets it applies create.
//
// This is the bug that shipped in the first draft. `Wanted.Parts` answered with
// the parts asked for while `SetsFor` applied whole sets, so `rig db up` created
// rig_identity_oauth and the next `rig validate` reported RIG2005 on it — a table
// under rig's prefix that, as far as the narrower list knew, rig had not created.
// Advice included, which nothing in that mode could follow: rename the migration
// that creates it.
func TestEveryTableASetCreatesIsAccountedFor(t *testing.T) {
	t.Parallel()

	for _, w := range []scaffold.Wanted{
		{Auth: true},
		{Auth: true, OAuth: true},
		{Files: true},
		{Notifications: true},
		{Auth: true, Files: true, Notifications: true},
	} {
		parts := w.Parts()
		known := scaffold.TablesFor(parts)

		var applied []string
		for _, s := range scaffold.SetsFor(parts) {
			applied = append(applied, s.Tables()...)
		}

		for _, table := range applied {
			if !slices.Contains(known, table) {
				t.Errorf("%+v applies a set that creates %s, which TablesFor does not "+
					"list: rig would refuse a table it had just created", w, table)
			}
		}
		for _, table := range known {
			if !slices.Contains(applied, table) {
				t.Errorf("%+v is told it has %s, which no set it applies creates", w, table)
			}
		}
	}
}

// The sets a list of parts needs, each once — a set is applied whole, because
// goose reads a directory rather than a list.
func TestSetsForNamesEachSetOnce(t *testing.T) {
	t.Parallel()

	got := scaffold.SetsFor([]string{"tenancy", "sessions", "notifications"})
	if len(got) != 2 {
		t.Fatalf("want auth and notify, got %d sets", len(got))
	}
	if got[0].Module != "rig/auth" || got[1].Module != "rig/notify" {
		t.Errorf("got %s then %s, want rig/auth then rig/notify", got[0].Module, got[1].Module)
	}

	if got := scaffold.SetsFor(nil); len(got) != 0 {
		t.Errorf("no parts, yet %d sets", len(got))
	}
}

// The two modes cannot describe different schemas, because there is one copy of
// the SQL and both read it.
//
// Vendoring writes what the set carries, so this asserts the thing that can
// actually break: every `_rig_<name>.sql` already committed in an example is still
// byte-identical to the migration of that name in the module's set. `make
// examples` regenerates generated code and would not notice — a migration file is
// not regenerated, it is copied once and then owned.
//
// Which is also the append-only rule seen from the other end. Editing a shipped
// migration is exactly what this fails on, and the reason it must: those files have
// been applied.
func TestVendoredMigrationsStillMatchTheirSets(t *testing.T) {
	t.Parallel()

	vendored, err := filepath.Glob(filepath.Join("..", "..", "examples", "*", "migrations", "*_rig_*.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if len(vendored) == 0 {
		t.Skip("no example vendors the foundation")
	}

	for _, path := range vendored {
		base := filepath.Base(path)
		// `00007_rig_notifications.sql` names the part `notifications`.
		name := strings.TrimSuffix(base, ".sql")
		if i := strings.Index(name, "_rig_"); i >= 0 {
			name = name[i+len("_rig_"):]
		}

		set, ok := scaffold.SetOf(name)
		if !ok {
			t.Errorf("%s: no set ships a migration named %q", path, name)
			continue
		}
		want, err := set.Read(name)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", path, err)
			continue
		}
		if string(got) != string(want) {
			t.Errorf("%s has drifted from %s's %q: a migration that has been applied "+
				"cannot be edited, so this is either an accidental change here or an "+
				"upgrade that should have been a new migration", path, set.Module, name)
		}
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

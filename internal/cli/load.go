package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/migcheck"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// loadProject finds and reads rig.yaml.
func (e *env) loadProject() (*project.Project, diag.List) {
	if e.configPath != "" {
		return project.LoadFile(e.configPath)
	}
	return project.Load(e.dir)
}

// loadTables reads every table configuration file the layout points at.
func loadTables(p *project.Project) (*tableconf.Set, diag.List) {
	var diags diag.List

	paths, err := p.TableConfigPaths()
	if err != nil {
		diags.Add(diag.CodeConfigFile, diag.Anchor{}, "%v", err)
		return tableconf.NewSet(), diags
	}

	set, d := tableconf.LoadDir(paths)
	diags.Append(d)
	return set, diags
}

// addFoundationConfigs fills in rig's own answer for every table of rig's the
// project did not write a file for.
//
// A table rig created is a table rig knows how to project: the API name the
// `rig_` prefix strips to, which operations belong on it, which of its columns
// are read-only, and what each of its enum values means. All of that is in
// [scaffold.TableConfig] already — it is the text `rig setup-project --expose`
// writes — and until this ran it reached a project only by being written to disk.
// So every project that exposed one of rig's tables kept a few hundred lines
// describing rig's schema, most of it copied verbatim out of the COMMENT ON
// statements the module's own migration carries.
//
// A file on disk wins outright rather than being merged with this. Wholesale is
// the rule that can be stated in one sentence: what a project reads in its own
// services/ directory is what applies. Merging would mean every key having an
// answer to "and what if both say something", and a project could no longer
// widen an operation list by editing the file in front of it.
//
// The path is not a path. Nothing reads a foundation configuration off disk, and
// a diagnostic anchored to a file somebody could go and open would send them
// looking for one that is not there.
func addFoundationConfigs(set *tableconf.Set, schema ir.Schema, foundation, ignore []string) diag.List {
	var diags diag.List
	for _, table := range foundation {
		if set.Get(table) != nil || set.Failed(table) {
			continue
		}
		// Only the ones being projected. A configuration for a table rig
		// generates nothing from is refused as RIG3107, and rightly — it is a
		// file describing a table that will not be in the document — so an
		// ignored table gets nothing, and neither does one whose migration has
		// not been applied yet: RIG3102 is what a file for a table that is not
		// in the database earns.
		if slices.Contains(ignore, table) || !hasTable(schema, table) {
			continue
		}
		content, ok := scaffold.TableConfig(table)
		if !ok {
			continue
		}
		loaded, d := tableconf.Parse("rig/"+table, content)
		diags.Append(d)
		if loaded != nil {
			// Marked as rig's, because one rule has to tell this apart from a
			// file: rig's configurations list no columns, and
			// `unmentioned_column` over them is sixty warnings telling a project
			// to run `rig sync` against a file `rig sync` will not write.
			loaded.Builtin = true
			set.Add(loaded)
		}
	}
	return diags
}

// compileFrom builds the document from a schema already in hand.
//
// Until introspection lands, the schema comes from a file — which is also how
// the compiler is exercised in tests, and how it will keep being exercised
// afterwards.
func compileFrom(p *project.Project, schema ir.Schema) (*ir.Document, diag.List) {
	var diags diag.List

	set, d := loadTables(p)
	diags.Append(d)

	ignore, foundation, err := foundationTables(p)
	if err != nil {
		diags.Add(diag.CodeConfigFile, diag.Anchor{}, "%v", err)
	}
	if !p.Config.Auth.Own {
		// `auth.own` means the project took rig's schema over, so the tables are
		// its own to describe and rig has no answer to offer about them.
		diags.Append(addFoundationConfigs(set, schema, foundation, ignore))
	}
	diags.Append(checkFoundationMode(p))
	diags.Append(checkMigrationFiles(p))
	diags.Append(checkFoundationPresent(p))
	for _, block := range foundationBlocks(p) {
		diags.Append(checkFoundationBlock(p, block))
	}

	doc, d := compile.Compile(schema, set, compile.Options{
		Project:      p,
		Tool:         "rig " + Version,
		IgnoreTables: ignore,
		Foundation:   foundation,
	})
	diags.Append(d)

	return doc, diags
}

// hasTable reports whether the introspected schema holds a table by this name.
func hasTable(schema ir.Schema, table string) bool {
	return slices.ContainsFunc(schema.Tables, func(t ir.Table) bool { return t.Name == table })
}

// foundationParts names the parts of rig's foundation this project has, by
// whichever evidence its mode leaves available.
//
// Vendored — the default — reads the migration files, because a part rig wrote is
// named for itself. That is the stronger reading, and the reason is that it can
// tell a scaffolded rig_account from one somebody wrote by hand: the migration is
// there or it is not.
//
// Embedded reads the configuration instead, because there are no files here to
// read — the modules carry the schema, so what a project has is what it turned
// on. Turned on and everything that came with it: [scaffold.Wanted.Parts] widens
// a configuration to whole sets, because that is what `rig db up` applies from
// [foundationSources], and the two readings have to be the same one. They were
// not once, and the symptom was RIG2005 on a table rig had created a command
// earlier.
//
// It is weaker than the vendored reading in exactly one way worth naming: with no
// migration in the project to point at, it cannot tell rig's rig_account from a
// hand-written one, and a project that wrote its own would have it silently
// treated as the auth module's. What stands in for the missing evidence is that
// the mode itself is declared — `foundation: embedded` says the modules own those
// tables, and `auth.own` says the opposite loudly enough that the two together are
// refused rather than reconciled ([checkFoundationMode]).
func foundationParts(p *project.Project) ([]string, error) {
	if !p.Config.Migrations.Vendored() {
		return scaffold.Wanted{
			Auth:          p.Config.Auth.Enabled,
			OAuth:         len(p.Config.Auth.OAuth.Providers) > 0,
			Files:         p.Config.Files.Enabled,
			Notifications: p.Config.Notifications.Enabled,
			Presence:      p.Config.Presence.Enabled,
		}.Parts(), nil
	}

	names, err := migrationNames(p.MigrationsDir())
	if err != nil {
		if os.IsNotExist(err) {
			// No migrations directory means no foundation, which is the state of
			// a project between `rig init` and its first migration.
			return nil, nil
		}
		return nil, err
	}
	return scaffold.AppliedParts(names), nil
}

// foundationManaged are the tables rig's foundation creates in this project,
// whichever way [foundationParts] found out about them.
func foundationManaged(p *project.Project) ([]string, error) {
	parts, err := foundationParts(p)
	if err != nil {
		return nil, err
	}
	return scaffold.TablesFor(parts), nil
}

// foundationTables answers two questions from one reading of the foundation.
//
// `ignore` are the tables rig generates nothing for. `foundation` are the ones
// rig created at all, exposed or not — which is a wider set, because exposing a
// table takes it out of the first list and leaves it rig's. That difference is
// the whole reason for two return values: it is what tells a scaffolded
// rig_account from one somebody wrote by hand, and nothing else can.
//
// Neither is guessed from the table's name. Guessing would silently stop
// generating a repository somebody depends on.
func foundationTables(p *project.Project) (ignore, foundation []string, err error) {
	foundation, err = foundationManaged(p)
	if err != nil {
		return nil, nil, err
	}

	if p.Config.Auth.Own {
		// The project has taken the schema over, so there is nothing to leave
		// out and every rule applies to all of it. `foundation` is still true and
		// simply unused: the reserved-name rules read `auth.own` themselves and
		// stop before they ask.
		return nil, foundation, nil
	}

	var out []string
	for _, table := range foundation {
		// rig_file has a switch of its own rather than a place in that list,
		// because the reason to expose it is not the reason to expose any of the
		// others. The url lives on the row, so a client that cannot read
		// rig_file cannot use the column that exists for it — and a live-sync
		// endpoint on an unexposed resource is refused, correctly.
		//
		// `auth.expose` deliberately does not reach it. It would happen to work
		// and would leave `files.expose` — which the rest of rig reads, and which
		// checkFoundationBlock guards — saying the opposite.
		if table == compile.FileTable {
			if p.Config.Files.Enabled && p.Config.Files.Expose {
				continue
			}
			out = append(out, table)
			continue
		}
		// The notification tables are the opposite case, and the difference is
		// worth reading beside rig_file rather than discovering. They are never
		// ignored while notifications are on — not even with `expose` off —
		// because a link table is classified against a map built after the
		// ignored tables have been dropped. Ignore rig_notification and every
		// project's `<subject>_notification` silently stops being a link table,
		// every notifiable resource silently stops being one, and nothing says
		// why. `notifications.expose` marks them unexposed instead, which keeps
		// them classified and generates nothing — no endpoints, and since
		// [ir.Resource.Unreachable] no model or repository either. The resource
		// exists so the link tables around it can be recognised; that is the
		// whole of what it is for.
		if slices.Contains(compile.NotificationTables(), table) {
			if p.Config.Notifications.Enabled {
				continue
			}
			out = append(out, table)
			continue
		}
		// rig_presence is the notification case again, and the consequence of
		// getting it wrong is more direct. The live shape *is* how presence is
		// read — a heartbeat is a row change and the stream is what carries it to
		// everybody else — and an ignored table has no resource, so no shape, so
		// no route. Presence would be a table nothing reads and a browser package
		// with nowhere to subscribe, and the only symptom would be a 404.
		//
		// `presence.expose` marks it unexposed instead, which keeps the resource
		// and its shape and generates no endpoints — and no model or repository,
		// which is [ir.Resource.Unreachable]. The shape is built from the
		// document rather than from the model, so it survives that.
		if table == compile.PresenceTable {
			if p.Config.Presence.Enabled {
				continue
			}
			out = append(out, table)
			continue
		}
		// An exposed table is projected like any other: the point of the list is
		// to get a model and a repository back.
		if slices.Contains(p.Config.Auth.Expose, table) {
			continue
		}
		out = append(out, table)
	}
	return out, foundation, nil
}

// checkFoundationMode reports a `migrations.foundation` that contradicts the
// migrations directory.
//
// The mode is chosen once, and this is what makes that a statement rather than
// advice. The two modes record what they applied in different tables — the
// project's own under `vendored`, one per module under `embedded` — so a project
// that switches after it has a database finds the new mode's bookkeeping empty and
// re-applies a schema that is already there. That fails partway through `rig db up`
// on a CREATE TABLE, having already run whatever came before it, and the recovery
// is not obvious from the error.
//
// The contradiction worth its own code is one way round: `embedded` with rig's
// migrations still in the directory, which is a project that was vendored and had
// its mode changed under it.
//
// `auth.own` with `embedded` is the other contradiction, and it is refused here
// too rather than resolved in favour of one of them. The two keys say opposite
// things about who maintains rig's tables, so whichever won would be silent about
// the other — and the way that shows up is `rig db up` applying the modules' sets
// over the migrations the project forked, stopping on a table that already exists.
//
// The reverse — `vendored` with none of them while a block that needs them is on —
// is already reported, and better, by [checkFoundationPresent] and
// [checkFoundationBlock]: each names the block that wants the part and tells you
// to run `setup-project`, which in that mode is exactly right. Saying it a
// second time here would be two diagnostics for one mistake.
//
// Adopting an existing schema into a fresh bookkeeping table is a real feature and
// this is deliberately not it. There is no baseline command; what there is, is a
// refusal naming the one supported move.
func checkFoundationMode(p *project.Project) diag.List {
	var diags diag.List

	if p.Config.Migrations.Vendored() {
		return diags
	}

	// `auth.own` says the project forked rig's migrations and maintains those
	// tables itself, which is the opposite of what `embedded` says. Reported
	// before the directory is read, because it is the stronger contradiction: it
	// holds however the forked migrations happen to be named, and those names are
	// the only thing the check below can see.
	if p.Config.Auth.Own {
		diags.Add(diag.CodeFoundationMode, p.At("migrations", "foundation"),
			"migrations.foundation is embedded but auth.own is set, and they say opposite "+
				"things about who maintains rig's tables; the modules would apply their own "+
				"schema over the migrations this project forked. Leave the mode vendored, or "+
				"drop auth.own")
		return diags
	}

	names, err := migrationNames(p.MigrationsDir())
	if err != nil {
		// No directory is a project between `rig init` and its first migration,
		// which contradicts nothing. Any other read error is foundationTables'.
		return diags
	}

	if applied := scaffold.AppliedParts(names); len(applied) > 0 {
		diags.Add(diag.CodeFoundationMode, p.At("migrations", "foundation"),
			"migrations.foundation is embedded, but %s still holds rig's own migrations "+
				"(%s); the modules would re-apply that schema under their own bookkeeping "+
				"and fail on a table that already exists",
			p.Config.Migrations.Dir, strings.Join(applied, ", "))
	}
	return diags
}

// checkMigrationFiles reports the migration names in this project that goose or
// rig will not read the way somebody meant them.
//
// Both rules are answerable from the directory listing alone, which is why they
// run here rather than waiting for `rig db up`: a duplicate version is a
// migration that never runs, and it surfaces at a fresh replay on somebody
// else's machine, weeks later, as a table that is not there.
//
// The third rule — a version at or below what the base branch already has —
// is not here. It needs a git ref, and `rig validate` cannot assume it is in a
// repository, let alone which ref this branch is aimed at. That one is
// `rig migration check --base`.
//
// Under `migrations.foundation: embedded` there is still exactly one directory
// to check, and it is still the project's own: the modules carry their own sets,
// each numbered from one in a namespace of its own, and a project's numbering
// has nothing to say about them.
func checkMigrationFiles(p *project.Project) diag.List {
	var diags diag.List

	names, err := migrationNames(p.MigrationsDir())
	if err != nil {
		// No directory is a project between `rig init` and its first migration,
		// which has nothing to check. Any other read error is foundationTables'
		// to report, so that one unreadable directory is one diagnostic.
		return diags
	}

	dir := p.Config.Migrations.Dir
	diags.Append(migcheck.CheckNames(dir, names,
		p.Severity(p.Config.Validate.MigrationFilename, diag.CodeMigrationFilename)))
	diags.Append(migcheck.CheckDuplicates(dir, names))
	return diags
}

// foundationBlock is one rig.yaml block whose tables rig maintains: whether it
// is on, and which tables it owns.
//
// A description rather than three near-identical functions. It carried two
// checks and now carries one, because the half that mattered — an exposed table
// with no configuration saying what may be done to it — became unreachable when
// [addFoundationConfigs] started supplying that configuration. What is left is
// reported for tidiness: a project missing the migration fails on its first
// request either way.
type foundationBlock struct {
	// name is the rig.yaml key. It is the anchor and the first word of the
	// message, which is why there is only one of it.
	name    string
	enabled bool
	tables  []string
}

// foundationBlocks describes every block that owns rig tables.
//
// Adding one here is the whole of adding it, and the order is the order the
// diagnostics come out in.
func foundationBlocks(p *project.Project) []foundationBlock {
	return []foundationBlock{
		{
			name:    "files",
			enabled: p.Config.Files.Enabled,
			tables:  []string{compile.FileTable},
		},
		{
			name:    "notifications",
			enabled: p.Config.Notifications.Enabled,
			tables:  compile.NotificationTables(),
		},
		{
			name:    "presence",
			enabled: p.Config.Presence.Enabled,
			tables:  []string{compile.PresenceTable},
		},
	}
}

// checkFoundationBlock reports a block whose tables are not there.
//
// It used to also refuse an exposed table with no configuration saying what may
// be done to it — the inbox arriving with a generated PATCH over what somebody
// was told, presence arriving with a generated Create. [addFoundationConfigs]
// retired that half by making it unreachable: rig writes the answer for its own
// table into the set now, so there is no reading left under which one of them
// arrives with the full CRUD default.
func checkFoundationBlock(p *project.Project, b foundationBlock) diag.List {
	var diags diag.List
	if !b.enabled {
		return diags
	}

	managed, err := foundationManaged(p)
	if err != nil {
		return diags
	}

	for _, table := range b.tables {
		if !slices.Contains(managed, table) {
			diags.Add(diag.CodeConfigInvalid, p.At(b.name, "enabled"),
				"%s.enabled is set but this project has no %s migration; "+
					"run `rig setup-project`", b.name, table)
			// One diagnostic and not one per table: they are missing for the
			// same reason, and `setup-project` writes all of them at once.
			return diags
		}
	}
	return diags
}

// checkFoundationPresent reports an enabled authentication block with nothing
// behind it.
//
// `auth.enabled` says the endpoints are mounted and the tables are the auth
// module's; a project that has not scaffolded them would compile, generate, and
// then fail on its first request against tables that do not exist. Saying so
// here costs one directory listing.
func checkFoundationPresent(p *project.Project) diag.List {
	var diags diag.List
	if !p.Config.Auth.Enabled || p.Config.Auth.Own {
		return diags
	}

	managed, err := foundationManaged(p)
	if err != nil {
		// Any read error has already been reported by foundationTables.
		return diags
	}

	if len(managed) == 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("auth", "enabled"),
			"auth.enabled is set but this project has no foundation migrations; "+
				"run `rig setup-project`, or set auth.own if you maintain the tables yourself")
	}
	return diags
}

// readSchemaFile reads a schema dump.
func readSchemaFile(path string) (ir.Schema, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ir.Schema{}, fmt.Errorf("read schema: %w", err)
	}

	// Unknown fields are rejected, so a stale dump written by a different rig
	// fails loudly instead of quietly losing columns.
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()

	var schema ir.Schema
	if err := dec.Decode(&schema); err != nil {
		return ir.Schema{}, fmt.Errorf("%s: %w", path, err)
	}
	return schema, nil
}

// writeOutput writes to a path, or to standard output when the path is empty
// or "-".
func (e *env) writeOutput(path string, content []byte) error {
	if path == "" || path == "-" {
		_, err := e.out.Write(content)
		return err
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Fprintf(e.errOut, "wrote %s\n", path)
	return nil
}

// mustProject loads the project, reporting and failing when it cannot.
//
// Commands that cannot do anything useful without a project use this instead of
// threading diagnostics through, since the only diagnostic possible at that
// point is "there is no project here".
func (e *env) mustProject() (*project.Project, error) {
	p, diags := e.loadProject()
	if p == nil {
		return nil, e.report(&diags)
	}
	if err := e.report(&diags); err != nil {
		return nil, err
	}
	return p, nil
}

// managedProject is [env.mustProject], refused unless rig is the one that runs
// this project's database.
//
// `database.url` is how a project points rig at a database somebody else runs,
// and a command that stops one or throws one away had better not. Two commands
// do — `db down` and `db reset` — and they refused in the same sentence, which
// is the whole reason this is a function rather than the six lines it replaces:
// one rule that two places word for themselves is one rule that can come to be
// worded two ways.
//
// The two commands that look like they belong here and do not. `db up` does not
// refuse at all: pointed at a database it does not manage, applying the
// migrations is exactly the useful thing to do, and its one refusal is about
// `database.electric`, which is a sync service rig would have to run itself.
// And `db url` answers for an unmanaged database rather than refusing, because
// printing a connection string costs the database nothing.
func (e *env) managedProject() (*project.Project, error) {
	p, err := e.mustProject()
	if err != nil {
		return nil, err
	}
	if !p.UsesContainer() {
		return nil, fmt.Errorf("database.url is set, so rig does not manage this database")
	}
	return p, nil
}

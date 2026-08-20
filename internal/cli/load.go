package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
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

// compileFrom builds the document from a schema already in hand.
//
// Until introspection lands, the schema comes from a file — which is also how
// the compiler is exercised in tests, and how it will keep being exercised
// afterwards.
func compileFrom(p *project.Project, schema ir.Schema) (*ir.Document, diag.List) {
	var diags diag.List

	set, d := loadTables(p)
	diags.Append(d)

	ignore, err := foundationTables(p)
	if err != nil {
		diags.Add(diag.CodeConfigFile, diag.Anchor{}, "%v", err)
	}
	diags.Append(checkFoundationPresent(p))
	diags.Append(checkFilesFoundation(p, set))
	diags.Append(checkNotificationsFoundation(p, set))

	doc, d := compile.Compile(schema, set, compile.Options{
		Project:      p,
		Tool:         "rig " + Version,
		IgnoreTables: ignore,
	})
	diags.Append(d)

	return doc, diags
}

// foundationTables are the tables rig generates nothing for.
//
// They are the authentication foundation's, and they are recognised by the
// project's own migration files rather than by name alone: `rig_account` is the
// auth module's table in a project that scaffolded it, and an ordinary table in
// a project that wrote its own. Guessing from the name would silently stop
// generating a repository somebody depends on.
func foundationTables(p *project.Project) ([]string, error) {
	if p.Config.Auth.Own {
		// The project has taken the schema over, so there is nothing to leave
		// out and every rule applies to all of it.
		return nil, nil
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

	var out []string
	for _, table := range scaffold.Managed(names) {
		// rig_file has a switch of its own rather than a place in that list,
		// because the reason to expose it is not the reason to expose any of the
		// others. The url lives on the row, so a client that cannot read
		// rig_file cannot use the column that exists for it — and a live-sync
		// endpoint on an unexposed resource is refused, correctly.
		//
		// `auth.expose` deliberately does not reach it. It would happen to work
		// and would leave `files.expose` — which the rest of rig reads, and which
		// checkFilesFoundation guards — saying the opposite.
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
		// the model and the repository and generates no endpoints.
		if slices.Contains(compile.NotificationTables(), table) {
			if p.Config.Notifications.Enabled {
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
	return out, nil
}

// checkFilesFoundation reports a files block whose table is not there, and an
// exposed rig_file with no configuration saying what may be done to it.
//
// The second one is the dangerous half. Removing rig_file from the ignore list
// is what makes it a resource; the table configuration is what makes that
// resource read-only and narrow. Without the configuration it would arrive with
// full CRUD over the storage key — which is a way to point a row at any object
// in the bucket, and precisely what the design refuses to generate.
func checkFilesFoundation(p *project.Project, set *tableconf.Set) diag.List {
	var diags diag.List
	if !p.Config.Files.Enabled {
		return diags
	}

	names, err := migrationNames(p.MigrationsDir())
	if err != nil && !os.IsNotExist(err) {
		return diags
	}

	if !slices.Contains(scaffold.Managed(names), compile.FileTable) {
		diags.Add(diag.CodeConfigInvalid, p.At("files", "enabled"),
			"files.enabled is set but this project has no %s migration; "+
				"run `rig setup-project`", compile.FileTable)
		return diags
	}

	if p.Config.Files.Expose && set != nil && set.Get(compile.FileTable) == nil {
		diags.Add(diag.CodeConfigInvalid, p.At("files", "expose"),
			"files.expose projects %s, but there is no table configuration for it, so it would "+
				"arrive with full CRUD over its storage key; "+
				"run `rig setup-project --expose %s`", compile.FileTable, compile.FileTable)
	}
	return diags
}

// checkNotificationsFoundation reports a notifications block whose tables are
// not there, and an exposed inbox with no configuration saying what may be done
// to it.
//
// The second one is the dangerous half, exactly as it is for rig_file. Exposing
// the inbox is what makes it a resource; the table configuration is what makes
// that resource read-and-delete. Without it, the inbox would arrive with a
// generated PATCH over `kind` and `event_count` — which is a way to rewrite what
// somebody was told, and a POST that could address a notification to anybody.
func checkNotificationsFoundation(p *project.Project, set *tableconf.Set) diag.List {
	var diags diag.List
	if !p.Config.Notifications.Enabled {
		return diags
	}

	names, err := migrationNames(p.MigrationsDir())
	if err != nil && !os.IsNotExist(err) {
		return diags
	}

	managed := scaffold.Managed(names)
	for _, table := range compile.NotificationTables() {
		if !slices.Contains(managed, table) {
			diags.Add(diag.CodeConfigInvalid, p.At("notifications", "enabled"),
				"notifications.enabled is set but this project has no %s migration; "+
					"run `rig setup-project`", table)
			return diags
		}
	}

	if !p.Config.Notifications.Expose || set == nil {
		return diags
	}
	for _, table := range compile.NotificationTables() {
		if set.Get(table) == nil {
			diags.Add(diag.CodeConfigInvalid, p.At("notifications", "expose"),
				"notifications.expose projects %s, but there is no table configuration for it, "+
					"so it would arrive with a generated write path over what somebody was told; "+
					"run `rig setup-project --expose %s`", table, table)
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

	names, err := migrationNames(p.MigrationsDir())
	if err != nil {
		// A missing migrations directory is itself the answer, and any other read
		// error has already been reported by foundationTables.
		if !os.IsNotExist(err) {
			return diags
		}
	}

	if len(scaffold.Managed(names)) == 0 {
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

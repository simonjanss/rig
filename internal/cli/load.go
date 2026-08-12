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
		// An exposed table is projected like any other: the point of the list is
		// to get a model and a repository back.
		if slices.Contains(p.Config.Auth.Expose, table) {
			continue
		}
		out = append(out, table)
	}
	return out, nil
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

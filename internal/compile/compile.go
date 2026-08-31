// Package compile turns a Postgres schema and its table configuration into the
// frozen document every generator reads.
//
// The pipeline is a sequence of pure functions. Introspection — the one impure
// step — happens elsewhere and hands its result in by value, so the whole
// compiler runs from a JSON fixture with no database and no container. That is
// what makes the bulk of rig's test suite finish in under a second, and it is
// the reason [Compile] takes a schema rather than a connection.
//
// Every stage appends to one diagnostic list rather than returning on the first
// problem, so a developer who has made five mistakes learns about all five from
// one run.
package compile

import (
	"slices"

	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// Options carry everything the pure stages need from the project.
type Options struct {
	// Project supplies naming, layout, API shape, and rule severities.
	Project *project.Project
	// Tool identifies the rig version in the produced document.
	Tool string
	// IgnoreTables are never projected; migration bookkeeping lives here.
	IgnoreTables []string
	// Foundation names the rig_-prefixed tables this project's own migrations
	// show rig created, exposed or not.
	//
	// Deliberately not [Options.IgnoreTables]. Exposing a foundation table takes
	// it out of that list while leaving it rig's, and `auth.own` empties the
	// list entirely — which is also exactly what a project that never scaffolded
	// looks like. [checkReserved] has to tell those two apart.
	Foundation []string
}

// Compile runs every stage after introspection.
//
// A document is always returned, even when validation failed. It is marked
// invalid and generators refuse to run against it, but it can still be
// inspected — which is what lets `rig sync` repair a project that does not yet
// validate. Refusing to produce anything would make a broken project harder to
// fix than it needs to be.
func Compile(raw ir.Schema, set *tableconf.Set, opt Options) (*ir.Document, diag.List) {
	var diags diag.List

	p := opt.Project
	n := p.Namer()
	cfg := p.Config

	schema, d := Normalize(raw, NormalizeOptions{
		IgnoreTables: append(Bookkeeping(p), opt.IgnoreTables...),
	})
	diags.Append(d)

	api, d := Project(schema, ProjectOptions{
		Name:           cfg.API.Name,
		Version:        cfg.API.Version,
		Description:    cfg.API.Description,
		BasePath:       cfg.API.BasePath,
		RevisionHeader: cfg.API.RevisionHeader,
		Namer:          n,
		Auth:           cfg.Auth.IR(),
		Files:          cfg.Files.IR(),
		Notifications:  cfg.Notifications.IR(),
		Presence:       cfg.Presence.IR(),
		Throttle:       cfg.Throttle.IR(),
		Cache:          cfg.Cache.IR(),
		Tracing:        cfg.Tracing.IR(cfg.Project.Name),
		Monitoring:     cfg.Monitoring.IR(cfg.Project.Name),
		Servers:        cfg.Servers.IR(),
		ServeOpenAPI:   cfg.API.OpenAPI.Serve,

		EmbeddedFoundation: !cfg.Migrations.Vendored(),
	})
	diags.Append(d)

	api, schema, d = ApplyConfig(api, schema, set, ConfigOptions{
		Namer:                 n,
		UnmentionedColumn:     p.Severity(cfg.Validate.UnmentionedColumn, diag.CodeUnmentionedColumn),
		Ignored:               opt.IgnoreTables,
		Notifications:         cfg.Notifications.IR(),
		Presence:              cfg.Presence.IR(),
		FileRestoreWindowDays: fileRestoreWindowDays(p),
		Cache:                 cfg.Cache.IR(),
	})
	diags.Append(d)

	api, d = Expand(api, ExpandOptions{
		Namer:        n,
		SearchMethod: string(cfg.API.SearchMethod),
		BasePath:     cfg.API.BasePath,
	})
	diags.Append(d)

	doc, d := Freeze(api, schema, Meta{Tool: opt.Tool, Permissions: cfg.API.Permissions})
	diags.Append(d)

	markFoundation(doc, p, opt.Foundation)

	diags.Append(checkReserved(doc, set, p, opt.Foundation))
	diags.Append(Validate(doc, set, p))

	// Validation runs after freezing, so the flag has to be refreshed with what
	// it found.
	doc.Valid = !diags.HasErrors()

	return doc, diags
}

// fileRestoreWindowDays is rig_file's restore window, or zero for a project
// that keeps no files.
//
// Zero rather than a default, because it is what tells the stages downstream
// that nothing here governs rig_file: a project with `auth.own` or an
// `auth.expose` naming it has an ordinary table by that name, and it answers to
// `restore_window_days` like any other.
func fileRestoreWindowDays(p *project.Project) int {
	if !p.Config.Files.Enabled {
		return 0
	}
	return p.Config.Files.RestoreWindowDays()
}

// namerOrDefault keeps the stages usable on their own, in tests and in tools
// that drive one of them directly.
func namerOrDefault(n *naming.Namer) *naming.Namer {
	if n != nil {
		return n
	}
	return naming.New(naming.Config{})
}

// markFoundation flags the resources over tables rig created.
//
// After freezing rather than during, because it changes nothing the later stages
// read — the stub writers are its only audience, and they run over the finished
// document. [ir.Resource.Foundation] says what it is for.
//
// Nothing is flagged under `auth.own`. Such a project has forked the migrations
// and maintains those tables itself, so they are its own to write rules about
// and its own to describe — which is the whole of what the flag turns off.
// [Options.Foundation] still names them, because a forked foundation and a
// project that never scaffolded look identical from the ignore list and
// [checkReserved] has to tell them apart.
func markFoundation(doc *ir.Document, p *project.Project, foundation []string) {
	if len(foundation) == 0 || p.Config.Auth.Own {
		return
	}
	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Storage != nil && slices.Contains(foundation, res.Storage.Table) {
			res.Foundation = true
		}
	}
}

package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/scaffold"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// migrateModule applies the sets. It is imported only into the file below, so a
// project that vendored its foundation never sees goose in its API package.
const migrateModule = "github.com/simonjanss/rig/migrate"

// foundationEmitter writes the migration wiring for a project whose modules
// carry their own schema.
//
// A type of its own for the reason [authEmitter] is: it reads a different part of
// the document, and every one of its methods is about the same call.
type foundationEmitter struct {
	doc *ir.Document
	cfg Options
}

// wanted is the parts this project's configuration asks for.
//
// Read off the document rather than passed in, because the document already says:
// an `auth:` block resolved into [ir.API.Auth], a `files:` block into
// [ir.API.Files], an inbox into [ir.API.Notifications]. The mode is the one fact
// that is not derivable from the schema, which is why it is the only thing
// [ir.API.EmbeddedFoundation] had to add.
func (e *foundationEmitter) wanted() scaffold.Wanted {
	api := e.doc.API
	return scaffold.Wanted{
		Auth:          api.Auth != nil,
		OAuth:         api.Auth != nil && api.Auth.OAuth != nil && len(api.Auth.OAuth.Providers) > 0,
		Files:         api.Files != nil && api.Files.Enabled,
		Notifications: api.Notifications != nil && api.Notifications.Enabled,
		Presence:      api.Presence != nil && api.Presence.Enabled,
	}
}

// sets are the module sets this project needs, in apply order.
func (e *foundationEmitter) sets() []setRef {
	var out []setRef
	for _, s := range scaffold.SetsFor(e.wanted().Parts()) {
		out = append(out, setRef{module: s.Module, importPath: scaffold.ImportPath(s), table: s.Table})
	}
	return out
}

// setRef is what the generated file needs to name a set: where to import it from,
// and what to call it in a comment.
type setRef struct {
	module     string
	importPath string
	table      string
}

// foundationFile is the migration wiring, and it exists only for a project whose
// modules carry their own schema.
//
// What it writes is one function, and the reason it is generated rather than
// documented is the order. rig's DDL never references a project's table while a
// project's routinely references rig's — a join table pointing at
// rig_notification, a file column pointing at rig_file — so rig's sets have to be
// applied first and the project's last. That is a fact about this project's
// configuration, known here and easy to get wrong by hand.
//
// It joins the package the rest of the API is generated into, so there is no
// second package for a project to import and keep in step. And it is the only
// file in that package that names rig/migrate: a project that vendored its
// foundation gets no file at all, which is what keeps its API package — and its
// module — free of goose. Same rule as auth.gen.go.
func (e *foundationEmitter) foundationFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	migrate := b.Import(migrateModule)

	sets := e.sets()
	imports := make([]string, len(sets))
	for i, s := range sets {
		imports[i] = b.Import(s.importPath)
	}

	b.Comment("MigrationSources is every migration set this project needs, in the order " +
		"they must be applied.\n\n" +
		"rig's own tables are carried by the modules that own them — this project's " +
		"`migrations.foundation` is `embedded` — so each of those sets is applied from " +
		"the module and recorded in a bookkeeping table of its own. Separate tables are " +
		"what lets those modules be upgraded independently: each is numbered from one, " +
		"and a shared table would make two of them collide on a version.\n\n" +
		"The project's own set is the argument, and it goes last. That is the whole " +
		"ordering rule, and the reason this function exists rather than a paragraph in " +
		"the documentation: rig's DDL never references a table this project created, " +
		"while this project's routinely references rig's — a join table pointing at " +
		"rig_notification, a file column pointing at rig_file.\n\n" +
		"The project's own set is an argument rather than generated because it is the " +
		"project's to describe: the filesystem is its embed directive, and the directory " +
		"and bookkeeping table are its rig.yaml. Name both on the Source. Dir and Table " +
		"are per-set facts, so UpAll and RequireAll read them from each Source and ignore " +
		"the ones on migrate.Options — a project that changed `migrations.dir` or " +
		"`migrations.table` and set them there instead would record its own set somewhere " +
		"`rig db up` never looks.\n\n" +
		"\tsrcs := api.MigrationSources(migrate.Source{\n" +
		"\t\tName:  \"myapp\",\n" +
		"\t\tFS:    migrations,\n" +
		"\t\tDir:   \"migrations\",     // migrations.dir\n" +
		"\t\tTable: \"rig_migrations\", // migrations.table\n" +
		"\t})\n" +
		"\tserve.Config{Migrate: migrate.RequireAll(srcs, migrate.Options{})}")
	b.L("func MigrationSources(project %s.Source) []%s.Source {", migrate, migrate)
	b.L("return []%s.Source{", migrate)
	for i, s := range sets {
		b.L("{Name: %q, FS: %s.Set().FS, Dir: %s.Set().Dir, Table: %s.Table},",
			s.module, imports[i], imports[i], imports[i])
	}
	b.L("project,")
	b.L("}")
	b.L("}")

	return artifact("foundation.gen.go", b)
}

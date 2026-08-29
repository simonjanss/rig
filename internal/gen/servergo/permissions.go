package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// permissionsFile emits the catalogue every endpoint's check draws from.
//
// It exists so that the database can be reconciled against the code without
// anybody maintaining a list. The auth package cannot derive this — it is a
// library that knows nothing about your tables — so the derived set is handed to
// it, and the one place it comes from is the frozen document.
func (e *emitter) permissionsFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)
	tenPkg := b.Import(runtimeModule + "/tenancy")

	b.Comment("Permissions is every permission this API's endpoints require.\n\n" +
		"Hand it to the authentication foundation at startup and it reconciles the\n" +
		"permission table against it: a key this API checks and the database does\n" +
		"not have is one no role can hold, which denies every caller — so the\n" +
		"missing ones are written. A row the database has and this API does not\n" +
		"check is reported and never deleted: it grants nothing, and removing it\n" +
		"would take live grants with it.\n\n" +
		"The names and descriptions come from the tables' own comments, so the\n" +
		"sentence somebody reads while handing out a permission is the one that was\n" +
		"already written about the table.")
	b.L("func Permissions() []%s.Permission {", tenPkg)
	b.L("return []%s.Permission{", tenPkg)
	for _, p := range e.doc.API.Permissions {
		b.L("{Key: %s, Name: %s, Description: %s},",
			gobuf.Quote(p.Key), gobuf.Quote(p.Name), gobuf.Quote(p.Description))
	}
	b.L("}")
	b.L("}")
	b.NL()

	b.Comment("PermissionKeys is [Permissions] as the keys alone.\n\n" +
		"It is what a role grant wants: the set an owner should hold is every\n" +
		"permission this API checks, and deriving it beats a list somebody\n" +
		"maintains — a hand-written one goes stale the moment a table is added, and\n" +
		"the owner of a brand new tenant silently cannot touch it.")
	b.L("func PermissionKeys() []string {")
	b.L("out := make([]string, 0, %d)", len(e.doc.API.Permissions))
	b.L("for _, p := range Permissions() {")
	b.L("out = append(out, p.Key)")
	b.L("}")
	b.L("return out")
	b.L("}")
	b.NL()

	return artifact("permissions.gen.go", b)
}

// hasPermissions reports whether anything requires one.
//
// A project whose every endpoint is public gets no catalogue rather than an empty
// function nobody calls.
func (e *emitter) hasPermissions() bool { return len(e.doc.API.Permissions) > 0 }

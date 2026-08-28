package modelgo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// aliasFile is what a table rig owns gets instead of a model.
//
// One file rather than the entity/input pair, because there is nothing to lay
// out: every declaration is a name pointing at
// [github.com/simonjanss/rig/notify/notifymodel]. An alias is not a wrapper —
// `model.Notification` and `notifymodel.Notification` are the same type — which
// is what lets a repository generated here satisfy an interface declared there,
// and what keeps `services/` compiling against `model.X` without knowing any of
// this happened.
//
// The query file is still written beside this one. That is the whole of what
// stays: a project's own table pointing at rig_notification puts a member on
// NotificationFilter, so the filter grammar is this project's even though the
// row is not.
func (e *emitter) aliasFile(res *ir.Resource, importPath string) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)
	pkg := b.Import(importPath)
	t := e.table(res)
	n := naming.New(naming.Config{})

	b.Comment(res.Description + "\n\n" +
		"rig created this table, so the type is rig's: this is an alias for " +
		"[" + importPath + "." + res.Name + "], not a second struct of the same " +
		"shape. What is generated here is the filter grammar beside it, because " +
		"that one carries a member per table of this project's that points at it.")
	b.L("type %s = %s.%s", res.Name, pkg, res.Name)
	b.NL()

	b.Comment("The write inputs, their per-field failures, and the validators a " +
		"hook is handed. Aliases for the same reason the row is.")
	b.L("type (")
	for _, suffix := range []string{
		"CreateInput", "CreateInputError", "CreateValidator",
		"UpdateInput", "UpdateInputError", "UpdateValidator",
		"DeleteInput", "ValidatorContext",
	} {
		b.L("%s%s = %s.%s%s", res.Name, suffix, pkg, res.Name, suffix)
	}
	b.L(")")
	b.NL()

	b.Comment("Table" + res.Name + " is the table this entity is stored in.")
	b.L("const Table%s = %s.Table%s", res.Name, pkg, res.Name)
	b.NL()

	b.Comment("Column names for " + res.Storage.Table + ", so nothing has to spell one out.")
	b.L("const (")
	for i := range t.Columns {
		name := res.Name + n.Go(t.Columns[i].Name)
		b.L("Column%s = %s.Column%s", name, pkg, name)
	}
	b.L(")")
	b.NL()

	b.Comment(res.Name + "Columns is every column, in the order the row is scanned.")
	b.L("var %sColumns = %s.%sColumns", res.Name, pkg, res.Name)
	b.NL()

	return e.artifact(naming.Snake(res.Name)+".gen.go", b)
}

// enumAliasFile is [aliasFile] for a Postgres enum rig's own schema declared.
//
// The values come with the type: they are constants of it, so naming them here
// is what keeps `model.NotificationStatePending` writable in a service that has
// never heard of the module it came from.
func (e *emitter) enumAliasFile(enum ir.Enum, importPath string) (gen.Artifact, error) {
	b := gobuf.New(e.pkg)
	pkg := b.Import(importPath)

	b.Comment(enum.Description + "\n\n" +
		"An alias for [" + importPath + "." + enum.Name + "]: rig's schema " +
		"declared the type, so rig declares the Go for it.")
	b.L("type %s = %s.%s", enum.Name, pkg, enum.Name)
	b.NL()

	b.Comment("The values of " + enum.Name + ".")
	b.L("const (")
	for _, v := range enum.Values {
		b.L("%s%s = %s.%s%s", enum.Name, v.Name, pkg, enum.Name, v.Name)
	}
	b.L(")")
	b.NL()

	b.Comment("All" + enum.Name + " is every value, in declaration order.")
	b.L("var All%s = %s.All%s", enum.Name, pkg, enum.Name)
	b.NL()

	return e.artifact(naming.Snake(enum.Name)+".gen.go", b)
}

package electricgo

import (
	"path/filepath"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// stubName is the file stem and the function a shape's stub declares.
//
// The live shape keeps the unsuffixed pair it has always had, so a project that
// regenerates after the trash and history shapes existed finds two new files
// and its own untouched.
func stubName(sh shape) (file, fn string) {
	switch sh.kind {
	case shapeDeleted:
		return "_deleted_shape.go", "DeletedShape"
	case shapeVersions:
		return "_versions_shape.go", "VersionsShape"
	default:
		return "_shape.go", "Shape"
	}
}

// stubSubject is how a stub's doc comment refers to what it narrows.
func stubSubject(sh shape) string {
	switch sh.kind {
	case shapeDeleted:
		return "'s trash: the rows somebody retired"
	case shapeVersions:
		return "'s history: the copies taken before each update"
	default:
		return "'s live-sync subscription"
	}
}

// stubFile emits one shape's scoping function, as a starting point.
//
// It is written once and then belongs to the developer. What it starts as is a
// function that adds nothing, which is correct: a shape is already filtered to
// the caller's tenant and to one generation of its rows, and most tables need
// nothing more.
func (e *emitter) stubFile(res *ir.Resource, sh shape) (gen.Artifact, error) {
	table := res.Storage.Table
	dir := e.expand(e.cfg.StubDir, res)

	pkg := e.cfg.StubPackage
	if pkg == "" {
		pkg = naming.Snake(table)
	}

	b := gobuf.NewHandOwned(pkg)

	var (
		ctxPkg  = b.Import("context")
		httpPkg = b.Import("net/http")
		elecPkg = b.Import(runtimeModule + "/electric")
		tenPkg  = b.Import(runtimeModule + "/tenancy")
		shapes  = b.Import(e.cfg.ShapeImport)
	)

	suffix, fn := stubName(sh)

	doc := fn + " narrows " + res.Name + stubSubject(sh) + ".\n\n" +
		"The filter it receives already carries the tenant and the lifecycle " +
		"conditions, and every condition is joined with AND — so this can only " +
		"ever show a subscriber less, never more.\n\n" +
		"Add conditions through the Where methods rather than as text: they bind " +
		"their values, and a shape filter built by concatenation is an injection " +
		"point with a streaming response attached.\n\n"

	if sh.kind != shapeLive {
		doc += "Wiring this into Handlers takes the place of the live shape's scope, " +
			"which is what this route uses while the field is nil. Whatever that one " +
			"adds, add here too unless the reason it added it stops applying to " +
			"these rows — otherwise this shape shows more than the live one does.\n\n"
	}

	b.Comment(doc + "Unlike the .gen.go files, this one is yours: rig writes it once " +
		"and never touches it again.")

	if sh.kind == shapeVersions {
		uuidPkg := b.Import("github.com/google/uuid")
		b.L("func %s(ctx %s.Context, r *%s.Request, claims %s.Claims, id %s.UUID, p %s.%sShapeParams, w *%s.Where) error {",
			fn, ctxPkg, httpPkg, tenPkg, uuidPkg, shapes, res.Name, elecPkg)
	} else {
		b.L("func %s(ctx %s.Context, r *%s.Request, claims %s.Claims, p %s.%sShapeParams, w *%s.Where) error {",
			fn, ctxPkg, httpPkg, tenPkg, shapes, res.Name, elecPkg)
	}

	switch {
	case len(res.Electric.Params) > 0:
		p := res.Electric.Params[0]
		b.L("// For example, using the %s parameter this shape declares:", p.Name)
		if p.Optional {
			b.L("//")
			b.L("//\tif p.Has%s {", p.Field)
			b.L("//\t\tw.Eq(%q, fmt.Sprint(p.%s))", p.Name, p.Field)
			b.L("//\t}")
		} else {
			b.L("//")
			b.L("//\tw.Eq(%q, fmt.Sprint(p.%s))", p.Name, p.Field)
		}
	case sh.kind == shapeVersions:
		b.L("// Nothing to add. The shape is already one row's history, and id is that")
		b.L("// row — refuse here to keep somebody out of a history they may not read.")
		b.L("//")
		b.L("// Delete this function and leave the field nil to keep the live scope.")
	case sh.kind == shapeDeleted:
		b.L("// Nothing to add. Delete this function and leave the field nil to keep the")
		b.L("// live shape's scope on this route.")
	default:
		b.L("// Nothing to add. Delete this function and pass nil if it stays that way.")
	}

	b.L("return nil")
	b.L("}")
	b.NL()

	b.Comment(fn + " satisfies the generated signature. The check is here so that a " +
		"parameter added to the configuration becomes a compile error rather than " +
		"a value nobody reads.")
	b.L("var _ %s.%sScope = %s", shapes, sh.name, fn)
	b.NL()

	path := filepath.Join(e.root, filepath.FromSlash(dir), naming.Snake(table)+suffix)
	return artifact(path, b, gen.CreateOnce)
}

// expand fills the layout placeholders in a stub directory template.
func (e *emitter) expand(tmpl string, res *ir.Resource) string {
	table := res.Storage.Table
	return strings.NewReplacer(
		"{table}", table,
		"{Table}", e.namer.Go(table),
		"{tables}", naming.Snake(e.namer.Plural(table)),
	).Replace(tmpl)
}

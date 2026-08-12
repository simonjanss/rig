package electricgo

import (
	"path/filepath"
	"strings"

	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// stubFile emits the scoping function's starting point.
//
// It is written once and then belongs to the developer. What it starts as is a
// function that adds nothing, which is correct: a shape is already filtered to
// the caller's tenant and to live rows, and most tables need nothing more.
func (e *emitter) stubFile(res *ir.Resource) (gen.Artifact, error) {
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

	b.Comment("Shape narrows " + res.Name + "'s live-sync subscription.\n\n" +
		"The filter it receives already carries the tenant and the lifecycle " +
		"conditions, and every condition is joined with AND — so this can only " +
		"ever show a subscriber less, never more.\n\n" +
		"Add conditions through the Where methods rather than as text: they bind " +
		"their values, and a shape filter built by concatenation is an injection " +
		"point with a streaming response attached.\n\n" +
		"Unlike the .gen.go files, this one is yours: rig writes it once and never " +
		"touches it again.")
	b.L("func Shape(ctx %s.Context, r *%s.Request, claims %s.Claims, p %s.%sShapeParams, w *%s.Where) error {",
		ctxPkg, httpPkg, tenPkg, shapes, res.Name, elecPkg)

	if len(res.Electric.Params) == 0 {
		b.L("// Nothing to add. Delete this function and pass nil if it stays that way.")
	} else {
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
	}

	b.L("return nil")
	b.L("}")
	b.NL()

	b.Comment("Shape satisfies the generated signature. The check is here so that a " +
		"parameter added to the configuration becomes a compile error rather than " +
		"a value nobody reads.")
	b.L("var _ %s.%sScope = Shape", shapes, res.Name)
	b.NL()

	path := filepath.Join(e.root, filepath.FromSlash(dir), naming.Snake(table)+"_shape.go")
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

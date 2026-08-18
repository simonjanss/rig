package servicego

import (
	"path/filepath"
	"strings"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// stubFile emits the service layer's starting point.
//
// It is written once and then belongs to the developer. That is the whole
// arrangement: the generated interface says what the resource offers, the
// generated default already answers all of it, and this file is where a rule
// goes when one is needed.
func (e *emitter) stubFile(res *ir.Resource) (gen.Artifact, error) {
	if res.Storage == nil {
		return gen.Artifact{}, nil
	}

	table := res.Storage.Table
	dir := e.expand(e.cfg.StubDir, res)
	pkg := e.cfg.StubPackage
	if pkg == "" {
		pkg = naming.Snake(table)
	}

	b := gobuf.NewHandOwned(pkg)

	var (
		api   = b.Import(e.cfg.APIImport)
		store = b.Import(e.cfg.StoreImport)
	)

	// context is imported only if a method below takes one. A read-only
	// resource with no custom endpoints has nothing to override, and an unused
	// import would stop the file compiling the moment it was written.
	ctxPkg := func() string { return b.Import("context") }

	b.Comment("rules is " + res.Name + "'s business logic.\n\n" +
		"It describes itself and nothing else: the hooks it wants, the endpoints " +
		"the configuration declared, and the writer it is handed in return. " +
		"Nothing here mentions the service — that is what makes New one line.\n\n" +
		"A rule about a field goes in the validator, something that has to happen " +
		"with a write goes in a hook, and an endpoint rig cannot write is a method " +
		"here.\n\n" +
		"Unlike the .gen.go files, this one is yours: rig writes it once and never " +
		"touches it again.")
	b.L("type rules struct {")
	b.L("repo %s.%sRepository", store, res.Name)
	if res.Storage != nil {
		b.Comment("write performs a write with the hooks below already attached. " +
			"Use it rather than the repository: reaching for the repository means " +
			"passing the hooks by hand, and forgetting once is a second way into " +
			"the table where the rules do not run.")
		b.L("write %s.%sWriter", api, res.Name)
	}
	b.L("}")
	b.NL()

	b.Comment("rules satisfies what the constructor asks for. The check is here so " +
		"that a new endpoint in the configuration becomes a compile error rather " +
		"than a route that answers 501 at runtime.")
	b.L("var _ %s.%sRules = (*rules)(nil)", api, res.Name)
	b.NL()

	b.Comment("New builds the service.\n\n" +
		"To override a generated operation, wrap what this returns and shadow the " +
		"promoted method:\n\n" +
		"\ttype Service struct{ " + api + ".Default" + res.Name + "Service }\n" +
		"\tfunc (s *Service) Get(ctx context.Context, r " + api + ".Request[…]) (…) { … }\n\n" +
		"The custom endpoints keep working through the value inside it, so only " +
		"what you shadow changes.")
	b.L("func New(repo %s.%sRepository) %s.Default%sService {", store, res.Name, api, res.Name)
	b.L("return %s.New%sService(repo, &rules{repo: repo})", api, res.Name)
	b.L("}")
	b.NL()

	if res.Storage != nil {
		b.Comment("Bind receives the writer built from the hooks below. rig calls " +
			"it once, during construction.")
		b.L("func (s *rules) Bind(w %s.%sWriter) { s.write = w }", api, res.Name)
		b.NL()
	}

	e.stubContract(b, res, api)
	e.stubValidator(b, res, ctxPkg)
	e.customStubs(b, res, api, ctxPkg)

	path := filepath.Join(e.root, filepath.FromSlash(dir), naming.Snake(table)+".go")
	return artifact(path, b, gen.CreateOnce)
}

// exampleField is the field the generated example rule is written against.
//
// A textual one if there is one: "must not be blank" reads as a rule, where the
// same example on a timestamp reads as a puzzle.
func exampleField(res *ir.Resource) (ir.ResourceField, bool) {
	fields := genutil.WritableFields(res, ir.FieldOpCreate)
	for _, f := range fields {
		if f.IsTextual() && !f.IsNullable() {
			return f, true
		}
	}
	if len(fields) > 0 {
		return fields[0], true
	}
	return ir.ResourceField{}, false
}

// stubContract emits the one function that says what this service owes.
//
// Every field is listed, nil included. Go does not require it, and that is the
// point: a validator written as an empty literal says nothing about which
// fields could have had a rule, so the next person has to go and look. Spelled
// out, adding a column to the table shows up here as a field nobody filled in.
func (e *emitter) stubContract(b *gobuf.Buf, res *ir.Resource, api string) {
	var (
		hookPkg             = b.Import(runtimeModule + "/dbhook")
		model               = e.model(b)
		example, hasExample = exampleField(res)
	)

	b.Comment("Hooks is everything about " + res.Name + " that the schema cannot " +
		"describe, in the order it runs: the rules, then Before and After inside " +
		"the transaction — returning an error from either undoes the write — then " +
		"AfterCommit once it has landed, which is the only safe place to tell " +
		"anything outside the database.\n\n" +
		"The rules are one function per field, against the row the request would " +
		"produce. Two sets, because whether a row may exist is not whether it may " +
		"change: an update has no entry for a column it cannot touch, and a " +
		"create none for one it cannot set.\n\n" +
		"It is asked for rather than set, so there is no way to end up with a " +
		"service whose rules were never attached. An empty one is a fine answer; " +
		"it is just an answer.")
	b.L("func (s *rules) Hooks() %s.%sHooks {", api, res.Name)
	b.L("return %s.%sHooks{", api, res.Name)
	b.L("Read: %s.ReadHooks[%s.%sFilter, %s.%s]{", hookPkg, model, res.Name, model, res.Name)
	b.L("Narrow: nil,")
	b.L("Rows: nil,")
	b.L("},")
	e.stubHookSet(b, hookPkg, model, res, "Create", "CreateHooks", res.Name+"CreateInput",
		res.Name+"CreateValidator", genutil.WritableFields(res, ir.FieldOpCreate), example, hasExample)
	e.stubHookSet(b, hookPkg, model, res, "Update", "UpdateHooks", res.Name+"UpdateInput",
		res.Name+"UpdateValidator", genutil.WritableFields(res, ir.FieldOpUpdate), example, hasExample)
	e.stubHookSet(b, hookPkg, model, res, "Delete", "DeleteHooks", res.Name+"DeleteInput",
		"", nil, example, false)
	if res.Storage.IsSoftDeletable() {
		// The update validator, and the same value: the rules about what a live
		// row may look like do not depend on how it got there.
		e.stubHookSet(b, hookPkg, model, res, "Restore", "RestoreHooks", res.Name+"UpdateInput",
			res.Name+"UpdateValidator", genutil.WritableFields(res, ir.FieldOpUpdate), example, hasExample)
	}
	b.L("}")
	b.L("}")
	b.NL()
}

// stubHookSet spells out one operation: its rules, then its three callbacks.
//
// An empty validator name means the operation takes none, which is a delete:
// there is nothing about an identifier for a field rule to check.
func (e *emitter) stubHookSet(b *gobuf.Buf, hookPkg, model string, res *ir.Resource,
	field, kind, input, validator string, fields []ir.ResourceField,
	example ir.ResourceField, hasExample bool,
) {
	b.L("%s: %s.%s[%s.%s, %s.%s]{", field, hookPkg, kind, model, input, model, res.Name)

	if validator != "" {
		b.L("Validator: %s.%s{", model, validator)
		for _, f := range fields {
			if hasExample && f.Name == example.Name {
				b.L("%s: s.validate%s,", f.Name, f.Name)
				continue
			}
			b.L("%s: nil,", f.Name)
		}
		b.L("Entity: nil,")
		b.L("},")
	}

	b.L("Before: nil,")
	b.L("After: nil,")
	b.L("AfterCommit: nil,")
	b.L("},")
}

// stubValidator emits the example rule the constructor wires up.
func (e *emitter) stubValidator(b *gobuf.Buf, res *ir.Resource, ctxPkg func() string) {
	f, ok := exampleField(res)
	if !ok {
		return
	}

	model := e.model(b)
	valueType := genutil.GoType(b, f.Field, func() string { return model })

	b.Comment("validate" + f.Name + " is an example rule. Delete it, and set " +
		f.Name + " back to nil in business, if " + res.Name + " needs none.\n\n" +
		"A rule that needs something reaches it through s, which is why the " +
		"rules are methods.\n\n" +
		"Returning a FieldError attaches the message to " + f.Column.Name + ", " +
		"so the client is answered with a 422 whose body names the field rather " +
		"than a sentence it has to read. Returning any other error fails the " +
		"request as that error.")
	b.L("func (s *rules) validate%s(ctx %s.Context, c *%s.%sValidatorContext, value %s) error {",
		f.Name, ctxPkg(), model, res.Name, valueType)
	b.Comment("c.Values is the whole row as it will be, c.Previous() is how it " +
		"was, and c." + f.Name + "Changed() reports whether this request " +
		"touched it — which is how an expensive check is kept off every write.")
	b.L("return nil")
	b.L("}")
	b.NL()
}

// customStubs emit a stub per endpoint the configuration declared.
//
// They are methods on rules because the whole set is named as an interface the
// constructor asks for, so an endpoint added to the configuration and not
// written here is a build failure
// at the line that hands the set over.
func (e *emitter) customStubs(b *gobuf.Buf, res *ir.Resource, api string, ctxPkg func() string) {
	errPkg := ""

	for i := range res.Endpoints {
		ep := &res.Endpoints[i]
		if ep.Impl.Kind != ir.EndpointCustom {
			continue
		}
		if errPkg == "" {
			errPkg = b.Import(runtimeModule + "/rigerr")
		}

		if doc := endpointDoc(ep); doc != "" {
			b.Comment(doc)
		} else {
			b.Comment(ep.Name + " is declared in the table configuration.")
		}
		b.L("func (s *rules) %s {", e.methodSignatureQualified(b, res, ep, ctxPkg, api))

		if successBodyObject(ep) == "" {
			b.L("return %s.Internal(nil, \"%s.%s is not implemented yet\")", errPkg, res.Name, ep.Name)
		} else {
			b.L("return nil, %s.Internal(nil, \"%s.%s is not implemented yet\")", errPkg, res.Name, ep.Name)
		}
		b.L("}")
		b.NL()
	}
}

// methodSignatureQualified is the interface signature with the API package
// qualified, for a stub that lives in a different package.
func (e *emitter) methodSignatureQualified(b *gobuf.Buf, res *ir.Resource, ep *ir.Endpoint, ctxPkg func() string, api string) string {
	// A type that already names its package — one of the model's inputs — is
	// left alone; only this package's own types need the prefix.
	qualify := func(t string) string {
		if t == "struct{}" || strings.Contains(t, ".") {
			return t
		}
		return api + "." + t
	}

	request := api + ".Request[" +
		qualify(e.slotType(b, res, ep, "path")) + ", " +
		qualify(e.slotType(b, res, ep, "query")) + ", " +
		qualify(e.slotType(b, res, ep, "body")) + "]"

	ret := "error"
	if obj := successBodyObject(ep); obj != "" {
		ret = "(*" + qualify(e.objectType(b, res, obj)) + ", error)"
	}

	return ep.Impl.ServiceMethod + "(ctx " + ctxPkg() + ".Context, r " + request + ") " + ret
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

package modelgo

import (
	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/ir"
)

// columnConst is the constant naming a field's column.
//
// It is derived from the column, not the field: configuration can rename a
// field — manager_email becomes ManagerEmailAddress — and the constant keeps
// the database's name.
func columnConst(res *ir.Resource, f ir.ResourceField) string {
	return "Column" + res.Name + naming.New(naming.Config{}).Go(f.Column.Name)
}

// validatorContext emits what a service-supplied rule is handed.
//
// The complete intended state, what it was before, and which fields this
// request actually changed. A rule that can only see the fields somebody sent
// is a rule that cannot check a relationship between two of them.
func (e *emitter) validatorContext(b *gobuf.Buf, res *ir.Resource) {
	fields := genutil.WritableFields(res, ir.FieldOpUpdate)

	b.Comment(res.Name + "ValidatorContext is what a rule sees.\n\n" +
		"Values is the row as it will be if this goes through — merged from the " +
		"previous state on an update, so every field is set whether or not the " +
		"request mentioned it. That is the point: \"ends after starts\" cannot be " +
		"answered from a request that only carried one of them.")
	tenPkg := b.Import(genutil.RuntimeModule + "/tenancy")

	b.L("type %sValidatorContext struct {", res.Name)
	b.L("// Values is the intended end state.")
	b.L("Values %s", res.Name)
	b.NL()
	b.Comment("Claims are who is asking. They are a value rather than something " +
		"to fetch from the context because a rule that has to look them up is a " +
		"rule that can forget to, and because there is no case where they are " +
		"absent: a write without a caller is refused by the repository before " +
		"any rule runs.")
	b.L("Claims %s.Claims", tenPkg)
	b.NL()
	b.L("// previous is the row before this change, and is the zero value on a")
	b.L("// create — there was nothing before.")
	b.L("previous %s", res.Name)
	b.L("isUpdate bool")
	b.L("changed  map[string]bool")
	b.L("}")
	b.NL()

	b.Comment("IsUpdate reports whether there was a row before this.")
	b.L("func (c *%sValidatorContext) IsUpdate() bool { return c.isUpdate }", res.Name)
	b.NL()

	b.Comment("Previous is the row as it was, and the zero value on a create. Check " +
		"IsUpdate before reading it.")
	b.L("func (c *%sValidatorContext) Previous() %s { return c.previous }", res.Name, res.Name)
	b.NL()

	b.Comment("Changed reports whether this request carried a new value for a " +
		"column.\n\n" +
		"It is what keeps an expensive rule from running on every update: a check " +
		"that reaches another service to confirm a reference only needs to run " +
		"when the reference actually moved. On a create everything is changed, " +
		"because everything is new.")
	b.L("func (c *%sValidatorContext) Changed(column string) bool { return c.changed[column] }", res.Name)
	b.NL()

	// A method per field, so a rule reads as prose rather than as a lookup with
	// a string that could be misspelled.
	for _, f := range fields {
		b.Comment(f.Name + "Changed reports whether this request set " +
			f.Column.Name + ".")
		b.L("func (c *%sValidatorContext) %sChanged() bool { return c.changed[%s] }",
			res.Name, f.Name, columnConst(res, f))
		b.NL()
	}
}

// validators emit the per-field hooks a service fills in.
//
// Two types, not one. A create and an update are different questions — "may
// this exist" against "may this change" — and they are asked about different
// sets of fields, since an immutable column is creatable and not updatable.
// One shared type would mean a rule about a column an update cannot touch
// running on every update, against a value that could not have changed.
func (e *emitter) validators(b *gobuf.Buf, res *ir.Resource) {
	create := genutil.WritableFields(res, ir.FieldOpCreate)
	update := genutil.WritableFields(res, ir.FieldOpUpdate)

	e.validatorType(b, res, "Create", create,
		"the rules for bringing a "+res.Name+" into existence")
	e.runCreate(b, res, create)
	e.runHooks(b, res, "Create", create)

	e.validatorType(b, res, "Update", update,
		"the rules for changing one that already exists")
	e.runUpdate(b, res, update)
	e.runHooks(b, res, "Update", update)
}

// validatorType emits one of the two validators.
func (e *emitter) validatorType(b *gobuf.Buf, res *ir.Resource, op string, fields []ir.ResourceField, what string) {
	var (
		ctxPkg  = b.Import("context")
		ctxType = res.Name + "ValidatorContext"
	)

	b.Comment(res.Name + op + "Validator is " + what + ": what the schema " +
		"cannot express.\n\n" +
		"One optional function per field this operation can set, so the set of " +
		"fields is the set of rules that could apply — a column an update " +
		"cannot touch has no hook here to write by mistake. A nil one is " +
		"skipped. Every configured hook runs, because a rule that fails should " +
		"not hide the next one, so a request reports everything wrong with it.\n\n" +
		"A hook returns a FieldError to attach the message to a specific field, " +
		"or any other error to fail the request outright.")
	b.L("type %s%sValidator struct {", res.Name, op)
	for _, f := range fields {
		if f.Description != "" {
			b.Comment(f.Description)
		}
		b.L("%s func(ctx %s.Context, c *%s, value %s) error",
			f.Name, ctxPkg, ctxType, e.goType(b, f.Field))
	}
	b.NL()
	b.Comment("Entity runs after the per-field hooks, for a rule that is about " +
		"the row rather than about one column.")
	b.L("Entity func(ctx %s.Context, c *%s) error", ctxPkg, ctxType)
	b.L("}")
	b.NL()
}

// runCreate emits the entry point for validating a create.
func (e *emitter) runCreate(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	ctxPkg := b.Import("context")

	b.Comment("RunCreate implements [dbhook.CreateValidator]: it runs the " +
		"service's rules against the row this input would produce.\n\n" +
		"The generated checks are not repeated here. The repository runs " +
		"Normalize and Validate first, so by the time a hook sees the input it " +
		"is tidy and the schema is satisfied — which is what lets a hook be " +
		"about the business rather than about NOT NULL.")
	b.L("func (v %sCreateValidator) RunCreate(ctx %s.Context, claims %s.Claims, i *%sCreateInput) error {",
		res.Name, ctxPkg, b.Import(genutil.RuntimeModule+"/tenancy"), res.Name)

	b.Comment("Everything is new, so everything counts as changed.")
	b.L("c := &%sValidatorContext{Claims: claims, changed: map[string]bool{}}", res.Name)
	for _, f := range fields {
		b.L("c.Values.%s = i.%s", f.Name, f.Name)
		b.L("c.changed[%s] = true", columnConst(res, f))
	}
	b.NL()
	b.L("failed, err := v.run(ctx, c)")
	b.L("if err != nil { return err }")
	b.L("if failed != nil { return failed }")
	b.L("return nil")
	b.L("}")
	b.NL()
}

// runUpdate emits the entry point for validating an update.
func (e *emitter) runUpdate(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	ctxPkg := b.Import("context")

	b.Comment("RunUpdate implements [dbhook.UpdateValidator]: it runs the " +
		"service's rules against the row this update would produce, with the row " +
		"as it was available for a rule about the change itself.")
	b.L("func (v %sUpdateValidator) RunUpdate(ctx %s.Context, claims %s.Claims, i *%sUpdateInput, prev *%s) error {",
		res.Name, ctxPkg, b.Import(genutil.RuntimeModule+"/tenancy"), res.Name, res.Name)

	b.L("c := &%sValidatorContext{", res.Name)
	b.L("Values:   i.Merged(prev),")
	b.L("Claims:   claims,")
	b.L("previous: *prev,")
	b.L("isUpdate: true,")
	b.L("changed:  map[string]bool{},")
	b.L("}")
	for _, f := range fields {
		if f.IsNullable() {
			b.L("c.changed[%s] = i.%s.Touched()", columnConst(res, f), f.Name)
			continue
		}
		b.L("c.changed[%s] = i.%s.IsSet()", columnConst(res, f), f.Name)
	}
	b.NL()
	b.L("failed, err := v.run(ctx, c)")
	b.L("if err != nil { return err }")
	b.L("if failed != nil { return failed }")
	b.L("return nil")
	b.L("}")
	b.NL()

	e.runRestore(b, res, fields)
}

// runRestore emits the entry point for validating a restore.
//
// The same rules as an update, and the same input: a restore is a row
// re-entering the live set, possibly with changes, and what a live row may look
// like does not depend on how it got there.
//
// What differs is one line. On an update a rule may skip a field the request
// did not mention, because that value is already live and already passed. On a
// restore none of it is live, so every value is entering the set and every rule
// runs — which is what catches a unique value taken by something created while
// the row was retired.
func (e *emitter) runRestore(b *gobuf.Buf, res *ir.Resource, fields []ir.ResourceField) {
	if res.Storage == nil || !res.Storage.IsSoftDeletable() {
		return
	}
	ctxPkg := b.Import("context")

	b.Comment("RunRestore implements [dbhook.RestoreValidator]: it runs the " +
		"service's rules against the row this restore would bring back.\n\n" +
		"Every rule runs, not only the ones whose field the request mentioned. " +
		"The row was not live, so nothing about it has been checked against the " +
		"world it is returning to.")
	b.L("func (v %sUpdateValidator) RunRestore(ctx %s.Context, claims %s.Claims, i *%sUpdateInput, prev *%s) error {",
		res.Name, ctxPkg, b.Import(genutil.RuntimeModule+"/tenancy"), res.Name, res.Name)

	b.L("c := &%sValidatorContext{", res.Name)
	b.L("Values:   i.Merged(prev),")
	b.L("Claims:   claims,")
	b.L("previous: *prev,")
	b.L("isUpdate: true,")
	b.L("changed:  map[string]bool{},")
	b.L("}")
	for _, f := range fields {
		b.L("c.changed[%s] = true", columnConst(res, f))
	}
	b.NL()
	b.L("failed, err := v.run(ctx, c)")
	b.L("if err != nil { return err }")
	b.L("if failed != nil { return failed }")
	b.L("return nil")
	b.L("}")
	b.NL()
}

// runHooks emits the body that calls whatever was configured on one validator.
func (e *emitter) runHooks(b *gobuf.Buf, res *ir.Resource, op string, fields []ir.ResourceField) {
	var (
		ctxPkg = b.Import("context")
		errPkg = b.Import(genutil.RuntimeModule + "/rigerr")
		name   = res.Name + op + "InputError"
	)

	b.Comment("run calls every configured hook and puts what each one said under " +
		"the field it was about.\n\n" +
		"Two kinds of answer. A [rigerr.FieldError] is about the input: it " +
		"lands on the field and the others still run, so one request reports " +
		"everything wrong with it. Anything else is the rule itself failing — " +
		"a lookup that could not reach another service — and there is nothing " +
		"to tell the caller about their input, so it comes back wrapped with " +
		"the rule that could not be run, keeping whatever code it carried and " +
		"becoming Internal if it carried none.")
	b.L("func (v %s%sValidator) run(ctx %s.Context, c *%sValidatorContext) (*%s, error) {",
		res.Name, op, ctxPkg, res.Name, name)
	b.L("var failed %s", name)
	b.NL()

	for _, f := range fields {
		b.L("if v.%s != nil {", f.Name)
		b.L("if err := v.%s(ctx, c, c.Values.%s); err != nil {", f.Name, f.Name)
		b.L("field, ok := %s.AsFieldError(err)", errPkg)
		b.L("if !ok { return nil, %s.Wrap(err, \"validate %s\") }", errPkg, f.Column.Name)
		b.L("failed.%s = field", f.Name)
		b.L("}")
		b.L("}")
	}

	b.NL()
	b.L("if v.Entity != nil {")
	b.L("if err := v.Entity(ctx, c); err != nil {")
	b.L("field, ok := %s.AsFieldError(err)", errPkg)
	b.L("if !ok { return nil, %s.Wrap(err, \"validate %s\") }", errPkg, res.Storage.Table)
	b.L("failed.Entity = field")
	b.L("}")
	b.L("}")
	b.NL()

	b.L("if failed.Empty() { return nil, nil }")
	b.L("return &failed, nil")
	b.L("}")
	b.NL()
}

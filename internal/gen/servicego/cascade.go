package servicego

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// The delete propagation, from both ends.
//
// A child declares what should happen when one of its parents goes away; a
// parent runs whatever its children declared. Neither of them names the other:
// the parent's repository does not need the child's, it needs the closure, and
// the closure already carries the repository it closed over. That is what keeps
// this small — nothing has to be injected backwards, and the wiring is one
// generated function in the API package rather than an object graph the
// application assembles.
//
// The compiler settled both the set of edges and the order they run in, so
// nothing here decides anything: it renders what the document already says.

// hasParents reports whether this resource points at anything rig can call.
func hasParents(res *ir.Resource) bool { return len(res.Parents) > 0 }

// hasChildren reports whether anything rig can call points at this resource.
func hasChildren(res *ir.Resource) bool { return len(res.Children) > 0 }

// childDeletesAlias is the name of the ordered slice a parent adopts.
func childDeletesAlias(res *ir.Resource) string { return res.Name + "ChildDeletes" }

// parentHooksStruct emits what a service says about its own parents.
//
// Two fields per foreign key rather than one, because the two halves can fail in
// different ways and only one of them may: Deleting runs inside the transaction
// and can refuse the delete, Deleted runs after it commits and cannot. Merging
// them into one callback with a flag would put a hook that must not fail and a
// hook that exists to be able to fail behind the same signature.
func (e *emitter) parentHooksStruct(b *gobuf.Buf, res *ir.Resource) {
	if !hasParents(res) {
		return
	}
	var (
		ctxPkg = b.Import("context")
		tenPkg = b.Import(runtimeModule + "/tenancy")
		model  = e.model(b)
	)

	name := res.Name + "ParentHooks"
	b.Comment(name + " is what " + res.Name + " does when a row it points at is " +
		"deleted.\n\n" +
		"One pair per foreign key. `<Parent>Deleting` runs inside the transaction " +
		"doing the delete, before that row is touched, and returning an error " +
		"refuses it and unwinds everything the children before it already did. " +
		"`<Parent>Deleted` runs once that transaction has committed, in the same " +
		"order, and returns nothing — the row is gone, so this is where the cache " +
		"eviction, the search index and the mail belong.\n\n" +
		"Every field is optional and nil does nothing, which is the default and " +
		"stays supported: a table that declares none behaves exactly as it did " +
		"before any of this existed — the foreign key refuses, and the 23503 " +
		"becomes a 409.\n\n" +
		"What a configuration key would have spelled `set_null`, `cascade` and " +
		"`restrict` are three obvious bodies here: an update that nulls the column, " +
		"a loop calling this resource's own Delete, and a returned error. The " +
		"fourth case — \"delete the drafts and reassign the published ones\" — is " +
		"the same function with an if in it, and it is the case every vocabulary of " +
		"four keywords runs out on.\n\n" +
		"Two things about the arguments. The whole parent row arrives and not this " +
		"table's own rows, so it is one call per relation rather than one per row: " +
		"nulling ten thousand links is one UPDATE written here, and the loop that " +
		"gets each row's hooks and snapshots instead costs a statement each — which " +
		"is the correct price for what it buys, and the two versions do not look " +
		"different. And the delete input is passed because Hard is the difference " +
		"between a soft delete the parent can undo and a permanent one: a body that " +
		"nulls a link on a soft delete has destroyed the only record of what to " +
		"re-link on a restore.")
	b.L("type %s struct {", name)

	for i, p := range res.Parents {
		if i > 0 {
			b.NL()
		}
		parent := e.parentEntity(b, p)

		b.Comment("The " + p.Parent + " that " + p.Column + " points at is going, " +
			"and has not gone yet.")
		b.L("%sDeleting func(ctx %s.Context, claims %s.Claims, parent *%s, in %s.%sDeleteInput) error",
			p.Name, ctxPkg, tenPkg, parent, model, p.Parent)
		b.Comment("It has gone, and the transaction that took it has committed.")
		b.L("%sDeleted func(ctx %s.Context, claims %s.Claims, parent *%s, in %s.%sDeleteInput)",
			p.Name, ctxPkg, tenPkg, parent, model, p.Parent)
	}

	b.L("}")
	b.NL()
}

// parentEntity is the model type of a parent resource.
func (e *emitter) parentEntity(b *gobuf.Buf, p ir.ParentLink) string {
	return e.model(b) + "." + p.Parent
}

// childDeletesType emits the ordered list a parent runs, as an alias.
//
// An alias rather than a defined type so that the slice a caller builds is the
// slice dbhook takes: this names a shape to make the signatures readable, and a
// new type would make them require a conversion for nothing.
func (e *emitter) childDeletesType(b *gobuf.Buf, res *ir.Resource) {
	if !hasChildren(res) {
		return
	}
	var (
		hookPkg = b.Import(runtimeModule + "/dbhook")
		model   = e.model(b)
	)

	b.Comment(childDeletesAlias(res) + " is what the tables referencing " + res.Name +
		" want to happen when one is deleted, in the order they are told.\n\n" +
		"The order is rig's and is derived from the schema: referencing tables " +
		"before referenced ones, which is the order the rows themselves would have " +
		"to go in. It does not matter for correctness — everything is in one " +
		"transaction, so a refusal unwinds everything before it — and it matters " +
		"for what one sibling can see of another and for which error a caller gets " +
		"when two of them would both refuse. `on_delete.order` overrides it.")
	b.L("type %s = []%s.ChildDelete[%s.%sDeleteInput, %s.%s]",
		childDeletesAlias(res), hookPkg, model, res.Name, model, res.Name)
	b.NL()
}

// adoptChildren emits the writer's end of the wiring.
//
// It is behind a pointer the constructor allocates, so that a service value
// already built — and already copied into an interface, which is how Handlers
// holds it — can still be given its children. The alternative is a registry
// parameter on every constructor, which would work and would change the
// signature of every service stub in every project that already exists.
func (e *emitter) adoptChildren(b *gobuf.Buf, res *ir.Resource, writer string) {
	if !hasChildren(res) {
		return
	}

	b.Comment("AdoptChildren receives the hooks of every table referencing " +
		res.Name + ", already in order.\n\n" +
		"[Link] calls it, and Register calls Link, so an ordinary server needs " +
		"nothing here. A program that builds services and serves them some other " +
		"way has to call Link itself: until it does, a delete runs this resource's " +
		"own hooks and none of its children's.")
	b.L("func (w %s) AdoptChildren(cs %s) { *w.children = cs }", writer, childDeletesAlias(res))
	b.NL()
}

// Link, the function that wires every edge, is server-go's: it reads Handlers,
// and Handlers is where the whole set of services is visible at once. The two
// generators write into the same package by construction — server-go's handlers
// call service-go's interfaces — so the halves meet without either importing the
// other.

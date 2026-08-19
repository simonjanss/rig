package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// linkFunc emits the one function that wires the delete propagation.
//
// It is here rather than in service-go because it reads [Handlers], and Handlers
// is the one place the whole set of services is visible at once. The two
// generators write into the same package by construction — a handler calls a
// service interface — so the child's end and the parent's end meet without
// either generator importing the other.
//
// Nothing in it decides anything. Which resources have children, which foreign
// key fills which hook, and what order the children run in are all facts the
// compiler derived; this renders them.
func (e *emitter) linkFunc(b *gobuf.Buf) {
	parents := e.cascadeParents()

	b.Comment("Link hands every resource the hooks of the tables that reference " +
		"it, so a delete reaches them.\n\n" +
		"Register calls it, which is all an ordinary server needs. It is exported " +
		"for the program that builds services and does not call Register — a batch " +
		"job, a test that drives the service layer directly — because a delete " +
		"that silently skipped every child is the same delete with different data " +
		"left behind.\n\n" +
		"Calling it twice is calling it once: each list is replaced rather than " +
		"appended to. A resource whose field is nil is skipped, and a parent whose " +
		"children are all nil adopts an empty list, which is what it had before.")
	b.L("func Link(h Handlers) {")

	if len(parents) == 0 {
		b.Comment("No table here references another one rig writes a service for, " +
			"so there is nothing to propagate.")
		b.L("_ = h")
		b.L("}")
		b.NL()
		return
	}

	var (
		hookPkg = b.Import(runtimeModule + "/dbhook")
		model   = e.model(b)
	)

	for _, res := range parents {
		b.L("if h.%s != nil {", res.Name)
		b.L("var cs %sChildDeletes", res.Name)
		for _, c := range res.Children {
			b.L("if h.%s != nil {", c.Child)
			b.L("p := h.%s.ParentHooks()", c.Child)
			b.L("cs = append(cs, %s.ChildDelete[%s.%sDeleteInput, %s.%s]{",
				hookPkg, model, res.Name, model, res.Name)
			b.L("Child: %s,", gobuf.Quote(c.Table))
			b.L("Deleting: p.%sDeleting,", c.Hook)
			b.L("Deleted: p.%sDeleted,", c.Hook)
			b.L("})")
			b.L("}")
		}
		b.L("h.%s.AdoptChildren(cs)", res.Name)
		b.L("}")
		b.NL()
	}

	b.L("}")
	b.NL()
}

// cascadeParents are the resources something else points at, in document order.
func (e *emitter) cascadeParents() []*ir.Resource {
	var out []*ir.Resource
	for _, res := range e.resources() {
		if len(res.Children) > 0 {
			out = append(out, res)
		}
	}
	return out
}

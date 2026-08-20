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

	// Imported where the first entry needs them rather than up front: a project
	// whose only child is its own notifications names neither type, and an
	// import nothing uses does not compile.
	for _, res := range parents {
		b.L("if h.%s != nil {", res.Name)
		b.L("var cs %sChildDeletes", res.Name)
		e.notifyChild(b, res)
		for _, c := range res.Children {
			b.L("if h.%s != nil {", c.Child)
			b.L("p := h.%s.ParentHooks()", c.Child)
			b.L("cs = append(cs, %s.ChildDelete[%s.%sDeleteInput, %s.%s]{",
				b.Import(runtimeModule+"/dbhook"), e.model(b), res.Name, e.model(b), res.Name)
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

// cascadeParents are the resources whose delete has to reach something: a table
// that points at them, or their own notifications.
//
// The second is why this is not simply "resources with children". Notifications
// about a row are a child of that row in every way that matters here — they
// point at it, they have to be told, and the telling is inside the same
// transaction — and treating them as one is what lets the propagation ride a
// mechanism that already exists instead of being a second one beside it.
func (e *emitter) cascadeParents() []*ir.Resource {
	var out []*ir.Resource
	for _, res := range e.resources() {
		if len(res.Children) > 0 || (e.hasNotifications() && res.Notifiable) {
			out = append(out, res)
		}
	}
	return out
}

// notifyChild emits the notification entry, first in the list.
//
// First because it is rig's, and a project's own hook that refuses the delete
// should refuse it after the bookkeeping rather than leaving the bookkeeping to
// a rollback. Both orders are correct — everything is one transaction — and this
// one puts rig's work where a reader expects it.
func (e *emitter) notifyChild(b *gobuf.Buf, res *ir.Resource) {
	if !e.hasNotifications() || !res.Notifiable || linkTableOf(res) == nil {
		return
	}
	b.Comment("The row's own notifications, which are a child of it in every way " +
		"that matters: they point at it, they have to be told, and the telling is " +
		"inside the same transaction.")
	b.L("if h.Notifications != nil {")
	b.L("cs = append(cs, notify%sDeletes(h.Notifications))", res.Name)
	b.L("}")
}

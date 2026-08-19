package servicego

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/ir"
)

// The module a project with a notifiable table imports. A project without one
// gets neither it nor the engine behind it.
const notifyModule = "github.com/simonjanss/rig/notify"

// notifiable reports whether notifications can be about this resource's rows.
func notifiable(res *ir.Resource) bool { return res.Notifiable }

// linkTableOf is the join a notifiable table declares itself through.
//
// Read off the relations rather than guessed from the resource name, because
// the join table is the declaration and its name is a recommendation: any link
// table one side of which is rig_notification makes the other side notifiable,
// and nothing should depend on what it was called.
func linkTableOf(res *ir.Resource) *ir.LinkTable {
	if res.Storage == nil {
		return nil
	}
	for i := range res.Storage.Relations {
		rel := &res.Storage.Relations[i]
		if rel.LinkTable == nil {
			continue
		}
		if rel.LinkTable.LeftTable == notificationTable || rel.LinkTable.RightTable == notificationTable {
			return rel.LinkTable
		}
	}
	return nil
}

// notificationTable is rig's own table, spelled here because the link is
// recognised by what it points at.
const notificationTable = "rig_notification"

// subjectColumn is this resource's own key in its link table.
func subjectColumn(res *ir.Resource, lt *ir.LinkTable) string {
	if lt.LeftTable == res.Storage.Table {
		return lt.LeftColumn
	}
	return lt.RightColumn
}

// notifyInterface emits the two questions rig asks a notifiable table.
//
// An interface of its own rather than two fields on the hooks struct, and for
// the reason the declared endpoints are one: both are required, so a value that
// could be left nil would move the failure from the constructor to the
// dispatcher — where it arrives as an audience of nobody, hours later, in a
// background job.
func (e *emitter) notifyInterface(b *gobuf.Buf, res *ir.Resource) {
	if !notifiable(res) {
		return
	}
	var (
		ctxPkg    = b.Import("context")
		timePkg   = b.Import("time")
		uuidPkg   = b.Import("github.com/google/uuid")
		notifyPkg = b.Import(notifyModule)
		model     = e.model(b)
	)

	b.Comment(res.Name + "Notify is what rig asks about notifications concerning a " +
		res.Name + ": when they are due, and who should hear about them.\n\n" +
		"Both are required, because this table declared itself notifiable by being " +
		"joined to rig_notification. A project that declares one and does not " +
		"answer these does not compile — not a default at runtime, a build failure, " +
		"which is the mechanism a declared endpoint already uses.")
	b.L("type %sNotify interface {", res.Name)

	b.Comment("NotifyAt says when notifications about a row are due, and whether " +
		"they are due at all.\n\n" +
		"Returning false cancels anything still pending about the row — a " +
		"publish_at that was cleared, a post put back to draft. The zero time with " +
		"true means now, which is the ordinary case and is not a special path: an " +
		"immediate notification and one scheduled for Friday differ by one column " +
		"and run the same code.\n\n" +
		"rig calls it after every create and every update, so a date that moves " +
		"takes its notifications with it and there is no hook to remember.")
	b.L("NotifyAt(row *%s.%s, kind string) (%s.Time, bool)", model, res.Name, timePkg)
	b.NL()

	b.Comment("NotifyWho answers, at the moment of sending, which accounts should " +
		"hear about a row.\n\n" +
		"It runs in the dispatcher rather than in a request, under System claims " +
		"for the row's own tenant, and that is what makes the answer current: an " +
		"account added to the group after the notification was written is in this " +
		"list, because this list is built now.\n\n" +
		"It must be a pure read, and it may be called more than once for the same " +
		"notification — a dispatcher that resolved and died before committing, two " +
		"replicas racing the same nudge. rig makes a repeat harmless with a unique " +
		"index on the inbox line; a method with side effects would make it visible.\n\n" +
		"One thing about those claims is surprising and is the one trap in writing " +
		"one of these. AccountID is the nil identifier, because there is no caller " +
		"— so an owner-scoped read inside this method returns nothing until it is " +
		"given readopt.WithoutOwnerScope(). It fails as an empty audience rather " +
		"than as an error, and an empty audience is the hardest bug in this system " +
		"to notice: a notification nobody was told about looks exactly like a " +
		"notification nobody was owed. The dispatcher counts them, which is the " +
		"only thing standing between that and a support ticket.")
	b.L("NotifyWho(ctx %s.Context, n *%s.Notification, row *%s.%s) ([]%s.UUID, error)",
		ctxPkg, notifyPkg, model, res.Name, uuidPkg)
	b.L("}")
	b.NL()
}

// notifySubject emits the adapter between the rules and the dispatcher.
//
// The dispatcher is a background job and NotifyWho is a method on a service, so
// something has to carry one to the other. It is the closure, and the closure
// already holds the service: nothing is injected backwards, which is the same
// answer the delete propagation gives to the same problem.
func (e *emitter) notifySubject(b *gobuf.Buf, res *ir.Resource) {
	lt := linkTableOf(res)
	if !notifiable(res) || lt == nil {
		return
	}
	var (
		ctxPkg     = b.Import("context")
		timePkg    = b.Import("time")
		uuidPkg    = b.Import("github.com/google/uuid")
		notifyPkg  = b.Import(notifyModule)
		readoptPkg = b.Import(runtimeModule + "/readopt")
		name       = res.Name + "Subject"
	)

	b.Comment(name + " is how the dispatcher reaches " + res.Name + "'s answers.\n\n" +
		"It is registered where the service is already wired, so adding a link " +
		"table and forgetting to register does not compile.")
	b.L("type %s struct {", name)
	b.L("svc Default%sService", res.Name)
	b.L("}")
	b.NL()

	b.Comment("New" + name + " adapts a service to the dispatcher's interface.")
	b.L("func New%s(svc Default%sService) %s { return %s{svc: svc} }", name, res.Name, name, name)
	b.NL()

	b.Comment("Table implements notify.Subjects.")
	b.L("func (s %s) Table() string { return %s }", name, gobuf.Quote(res.Storage.Table))
	b.NL()

	b.Comment("DueAt implements notify.Subjects.")
	b.L("func (s %s) DueAt(ctx %s.Context, id %s.UUID, kind string) (%s.Time, bool, error) {",
		name, ctxPkg, uuidPkg, timePkg)
	b.Comment("Without the owner narrowing, and deliberately. This runs without a " +
		"caller, so \"the caller's own rows\" is the empty set — and a row rig " +
		"cannot read is a notification rig cannot schedule.")
	b.L("row, err := s.svc.repo.Get(ctx, id, %s.WithoutOwnerScope())", readoptPkg)
	b.L("if err != nil { return %s.Time{}, false, err }", timePkg)
	b.L("at, due := s.svc.contract.Notify.NotifyAt(row, kind)")
	b.L("return at, due, nil")
	b.L("}")
	b.NL()

	b.Comment("Audience implements notify.Subjects.")
	b.L("func (s %s) Audience(ctx %s.Context, n *%s.Notification, id %s.UUID) ([]%s.UUID, error) {",
		name, ctxPkg, notifyPkg, uuidPkg, uuidPkg)
	b.L("row, err := s.svc.repo.Get(ctx, id, %s.WithoutOwnerScope())", readoptPkg)
	b.L("if err != nil { return nil, err }")
	b.L("return s.svc.contract.Notify.NotifyWho(ctx, n, row)")
	b.L("}")
	b.NL()

	e.notifySubjectHelper(b, res, lt)
}

// notifySubjectHelper emits the constructor an Announce call passes.
//
// The table names come from the compiled document and never from a request,
// which is what makes it safe for the notify module to build a statement around
// them — the same bargain the file owner makes.
func (e *emitter) notifySubjectHelper(b *gobuf.Buf, res *ir.Resource, lt *ir.LinkTable) {
	var (
		uuidPkg   = b.Import("github.com/google/uuid")
		notifyPkg = b.Import(notifyModule)
	)

	b.Comment("NotifyAbout" + res.Name + " names the row an announcement is about.\n\n" +
		"Use it rather than building the value by hand: the table and the join " +
		"are written here from the schema, so nothing a request carries reaches a " +
		"statement.")
	b.L("func NotifyAbout%s(id %s.UUID) %s.Subject {", res.Name, uuidPkg, notifyPkg)
	b.L("return %s.Subject{", notifyPkg)
	b.L("Table: %s,", gobuf.Quote(res.Storage.Table))
	b.L("LinkTable: %s,", gobuf.Quote(lt.Table))
	b.L("Column: %s,", gobuf.Quote(subjectColumn(res, lt)))
	b.L("ID: id,")
	b.L("}")
	b.L("}")
	b.NL()
}

// notifyStub writes the two bodies a fresh project starts from.
//
// They compile and they do nothing useful, which is the right starting point:
// the alternative is a project that does not build until somebody has read two
// doc comments, and the alternative to *that* is a default audience rig invented.
func (e *emitter) notifyStub(b *gobuf.Buf, res *ir.Resource, ctxPkg func() string) {
	if !notifiable(res) {
		return
	}
	var (
		timePkg = b.Import("time")
		uuidPkg = b.Import("github.com/google/uuid")
		model   = e.model(b)
	)

	b.Comment("NotifyAt says when notifications about this row are due.\n\n" +
		"The zero time means now. Return false to cancel anything still pending " +
		"about the row — which is what a cleared publish_at should do, and it is " +
		"why this is asked on every update rather than once.")
	b.L("func (s *rules) NotifyAt(row *%s.%s, kind string) (%s.Time, bool) {",
		model, res.Name, timePkg)
	b.L("return %s.Time{}, true", timePkg)
	b.L("}")
	b.NL()

	b.Comment("NotifyWho answers who should hear about this row, at the moment of " +
		"sending.\n\n" +
		"Returning nothing means nobody is told, which is a legal answer and an " +
		"invisible mistake: the dispatcher counts these, and the count is the only " +
		"thing that distinguishes \"nobody was owed this\" from \"the read came " +
		"back empty\".\n\n" +
		"The trap: this runs under System claims with no account, so a read of an " +
		"owner-scoped table returns nothing here until it is given " +
		"readopt.WithoutOwnerScope().")
	b.L("func (s *rules) NotifyWho(ctx %s.Context, n *%s.Notification, row *%s.%s) ([]%s.UUID, error) {",
		ctxPkg(), b.Import(notifyModule), model, res.Name, uuidPkg)
	b.L("return nil, nil")
	b.L("}")
	b.NL()

}

// notifyArg is the contract field a notifiable resource's front door fills in.
func notifyArg(res *ir.Resource) string {
	if !notifiable(res) {
		return ""
	}
	return ", Notify: rules"
}

package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The two modules a project with an inbox imports. Neither appears in a project
// without one, which is what keeps an application that serves a list of chores
// free of a dispatcher it never runs.
const (
	notifyModule     = "github.com/simonjanss/rig/notify"
	notifyhttpModule = "github.com/simonjanss/rig/notify/notifyhttp"
)

// hasNotifications reports whether this project has an inbox at all.
func (e *emitter) hasNotifications() bool {
	n := e.doc.API.Notifications
	return n != nil && n.Enabled
}

// notifiableResources are the tables notifications can be about, in document
// order.
func (e *emitter) notifiableResources() []*ir.Resource {
	var out []*ir.Resource
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		if res.Notifiable && !res.Unexposed {
			out = append(out, res)
		}
	}
	return out
}

// linkTableOf is the join a notifiable table declares itself through.
func linkTableOf(res *ir.Resource) *ir.LinkTable {
	if res.Storage == nil {
		return nil
	}
	for i := range res.Storage.Relations {
		if lt := res.Storage.Relations[i].LinkTable; lt != nil {
			if lt.LeftTable == notificationTable || lt.RightTable == notificationTable {
				return lt
			}
		}
	}
	return nil
}

// notificationTable is rig's own table, spelled here because a link is
// recognised by what it points at rather than by what it is called.
const notificationTable = "rig_notification"

// subjectColumn is a resource's own key in its link table.
func subjectColumn(res *ir.Resource, lt *ir.LinkTable) string {
	if lt.LeftTable == res.Storage.Table {
		return lt.LeftColumn
	}
	return lt.RightColumn
}

// notificationsFile emits the wiring for a project with an inbox.
//
// It is written here rather than in the notify module for the reason the file
// wiring is written here: it names the project's own tables. Everything that
// decides anything is in the module; this is the part that could not live there.
func (e *emitter) notificationsFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.notifyConstructor(b)
	e.notifyStarter(b)
	e.notifyLinks(b)
	e.notifyDispatcher(b)
	e.notifyPropagation(b)

	return artifact("notifications.gen.go", b)
}

// notifyConstructor emits the service and the engine.
func (e *emitter) notifyConstructor(b *gobuf.Buf) {
	notifyPkg := b.Import(notifyModule)

	b.Comment("NewNotifications builds this project's inbox from its " +
		"`notifications:` block.\n\n" +
		"The pool is the same one the repositories use, and that is not a detail: " +
		"an announcement is written in the transaction that caused it, and it " +
		"cannot be if the two live in different pools.\n\n" +
		"The registry is how the dispatcher reaches each notifiable table's " +
		"answers. It is a parameter rather than something assembled here because " +
		"those answers are methods on services, and a service is built where the " +
		"application decides — which is also why a project with notifications has " +
		"a constructor its server and its task both call, rather than building " +
		"services inside the mount closure.")
	b.L("func NewNotifications(db %s.DB, reg *%s.Registry) *%s.Service {",
		notifyPkg, notifyPkg, notifyPkg)
	b.L("return %s.NewService(%s.Config{DB: db, Registry: reg})", notifyPkg, notifyPkg)
	b.L("}")
	b.NL()

	b.Comment("NewNotificationEngine builds the dispatcher: the thing that takes " +
		"notifications whose time has come, asks who should hear about them, and " +
		"writes the inbox lines.\n\n" +
		"Start it and register its two shutdown steps beside the line that builds " +
		"it:\n\n" +
		"\tengine := api.NewNotificationEngine(app.Pool, reg, senders, app.Logger)\n" +
		"\tengine.Start()\n" +
		"\tapp.Drain(\"notifications\", engine.StopClaiming)\n" +
		"\tapp.CloseWithin(\"notifications\", 15*time.Second, engine.Close)\n\n" +
		"Draining stops it taking new work while the server is still answering, " +
		"which is the right order: the requests in flight are the last ones whose " +
		"commits will nudge it, and the time left is better spent finishing than " +
		"starting. Closing runs before the pool does, because what is in flight is " +
		"a write.")
	timePkg := b.Import("time")
	cfg := e.doc.API.Notifications

	b.L("func NewNotificationEngine(db %s.DB, reg *%s.Registry, senders map[%s.Channel]%s.Sender, logger *%s.Logger) *%s.Engine {",
		notifyPkg, notifyPkg, notifyPkg, notifyPkg, b.Import("log/slog"), notifyPkg)
	b.L("return %s.NewEngine(%s.EngineConfig{", notifyPkg, notifyPkg)
	b.L("Config: %s.Config{DB: db, Registry: reg},", notifyPkg)
	b.L("Links: NotificationLinks(),")
	b.Comment("app.Logger, so a pass says what it did wherever the server says " +
		"everything else. Nil is not silence — it is slog.Default, which is the " +
		"right answer for the cron task and the wrong one for a project with a " +
		"log file, because the file is teed into what the server was handed and " +
		"not into the default.")
	b.L("Logger: logger,")
	b.Comment("Every number here came from the `notifications:` block, so a " +
		"claim lease is a line in a file the documentation can quote rather " +
		"than a literal in a main function nobody diffs.")
	b.L("Senders: senders,")
	b.L("ClaimTTL: %d * %s.Second,", cfg.ClaimTTLSeconds, timePkg)
	b.L("SendTimeout: %d * %s.Second,", cfg.SendTimeoutSeconds, timePkg)
	b.L("MaxAttempts: %d,", cfg.MaxAttempts)
	b.L("BackoffBase: %d * %s.Second,", cfg.BackoffBaseSeconds, timePkg)
	b.L("BackoffCap: %d * %s.Second,", cfg.BackoffCapSeconds, timePkg)
	b.L("DefaultDigest: %s.Digest(%s),", notifyPkg, gobuf.Quote(cfg.DefaultDigest))
	b.L("})")
	b.L("}")
	b.NL()
}

// notifyLinks emits the join tables the dispatcher walks.
//
// One entry per notifiable table, written from the compiled document — so no
// table name the engine builds a statement around ever came from a request.
func (e *emitter) notifyLinks(b *gobuf.Buf) {
	notifyPkg := b.Import(notifyModule)

	b.Comment("NotificationLinks are the join tables that say which row a " +
		"notification is about.\n\n" +
		"Derived from the schema rather than declared: any link table one side of " +
		"which is rig_notification makes the other side notifiable, so the " +
		"recommended name is a recommendation and nothing depends on it.")
	b.L("func NotificationLinks() []%s.Subject {", notifyPkg)

	resources := e.notifiableResources()
	if len(resources) == 0 {
		b.Comment("Nothing in this schema is joined to rig_notification, so the " +
			"inbox is there and nothing fills it yet.")
		b.L("return nil")
		b.L("}")
		b.NL()
		return
	}

	b.L("return []%s.Subject{", notifyPkg)
	for _, res := range resources {
		lt := linkTableOf(res)
		if lt == nil {
			continue
		}
		b.L("{Table: %s, LinkTable: %s, Column: %s},",
			gobuf.Quote(res.Storage.Table), gobuf.Quote(lt.Table),
			gobuf.Quote(subjectColumn(res, lt)))
	}
	b.L("}")
	b.L("}")
	b.NL()
}

// notifyDispatcher emits the task an operator's cron invokes.
//
// The task is the guarantee and the in-process engine is latency. Say it in
// those words on the generated symbol, because the shape invites the opposite
// reading: nothing is lost when the nudge is skipped, since the row is still
// pending and this is still coming.
func (e *emitter) notifyDispatcher(b *gobuf.Buf) {
	var (
		ctxPkg    = b.Import("context")
		notifyPkg = b.Import(notifyModule)
		servePkg  = b.Import(runtimeModule + "/serve")
		poolPkg   = b.Import("github.com/jackc/pgx/v5/pgxpool")
	)

	b.Comment("NotificationDispatcher is the guarantee behind the inbox: it takes " +
		"everything the in-process engine did not — a process that died mid-pass, " +
		"a deliver_at in the future, a notification whose subject was slow to " +
		"answer.\n\n" +
		"A subcommand rather than a goroutine, so it is a cron job rather than " +
		"something racing itself in every replica. Register it in " +
		"serve.Config.Tasks and run `<binary> dispatch-notifications`.\n\n" +
		"The logger is where each pass's report goes — every count including " +
		"the zeros, because a pass that sent nothing is the ordinary case and " +
		"the absence of a line cannot be told from the job not running. A nil " +
		"logger is not silence: it is slog.Default, the same reading every " +
		"other Logger in rig gives it, which for a cron job is the terminal it " +
		"was started from.\n\n" +
		"At info, unlike the pair the in-process engine writes per interval. " +
		"This is not one line among thousands, it is the whole output of a run " +
		"that happens when nobody is watching, and slog.Default drops debug — " +
		"so a level that has to have been turned on in advance would make the " +
		"cron form silent, which is the one thing this line exists to prevent.")
	timePkg := b.Import("time")
	slogPkg := b.Import("log/slog")
	retention := e.doc.API.Notifications.RetentionSeconds

	b.L("func NotificationDispatcher(engine *%s.Engine, logger *%s.Logger) %s.Task {",
		notifyPkg, slogPkg, servePkg)
	b.L("if logger == nil { logger = %s.Default() }", slogPkg)
	b.L("return func(ctx %s.Context, _ *%s.Pool) error {", ctxPkg, poolPkg)
	b.L("resolved, err := engine.Resolve(ctx)")
	b.L(`logger.InfoContext(ctx, "notifications resolved", "counts", resolved.String())`)
	b.L("if err != nil { return err }")
	b.L("dispatched, err := engine.Dispatch(ctx)")
	b.L(`logger.InfoContext(ctx, "notifications dispatched", "counts", dispatched.String())`)
	b.L("if err != nil { return err }")
	b.Comment("And the housekeeping, in the same task for the reason the file " +
		"sweeper's two rules share one: a schema that grows forever is the " +
		"state every other table in rig is already in, and this milestone " +
		"added three more.")
	b.L("pruned, err := engine.Prune(ctx, %d * %s.Second)", retention, timePkg)
	// Under the same key as the two above, because Prune returns a report that
	// names its own counts exactly as they do. It was `count` while this was an
	// int, and the int is long gone.
	b.L(`logger.InfoContext(ctx, "notifications pruned", "counts", pruned.String())`)
	b.L("return err")
	b.L("}")
	b.L("}")
	b.NL()
}

// notifyPropagation emits what happens to a row's notifications when the row
// goes.
//
// "Somebody commented on ⟨deleted⟩" is the failure mode of every notification
// system, and the link table does not fix it on its own: the link row's foreign
// key restricts, so a hard delete fails on 23503 until something clears it. This
// is that something, and it is generated because it is pure bookkeeping — the
// one part of a notification system with no application decision in it at all.
//
// It rides the delete propagation rather than being a mechanism beside it. rig
// knows this child by name, which is what lets it ship; building it as a special
// case and then building the general one beside it would leave two things doing
// one job.
func (e *emitter) notifyPropagation(b *gobuf.Buf) {
	resources := e.notifiableResources()
	if len(resources) == 0 {
		return
	}
	var (
		ctxPkg    = b.Import("context")
		hookPkg   = b.Import(runtimeModule + "/dbhook")
		tenPkg    = b.Import(runtimeModule + "/tenancy")
		notifyPkg = b.Import(notifyModule)
		model     = e.model(b)
	)

	for _, res := range resources {
		lt := linkTableOf(res)
		if lt == nil {
			continue
		}
		name := "notify" + res.Name + "Deletes"

		b.Comment(name + " keeps " + res.Name + "'s notifications in step with its " +
			"rows.\n\n" +
			"Soft-deleted: cancel what is still pending about the row, and retire " +
			"the inbox lines of what was already resolved. The link rows stay, " +
			"because they are what says which lines to bring back. Hard-deleted: " +
			"the link rows go too, which is what lets the delete succeed at all " +
			"rather than failing on 23503.\n\n" +
			"All of it inside the transaction that deletes the row, so a rollback " +
			"takes it with it.\n\n" +
			"A restore is deliberately not here. Restore is the one path rig does " +
			"not walk, so bringing the inbox lines back is one line in this " +
			"resource's own Restore.After hook:\n\n" +
			"\tsvc.Restoring(ctx, api.NotifyAbout" + res.Name + "(row.ID))")
		b.L("func %s(svc *%s.Service) %s.ChildDelete[%s.%sDeleteInput, %s.%s] {",
			name, notifyPkg, hookPkg, model, res.Name, model, res.Name)
		b.L("return %s.ChildDelete[%s.%sDeleteInput, %s.%s]{",
			hookPkg, model, res.Name, model, res.Name)
		b.L("Child: %s,", gobuf.Quote(lt.Table))
		input := "in"
		if !res.Storage.IsSoftDeletable() {
			input = "_"
		}
		b.L("Deleting: func(ctx %s.Context, _ %s.Claims, row *%s.%s, %s %s.%sDeleteInput) error {",
			ctxPkg, tenPkg, model, res.Name, input, model, res.Name)
		b.L("subject := NotifyAbout%s(row.ID)", res.Name)
		if res.Storage.IsSoftDeletable() {
			b.L("if in.Hard { return svc.Deleted(ctx, subject) }")
			b.L("return svc.Deleting(ctx, subject)")
		} else {
			b.Comment("Every delete of this table is permanent, so there is no " +
				"reversible half to write.")
			b.L("return svc.Deleted(ctx, subject)")
		}
		b.L("},")
		b.L("}")
		b.L("}")
		b.NL()
	}
}

// notifyStarter emits the three lines every main that runs an engine used to
// write out.
//
// The engine is a parameter rather than something built here, for the reason
// NewNotificationEngine takes a registry: the audience for a notification is a
// method on a service, and a service is built where the application decides.
func (e *emitter) notifyStarter(b *gobuf.Buf) {
	var (
		notifyPkg = b.Import(notifyModule)
		servePkg  = b.Import(runtimeModule + "/serve")
	)

	b.Comment("StartNotificationEngine starts the engine and registers its two " +
		"shutdown steps, which is the whole of what a main function does with " +
		"one:\n\n" +
		"\tengine := api.NewNotificationEngine(app.Pool, reg, senders, app.Logger)\n" +
		"\tapi.StartNotificationEngine(app, engine)\n\n" +
		"Draining stops it claiming while the server is still answering, which is " +
		"the right order: the requests in flight are the last ones whose commits " +
		"will nudge it, and the time left is better spent finishing than " +
		"starting. Closing runs before the pool goes, because what is in flight " +
		"is a write.\n\n" +
		"The engine this runs is latency, not the guarantee. " +
		"[NotificationDispatcher] behind a `Tasks:` entry is the guarantee: it is " +
		"what takes everything a process that died mid-pass, or a replica that " +
		"never ran one, did not.")
	b.L("func StartNotificationEngine(app *%s.App, engine *%s.Engine) {", servePkg, notifyPkg)
	b.L("engine.Start()")
	b.L("app.Drain(%s, engine.StopClaiming)", gobuf.Quote(notificationsStep))
	b.L("app.CloseWithin(%s, %s, engine.Close)", gobuf.Quote(notificationsStep), notificationsConst)
	b.L("}")
	b.NL()
}

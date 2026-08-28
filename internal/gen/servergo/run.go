package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// The probe paths a project gets without asking. serve registers nothing for an
// empty one, which is the right default for a package every server imports and
// the wrong one for a generated main: every rig project wants two probes, and
// the two questions they answer are different enough that answering only one is
// a mistake rather than a preference.
const (
	livenessPath  = "/livez"
	readinessPath = "/readyz"
)

// hasAuth reports whether this project authenticates its callers.
func (e *emitter) hasAuth() bool { return e.doc.API.Auth != nil }

// part is one field on the generated Parts: what it is called, what its type is,
// and what the server does with it.
type part struct {
	field string
	// noun is what to call it in a sentence about the struct as a whole. The
	// same phrasing [emitter.shutdownSteps] uses, because it is the same thing
	// seen from the other end: a field here is a step there.
	noun string
	typ  func(b *gobuf.Buf) string
	doc  string
	// missing is what to say when the application left it nil, and is empty for
	// a field that may be. It is a sentence about consequence rather than about
	// the field, because the reader is somebody whose server just refused to
	// start.
	missing string
	// said and cost are the line a nil optional field gets at boot: what is not
	// running, and what that costs. Said rather than left to be discovered, the
	// way [emitter.attachMethod] says which half of the monitoring page is
	// unarmed.
	//
	// Optional at all because rig cannot tell the difference between a project
	// that forgot and one that meant it. `notifications: enabled` gives every
	// project a shape over rig_notification_recipient, so requiring a proxy
	// would make an inbox imply a sync service; an engine is latency and the
	// cron task is the guarantee behind it, so a project can legitimately run
	// only the task. What is left to do is make leaving one out visible at the
	// moment it happens rather than under load two months later.
	said string
	cost string
	// attach is the generated call that starts, drains or closes it. Empty for
	// Handler, which is returned rather than registered.
	attach func(b *gobuf.Buf) string
}

// parts are the lifetimes this project's configuration gives the server beyond
// a request's.
//
// One list, read three times — by the struct, by the checks and by the
// registrations — so a field that exists and a field that is attached cannot
// come apart. That is the same argument [emitter.shutdownSteps] makes about the
// numbers, one level up: the sequence a main function used to write out is the
// thing being generated, so it has to be generated from one place.
func (e *emitter) parts() []part {
	list := []part{{
		field: "Handler",
		typ:   func(b *gobuf.Buf) string { return b.Import("net/http") + ".Handler" },
		doc: "Handler is every route this server answers: the generated API, and " +
			"anything else this application mounts beside it.\n\n" +
			"An http.Handler rather than the mux it probably is, so that the " +
			"cross-cutting wrappers go here — a panic recovery, a compressor, a " +
			"middleware of your own. rig answers the two probes outside whatever " +
			"this is, so a readiness check every second is not a request through " +
			"all of it.",
		missing: "there is nothing to serve",
	}}

	if e.hasNotifications() {
		list = append(list, part{
			field: "Engine",
			noun:  "the notification engine",
			typ:   func(b *gobuf.Buf) string { return "*" + b.Import(notifyModule) + ".Engine" },
			doc: "Engine turns a committed notification into inbox lines, and is " +
				"[NewNotificationEngine]'s return value.\n\n" +
				"Started and shut down by rig, because both ends of it are numbers " +
				"out of rig.yaml. What it is built from is not: the audience for a " +
				"notification is a method on a service, so the registry it resolves " +
				"through is filled where this application builds its services.",
			said: "no notification engine in this server",
			cost: "an inbox line waits for the dispatch-notifications task " +
				"rather than arriving milliseconds after the commit",
			attach: func(b *gobuf.Buf) string {
				return "StartNotificationEngine(app, parts.Engine)"
			},
		})
	}

	if e.hasShapes() {
		list = append(list, part{
			field: "Shapes",
			noun:  "the live subscriptions",
			typ:   func(b *gobuf.Buf) string { return "*" + b.Import(electricModule) + ".Proxy" },
			doc: "Shapes is the live-sync proxy the generated shape routes forward " +
				"through.\n\n" +
				"It is here for its ending rather than its beginning — nothing " +
				"starts, since a shape route runs when a browser asks — and that " +
				"ending is the one in this struct that is not a courtesy. " +
				"[AttachShapes] says what an undrained subscription costs.",
			said: "no live-sync proxy to drain",
			cost: "a shape route mounted on this server holds an open " +
				"subscription until the shutdown budget runs out",
			attach: func(b *gobuf.Buf) string { return "AttachShapes(app, parts.Shapes)" },
		})
	}

	if e.hasAuth() {
		list = append(list, part{
			field: "Auth",
			noun:  "the auth cache's invalidation channel",
			typ:   func(b *gobuf.Buf) string { return "*" + b.Import(authModule) + ".Auth" },
			doc: "Auth is the authentication foundation over this project's pool, " +
				"and is [New]'s return value.\n\n" +
				"It is in this struct for its invalidation channel, which is a " +
				"connection and a goroutine of its own. A project with no auth cache " +
				"configured closes nothing and pays nothing — which is exactly why " +
				"this was the field worth generating rather than documenting: it " +
				"costs nothing until the day it costs a connection, and by then " +
				"nobody is looking at the main function.",
			said: "no auth foundation to close",
			cost: "a configured auth cache holds its invalidation channel's " +
				"connection until this process exits",
			attach: func(b *gobuf.Buf) string { return "AttachAuth(app, parts.Auth)" },
		})
	}

	return list
}

// runFile emits the sequence a main function used to write out: the process
// around the server, the order rig's own parts come to exist in, and the
// shutdown each of them registers.
//
// Everything in it was already generated one call at a time — StartPresenceSweeper,
// StartNotificationEngine, AttachShapes, the Process. What was not, and what this
// file is, is the order between them. It lived in a main function for one reason:
// the calls need what the application built, the application's package imports
// this one, and a generated file cannot import its way back. Naming the struct
// here rather than there turns that around, and the sequence comes with it.
//
// What that buys is not the lines. Three of those calls are load-bearing and
// silently omissible — a shutdown that spends its budget on an open tab, a
// connection held until the process exits — and each was a paragraph in the
// documentation asking somebody to remember. A field on a struct is not
// something to remember.
func (e *emitter) runFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.partsType(b)
	e.buildType(b)
	e.mountFunc(b)
	if e.tracing() {
		e.processMountMethod(b)
	}
	e.mainFunc(b)
	e.settleFunc(b)

	return artifact("run.gen.go", b)
}

// partsType emits what the application hands back.
func (e *emitter) partsType(b *gobuf.Buf) {
	list := e.parts()

	fields := make([]string, 0, len(list)-1)
	for _, p := range list[1:] {
		fields = append(fields, p.noun)
	}

	doc := "Parts is what this application's own wiring built, as far as the " +
		"process around it has to care: the routes to serve"
	if len(fields) > 0 {
		doc += ", and " + english(fields) + " — the things whose lifetime is " +
			"longer than a request's"
	}
	doc += ".\n\n" +
		"Each field is something rig starts, drains or closes on the other side " +
		"of the one call that returns this, and each used to be a line in a main " +
		"function — with no compiler, no test and usually no symptom until a " +
		"deploy under load. Naming them here is what makes them slots to fill " +
		"rather than calls to remember, and what makes turning a block on in " +
		"rig.yaml show up as a field in the one function that has to know about " +
		"it.\n\n" +
		"Handler is required. The rest may be nil, because rig cannot tell a " +
		"project that meant it from one that forgot: an engine is latency and the " +
		"`dispatch-notifications` task is the guarantee behind it, and every " +
		"project with an inbox gets a shape over rig_notification_recipient " +
		"whether or not it runs a sync service at all. What a nil one gets " +
		"instead is a line at startup naming what is not running and what that " +
		"costs."
	b.Comment(doc)

	b.L("type Parts struct {")
	for i, p := range list {
		if i > 0 {
			b.NL()
		}
		b.Comment(p.doc)
		b.L("%s %s", p.field, p.typ(b))
	}
	b.L("}")
	b.NL()
}

// buildType emits the one function an application still writes.
func (e *emitter) buildType(b *gobuf.Buf) {
	var (
		ctxPkg   = b.Import("context")
		servePkg = b.Import(runtimeModule + "/serve")
	)

	doc := "Build is this application's own wiring: everything the server is made " +
		"of, from a pool that is already open.\n\n" +
		"It runs inside the startup budget, before anything is listening, and " +
		"anything it builds with a shutdown of its own is registered on the app " +
		"it is given — beside the line that built it, the way rig registers its " +
		"own below.\n\n"
	if e.monitoring() {
		doc += "page is rig's monitoring page, for an application with somewhere to " +
			"link to it from. It is nil when there is no page to reach: an unarmed " +
			"one, or a [Mount] built without a process.\n\n"
	}
	doc += "A function rather than a value, because a task builds the same object " +
		"graph the server does and has no App to build it from. Keeping the " +
		"application's own constructor taking a pool, and adapting it here, is " +
		"what lets `dispatch-notifications` ask a service who should be told:\n\n" +
		"\tapi.Main(cfg, func(ctx context.Context, app *serve.App" +
		func() string {
			if e.monitoring() {
				return ", page *observe.Page"
			}
			return ""
		}() + ") (api.Parts, error) {\n" +
		"\t\treturn myapp.New(ctx, app.Pool, app.Logger" +
		func() string {
			if e.monitoring() {
				return ", page"
			}
			return ""
		}() + ")\n" +
		"\t})"
	b.Comment(doc)

	b.P("type Build func(ctx %s.Context, app *%s.App", ctxPkg, servePkg)
	if e.monitoring() {
		b.P(", page *%s.Page", b.Import(observeModule))
	}
	b.L(") (Parts, error)")
	b.NL()
}

// mountFunc emits the sequence itself.
func (e *emitter) mountFunc(b *gobuf.Buf) {
	var (
		ctxPkg   = b.Import("context")
		httpPkg  = b.Import("net/http")
		servePkg = b.Import(runtimeModule + "/serve")
	)

	doc := "Mount is a [Build] as a serve.Mount, with everything rig starts before " +
		"it and everything rig shuts down registered after it.\n\n" +
		"[Main] is this and a serve.Config; this is for a caller keeping the " +
		"process — serve.Run in a test, a binary that is more than a server. The " +
		"order is the same either way, which is the reason to reach for this " +
		"rather than write the sequence out:\n\n" +
		"\tserve.Run(ctx, cfg, api.Mount(build))\n\n"
	if e.tracing() {
		doc += "It attaches no process, so nothing here flushes spans and " +
			"[Process.Attach] is still a caller's to write. [Process.Mount] is the " +
			"pair that does both.\n\n"
	}
	doc += "What comes back is registered in the order it has to be, and anything " +
		"left nil is said out loud on the way past — see [Parts]."
	b.Comment(doc)

	if e.monitoring() {
		b.L("func Mount(build Build) %s.Mount { return mountWith(nil, build) }", servePkg)
		b.NL()

		b.Comment("mountWith is [Mount] with the page threaded through, which is the " +
			"only thing [Process.Mount] adds to it besides the flush.")
		b.L("func mountWith(page *%s.Page, build Build) %s.Mount {", b.Import(observeModule), servePkg)
	} else {
		b.L("func Mount(build Build) %s.Mount {", servePkg)
	}

	b.L("return func(ctx %s.Context, app *%s.App) (%s.Handler, error) {", ctxPkg, servePkg, httpPkg)

	if e.hasPresence() {
		b.Comment("Before the application's own wiring, because it needs nothing from " +
			"it: the service it sweeps through is its own, over app.Pool.")
		b.L("StartPresenceSweeper(app)")
		b.NL()
	}

	if e.monitoring() {
		b.L("parts, err := build(ctx, app, page)")
	} else {
		b.L("parts, err := build(ctx, app)")
	}
	b.L("if err != nil {")
	b.L("return nil, err")
	b.L("}")
	b.NL()

	list := e.parts()

	errsPkg := b.Import("errors")
	for _, p := range list {
		if p.missing == "" {
			continue
		}
		b.L("if parts.%s == nil {", p.field)
		b.L("return nil, %s.New(%s)", errsPkg,
			gobuf.Quote(e.cfg.Package+": Parts."+p.field+" is nil: "+p.missing))
		b.L("}")
		b.NL()
	}

	for _, p := range list {
		if p.attach == nil {
			continue
		}
		b.L("if parts.%s != nil {", p.field)
		b.L("%s", p.attach(b))
		b.L("} else {")
		b.Comment("Said rather than left to be discovered. Leaving it out is " +
			"allowed — rig cannot tell a project that meant it from one that " +
			"forgot — so what it can do is say so once, while it is still cheap " +
			"to fix.")
		b.L("app.Logger.InfoContext(ctx, %s, %s, %s)",
			gobuf.Quote(p.said), gobuf.Quote("cost"), gobuf.Quote(p.cost))
		b.L("}")
		b.NL()
	}

	b.L("return parts.Handler, nil")
	b.L("}")
	b.L("}")
	b.NL()
}

// processMountMethod emits the pair that also flushes.
func (e *emitter) processMountMethod(b *gobuf.Buf) {
	var (
		ctxPkg   = b.Import("context")
		httpPkg  = b.Import("net/http")
		servePkg = b.Import(runtimeModule + "/serve")
	)

	b.Comment("Mount is [Mount] with this process attached to the same server: the " +
		"trace flush, and a line about anything rig.yaml armed that the " +
		"environment did not.\n\n" +
		"[Main] uses it, so a main function does not. This is for one that called " +
		"[NewProcess] itself and is running serve.Run rather than serve.Main.")
	b.L("func (p *Process) Mount(build Build) %s.Mount {", servePkg)
	b.L("return func(ctx %s.Context, app *%s.App) (%s.Handler, error) {", ctxPkg, servePkg, httpPkg)
	b.L("p.Attach(app)")
	if e.monitoring() {
		b.L("return mountWith(p.page, build)(ctx, app)")
	} else {
		b.L("return Mount(build)(ctx, app)")
	}
	b.L("}")
	b.L("}")
	b.NL()
}

// mainFunc emits the whole of a main function.
func (e *emitter) mainFunc(b *gobuf.Buf) {
	servePkg := b.Import(runtimeModule + "/serve")

	example := "\tfunc main() {\n" +
		"\t\tapi.Main(serve.Config{\n" +
		"\t\t\tAddr:    cmp.Or(os.Getenv(\"ADDR\"), \"127.0.0.1:8080\"),\n" +
		"\t\t\tTasks:   map[string]serve.Task{\"migrate\": migrate.Apply(migrations, migrate.Options{})},\n" +
		"\t\t\tMigrate: migrate.Require(migrations, migrate.Options{}),\n" +
		"\t\t}, func(ctx context.Context, app *serve.App" +
		func() string {
			if e.monitoring() {
				return ", page *observe.Page"
			}
			return ""
		}() + ") (api.Parts, error) {\n" +
		"\t\t\treturn myapp.New(ctx, app.Pool, app.Logger" +
		func() string {
			if e.monitoring() {
				return ", page"
			}
			return ""
		}() + ")\n" +
		"\t\t})\n" +
		"\t}"

	doc := "Main is the whole of a main function: this project's configuration, " +
		"and the one function only this application can write.\n\n" +
		example + "\n\n" +
		"What it settles is what rig.yaml already decided — [settle] lists it " +
		"field by field — and every one of those is still a field, so setting it " +
		"is how a project disagrees. What is left in the literal above is what a " +
		"configuration file cannot hold: where the migrations are embedded, what " +
		"this binary's subcommands are, and the two addresses.\n\n"
	if e.tracing() {
		doc += "The process is built here rather than by a caller, which is why " +
			"there is no `process, err := api.NewProcess()` above it. It has to " +
			"exist before the serve.Config it fills in and after the App it " +
			"registers a flush with, and holding a value across those two ends was " +
			"the whole reason a main function used to name it. Failing to build one " +
			"is an exit before there is a logger to say so with — [NewProcess] says " +
			"which of the three ways that happens.\n\n"
	}
	doc += "A project that wants a different order, or a process it keeps hold of, " +
		"uses [Mount] with serve.Main or serve.Run instead. This is the " +
		"arrangement rig.yaml describes, not the only one it allows."
	b.Comment(doc)

	b.L("func Main(cfg %s.Config, build Build) {", servePkg)
	if e.tracing() {
		var (
			slogPkg = b.Import("log/slog")
			osPkg   = b.Import("os")
		)
		b.L("process, err := NewProcess()")
		b.L("if err != nil {")
		b.Comment("There is no application logger yet: this is the thing that would " +
			"have been half of one. slog.Default writes to stderr.")
		b.L("%s.Error(\"cannot set this process up\", \"error\", err)", slogPkg)
		b.L("%s.Exit(1)", osPkg)
		b.L("}")
		b.NL()
		b.Comment("No `defer process.Close()`, and that is the fix rather than the " +
			"omission it looks like. Configure sets it as serve.Config.OnExit, so " +
			"serve.Main runs it on every way out — including the three that end in " +
			"os.Exit, where a deferred call runs not at all.")
		b.L("%s.Main(process.Configure(settle(cfg)), process.Mount(build))", servePkg)
	} else {
		b.L("%s.Main(settle(cfg), Mount(build))", servePkg)
	}
	b.L("}")
	b.NL()
}

// settleFunc emits the defaults that came out of rig.yaml.
func (e *emitter) settleFunc(b *gobuf.Buf) {
	servePkg := b.Import(runtimeModule + "/serve")

	doc := "settle fills in the half of a serve.Config this project's configuration " +
		"already decided, and leaves anything already set alone.\n\n" +
		"Tasks are merged rather than replaced, the way [Tasks] merges them: the " +
		"application's half wins on a name they share.\n\n"
	if len(e.shutdownSteps()) > 0 {
		doc += "MaxShutdown is [ShutdownBudget] plus the drain delay, which is what " +
			"serve checks the registered steps against — it counts the delay too. " +
			"Stating it is still the better answer for anything that ships: it is " +
			"the number that belongs in Kubernetes' terminationGracePeriodSeconds, " +
			"and an operator should be able to read it off a struct rather than run " +
			"the binary. ShutdownBudget's own documentation states the total in " +
			"words for exactly that.\n\n"
	}
	doc += "The probe paths are two questions rather than one, which is why both " +
		"are filled rather than neither. Liveness asks whether to restart this " +
		"process and touches nothing; readiness asks whether to send it work and " +
		"turns false the moment a shutdown begins. One check for both is either a " +
		"wedged process nobody restarts or a fleet restarted because the database " +
		"was slow."
	b.Comment(doc)

	b.L("func settle(cfg %s.Config) %s.Config {", servePkg, servePkg)
	b.L("cfg.Tasks = Tasks(cfg.Tasks)")
	if len(e.shutdownSteps()) > 0 {
		b.L("if cfg.MaxShutdown == 0 {")
		b.L("cfg.MaxShutdown = ShutdownBudget() + cfg.DrainDelay")
		b.L("}")
	}
	b.L("if cfg.LivenessPath == \"\" {")
	b.L("cfg.LivenessPath = %s", gobuf.Quote(livenessPath))
	b.L("}")
	b.L("if cfg.ReadinessPath == \"\" {")
	b.L("cfg.ReadinessPath = %s", gobuf.Quote(readinessPath))
	b.L("}")
	b.L("return cfg")
	b.L("}")
	b.NL()
}

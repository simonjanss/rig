package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
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
			field: "Proxy",
			noun:  "the sync proxy",
			typ:   func(b *gobuf.Buf) string { return "*" + b.Import(electricModule) + ".Proxy" },
			doc: "Proxy is the sync service this server's shape routes forward " +
				"through — the same *electric.Proxy given to Handlers.Shapes, named " +
				"again here.\n\n" +
				"Twice because the two uses are different questions. Register mounts " +
				"the routes with it and cannot fail; this is the one that asks the " +
				"sync service whether it is there, while the server is still starting " +
				"and while refusing to start is still an option. [CheckSyncService] " +
				"is what happens to the answer.\n\n" +
				"Nil is allowed, and is what a project that generated its shapes and " +
				"has not written a front end for them yet has. It is said out loud " +
				"rather than refused, because rig cannot tell that project from one " +
				"that meant to wire a proxy and did not.",
			said: "no sync service in this server",
			cost: "no shape route is mounted and nothing on this server live-syncs",
			attach: func(b *gobuf.Buf) string {
				if e.monitoring() {
					return "if err := CheckSyncService(ctx, app, page, parts.Proxy); err != nil {\nreturn nil, err\n}"
				}
				return "if err := CheckSyncService(ctx, app, parts.Proxy); err != nil {\nreturn nil, err\n}"
			},
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
// StartNotificationEngine, the Process. What was not, and what this
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
	doc += ".\n\n"

	if len(fields) == 0 {
		// Nothing to say about optional fields, because this project has none:
		// the `rig init` case, where Parts is a handler and the paragraph worth
		// writing is the one about the field a new block will add.
		doc += "One field, because this project's configuration gives the server " +
			"nothing whose lifetime outlasts a request. Turning a block on in " +
			"rig.yaml adds one — something rig starts, drains or closes on the " +
			"other side of the one call that returns this, and something that used " +
			"to be a line in a main function with no compiler, no test and usually " +
			"no symptom until a deploy under load."
	} else {
		doc += "Every field beside the handler is something rig starts, drains or " +
			"closes on the other side of the one call that returns this, and each " +
			"used to be a line in a main function — with no compiler, no test and " +
			"usually no symptom until a deploy under load. Naming them here is what " +
			"makes them slots to fill rather than calls to remember, and what makes " +
			"turning a block on in rig.yaml show up as a field in the one function " +
			"that has to know about it.\n\n" +
			"Handler is required. The rest may be nil, because rig cannot tell a " +
			"project that meant it from one that forgot"
		if e.hasNotifications() {
			doc += ": an engine is latency and the `dispatch-notifications` task is " +
				"the guarantee behind it, and every project with an inbox gets a " +
				"shape over rig_notification_recipient whether or not it runs a " +
				"sync service at all"
		}
		doc += ". What a nil one gets instead is a line at startup naming what is " +
			"not running and what that costs."
	}
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
			"link to it from. It is nil when there is no process to take one " +
			"from — a [Mount] built without one, or a task, which never reaches " +
			"this function at all.\n\n" +
			"A page nobody armed is not nil, and a link is not what to build out " +
			"of the pointer. An environment that set no address and no password " +
			"gets a real page whose " +
			"[github.com/simonjanss/rig/observe.Page.Addr] is empty, which is the " +
			"half a link needs — so that, or " +
			"[github.com/simonjanss/rig/observe.Page.Unarmed], is what says " +
			"whether there is anywhere to send somebody.\n\n"
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
	doc += "app.Logger is labelled with the request before build is called, so " +
		"everything built out of it — the services, their repositories, the " +
		"authentication configuration — writes lines that say which request " +
		"they belong to. See " +
		"[github.com/simonjanss/rig/runtime/apibase.RequestLogger].\n\n"
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

	basePkg := b.Import(runtimeModule + "/apibase")
	b.L("app.Logger = %s.RequestLogger(app.Logger)", basePkg)
	b.NL()

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

	if e.servesOpenAPI() {
		e.openAPIAnnounce(b)
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
		"\t\t\tAddr:              cmp.Or(os.Getenv(\"ADDR\"), \"127.0.0.1:8080\"),\n" +
		"\t\t\tLivenessPath:      \"/livez\",\n" +
		"\t\t\tReadinessPath:     \"/readyz\",\n" +
		"\t\t\tMaxStartup:        30 * time.Second,\n" +
		"\t\t\tConnectTimeout:    10 * time.Second,\n" +
		"\t\t\tProbeTimeout:      2 * time.Second,\n" +
		"\t\t\tReadHeaderTimeout: 5 * time.Second,\n" +
		"\t\t\tReadTimeout:       30 * time.Second,\n" +
		"\t\t\tWriteTimeout:      30 * time.Second,\n" +
		"\t\t\tIdleTimeout:       2 * time.Minute,\n" +
		"\t\t\tMaxShutdown:       " + duration("time", e.shutdownBudget()) + ", // ShutdownBudget()\n" +
		"\t\t\tTasks:             map[string]serve.Task{\"migrate\": migrate.Apply(migrations, migrate.Options{})},\n" +
		"\t\t\tMigrate:           migrate.Require(migrations, migrate.Options{}),\n" +
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
		"is how a project disagrees. What is left in the literal above is " +
		"everything serve will not choose for itself — every timeout, both probe " +
		"paths, the address and the shutdown budget — together with what a " +
		"configuration file cannot hold: where the migrations are embedded and " +
		"what this binary's subcommands are.\n\n" +
		"There is no Logger in it, and that is not an omission. serve refuses a " +
		"config that states none, and it is filled in" +
		func() string {
			if e.tracing() {
				return " by [Process.Configure], from the sink [NewProcess] built " +
					"— so the log file the monitoring page reads is the one this " +
					"server writes to"
			}
			return " by settle, with the default logger, there being no sink to " +
				"choose between without a `tracing:` block"
		}() + ". State one to send the lines somewhere else; [Mount] wraps " +
		"whatever it ends up being, so a line written inside a request says " +
		"which request.\n\n" +
		"MaxShutdown is the fourth, and it is there for a different reason than " +
		"the other three. rig knows what it should be — [ShutdownBudget] adds it " +
		"up — and settles it anyway not at all, because it is the one number in " +
		"that struct that leaves the program. Whoever writes " +
		"terminationGracePeriodSeconds has to read it off this literal rather " +
		"than run the binary, so a value nobody wrote is a deployment that stops " +
		"this server faster than it can drain and nothing that says so. Left out, " +
		"it is refused before anything starts, with the number to write.\n\n"
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

	slogPkg := b.Import("log/slog")
	osPkg := b.Import("os")

	b.Comment("What this project's own shutdown needs, which is [ShutdownBudget] " +
		"unless the deployment sized its steps.\n\n" +
		"Asked through an interface rather than by asserting [Shutdown] back out " +
		"of the field, so that a pointer to one — or a set a project wrote " +
		"itself — is read here too. What the two checks below say is only worth " +
		"anything if the number in it is the one the config being read would " +
		"actually produce.")
	b.L("budget := ShutdownBudget()")
	if len(e.shutdownSteps()) > 0 {
		b.L("if s, ok := cfg.Shutdown.(interface{ Budget() %s.Duration }); ok {", b.Import("time"))
		b.L("budget = s.Budget()")
		b.L("}")
	}
	b.NL()

	b.Comment("Before a database is opened or a process is built, because this is " +
		"answerable without either and the answer belongs in a deployment rather " +
		"than in a log at shutdown.\n\n" +
		"serve refuses this too, and names what was actually registered — " +
		"including closers this project wrote, which nothing here can know " +
		"about. What only this side knows is the number rig.yaml adds up to, so " +
		"that is what this one prints. Neither is the other's duplicate; " +
		"removing either loses the half it names.")
	b.L("if cfg.MaxShutdown == 0 {")
	b.L("%s.Error(\"MaxShutdown is required: state it in the serve.Config above\",", slogPkg)
	b.L("\"budget\", budget, \"drain delay\", cfg.DrainDelay, \"write\", budget+cfg.DrainDelay)")
	b.L("%s.Exit(2)", osPkg)
	b.L("}")
	b.NL()

	b.Comment("And a number that was stated but is smaller than what this project " +
		"asks for, which is the same failure one step later: a total written " +
		"once and left behind by a block turned on in rig.yaml, or by a step " +
		"sized in the serve.Config above.\n\n" +
		"Said rather than refused, because this side cannot tell the difference " +
		"between a step that is counted and a step that is registered. " +
		"[ShutdownBudget] counts every step this project's configuration " +
		"describes, including one whose part a build returns nil for — so a " +
		"number below it is sometimes exactly right, and refusing it here would " +
		"be refusing a server rig cannot see. serve has the last word: it adds " +
		"up what was actually registered and refuses a budget that cannot hold " +
		"it. What this catches is the case that reaches serve as a warning about " +
		"a shutdown with nothing left for the requests in flight, at the moment " +
		"there is still a literal on screen to fix.")
	b.L("if cfg.MaxShutdown < budget+cfg.DrainDelay {")
	b.L("%s.Warn(\"MaxShutdown is less than this project's own shutdown asks for: "+
		"the requests still in flight get what is left of it\",", slogPkg)
	b.L("\"max shutdown\", cfg.MaxShutdown, \"budget\", budget, " +
		"\"drain delay\", cfg.DrainDelay, \"write\", budget+cfg.DrainDelay)")
	b.L("}")
	b.NL()

	if e.tracing() {
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
	if !e.tracing() {
		doc += "The logger is among them, and it is the one that is not out of " +
			"rig.yaml — there is no `logging:` block to read. It is here because " +
			"serve refuses a config that states no logger, and with no `tracing:` " +
			"block there is no sink to choose between: the answer is the default " +
			"logger, and a main function writing it out would be a line that could " +
			"only have said that. A stated one is left alone. Either way [Mount] " +
			"wraps what comes out, so the request is on every line written inside " +
			"one — which is labelling a logger rather than choosing one, and is " +
			"why this does not belong with the three below.\n\n"
	}
	doc += "MaxShutdown is deliberately not among them, though this is the one " +
		"place that knows the answer. It is what a deployment's " +
		"terminationGracePeriodSeconds has to agree with, so it is written in the " +
		"serve.Config where it can be read rather than settled where it cannot. " +
		"[Main] refuses one that was left out, and [ShutdownBudget] is the number " +
		"to write — plus the drain delay, which serve counts against it too.\n\n" +
		"The probe paths are not among them either, and for the same reason one " +
		"step down. They are two questions rather than one — liveness asks " +
		"whether to restart this process and touches nothing, readiness asks " +
		"whether to send it work and turns false the moment a shutdown begins — " +
		"and both are read by whatever is checking them, which is not this " +
		"binary. A path settled here is one an orchestrator has to be told about " +
		"anyway, so it is written where it can be read. serve refuses either left " +
		"empty; serve.NoProbe is how a project says it wants none."
	b.Comment(doc)

	b.L("func settle(cfg %s.Config) %s.Config {", servePkg, servePkg)
	if !e.tracing() {
		b.Comment("serve refuses a config with no logger, and there is nothing for " +
			"this project to decide about one: with no `tracing:` block there is no " +
			"sink to build, so the answer is the default logger and a main function " +
			"writing it out is a line that could only have said this.\n\n" +
			"Only when it was left unset. A logger stated in the serve.Config is " +
			"the application's and is kept — [Mount] wraps whatever this ends up " +
			"being, so a stated one is labelled rather than replaced.")
		b.L("if cfg.Logger == nil {")
		b.L("cfg.Logger = %s.Default()", b.Import("log/slog"))
		b.L("}")
		b.NL()
	}
	b.L("cfg.Tasks = Tasks(cfg.Tasks)")
	b.L("return cfg")
	b.L("}")
	b.NL()
}

package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// electricModule is the proxy a shape route forwards through. It appears in a
// project's dependency graph only where a table asked for live sync.
const electricModule = "github.com/simonjanss/rig/runtime/electric"

// hasShapes reports whether any table in this project is subscribed to live.
//
// It reads a field per table rather than an `electric:` block of its own,
// because there is not one: live sync is a line per table, and the question
// here is whether any of them said yes.
func (e *emitter) hasShapes() bool { return len(e.shapes()) > 0 }

// shapes are the resources that asked for a live-sync endpoint.
//
// Unexposed is not consulted, unlike [emitter.resources]. Live sync is its own
// read surface: rig_notification_recipient has no CRUD routes at all and is
// still subscribed to, which is how a bell knows to ring.
func (e *emitter) shapes() []*ir.Resource {
	var out []*ir.Resource
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		if res.Electric != nil && res.Storage != nil {
			out = append(out, res)
		}
	}
	return out
}

// shapeKind distinguishes the routes a table's live-sync surface has.
type shapeKind int

const (
	// shapeLive streams the rows an ordinary read returns.
	shapeLive shapeKind = iota
	// shapeDeleted streams the retired ones — the trash.
	shapeDeleted
	// shapeVersions streams one row's prior versions — its history.
	shapeVersions
)

// shape is one route of a table's live-sync surface.
type shape struct {
	kind shapeKind
	// name is the Go stem for this shape's scope type, handler and field.
	name string
	// path is the route, for example "/api/v1/lesson/_deleted/_stream".
	path string
}

// shapesFor is the routes a resource exposes.
//
// Nothing configures this. A table that retires its rows has a trash to stream,
// and a table that keeps its previous versions has a history — the schema
// already answers both questions, and answering them again in a configuration
// key would only create a way for the two answers to disagree.
//
// It is the columns alone, and deliberately not the resource's operations. The
// API asks for both — listDeleted needs List and versions needs Get — but live
// sync is its own read surface and has never been gated on the CRUD set: a table
// with no operations at all still gets a live shape, which is how an unexposed
// table like rig_notification_recipient is subscribed to. Reading the operations
// here and not there would mean a table whose only read surface is a shape could
// never have a trash, so what the two rules have in common is the columns and
// that is what is asked.
func (e *emitter) shapesFor(res *ir.Resource) []shape {
	shapes := []shape{{kind: shapeLive, name: res.Name, path: res.Electric.Path}}
	if res.Storage.IsSoftDeletable() {
		shapes = append(shapes, shape{
			kind: shapeDeleted,
			name: res.Name + "Deleted",
			path: res.Electric.DeletedPath(),
		})
	}
	if res.Storage.IsSnapshotable() {
		shapes = append(shapes, shape{
			kind: shapeVersions,
			name: res.Name + "Versions",
			path: res.Electric.VersionsPath(),
		})
	}
	return shapes
}

// electricFile emits the live-sync half of the registration surface: the struct
// an application fills in, and the sync service this project was generated
// against.
//
// It is in the API package rather than one of its own because a shape route is
// an API route. It answers on the same mux, identifies its caller with the same
// claims lookup, refuses with the same error mapper, and its drain is a term of
// [ShutdownBudget] — four things that were four copies for as long as the two
// halves were two packages, and one of which was already drifting.
func (e *emitter) electricFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.shapesStruct(b)

	return artifact("electric.gen.go", b)
}

// shapesStruct is the live-sync registration surface.
func (e *emitter) shapesStruct(b *gobuf.Buf) {
	var (
		elecPkg  = b.Import(electricModule)
		servePkg = b.Import(runtimeModule + "/serve")
		tenPkg   = b.Import(runtimeModule + "/tenancy")
	)

	if e.cfg.ElectricURL != "" {
		b.Comment("DefaultElectricURL is the sync service configured when this was " +
			"generated. A deployment can point somewhere else by building its own proxy.")
		b.L("const DefaultElectricURL = %s", gobuf.Quote(e.cfg.ElectricURL))
		b.NL()
	}

	e.syncCheck(b)

	b.Comment("Shapes is this server's live-sync half: the proxy the shape routes " +
		"forward through, and one scoping function per shape.\n\n" +
		"It is a field of [Handlers] rather than a second registration of its own, " +
		"so mounting the API and mounting its shapes are the same call. They were " +
		"two, and the cost of that was not the extra line: a shape route " +
		"identifies its caller, refuses a request and drains at shutdown exactly " +
		"the way every other route does, and every one of those was a field an " +
		"application had to remember to set twice.\n\n" +
		"A nil Proxy mounts nothing at all, which is what a project that generated " +
		"its shapes and has not written the front end yet wants — the routes are " +
		"absent rather than answering.\n\n" +
		"A nil scope is not an error either: it means the shape is filtered by " +
		"tenant and lifecycle and nothing else, which is exactly right for most " +
		"tables.\n\n" +
		"A soft-deletable table has a trash shape here as well as a live one, and " +
		"a table that keeps its previous versions has a history shape. Neither is " +
		"configured: the columns are what decide, the same way they decide whether " +
		"the API has a GET /_deleted.\n\n" +
		"Those two inherit the live shape's scope while their own field is nil. " +
		"They carry the same table's rows, so a narrowing that mattered for the " +
		"live shape almost always matters for the trash and the history too — and " +
		"a narrowing the application has to remember to repeat on a route rig " +
		"added is one that eventually does not get repeated. Set the field to " +
		"scope them differently; setting it replaces the inherited scope rather " +
		"than adding to it.")
	b.L("type Shapes struct {")

	b.Comment("Proxy forwards to the sync service. Nil mounts no shape routes.")
	b.L("Proxy *%s.Proxy", elecPkg)
	b.NL()

	b.Comment("App is the lifecycle the drain registers on.\n\n" +
		"A shape route is a request the server is deliberately not answering yet, " +
		"so http.Server.Shutdown waits for it, and waits: nothing in the poll is " +
		"late, and Shutdown does not cancel a request's context. One open tab is a " +
		"shutdown that spends its budget waiting for the sync service to have " +
		"news, after which the flush, the sweep and the notification engine's own " +
		"close each find a deadline that has already passed.\n\n" +
		"The drain runs first, once readiness is already false, which is the point " +
		"at which there is nothing left to gain from holding a subscription open. " +
		"The subscriber is told to come back and resumes from the same offset " +
		"against a replica that is still serving. Nothing is lost, because a poll " +
		"that had not answered had nothing in it yet.\n\n" +
		"Inside Build it is the App that was handed in:\n\n" +
		"\tShapes: api.Shapes{App: app, Proxy: proxy},\n\n" +
		"Nil mounts the routes and registers no drain, and says so on the way " +
		"past. That is for the caller that has no App and owns the ending itself " +
		"— a test building this handler from a bare pool — rather than for " +
		"forgetting.")
	b.L("App *%s.App", servePkg)
	b.NL()

	b.Comment("IsAdmin reports whether a caller may subscribe to a shape marked " +
		"admin-only. Nil refuses every one of them, which is the safe way to leave " +
		"it unconfigured.")
	b.L("IsAdmin func(%s.Claims) bool", tenPkg)
	b.NL()

	for _, res := range e.shapes() {
		for _, sh := range e.shapesFor(res) {
			b.L("%s %sScope", sh.name, sh.name)
		}
	}
	b.L("}")
	b.NL()
}

// shapesMount emits the live-sync half of Register: the inherited scopes, the
// routes, and the drain.
//
// The drain is registered here rather than by Mount because this is where the
// proxy is named. It used to travel back to rig as a field of Parts, which
// worked and asked an application to say the same thing twice — once to mount
// the routes and once so the shutdown would know about them. Saying it once is
// the point of the struct.
func (e *emitter) shapesMount(b *gobuf.Buf) {
	shapes := e.shapes()

	b.NL()
	b.Comment("The live-sync shapes, on the same mux as everything else. Nil " +
		"mounts nothing: the routes are absent rather than answering, which is " +
		"what a project that has not built a front end for them yet wants.")
	b.L("if h.Shapes.Proxy != nil {")

	e.inherit(b, shapes)

	for _, res := range shapes {
		for _, sh := range e.shapesFor(res) {
			b.L("mux.HandleFunc(%s, handle%sShape(h.Server, h.Shapes))",
				gobuf.Quote("GET "+sh.path), sh.name)
		}
	}
	b.NL()

	b.Comment("And the ending, which is the whole of what there is to register: " +
		"nothing is started, unlike the sweeper and the engine beside it, because " +
		"a shape route runs when a browser asks and not before.\n\n" +
		"Without an App there is nothing to register it on, which is allowed: a " +
		"caller may own the ending itself, and a task that builds this handler " +
		"to reach a service through it never serves a route at all. Said at info " +
		"rather than refused or warned about, the way Mount says the same kind of " +
		"thing about a part it cannot tell an omission from a decision about.")
	b.L("if h.Shapes.App != nil {")
	b.L("h.Shapes.App.DrainWithin(%s, %s, h.Shapes.Proxy.Drain)",
		gobuf.Quote(shapesStep), shapesConst)
	b.L("} else if h.Server.Logger != nil {")
	b.L("h.Server.Logger.Info(%s, %s, %s)",
		gobuf.Quote("live-sync shapes are mounted with no App to drain them"),
		gobuf.Quote("cost"),
		gobuf.Quote("a shape route on this server holds an open subscription until the shutdown budget runs out"))
	b.L("}")
	b.L("}")
}

// inherit points a derived shape with no scope of its own at the live one.
//
// The trash and the history are the same table's rows, so an application that
// narrowed the live shape meant something by it, and the narrowing is what
// decides who may see those rows. Defaulting these to nil would mean a project
// that regenerates gains two routes that show more than the one it already had,
// without a line of its own code changing and without failing to compile. This
// is the same argument the handler makes for building the owner predicate
// itself: a narrowing somebody has to add by hand is one somebody eventually
// does not.
//
// Inheriting can only ever show less, which is the direction to be wrong in. An
// application that wants these scoped differently says so by setting the field.
func (e *emitter) inherit(b *gobuf.Buf, shapes []*ir.Resource) {
	var wrote bool
	for _, res := range shapes {
		for _, sh := range e.shapesFor(res) {
			if sh.kind == shapeLive {
				continue
			}
			if !wrote {
				b.Comment("A derived shape falls back to the live shape's scope. Nil " +
					"stays nil: a table nobody scoped is scoped by tenant and lifecycle " +
					"on all three of its routes.")
				wrote = true
			}
			b.L("if h.Shapes.%s == nil {", sh.name)
			if sh.kind == shapeVersions {
				b.L("h.Shapes.%s = versionsFromLive%s(h.Shapes.%s)", sh.name, res.Name, res.Name)
			} else {
				b.L("h.Shapes.%s = %sScope(h.Shapes.%s)", sh.name, sh.name, res.Name)
			}
			b.L("}")
		}
	}
	if wrote {
		b.NL()
	}
}

// syncCheck emits the boot-time answer to "is the sync service there".
//
// It exists because a server that uses live sync starts perfectly well without
// it and, until this, said nothing: the pool is pinged and hinted about, and the
// sync service was discovered ten seconds into somebody's first subscription, as
// a 502 or a silently degraded snapshot. The three ways to get here are ordinary
// — no `rig db up`, no database.electric block, a stale $ELECTRIC_URL under an
// isolated database — and all three are cheap to fix at the moment the server
// starts and expensive to work out from a blank page.
//
// Whether that is a warning or a refusal is [Options.ElectricRequired], and it
// has to be the project's answer rather than rig's: a shape with a fallback
// serves a snapshot through an outage, so refusing to start would be rig
// deciding that a degraded server is worth less than no server.
func (e *emitter) syncCheck(b *gobuf.Buf) {
	var (
		ctxPkg   = b.Import("context")
		elecPkg  = b.Import(electricModule)
		fmtPkg   = b.Import("fmt")
		servePkg = b.Import(runtimeModule + "/serve")
		timePkg  = b.Import("time")
	)

	b.Comment("ElectricRequired is whether this project decided it cannot serve " +
		"without live sync.\n\n" +
		"It comes from server-go's electric_required in rig.yaml, and it is what " +
		"[CheckSyncService] does with a sync service that is not answering: false " +
		"warns and carries on, true refuses to start and puts the sync service in " +
		"the readiness check.")
	b.L("const ElectricRequired = %t", e.cfg.ElectricRequired)
	b.NL()

	b.Comment("syncProbeTimeout bounds the one question asked at boot. It is inside " +
		"the server's own MaxStartup, and short because the answer wanted here is " +
		"\"is it there\" rather than \"will it eventually be\": a sync service still " +
		"connecting to its database is reported as not serving, which is the " +
		"honest answer and the one that comes back on its own.")
	b.L("const syncProbeTimeout = 5 * %s.Second", timePkg)
	b.NL()

	b.Comment("syncHint is what to tell somebody whose sync service is not there.\n\n" +
		"The counterpart of the serve.Config.Hint the database has, and it is here " +
		"for the same reason that one is a field: the address a shape route " +
		"forwards to is a container in development and a deployed service in " +
		"production, and printing the wrong one is worse than printing nothing.")
	b.L("const syncHint = %s", gobuf.Quote(
		"run `rig db up` to start the sync service, or set $ELECTRIC_URL"))
	b.NL()

	b.Comment("CheckSyncService asks the sync service whether it is serving, once, " +
		"while the server is still starting.\n\n" +
		"Called by [Mount] with whatever the application put in Parts.Proxy, so a " +
		"project gets this without writing it. What it does with a bad answer is " +
		"[ElectricRequired]: an error that stops the boot, or a warning and a " +
		"server that starts anyway.\n\n" +
		"A nil proxy is not a failure in either case, and returns without a word: " +
		"rig cannot tell a project that has not built a front end for its shapes " +
		"yet from one that forgot to wire the proxy, and [Mount] is where that is " +
		"said.\n\n" +
		syncCheckRegisters(e.monitoring()))
	// The page is a parameter only where there is one. A project with no
	// monitoring block has no rig/observe in its module at all, and naming the
	// type here would put it there for the sake of an argument nobody passes.
	page := ""
	if e.monitoring() {
		page = ", page *" + b.Import(observeModule) + ".Page"
	}
	b.L("func CheckSyncService(ctx %s.Context, app *%s.App%s, proxy *%s.Proxy) error {",
		ctxPkg, servePkg, page, elecPkg)

	b.L("if proxy == nil {")
	b.Comment("Nothing to ask, and nothing said either: [Mount] is the caller " +
		"that can tell a nil Parts.Proxy from an absent one, and it already " +
		"logged which. Saying it again here would be the same line twice for " +
		"every project that left the field alone.")
	b.L("return nil")
	b.L("}")
	b.NL()

	if e.monitoring() {
		b.Comment("The probe rather than a state, so the pill on the monitoring page " +
			"answers whether the sync service is there now instead of whether it " +
			"was there when something last happened to touch it. A nil page — a " +
			"caller using Mount rather than Process.Mount — registers nothing.")
		b.L("page.Watch(%s, proxy.Health)", gobuf.Quote("sync service"))
		b.NL()
	}

	b.L("probe, cancel := %s.WithTimeout(ctx, syncProbeTimeout)", ctxPkg)
	b.L("defer cancel()")
	b.NL()

	b.L("if err := proxy.Health(probe); err != nil {")
	b.L("if ElectricRequired {")
	b.L("return %s.Errorf(\"%%w (%%s)\", err, syncHint)", fmtPkg)
	b.L("}")
	b.L("app.Logger.WarnContext(ctx, %s, %s, err, %s, syncHint, %s, %s)",
		gobuf.Quote("the sync service is not answering"),
		gobuf.Quote("error"),
		gobuf.Quote("hint"),
		gobuf.Quote("cost"),
		gobuf.Quote("a shape with a fallback serves a snapshot; the rest answer 502"))
	b.L("return nil")
	b.L("}")
	b.NL()

	b.L("app.Logger.InfoContext(ctx, %s)", gobuf.Quote("the sync service is answering"))
	b.NL()

	b.L("if ElectricRequired {")
	b.Comment("Only when the project said so. By default a sync outage must not " +
		"take every replica out of the load balancer at once: the shapes with a " +
		"fallback are still serving, and so is every route that never touched " +
		"the sync service.")
	b.L("app.Ready(%s, proxy.Health)", gobuf.Quote("sync service"))
	b.L("}")
	b.L("return nil")
	b.L("}")
	b.NL()
}

// syncCheckRegisters is the last paragraph of CheckSyncService's documentation:
// what it hangs the probe on, which depends on whether this project has a
// monitoring page to hang it on at all.
func syncCheckRegisters(monitoring bool) string {
	if monitoring {
		return "It also registers what it found, in the two places it belongs: the " +
			"monitoring page gets the probe itself, so a sync service that comes " +
			"back shows up there without any subscriber having to discover it, and " +
			"— only when this project said it is required — so does the readiness " +
			"check."
	}
	return "When this project said live sync is required, it also registers the " +
		"probe as a readiness check, so an instance that loses the sync service " +
		"is taken out of the load balancer rather than left in it answering " +
		"nothing."
}

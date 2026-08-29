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
		gobuf.Quote("shapes"), shapesConst)
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

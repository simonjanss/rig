package servergo

import (
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
)

// electricModule is the proxy a shape route forwards through. It appears in a
// project's dependency graph only where a table asked for live sync.
const electricModule = "github.com/simonjanss/rig/runtime/electric"

// hasShapes reports whether any table in this project is subscribed to live.
//
// It reads the same field electricgo reads, rather than a `electric:` block of
// its own, because there is not one: live sync is a line per table, and the
// question here is whether any of them said yes.
func (e *emitter) hasShapes() bool {
	for i := range e.doc.API.Resources {
		res := &e.doc.API.Resources[i]
		if res.Electric != nil && res.Storage != nil {
			return true
		}
	}
	return false
}

// electricFile emits the one thing a main function does with the proxy it built
// beyond mounting it.
//
// It is in this package and not in the generated shape package because what it
// registers is a number, and the numbers live here: shapesShutdown is a term of
// ShutdownBudget, and a step registered with a duration the budget did not count
// is exactly the drift these files exist to prevent. It also means the shape
// generator keeps working for a project that does not use this one — it names
// rig/runtime/electric, which is the proxy's own module, and nothing generated.
func (e *emitter) electricFile() (gen.Artifact, error) {
	b := gobuf.New(e.cfg.Package)

	e.shapesAttacher(b)

	return artifact("electric.gen.go", b)
}

// shapesAttacher emits the drain registration.
func (e *emitter) shapesAttacher(b *gobuf.Buf) {
	var (
		servePkg    = b.Import(runtimeModule + "/serve")
		electricPkg = b.Import(electricModule)
	)

	b.Comment("AttachShapes registers the shutdown for live sync, which is the " +
		"whole of what a main function does with the proxy beyond mounting it:\n\n" +
		"\tapi.AttachShapes(app, proxy)\n\n" +
		"Nothing is started, unlike the sweeper and the engine beside it: a shape " +
		"route runs when a browser asks and not before. What there is to register " +
		"is the ending, and it is a drain rather than a close because of what a " +
		"live subscription is — a request the server is deliberately not " +
		"answering yet.\n\n" +
		"That makes it an in-flight request, so http.Server.Shutdown waits for " +
		"it, and waits: nothing in the poll is late, and Shutdown does not cancel " +
		"a request's context. One open tab is a shutdown that spends its budget " +
		"waiting for the sync service to have news, and a sync service that hangs " +
		"rather than refuses is a shutdown that spends all of it — after which " +
		"the flush, the sweep and the notification engine's own close each find a " +
		"deadline that has already passed.\n\n" +
		"A drain step runs first, once readiness is already false, which is the " +
		"point at which there is nothing left to gain from holding a subscription " +
		"open. The subscriber is told to come back and resumes from the same " +
		"offset against a replica that is still serving. Nothing is lost, because " +
		"a poll that had not answered had nothing in it yet.")
	b.L("func AttachShapes(app *%s.App, proxy *%s.Proxy) {", servePkg, electricPkg)
	b.L("app.DrainWithin(%s, %s, proxy.Drain)", gobuf.Quote("shapes"), shapesConst)
	b.L("}")
	b.NL()
}

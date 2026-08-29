package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/pkg/ir"
)

// The order rig's own parts come to exist in is generated, not documented.
//
// It is the whole point of the file. Every call in it was already generated one
// at a time; what a main function still had to get right was which came before
// which — the sweeper before the application's wiring because it needs nothing
// from it, and everything else after, because it needs what the wiring built.
func TestTheSequenceIsGeneratedInOrder(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "presence.ir.json"))
	doc.API.Tracing = &ir.Tracing{Enabled: true, ServiceName: "presence"}
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "run.gen.go")

	steps := []string{
		"StartPresenceSweeper(app)",
		"parts, err := build(ctx, app)",
		"return parts.Handler, nil",
	}
	at := -1
	for _, step := range steps {
		next := strings.Index(src, step)
		if next < 0 {
			t.Fatalf("the sequence is missing %q:\n%s", step, src)
		}
		if next < at {
			t.Errorf("%q comes out of order", step)
		}
		at = next
	}
}

// A Parts with no handler is a server with nothing to serve, and that is the one
// field there is no defensible reason to leave nil.
//
// It fails inside the startup budget, before anything has listened, which is the
// only moment the answer is cheap.
func TestAHandlerlessPartsIsRefused(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "run.gen.go")

	if !strings.Contains(src, `errors.New("api: Parts.Handler is nil: there is nothing to serve")`) {
		t.Errorf("a Parts with no handler is not refused:\n%s", src)
	}
}

// The rest may be nil, and a nil one is said out loud rather than left to be
// discovered.
//
// Requiring them would be wrong rather than strict: `notifications: enabled`
// gives every project a shape over rig_notification_recipient, so a required
// proxy would make an inbox imply a sync service to forward it to. What rig can
// do instead is name what is not running, at the moment it is still cheap to
// fix, which is what [Process.Attach] already does for a page nobody armed.
func TestANilOptionalPartIsSaidRatherThanRefused(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "run.gen.go")

	for _, field := range []string{"Engine", "Auth"} {
		if strings.Contains(src, "Parts."+field+" is nil") {
			t.Errorf("Parts.%s is refused rather than reported", field)
		}
		if !strings.Contains(src, "if parts."+field+" != nil {") {
			t.Errorf("Parts.%s is attached without checking it is there", field)
		}
	}
	for _, said := range []string{
		`"no notification engine in this server"`,
		`"no auth foundation to close"`,
	} {
		if !strings.Contains(src, said) {
			t.Errorf("nothing is said about a nil part: no %s", said)
		}
	}
}

// A field exists exactly when the block that gives it a lifetime is on.
//
// That is what makes the struct a list of what this project has rather than a
// list of what rig can do: turning `notifications:` on adds a field to the one
// function that has to know about it.
func TestPartsHasAFieldPerBlock(t *testing.T) {
	t.Parallel()

	bare := artifactNamed(t, gentest.Run(t, servergo.New(),
		func() *ir.Document {
			doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
			for i := range doc.API.Resources {
				doc.API.Resources[i].Electric = nil
			}
			return doc
		}(), opts()), "run.gen.go")

	for _, absent := range []string{"Engine *notify.Engine", "Auth *auth.Auth"} {
		if strings.Contains(bare, absent) {
			t.Errorf("a project with none of those blocks got %q", absent)
		}
	}
	if !strings.Contains(bare, "Handler http.Handler") {
		t.Error("every project has routes, so every Parts has a handler")
	}

	full := artifactNamed(t, gentest.Run(t, servergo.New(),
		gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json")), opts()), "run.gen.go")
	for _, want := range []string{"Engine *notify.Engine", "Auth *auth.Auth"} {
		if !strings.Contains(full, want) {
			t.Errorf("a project with the block got no field: no %q", want)
		}
	}

	// The live-sync proxy is deliberately not one of them. It is named where it
	// is used — Handlers.Shapes, which mounts the routes and registers their
	// drain in one place — rather than travelling back here as a second thing
	// to remember about the same object.
	if strings.Contains(full, "Shapes *electric.Proxy") {
		t.Error("the proxy should be wired through Handlers.Shapes, not Parts")
	}
}

// The process is built by Main rather than by a main function, which is what
// takes the value that had to straddle serve.Main's two ends out of one.
//
// A project with no `tracing:` block gets a Main with no process in it at all —
// and, as everywhere else in this package, no import of rig/observe.
func TestMainOwnsTheProcess(t *testing.T) {
	t.Parallel()

	withProcess := artifactNamed(t, gentest.Run(t, servergo.New(), monitored(t), opts()), "run.gen.go")

	for _, want := range []string{
		"process, err := NewProcess()",
		"serve.Main(process.Configure(settle(cfg)), process.Mount(build))",
		"func (p *Process) Mount(build Build) serve.Mount {",
		"p.Attach(app)",
	} {
		if !strings.Contains(withProcess, want) {
			t.Errorf("Main does not own the process: no %q", want)
		}
	}

	plain := artifactNamed(t, gentest.Run(t, servergo.New(),
		gentest.LoadDocument(t, filepath.Join("testdata", fixture)), opts()), "run.gen.go")
	for _, absent := range []string{"observe", "NewProcess"} {
		if strings.Contains(plain, absent) {
			t.Errorf("an untraced project got %q in its run.gen.go", absent)
		}
	}
	if !strings.Contains(plain, "serve.Main(settle(cfg), Mount(build))") {
		t.Errorf("an untraced project's Main is not the plain one:\n%s", plain)
	}
}

// What rig.yaml already decided is filled in, and anything already set is left
// alone — so a field is still how a project disagrees.
func TestSettleMergesTheTasksAndNothingElse(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "presence.ir.json"))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "run.gen.go")

	if !strings.Contains(src, "cfg.Tasks = Tasks(cfg.Tasks)") {
		t.Error("settle does not merge the tasks")
	}

	// And fills in nothing else. Every other field serve needs is one the
	// project states, because every one of them is read by something outside
	// this binary — an orchestrator checking a path, a manifest naming a
	// budget — and a value settled here is one nobody can read.
	for _, absent := range []string{"cfg.LivenessPath", "cfg.ReadinessPath", "cfg.MaxShutdown"} {
		if strings.Contains(src[strings.Index(src, "func settle("):], absent) {
			t.Errorf("settle fills in %q", absent)
		}
	}
}

// settle does not touch MaxShutdown, for any project.
//
// It is the one field rig knows the answer to and fills in anyway not at all:
// the number has to be readable in the serve.Config a main function writes,
// because that is where whoever writes terminationGracePeriodSeconds reads it.
// Main refuses one that was left out instead, which is a boot that fails with
// the number to write rather than a deployment that disagrees silently.
func TestSettleNeverFillsInTheShutdownBudget(t *testing.T) {
	t.Parallel()

	for _, name := range []string{fixture, "presence.ir.json"} {
		doc := gentest.LoadDocument(t, filepath.Join("testdata", name))
		src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "run.gen.go")

		settle := src[strings.Index(src, "func settle("):]
		if strings.Contains(settle, "MaxShutdown") {
			t.Errorf("%s: settle fills in MaxShutdown:\n%s", name, settle)
		}
		// And Main refuses it, naming the number this project adds up to.
		for _, want := range []string{"if cfg.MaxShutdown == 0 {", "ShutdownBudget()", "os.Exit(2)"} {
			if !strings.Contains(src, want) {
				t.Errorf("%s: Main does not refuse an unstated MaxShutdown: missing %q", name, want)
			}
		}
	}
}

// The auth cache's channel is closed with the number the budget counts for it,
// which is what took the last hand-written CloseWithin out of every main.
func TestTheAuthCloserIsGeneratedWithTheNumberTheBudgetCounts(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	artifacts := gentest.Run(t, servergo.New(), doc, opts())

	if want := `app.CloseWithin("auth", authShutdown, a.Close)`; !strings.Contains(
		artifactNamed(t, artifacts, "auth.gen.go"), want) {
		t.Errorf("the auth closer is not registered as %s", want)
	}
	if !strings.Contains(artifactNamed(t, artifacts, "process.gen.go"), "authShutdown = 5 * time.Second") {
		t.Error("the constant the closer names is not the one the process file declares")
	}
}

// A project with no tables at all still gets a coherent run.gen.go.
//
// That is the `rig init` case: api.Main is a line the first main function can
// contain, and the first table does not change it.
func TestAProjectWithNoTablesStillGetsMain(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.API.Resources = nil
	doc.Schema.Tables = nil
	doc.Reindex()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "run.gen.go")

	for _, want := range []string{
		"type Parts struct {",
		"Handler http.Handler",
		"func Main(cfg serve.Config, build Build) {",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("an empty project's run.gen.go is missing %q:\n%s", want, src)
		}
	}
}

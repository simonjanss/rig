package servergo_test

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/pkg/ir"
)

// Every project gets the file, even the one with nothing in it but the pruner.
//
// That is what makes `api.Tasks(…)` a line a main function writes once: turning
// `files:` on later changes what the binary can do without changing a line of
// the main that runs it.
func TestEveryProjectGetsTheTaskTable(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	if !strings.Contains(src, "func Tasks(own map[string]serve.Task) map[string]serve.Task {") {
		t.Error("a project with no feature block got no task table")
	}
	if !strings.Contains(src, `"prune-idempotency": IdempotencyPruner(0)`) {
		t.Error("the one task every project has is not in it")
	}
}

// The project's half is merged last, so a name it uses twice is its own.
//
// The alternative — refusing, or letting the generated entry win — would make a
// generated file something a project has to edit to get past, which is the one
// thing rig's generated files are never supposed to be.
func TestTasksLetTheProjectWin(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "presence.ir.json"))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	generated := strings.Index(src, `"sweep-presence"`)
	merge := strings.Index(src, "for name, task := range own {")
	if generated < 0 || merge < 0 || generated > merge {
		t.Error("the application's tasks are merged before the generated ones, so a " +
			"name it uses twice is silently the generated one")
	}
}

// A project that asked for no spans gets the file and no page, no provider and
// no import of rig/observe in it.
//
// The rule the four conditional files beside it keep by not existing, this one
// has to keep by what is in it.
func TestProcessIsAbsentWithoutTracing(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	for _, absent := range []string{"observe", "type Process struct", "func NewProcess"} {
		if strings.Contains(src, absent) {
			t.Errorf("a project with no tracing block got %q in its process.gen.go", absent)
		}
	}
}

// The budget is the sum of the steps rig registers plus what is left for the
// requests still in flight — and the sentence that states the total is written
// from the same parts.
//
// The sentence is the point of the test. It is what an operator copies into
// terminationGracePeriodSeconds without running the binary, so a comment that
// says a number the body does not produce is worse than no comment at all.
func TestTheShutdownBudgetIsTheSumOfItsSteps(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	doc.API.Tracing = &ir.Tracing{Enabled: true, ServiceName: "notify"}
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	body := regexp.MustCompile(`func ShutdownBudget\(\) time\.Duration \{\n\treturn ([^\n]+)\n\}`).
		FindStringSubmatch(src)
	if body == nil {
		t.Fatalf("no ShutdownBudget to read:\n%s", src)
	}

	// 5s for the flush, 15s for the engine, 5s for the shapes this fixture
	// syncs live, 5s for the auth cache it authenticates through, 10s left over.
	for _, want := range []string{
		"tracesShutdown", "notificationsShutdown", "shapesShutdown", "authShutdown", "shutdownHeadroom",
	} {
		if strings.Count(body[1], want) != 1 {
			t.Errorf("the budget does not count %s exactly once: %q", want, body[1])
		}
	}
	if strings.Contains(body[1], "presenceShutdown") {
		t.Error("the budget counts a step this project does not register")
	}
	if !strings.Contains(src, "For this project that is 40s:") {
		t.Errorf("the stated total is not the one the body sums:\n%s", src)
	}
}

// A project with none of rig's own closers still gets a budget, and it is the
// headroom on its own.
//
// This used to be the opposite. The reasoning was that a headroom-only budget
// would be shorter than serve's default, so a project with closers of its own
// would get a tighter budget for having asked rig for nothing — which was true
// while serve had a default. It has none now: MaxShutdown is stated or the
// server does not start, so a project with no rig step is the one that most
// needs somewhere to read a number from. Emitting none would leave `rig init`
// producing something that refuses to boot and offers nothing to copy.
func TestAProjectWithNoRigStepStillGetsAHeadroomBudget(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	// The fixture syncs a table live, and that is a step: what this test is
	// about is a project with none at all, so the one it has is taken away.
	for i := range doc.API.Resources {
		doc.API.Resources[i].Electric = nil
	}
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	for _, want := range []string{"func ShutdownBudget()", "shutdownHeadroom", "For this project that is 10s:"} {
		if !strings.Contains(src, want) {
			t.Errorf("a project that registers no closer of rig's is missing %q:\n%s", want, src)
		}
	}
	if strings.Contains(src, "shapesShutdown") {
		t.Error("the budget counts a step this project does not register")
	}
}

// A live subscription is drained rather than closed, and with the number the
// budget counted for it.
//
// The order is the whole of it. A poll the server is still holding is an
// in-flight request, so it has to end before the server stops accepting rather
// than after — a close step would run once http.Server.Shutdown had already
// spent the budget waiting for the very polls it was supposed to release.
func TestTheShapeDrainIsRegisteredWithTheNumberTheBudgetCounts(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servergo.New(), doc, opts())

	// In Register, beside the routes it is the ending of, rather than in a
	// second call an application makes with the same proxy.
	if want := `h.Shapes.App.DrainWithin("shapes", shapesShutdown, h.Shapes.Proxy.Drain)`; !strings.Contains(
		artifactNamed(t, artifacts, "server.gen.go"), want) {
		t.Errorf("the shape drain is not registered as %s", want)
	}
	if body := artifactNamed(t, artifacts, "process.gen.go"); !strings.Contains(body, "shapesShutdown = 5 * time.Second") {
		t.Error("the constant the drain names is not the one the process file declares")
	}
}

// A project with no table synced live gets no shape file and no shape constant.
func TestNoLiveSyncMeansNoShapeShutdown(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	for i := range doc.API.Resources {
		doc.API.Resources[i].Electric = nil
	}
	artifacts := gentest.Run(t, servergo.New(), doc, opts())

	for _, a := range artifacts {
		if a.Path == "electric.gen.go" {
			t.Error("a project that syncs nothing live got a shape shutdown to register")
		}
	}
	if strings.Contains(artifactNamed(t, artifacts, "process.gen.go"), "shapesShutdown") {
		t.Error("the budget counts a drain this project does not register")
	}
}

// The constant a step is registered with and the constant the budget counts for
// it are one constant.
//
// Two literals that have to agree is the arithmetic this whole file exists to
// stop a main function doing by hand; doing it in the generated source instead
// would only have moved it.
func TestAStepIsRegisteredWithTheNumberTheBudgetCounts(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "presence.ir.json"))
	artifacts := gentest.Run(t, servergo.New(), doc, opts())

	if want := `app.CloseWithin("presence", presenceShutdown, sweeper.Close)`; !strings.Contains(
		artifactNamed(t, artifacts, "presence.gen.go"), want) {
		t.Errorf("the sweeper is not registered with %q", want)
	}
	if !strings.Contains(artifactNamed(t, artifacts, "process.gen.go"), "presenceShutdown = 5 * time.Second") {
		t.Error("the constant the sweeper is registered with is not declared beside the budget")
	}
}

// The three lines a main used to copy out of a doc comment are a function.
func TestTheBackgroundLoopsAreStarted(t *testing.T) {
	t.Parallel()

	presence := artifactNamed(t, gentest.Run(t, servergo.New(),
		gentest.LoadDocument(t, filepath.Join("testdata", "presence.ir.json")), opts()), "presence.gen.go")
	for _, want := range []string{
		"func StartPresenceSweeper(app *serve.App) {",
		"sweeper := NewPresenceSweeper(NewPresence(app.Pool))",
		"sweeper.Start()",
	} {
		if !strings.Contains(presence, want) {
			t.Errorf("the presence sweeper's wiring does not contain %q", want)
		}
	}

	notify := artifactNamed(t, gentest.Run(t, servergo.New(),
		gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json")), opts()), "notifications.gen.go")
	for _, want := range []string{
		"func StartNotificationEngine(app *serve.App, engine *notify.Engine) {",
		"engine.Start()",
		`app.Drain("notifications", engine.StopClaiming)`,
		`app.CloseWithin("notifications", notificationsShutdown, engine.Close)`,
	} {
		if !strings.Contains(notify, want) {
			t.Errorf("the engine's wiring does not contain %q", want)
		}
	}
}

// A caller's own request identifier reaches an error body and every log line
// this request writes, so it is bounded and checked before it is believed.
//
// Refusing rather than truncating: a header this API does not understand is not
// one it should half-quote into a log line.
func TestTheCallersRequestIDIsBounded(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "server.gen.go")

	// The bound itself is runtime/apibase's and is tested there. What this
	// generator owes is the header this project names, handed over so the shared
	// plumbing reads the right one.
	for _, want := range []string{
		`const RequestIDHeader = "X-Request-Id"`,
		"h.Server.RequestIDHeader = RequestIDHeader",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the request identifier header is not wired: no %q", want)
		}
	}

	// Untraced, so nothing hands the plumbing a trace to fall back to and the
	// caller's own is the only identifier there is.
	if strings.Contains(src, "h.Server.Tracer") {
		t.Errorf("an untraced project should wire no tracer:\n%s", src)
	}
}

// A project with no tables at all still gets a coherent process.gen.go.
//
// That is the `rig init` case: the file is written before there is anything to
// serve, so `api.Tasks(…)` is a line the first main function can contain and
// the first table does not change it.
func TestAProjectWithNoTablesStillGetsTheFile(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.API.Resources = nil
	doc.Schema.Tables = nil
	doc.Reindex()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	if !strings.Contains(src, `"prune-idempotency": IdempotencyPruner(0)`) {
		t.Errorf("an empty project's task table is not the one every project has:\n%s", src)
	}
}

// The one thing above a deployment can disagree with is a struct with a field
// per step this project registers, and no others.
//
// A map keyed on the names serve matches would have done the same job and would
// have made a misspelling a number nobody read. The names stay strings inside
// the generated file, which is where they already were; what a main function
// writes is a field, and what a field costs to get wrong is a compilation.
func TestTheShutdownSetHasAFieldPerStepAndNoOthers(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	doc.API.Tracing = &ir.Tracing{Enabled: true, ServiceName: "notify"}
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	body := regexp.MustCompile(`type Shutdown struct \{\n((?s).*?)\n\}`).FindStringSubmatch(src)
	if body == nil {
		t.Fatalf("no Shutdown to read:\n%s", src)
	}
	for _, want := range []string{"Traces", "Notifications", "Shapes", "Auth"} {
		if !strings.Contains(body[1], want+" time.Duration") {
			t.Errorf("a step this project registers has no field: %s", want)
		}
	}
	// The fixture has no presence, so there is no way to write a number for a
	// sweeper this server does not run.
	if strings.Contains(body[1], "Presence") {
		t.Errorf("a step this project does not register is settable:\n%s", body[1])
	}

	// And each field reaches the name that step is registered under, which is
	// the only place those two spellings meet.
	for field, name := range map[string]string{
		"Traces": "traces", "Notifications": "notifications", "Shapes": "shapes", "Auth": "auth",
	} {
		want := `steps = append(steps, serve.Step{Name: "` + name + `", Timeout: s.` + field + `})`
		if !strings.Contains(src, want) {
			t.Errorf("Steps does not carry %s across: want %q", field, want)
		}
	}
}

// Budget is the same sum ShutdownBudget is, with whatever was set in place of
// what it replaced — and the headroom, which is not a step and not this
// struct's to shorten.
func TestTheShutdownSetsBudgetIsTheSameSum(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "presence.ir.json"))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	body := regexp.MustCompile(`func \(s Shutdown\) Budget\(\) time\.Duration \{\n\treturn ([^\n]+)\n\}`).
		FindStringSubmatch(src)
	if body == nil {
		t.Fatalf("no Shutdown.Budget to read:\n%s", src)
	}
	if !strings.Contains(body[1], "cmp.Or(s.Presence, presenceShutdown)") {
		t.Errorf("the budget does not fall back to the generated number: %q", body[1])
	}
	if !strings.HasSuffix(body[1], "+ shutdownHeadroom") {
		t.Errorf("the headroom is not in the budget, or is not last: %q", body[1])
	}
	if strings.Contains(src, "Headroom time.Duration") {
		t.Error("the headroom is settable, and it is not a step")
	}
}

// A project with no step of rig's gets no set either. There would be no field
// to put in it.
func TestAProjectWithNoRigStepGetsNoShutdownSet(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for i := range doc.API.Resources {
		doc.API.Resources[i].Electric = nil
	}
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	if strings.Contains(src, "type Shutdown struct") {
		t.Errorf("a project with nothing to size got a set to size it with:\n%s", src)
	}
}

// The flush is the one step with a half that never sees an App, so it is the
// one that has to be told twice.
//
// Attach registers the server's half on an App, which is where serve applies
// what Config.Shutdown said. Close is what a `Tasks:` entry reaches — no mount,
// no App, nowhere for that to have happened — so Configure reads the same
// number on the way past and both halves spend it.
func TestTheFlushIsSizedOnBothOfItsHalves(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.API.Tracing = &ir.Tracing{Enabled: true, ServiceName: "traced"}
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	// Aligned by gofmt against whatever else is in the literal, so the field
	// and its value are matched rather than the spacing between them.
	if !regexp.MustCompile(`traces:\s+tracesShutdown,`).MatchString(src) {
		t.Errorf("the flush does not start at the generated number:\n%s", src)
	}
	for _, want := range []string{
		"for _, s := range cfg.Shutdown.Steps() {",
		`if s.Name == "traces" && s.Timeout > 0 {`,
		"p.traces = s.Timeout",
		`app.CloseWithin("traces", p.traces, p.tracing.Shutdown)`,
		"flushing, cancel := context.WithTimeout(context.Background(), p.traces)",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the flush's two halves are not sized together: missing %q\n%s", want, src)
		}
	}
}

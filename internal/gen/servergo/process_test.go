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

	// 5s for the flush, 15s for the engine, 10s left over.
	for _, want := range []string{"tracesShutdown", "notificationsShutdown", "shutdownHeadroom"} {
		if strings.Count(body[1], want) != 1 {
			t.Errorf("the budget does not count %s exactly once: %q", want, body[1])
		}
	}
	if strings.Contains(body[1], "presenceShutdown") {
		t.Error("the budget counts a step this project does not register")
	}
	if !strings.Contains(src, "For this project that is 30s:") {
		t.Errorf("the stated total is not the one the body sums:\n%s", src)
	}
}

// A project with none of rig's own closers has no budget to state, and states
// its own MaxShutdown instead.
//
// Emitting one that was only the headroom would be worse than emitting none: it
// would be shorter than serve's own default, so a project with closers of its
// own would get a tighter budget for having asked rig for nothing.
func TestAProjectWithNoRigStepGetsNoBudget(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := artifactNamed(t, gentest.Run(t, servergo.New(), doc, opts()), "process.gen.go")

	for _, absent := range []string{"ShutdownBudget", "shutdownHeadroom"} {
		if strings.Contains(src, absent) {
			t.Errorf("a project that registers no closer of rig's got %q", absent)
		}
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

	for _, want := range []string{
		`const RequestIDHeader = "X-Request-Id"`,
		"const maxRequestIDBytes = 128",
		"func callerRequestID(r *http.Request) string {",
		"if id == \"\" || len(id) > maxRequestIDBytes {",
		"if id[i] < 0x20 || id[i] > 0x7e {",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the request identifier is taken on trust: no %q", want)
		}
	}

	// Untraced, so the caller's own is the only identifier there is — and it is
	// still the default rather than something a main has to wire.
	if !strings.Contains(src, "rc.RequestID = callerRequestID(r)") {
		t.Errorf("a project with no RequestID of its own reads no header:\n%s", src)
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

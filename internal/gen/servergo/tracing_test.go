package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/internal/gen/servicego"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// traced is the same fixture with the block on, because the difference between
// a project that traces and one that does not should be exactly that: one flag
// in rig.yaml and nothing else about the project.
func traced(t *testing.T) *ir.Document {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.API.Tracing = &ir.Tracing{Enabled: true, ServiceName: "lifecycle"}
	return doc
}

func TestTracingGolden(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, servergo.New(), traced(t), opts())
	gentest.Golden(t, filepath.Join("testdata", "tracing"), artifacts, *update)
}

// Optional means absent. A project that did not ask to be traced has no import
// of rig/observe anywhere in its API package — which is what keeps otel out of
// its go.mod, the same way a project without an auth block keeps argon2 out.
func TestWithoutTheBlockNothingNamesObserve(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for _, a := range gentest.Run(t, servergo.New(), doc, opts()) {
		if a.Path == "tracing.gen.go" {
			t.Error("an untraced project got a tracing.gen.go")
		}
		if strings.Contains(string(a.Content), "rig/observe") {
			t.Errorf("%s names rig/observe in a project that asked for no spans", a.Path)
		}
	}
}

// Nothing registers on the mux directly. A span opened inside each generated
// handler covered only the routes a generator wrote and left the mounted ones —
// auth, the shapes, the OpenAPI document — with no span at all; a route
// registered through the router gets one whoever registered it.
func TestEveryRouteIsRegisteredThroughTheTracingRouter(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), traced(t), opts()), "server.gen.go")

	body, ok := between(src, "func Register(", "\n}")
	if !ok {
		t.Fatal("no Register function")
	}

	if !strings.Contains(body, "routes := apibase.Tracing(mux, h.Server.Tracer)") {
		t.Errorf("Register builds no tracing router, so nothing opens a span:\n%s", body)
	}

	for _, want := range []string{
		"registerLesson(routes,",
		"routes.HandleFunc(\"GET /api/v1/lesson/_stream\"",
		"h.Server.Auth.Mount(routes)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("not registered through the router, so it is untraced:\n%s", want)
		}
	}

	// And the mux itself is what comes back, so a caller can still mount its
	// own catch-all on it — which is the thing a handler wrapped around the mux
	// would have taken for itself.
	if !strings.Contains(body, "return mux") {
		t.Error("Register no longer returns the mux it registered on")
	}
}

// A generated handler names no tracing library of its own. Since the wrapper
// took the job over, a route file that still imported rig/observe would be a
// second span nested inside the first on exactly the routes a generator wrote.
func TestAHandlerOpensNoSpanOfItsOwn(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), traced(t), opts()), "lesson_routes.gen.go")

	if strings.Contains(src, "observe.") {
		t.Errorf("a generated handler still opens its own span:\n%s", src)
	}
}

// With tracing on and no RequestID of the project's own, the identifier in the
// error body is the caller's own if it sent one worth trusting, and this
// request's trace otherwise. That is the whole correlation story, and nobody has
// to write it.
//
// The order is the point rather than the fallback: a client that labelled its
// own request is believed, because it is the one correlating two sides. Only a
// request nobody named gets a name invented for it.
func TestTheRequestIDFallsBackToTheTrace(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), traced(t), opts()), "server.gen.go")

	// One field carries both: what to label a request nobody labelled, and which
	// span to redden when one fails. The order between the caller's own header
	// and the trace is runtime/apibase's, and is tested there — what a traced
	// project owes is handing it somewhere to ask.
	if !strings.Contains(src, "h.Server.Tracer = observe.APITracer{}") {
		t.Errorf("no tracer for the shared plumbing to ask:\n%s", src)
	}
}

// The configuration a main function hands to observe.Setup, with the name from
// rig.yaml so that nothing is typed twice.
func TestTracingConfigCarriesTheServiceName(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), traced(t), opts()), "tracing.gen.go")

	if !strings.Contains(src, `observe.Config{ServiceName: "lifecycle"}`) {
		t.Errorf("the generated configuration does not name the service:\n%s", src)
	}
}

// The check that matters most: a span opened on a path that returns early, or a
// helper called with the wrong arguments, is a compile error rather than a
// golden diff nobody reads.
func TestTracedCodeCompiles(t *testing.T) {
	t.Parallel()

	doc := traced(t)

	api := gentest.Run(t, servicego.New(), doc, gen.Options{Raw: map[string]any{
		"package": "api", "model_import": "rigtest/model", "store_import": "rigtest/store",
	}})
	api = append(api, gentest.Run(t, servergo.New(), doc, opts())...)

	gentest.MustCompileAll(t,
		gentest.Package{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		gentest.Package{
			Dir: "store",
			Artifacts: gentest.Run(t, persistgo.New(), doc, gen.Options{Raw: map[string]any{
				"package": "store", "model_import": "rigtest/model",
			}}),
		},
		gentest.Package{Dir: "api", Artifacts: api},
	)
}

// artifactNamed is one generated file by base name.
func artifactNamed(t *testing.T, artifacts []gen.Artifact, name string) string {
	t.Helper()

	for _, a := range artifacts {
		if filepath.Base(a.Path) == name {
			return string(a.Content)
		}
	}
	t.Fatalf("no %s among the generated files", name)
	return ""
}

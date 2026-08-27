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

// The span is opened inside the handler and ended by a defer. Both halves
// matter: inside, because the route is only known once the mux has dispatched,
// and deferred, because a handler has a dozen ways out and none of them should
// have to remember.
func TestTheRequestSpanIsNamedByTheRouteAndDeferred(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, servergo.New(), traced(t), opts()), "lesson_routes.gen.go")

	if !strings.Contains(src, `observe.Server(r, "DELETE /api/v1/lessons/{id}", rec.Status)`) {
		t.Errorf("the read handler does not open a span named by its route:\n%s", src)
	}
	if !strings.Contains(src, "defer span.End()") {
		t.Error("the span is not ended by a defer")
	}

	// Before prepare, so a caller refused by a pre-hook, by the revision check
	// or by a permission is inside the span rather than invisible to it.
	span := strings.Index(src, "observe.Server(r,")
	prepare := strings.Index(src, "prepare(s, w, r)")
	if span < 0 || prepare < 0 || span > prepare {
		t.Error("the span is opened after prepare, so a refusal happens outside it")
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

	if !strings.Contains(src, "rc.RequestID = cmp.Or(callerRequestID(r), observe.TraceID(r))") {
		t.Errorf("no fallback to the trace id:\n%s", src)
	}
	if !strings.Contains(src, "observe.Fail(r.Context(), code.HTTPStatus(), err)") {
		t.Error("a failure is not recorded on the span")
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

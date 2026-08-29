package persistgo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// traced is the same fixture with the block on, so the difference between a
// project that traces and one that does not is exactly one flag in rig.yaml.
func traced(t *testing.T) *ir.Document {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	doc.API.Tracing = &ir.Tracing{Enabled: true, ServiceName: "lifecycle"}
	return doc
}

func TestTracingGolden(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, persistgo.New(), traced(t), opts())
	gentest.Golden(t, filepath.Join("testdata", "tracing"), artifacts, *update)
}

// Optional means absent: no import, no field, no span in a project that asked
// for none.
func TestWithoutTheBlockNothingNamesObserve(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	for _, a := range gentest.Run(t, persistgo.New(), doc, opts()) {
		src := string(a.Content)
		if strings.Contains(src, "rig/observe") || strings.Contains(src, "opentelemetry") {
			t.Errorf("%s names a tracing library in a project that asked for no spans", a.Path)
		}
	}
}

// One span per function, ended by a defer. A repository method has a dozen ways
// out — a refused claim, a missing row, a failed statement — and none of them
// should have to remember to close anything.
func TestEveryMethodOpensOneSpanAndDefersIt(t *testing.T) {
	t.Parallel()

	src := repositorySource(t)

	for _, name := range []string{"Get", "List", "Create", "Update", "Delete", "Restore"} {
		want := `r.db.tracer.Start(ctx, "repository.Lesson.` + name + `")`
		if !strings.Contains(src, want) {
			t.Errorf("%s opens no span of its own", name)
		}
	}

	starts := strings.Count(src, "r.db.tracer.Start(ctx,")
	ends := strings.Count(src, "defer span.End()")
	if starts != ends {
		t.Errorf("%d spans opened directly and %d deferred ends; every one of them is a "+
			"function's span and ends with the function", starts, ends)
	}
}

// A stage is a callback, which is what makes its span a function's span: opened
// and ended in the one helper, with nothing at the call site holding one.
func TestEachHookIsAStageOfItsOwn(t *testing.T) {
	t.Parallel()

	src := repositorySource(t)

	for _, want := range []string{
		`r.trace(ctx, "repository.Lesson.Create.Validator"`,
		`r.trace(ctx, "repository.Lesson.Create.Before"`,
		`r.trace(ctx, "repository.Lesson.Create.After"`,
		`r.trace(ctx, "repository.Lesson.Update.Before"`,
		`r.trace(ctx, "repository.Lesson.Update.Validator"`,
		`r.trace(ctx, "repository.Lesson.Delete.Before"`,
		`r.trace(ctx, "repository.Lesson.Restore.Validator"`,
	} {
		if !strings.Contains(src, want) {
			t.Errorf("no stage span for %s", want)
		}
	}

	// The callback takes the context, so what the hook does — a query, another
	// repository call — lands under the stage rather than beside it.
	if !strings.Contains(src, "func(ctx context.Context) error {") {
		t.Error("a stage does not hand its own context to the hook")
	}
}

// The method that registered the callback has returned by the time it runs, so
// its span is closed. This one is opened inside, under the transaction's
// context, which is what keeps it under the request that caused it.
func TestAfterCommitOpensItsSpanInsideTheCallback(t *testing.T) {
	t.Parallel()

	src := repositorySource(t)

	i := strings.Index(src, "dbx.AfterCommit(ctx, func() {")
	if i < 0 {
		t.Fatal("no after-commit callback")
	}
	rest := src[i:]
	span := strings.Index(rest, `r.db.tracer.Start(ctx, "repository.Lesson.Create.AfterCommit")`)
	done := strings.Index(rest, "done(ctx, who,")
	if span < 0 || done < 0 || span > done {
		t.Error("the after-commit span is not opened inside the callback, before the hook runs")
	}
}

// The tracer is handed to the store rather than reached for, which is the rule
// the logger already follows. Nil is settled once, in New, so no repository has
// to ask whether it has one.
func TestTheTracerIsAField(t *testing.T) {
	t.Parallel()

	src := artifactNamed(t, gentest.Run(t, persistgo.New(), traced(t), opts()), "store.gen.go")

	if !strings.Contains(src, "Tracer trace.Tracer") {
		t.Error("Config takes no tracer")
	}
	if !strings.Contains(src, "if cfg.Tracer == nil {") {
		t.Error("a nil tracer is not settled in New")
	}
	if !strings.Contains(src, "observe.Tracer()") {
		t.Error("the fallback is not rig's own tracer")
	}
}

// The check a golden cannot make: a span opened on a path that returns early,
// or a closure that captures the wrong context, is a compile error.
func TestTracedCodeCompiles(t *testing.T) {
	t.Parallel()

	doc := traced(t)
	gentest.MustCompileAll(t,
		gentest.Package{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		gentest.Package{Dir: pkg, Artifacts: gentest.Run(t, persistgo.New(), doc, opts())},
	)
}

func repositorySource(t *testing.T) string {
	t.Helper()
	return artifactNamed(t, gentest.Run(t, persistgo.New(), traced(t), opts()), "lesson_repository.gen.go")
}

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

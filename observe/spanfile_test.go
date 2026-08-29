package observe_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/simonjanss/rig/observe"
)

// The file has a ceiling and one generation behind it, so the disk cost is
// twice the cap and not "however long the process ran".
func TestTheSpanFileRotates(t *testing.T) {
	const limit = 4 << 10

	path := spanFile(t)
	p := setup(t, observe.Config{ServiceName: "todo", File: path, FileMaxBytes: limit})

	for i := range 200 {
		_, span := observe.Tracer().Start(t.Context(), "repository.Todo.Get."+strconv.Itoa(i))
		span.End()
	}
	flush(t, p)

	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if current.Size() > limit {
		t.Errorf("the span file is %d bytes, over its %d limit", current.Size(), limit)
	}

	rotated, err := os.Stat(path + ".1")
	if err != nil {
		t.Fatalf("nothing was rotated aside, so 200 spans fit in %d bytes: %v", limit, err)
	}
	if rotated.Size() > limit {
		t.Errorf("the rotated file is %d bytes, over its %d limit", rotated.Size(), limit)
	}

	// What survived is the recent end. A file that kept the first few hundred
	// spans and dropped everything since would be exactly the wrong half.
	spans := readSpans(t, path)
	if len(spans) == 0 {
		t.Fatal("the current file is empty")
	}
	if last := spans[len(spans)-1].Name; last != "repository.Todo.Get.199" {
		t.Errorf("the newest span in the file is %q", last)
	}
}

// A run that finds a file from a previous run appends to it, and the ceiling is
// on the file rather than on this process's share of it.
func TestTheSpanFileAppends(t *testing.T) {
	path := spanFile(t)

	first := setup(t, observe.Config{ServiceName: "todo", File: path})
	_, span := observe.Tracer().Start(t.Context(), "before the restart")
	span.End()
	flush(t, first)

	second := setup(t, observe.Config{ServiceName: "todo", File: path})
	_, span = observe.Tracer().Start(t.Context(), "after the restart")
	span.End()
	flush(t, second)

	spans := readSpans(t, path)
	if len(spans) != 2 {
		t.Fatalf("want both runs' spans, got %d", len(spans))
	}
	if spans[0].Name != "before the restart" {
		t.Errorf("the first run's span is gone: %q", spans[0].Name)
	}
}

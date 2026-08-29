package gentest_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every generator is tested against a copy of a compiler document, and each of
// those documents is the compiler's own golden output.
//
// A copy that drifts is the worst kind of green suite: the generators keep
// passing against an IR the compiler stopped producing, and the first thing to
// notice is a real project. This is what caught four endpoints that had been
// added to the compiler and reached no generator fixture at all.
//
// The name pairs the two: internal/gen/*/testdata/<case>.ir.json must equal
// internal/compile/testdata/<case>/ir.golden.json. To refresh, run
// `make update-golden` and copy the compiler's file over each of these.
func TestTheGeneratorFixturesAreTheCompilersOutput(t *testing.T) {
	t.Parallel()

	copies, err := filepath.Glob(filepath.Join("..", "*", "testdata", "*.ir.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) == 0 {
		t.Fatal("no generator fixtures found; this test is watching nothing")
	}

	for _, path := range copies {
		source := filepath.Join("..", "..", "compile", "testdata",
			strings.TrimSuffix(filepath.Base(path), ".ir.json"), "ir.golden.json")

		want, err := os.ReadFile(source)
		if err != nil {
			t.Errorf("%s: %v\nA generator fixture must be named after the compiler "+
				"case it was copied from.", path, err)
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s has drifted from the compiler's golden document.\n"+
				"Copy %s over it.", path, source)
		}
	}
}

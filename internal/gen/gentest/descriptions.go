package gentest

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// DescriptionsSurvive asserts that a generator carried the document's own words
// into its output.
//
// ir.Field.Description is the single copy of the human-readable text, and the
// point of that is that one explanation reaches every surface: the Go struct,
// the OpenAPI schema, the TypeScript client. Nothing enforces it, though — a
// generator that emits a field and drops its description produces output that
// compiles, matches its golden, and quietly tells a reader less than the
// document knows. That is what this catches.
//
// Every generator with somewhere to put a description calls it. The argument
// says how that output declares a shape, since only the shapes this generator
// actually emitted can be held to it:
//
//	gentest.DescriptionsSurvive(t, doc, artifacts, func(name string) string {
//		return "type " + name + " struct"     // Go
//		return "export interface " + name     // TypeScript
//		return "\"" + name + \"\": {"         // an OpenAPI component
//	})
//
// Matching ignores whitespace, so a wrapped comment counts and a generator is
// free to lay one out however its formatter likes. It does not ignore words: a
// paraphrase fails, which is the intent.
//
// Adding to the description passes — service-go's page of rows says what the
// document says and then a sentence about why the rows are pointers, which is
// true of Go and of nothing else. That is the shape to reach for when an output
// needs to say more: the shared words, then the local ones. Replacing the
// shared words with better ones means editing the document, where every output
// will pick them up.
func DescriptionsSurvive(t *testing.T, doc *ir.Document, artifacts []gen.Artifact, declares func(name string) string) {
	t.Helper()

	var all strings.Builder
	for _, a := range artifacts {
		all.Write(a.Content)
		all.WriteString("\n")
	}
	// Comment markers are the one thing that may sit between the words, so they
	// come out before whitespace is collapsed.
	haystack := flatten(strings.NewReplacer("//", " ", "/*", " ", "*/", " ", "*", " ", "#", " ").
		Replace(all.String()))

	var checked int
	for _, obj := range doc.API.Objects {
		if !strings.Contains(haystack, flatten(declares(obj.Name))) {
			continue // a shape this generator does not emit
		}

		if obj.Description != "" {
			checked++
			if !strings.Contains(haystack, flatten(obj.Description)) {
				t.Errorf("%s: the object's description did not reach the output:\n%s",
					obj.Name, obj.Description)
			}
		}

		for _, f := range obj.Fields {
			if f.Description == "" {
				continue
			}
			checked++
			if !strings.Contains(haystack, flatten(f.Description)) {
				t.Errorf("%s.%s: the field's description did not reach the output:\n%s",
					obj.Name, f.Name, f.Description)
			}
		}
	}

	// A generator whose output declares none of the document's shapes has
	// nothing to say about descriptions, and a test that checked nothing should
	// say so rather than pass.
	if checked == 0 {
		t.Error("no described shape from the document was found in the output; " +
			"either the declaration marker is wrong or this generator has no place " +
			"for a description and should not call this")
	}
}

// flatten reduces text to its words, so that a description matches however the
// output chose to wrap it.
func flatten(s string) string { return strings.Join(strings.Fields(s), " ") }

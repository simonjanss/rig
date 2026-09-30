package tsclient_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/tsclient"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The browser half of a declared endpoint that carries files.
//
// It bites here the way it bites in Go: a generator that knows nothing about a
// form emits a JSON-only method, which calls the right route and sends none of
// the files. The difference is the shape of the fix — the front end has one
// method rather than two, because this endpoint never had a version without its
// files for anybody to have written down.
func TestADeclaredEndpointSendsItsFilesAsAForm(t *testing.T) {
	t.Parallel()

	artifacts := submitArtifacts(t)
	input := collapse(artifactNamed(t, artifacts, "lesson_input.gen.ts"))
	client := collapse(artifactNamed(t, artifacts, "lesson_client.gen.ts"))

	// One member per part, and which is which is a compile error rather than a
	// 422: a list where the part repeats, a plain member where it has to be
	// there, and an optional one where it need not.
	for _, want := range []string{
		"export type LessonSubmitFiles = {",
		"attachments: Upload[];",
		"cover?: Upload;",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("the input module should contain %q", want)
		}
	}

	for _, want := range []string{
		// Beside the body rather than instead of it, and one method rather than
		// two.
		"submit(input: LessonSubmitBody, files: LessonSubmitFiles, options?: CallOptions)",
		// The body goes in the json part, which `multipart` writes first
		// because the server reads the parts in order.
		"const form = multipart(input, [",
		// A part that repeats is spread into one entry per file, under the one
		// name — which is what the server's part loop reads.
		`...files.attachments.map((file): [string, Upload] => ["attachments", file]),`,
		`["cover", files.cover],`,
		"form,",
	} {
		if !strings.Contains(client, want) {
			t.Errorf("the client module should contain %q", want)
		}
	}

	if strings.Contains(client, "submitWithFiles") {
		t.Error("a declared endpoint has one method, which takes its files")
	}
}

func submitArtifacts(t *testing.T) []gen.Artifact {
	t.Helper()

	// The lifecycle fixture, whose one table has no file column anywhere — so
	// everything asserted above is there because the endpoint declared its
	// parts and for no other reason.
	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("the lifecycle fixture has no Lesson resource")
	}
	res.Endpoints = append(res.Endpoints, submitEndpoint())
	doc.Reindex()

	return gentest.Run(t, tsclient.New(), doc, opts())
}

func artifactNamed(t *testing.T, artifacts []gen.Artifact, name string) string {
	t.Helper()
	for _, a := range artifacts {
		if filepath.Base(a.Path) == name {
			return string(a.Content)
		}
	}
	t.Fatalf("no artifact named %s", name)
	return ""
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

// submitEndpoint is what `file_parts:` on a declared endpoint compiles to: a
// body, a part that may repeat and may be left out, and one that may be left
// out and cannot repeat.
func submitEndpoint() ir.Endpoint {
	return ir.Endpoint{
		Name:        "Submit",
		Method:      "POST",
		Path:        "/_submit",
		Pattern:     "POST /api/v1/lessons/_submit",
		OperationID: "submitLesson",
		Summary:     "Submit a lesson.",
		Request: ir.EndpointRequest{
			ContentTypes: []string{ir.MediaJSON, ir.MediaMultipart},
			BodyParams: []ir.Field{{
				Name: "Note", Wire: "note", Type: ir.TypeString,
				TypeKind: ir.TypeKindPrimitive, GoType: "string",
			}},
			FileParts: []ir.FilePart{
				{Name: "attachments", Field: "Attachments",
					Description: "Whatever was dragged in.", Array: true},
				{Name: "cover", Field: "Cover",
					Description: "The one image this is shown by."},
			},
		},
		Responses: []ir.EndpointResponse{{
			StatusCode: 201, ContentTypes: []string{ir.MediaJSON},
			BodyObject: "Lesson", Description: "The lesson.",
		}},
		Errors: []int{400, 401, 403, 413, 415, 429, 500},
		Impl: ir.EndpointImpl{
			Kind:          ir.EndpointCustom,
			ServiceMethod: "Submit",
			HandlerName:   "SubmitLesson",
		},
	}
}

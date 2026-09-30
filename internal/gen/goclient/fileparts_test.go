package goclient_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/goclient"
	"github.com/simonjanss/rig/pkg/ir"
)

// The SDK half of a declared endpoint that carries files.
//
// A client generator that knows nothing about a form emits a JSON-only method
// for such an endpoint: something that compiles, calls the right route, and
// sends none of the files. The route is right and the request is wrong, so the
// failure arrives as a 422 about a part nobody sent rather than as anything a
// reader would connect to the method they called. That is why the SDKs are half
// of this feature rather than a milestone after it.
func TestADeclaredEndpointSendsItsFilesAsAForm(t *testing.T) {
	t.Parallel()

	client := collapse(submitClient(t))

	for _, want := range []string{
		// One member per part, and what each one is is a compile error rather
		// than a 422: a list where the part repeats, a plain Upload where it
		// has to be there, and a pointer where it need not.
		"type LessonSubmitFiles struct {",
		"Attachments []rigclient.Upload",
		"Cover *rigclient.Upload",
		// Beside the body rather than instead of it.
		"func (c *LessonClient) Submit(ctx context.Context, in LessonSubmitBody, files LessonSubmitFiles, opts ...rigclient.CallOption)",
		// The body goes in the json part, which is the part the server reads it
		// from.
		"Multipart: &rigclient.Multipart{JSON: in},",
		// A part that repeats is one part per file, under the one name.
		`for _, f := range files.Attachments { op.Multipart.Files = append(op.Multipart.Files, rigclient.Part("attachments", f)) }`,
		// And an optional one is left out rather than sent empty.
		`if files.Cover != nil { op.Multipart.Files = append(op.Multipart.Files, rigclient.Part("cover", *files.Cover)) }`,
	} {
		if !strings.Contains(client, want) {
			t.Errorf("the client should contain %q", want)
		}
	}

	// There is one method, not two. A create has a second because the JSON one
	// already existed and callers had written it down; this endpoint never had
	// a shape without its files.
	if strings.Contains(client, "SubmitWithFiles") {
		t.Error("a declared endpoint has one method, which takes its files")
	}
}

// submitClient generates the Lesson client with the endpoint under test on it.
//
// The lifecycle fixture, whose one table has no file column anywhere — so
// everything asserted above is there because the endpoint declared its parts.
func submitClient(t *testing.T) string {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("the lifecycle fixture has no Lesson resource")
	}
	res.Endpoints = append(res.Endpoints, submitEndpoint())
	doc.Reindex()

	for _, a := range gentest.Run(t, goclient.New(), doc, opts()) {
		if filepath.Base(a.Path) == "lesson_client.gen.go" {
			return string(a.Content)
		}
	}
	t.Fatal("no lesson_client.gen.go")
	return ""
}

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

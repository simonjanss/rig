package servicego_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servicego"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// What a declared endpoint's uploads do to the service layer.
//
// Two things, and the second is the one that used to be missing. The method
// receives the pending records beside the decoded body, because the handler
// stores each part as it arrives and has nowhere else to put them. And the
// resource needs a *files.Service even with no file column of its own — the
// handler reaches the store through svc.Files(), so a service that did not hold
// one would generate a handler calling a method its own interface does not have.
func TestADeclaredEndpointsUploadsReachTheServiceLayer(t *testing.T) {
	t.Parallel()

	// The lifecycle fixture, whose one table has no file column anywhere — so
	// everything below is there because the endpoint declared its parts and for
	// no other reason.
	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("the lifecycle fixture has no Lesson resource")
	}
	res.Endpoints = append(res.Endpoints, submitEndpoint())
	doc.Reindex()

	artifacts := gentest.Run(t, servicego.New(), doc, stubOpts())
	api := collapse(find(t, artifacts, "lesson_service.gen.go"))

	for _, want := range []string{
		// The interface, so nothing can implement it and quietly drop the files.
		"Submit(ctx context.Context, r Request[struct{}, struct{}, LessonSubmitBody], pending []*files.Pending)",
		// And the forwarder to the implementation, which is the one place the
		// parameter can be declared and then not passed. It does not compile
		// when it is missing, which is the only reason this was ever found —
		// so it is asserted here rather than left to the next project.
		"return s.contract.Endpoints.Submit(ctx, r, pending)",
		// And the way to the store, which is how the handler gets there.
		"Files() *files.Service",
		"func NewLessonService(repo store.LessonRepository, rules LessonRules, files *files.Service)",
	} {
		if !strings.Contains(api, want) {
			t.Errorf("the API layer should contain %q", want)
		}
	}

	// The scaffolded service layer has to satisfy the interface above, and its
	// signature is written by a second function that had the same hole.
	stub := collapse(find(t, artifacts, "lesson.go"))
	want := "func (s *rules) Submit(ctx context.Context, r api.Request[struct{}, struct{}, api.LessonSubmitBody], pending []*files.Pending)"
	if !strings.Contains(stub, want) {
		t.Errorf("the scaffolded service layer should contain %q:\n%s", want, stub)
	}
}

// stubOpts asks for the scaffolded service layer as well, which the ordinary
// options leave out.
func stubOpts() gen.Options {
	o := opts()
	o.Raw["api_import"] = "rigtest/api"
	o.Raw["stub_dir"] = "services/{table}"
	return o
}

// submitEndpoint is what `file_parts:` on a declared endpoint compiles to: a
// body, a part that may repeat and may be left out, and one that has to be
// there exactly once.
func submitEndpoint() ir.Endpoint {
	return ir.Endpoint{
		Name:        "Submit",
		Method:      "POST",
		Path:        "/_submit",
		Pattern:     "POST /api/v1/lessons/_submit",
		OperationID: "submitLesson",
		Summary:     "Submit an attachment.",
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
					Description: "The one image this is shown by.", Required: true},
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

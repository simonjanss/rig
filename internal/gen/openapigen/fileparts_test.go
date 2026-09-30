package openapigen_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/openapigen"
	"github.com/simonjanss/rig/pkg/ir"
)

// What the specification says about an endpoint that declared its own files.
//
// It is the half a reader meets first and the half nothing else checks: a
// document that describes the request as JSON, for a route that reads a form, is
// wrong in a way both SDKs and the server agree to ignore. The parts are here,
// the one that repeats is a list, and the one that has to be there is required.
func TestADeclaredEndpointsFormIsInTheDocument(t *testing.T) {
	t.Parallel()

	// The lifecycle fixture, whose one table has no file column anywhere — so
	// the form below is there because the endpoint declared its parts.
	doc := gentest.LoadDocument(t, filepath.Join("testdata", primary+".ir.json"))
	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("the lifecycle fixture has no Lesson resource")
	}
	res.Endpoints = append(res.Endpoints, submitEndpoint())
	doc.Reindex()

	m := model(t, gentest.Run(t, openapigen.New(), doc, opts()))

	op := findOperation(t, m, "submitLesson")
	if op == nil {
		t.Fatal("the endpoint is not in the document at all")
	}
	if op.RequestBody == nil {
		t.Fatal("an endpoint that takes a body should say so")
	}

	// Beside JSON rather than instead of it, which is what the server accepts.
	form, ok := op.RequestBody.Content.Get("multipart/form-data")
	if !ok {
		t.Fatal("the request should be documented as a form as well as as JSON")
	}
	if _, ok := op.RequestBody.Content.Get("application/json"); !ok {
		t.Error("a caller with nothing to attach still sends the body it always sent")
	}

	schema := form.Schema.Schema()
	if schema == nil {
		t.Fatal("the form has no schema")
	}

	// The body travels in the part the client sends it in, and it is required:
	// the endpoint declares body fields, so a form without it is a request
	// missing half of itself.
	if _, ok := schema.Properties.Get("json"); !ok {
		t.Error("the body should be a part named json")
	}
	if !slices.Contains(schema.Required, "json") {
		t.Errorf("required = %v, want json among them", schema.Required)
	}

	// A part that may repeat is an array of files, which is how 3.1 says
	// "several parts under one name".
	attachments, ok := schema.Properties.Get("attachments")
	if !ok {
		t.Fatal("the repeating part is missing")
	}
	got := attachments.Schema()
	if !slices.Contains(got.Type, "array") {
		t.Errorf("attachments type = %v, want array", got.Type)
	}
	if got.Items == nil || got.Items.A == nil {
		t.Fatal("an array with no items says nothing about what it carries")
	}
	if item := got.Items.A.Schema(); item.ContentMediaType != ir.MediaOctet {
		t.Errorf("the items should be opaque bytes, got contentMediaType %q", item.ContentMediaType)
	}
	if slices.Contains(schema.Required, "attachments") {
		t.Error("the repeating part is optional here and the document should say so")
	}

	// A part that has to be there is a single file and is required.
	cover, ok := schema.Properties.Get("cover")
	if !ok {
		t.Fatal("the required part is missing")
	}
	if c := cover.Schema(); !slices.Contains(c.Type, "string") || c.ContentMediaType != ir.MediaOctet {
		t.Errorf("cover should be opaque bytes, got type %v and contentMediaType %q",
			c.Type, c.ContentMediaType)
	}
	if !slices.Contains(schema.Required, "cover") {
		t.Errorf("required = %v, want cover among them", schema.Required)
	}

	// Each part's content type is stated, because without it a client is free
	// to send the body as text/plain and the server's dispatch reads the type.
	if op.RequestBody.Content == nil {
		t.Fatal("no content")
	}
	if form.Encoding == nil {
		t.Fatal("the parts should each state their content type")
	}
	for _, part := range []string{"json", "attachments", "cover"} {
		if _, ok := form.Encoding.Get(part); !ok {
			t.Errorf("no encoding for the %s part", part)
		}
	}
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

package goclient_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/goclient"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// A success body the configuration spelled out rather than naming.
//
// It used to reach the specification and stop there: this client returned
// `error` for it, so a caller reading the document for a shape found a method
// that could not hand one back, and nothing anywhere said which of the two was
// wrong.
func TestAResponseSpelledOutIsAShapeTheClientDecodesInto(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("the lifecycle fixture has no Lesson resource")
	}
	res.Endpoints = append(res.Endpoints, reportEndpoint())
	doc.Reindex()

	artifacts := gentest.Run(t, goclient.New(), doc, gen.Options{
		OutDir: ".", Raw: map[string]any{"package": "client"},
	})

	var input, client string
	for _, a := range artifacts {
		switch filepath.Base(a.Path) {
		case "lesson_input.gen.go":
			input = string(a.Content)
		case "lesson_client.gen.go":
			client = string(a.Content)
		}
	}

	shape, ok := between(input, "type LessonReportResult struct {", "\n}")
	if !ok {
		t.Fatalf("no LessonReportResult:\n%s", input)
	}
	if !strings.Contains(collapse(shape), "Titles []string") {
		t.Errorf("the response's fields are the shape's:\n%s", shape)
	}

	if !strings.Contains(collapse(client), "Report(ctx context.Context, opts ...rigclient.CallOption) (*LessonReportResult, error)") {
		t.Errorf("the method should hand the shape back:\n%s", client)
	}
}

// reportEndpoint is a read whose answer is a composite no table has: several
// lists read side by side, which is what a body_fields response is for.
func reportEndpoint() ir.Endpoint {
	return ir.Endpoint{
		Name:        "Report",
		Method:      "GET",
		Path:        "/_report",
		Pattern:     "GET /api/v1/lessons/_report",
		OperationID: "reportLesson",
		Responses: []ir.EndpointResponse{{
			StatusCode:   200,
			Description:  "What may be started here.",
			ContentTypes: []string{ir.MediaJSON},
			BodyFields: []ir.Field{{
				Name: "Titles", Wire: "titles",
				Description: "One per lesson that may be started.",
				Type:        ir.TypeString, TypeKind: ir.TypeKindPrimitive,
				GoType:    "[]string",
				Modifiers: []string{ir.ModifierArray},
			}},
		}},
		Impl: ir.EndpointImpl{
			Kind:          ir.EndpointCustom,
			ServiceMethod: "Report",
			HandlerName:   "ReportLesson",
		},
	}
}

package servicego_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servicego"
	"github.com/simonjanss/rig/pkg/ir"
)

// A success body the configuration spelled out rather than naming.
//
// The service interface is where this matters most: a method returning `error`
// gives the hand-written half no way to answer at all, so the endpoint could be
// declared, documented, routed — and still had nowhere to put the answer.
func TestAResponseSpelledOutIsAShapeTheServiceReturns(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("the lifecycle fixture has no Lesson resource")
	}
	res.Endpoints = append(res.Endpoints, ir.Endpoint{
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
	})
	doc.Reindex()

	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	types := find(t, artifacts, "lesson.gen.go")
	shape, ok := between(types, "type LessonReportResult struct {", "\n}")
	if !ok {
		t.Fatalf("no LessonReportResult:\n%s", types)
	}
	if !strings.Contains(collapse(shape), "Titles []string") {
		t.Errorf("the response's fields are the shape's:\n%s", shape)
	}

	service := find(t, artifacts, "lesson_service.gen.go")
	want := "Report(ctx context.Context, r Request[struct{}, struct{}, struct{}]) (*LessonReportResult, error)"
	if !strings.Contains(collapse(service), want) {
		t.Errorf("the interface should return the shape:\n%s", service)
	}
	// The delegation has to return two values too, or the package does not
	// compile — which is the half a signature-only assertion would miss.
	if !strings.Contains(collapse(service), "return nil, rigerr.Internal(nil, \"Lesson.Report has no implementation\")") {
		t.Errorf("the guard should return the pair:\n%s", service)
	}
}

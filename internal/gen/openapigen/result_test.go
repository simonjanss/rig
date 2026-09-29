package openapigen_test

import (
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/openapigen"
	"github.com/simonjanss/rig/pkg/ir"
)

// A success body the configuration spelled out is a named component, not an
// anonymous object.
//
// The name is genutil's, the same one both SDKs declare, so a reader who finds
// LessonReportResult in the document finds it in the client too. This generator
// was the only one that ever rendered these fields at all — it inlined them —
// and an inline schema beside two SDKs that returned nothing was the shape of
// the bug rather than a description anybody could use.
func TestAResponseSpelledOutIsANamedComponent(t *testing.T) {
	t.Parallel()

	doc := load(t, primary)
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
		Summary:     "What may be started here.",
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

	m := model(t, gentest.Run(t, openapigen.New(), doc, opts()))

	schema, ok := m.Components.Schemas.Get("LessonReportResult")
	if !ok {
		t.Fatal("no LessonReportResult in components/schemas")
	}
	if _, ok := schema.Schema().Properties.Get("titles"); !ok {
		t.Error("the response's fields are the component's")
	}

	op := findOperation(t, m, "reportLesson")
	resp, ok := op.Responses.Codes.Get("200")
	if !ok {
		t.Fatal("reportLesson has no 200")
	}
	media, ok := resp.Content.Get(ir.MediaJSON)
	if !ok {
		t.Fatal("reportLesson's 200 has no JSON body")
	}
	if got := media.Schema.GetReference(); got != "#/components/schemas/LessonReportResult" {
		t.Errorf("the operation should reference the component, got %q", got)
	}
}

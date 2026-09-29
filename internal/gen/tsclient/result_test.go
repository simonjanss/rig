package tsclient_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/tsclient"
	"github.com/simonjanss/rig/pkg/ir"
)

// A success body the configuration spelled out rather than naming.
//
// This client used to answer Promise<void> for it while the specification
// described the object in full — the same endpoint documented two ways, with
// nothing anywhere saying which was right.
func TestAResponseSpelledOutIsATypeTheClientDecodesInto(t *testing.T) {
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

	var input, client string
	for _, a := range gentest.Run(t, tsclient.New(), doc, opts()) {
		switch filepath.Base(a.Path) {
		case "lesson_input.gen.ts":
			input = string(a.Content)
		case "lesson_client.gen.ts":
			client = string(a.Content)
		}
	}

	shape, ok := between(input, "export type LessonReportResult = {", "\n}")
	if !ok {
		t.Fatalf("no LessonReportResult:\n%s", input)
	}
	if !strings.Contains(strings.Join(strings.Fields(shape), " "), "titles") {
		t.Errorf("the response's fields are the type's:\n%s", shape)
	}

	if !strings.Contains(strings.Join(strings.Fields(client), " "), "Promise<LessonReportResult>") {
		t.Errorf("the method should hand the type back:\n%s", client)
	}
	// And the import has to be findable, or the module does not typecheck.
	if !strings.Contains(client, "LessonReportResult") ||
		!strings.Contains(client, "lesson_input.gen") {
		t.Errorf("the client should import the type from the input module:\n%s", client)
	}
}

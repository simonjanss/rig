package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/pkg/ir"
)

// A success body the configuration spelled out rather than naming.
//
// The handler never spells the type — it writes whatever the service returned —
// so all this generator needs to know is that there is one. It used to read
// BodyObject alone, which meant it answered an empty 200 for an endpoint the
// specification said carried an object.
func TestAResponseSpelledOutIsWrittenBackToTheCaller(t *testing.T) {
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

	routes := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_routes.gen.go")

	if !strings.Contains(routes, "func handleReportLesson(") {
		t.Fatalf("no handleReportLesson:\n%s", routes)
	}

	// Read off the whole file rather than a slice of it: svc.Report is named in
	// one handler and nowhere else, so there is nothing to disambiguate.
	flat := collapse(routes)
	if !strings.Contains(flat, "out, err := svc.Report(ctx, req)") {
		t.Errorf("the handler should read the answer:\n%s", routes)
	}
	if !strings.Contains(flat, "writeJSON(w, http.StatusOK, out)") {
		t.Errorf("the handler should write the answer:\n%s", routes)
	}
}

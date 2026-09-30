package compile_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// Files on an endpoint the configuration declared.
//
// Until this existed, `file_parts:` was something only Expand could write: a
// create on a table with a file column accepted a form and nothing else could,
// so an endpoint that collected attachments had to be mounted by hand outside
// everything rig generates — no specification entry, no client on either side,
// and its own copy of the claims, the error mapping and the idempotency record.
//
// The three things a generator reads off the document are asserted here, because
// each of them is a separate output that would otherwise be quietly wrong: the
// parts themselves, the second content type that says a form is accepted at all,
// and the two statuses a form can fail with.
func TestAnEndpointMayDeclareTheFilesItCarries(t *testing.T) {
	t.Parallel()

	ep := declaredEndpoint(t, tableconf.EndpointRequest{
		Body: []tableconf.Param{{Name: "Note", Type: "string"}},
		FileParts: []tableconf.FilePart{
			{Name: "Attachments", Description: "Whatever was dragged in.",
				Optional: true, Array: true},
			{Name: "Cover", Description: "The one image this is shown by."},
		},
	})

	if len(ep.Request.FileParts) != 2 {
		t.Fatalf("both parts should survive: %+v", ep.Request.FileParts)
	}

	attachments := ep.Request.FileParts[0]
	if attachments.Name != "attachments" || attachments.Field != "Attachments" {
		t.Errorf("the wire name is the namer's and the field is the configuration's, got %+v",
			attachments)
	}
	if !attachments.Array || attachments.Required {
		t.Errorf("an optional repeating part: %+v", attachments)
	}
	if attachments.Role != "" {
		t.Errorf("a declared part has no column, so no role: %q", attachments.Role)
	}
	if attachments.Description != "Whatever was dragged in." {
		t.Errorf("the description is what says what the part is for, got %q",
			attachments.Description)
	}

	if cover := ep.Request.FileParts[1]; !cover.Required || cover.Array {
		t.Errorf("a part not marked optional has to be there, once: %+v", cover)
	}

	// Beside JSON rather than instead of it. A caller with nothing to attach
	// sends the body it always sent, and the generated server reads the same
	// struct out of either.
	if want := []string{"application/json", "multipart/form-data"}; !slices.Equal(ep.Request.ContentTypes, want) {
		t.Errorf("content types = %v, want %v", ep.Request.ContentTypes, want)
	}

	// A form is the one body that can be too big or of a type the store refuses,
	// and an endpoint whose documentation did not say so would be promising
	// something it cannot keep.
	for _, code := range []int{413, 415} {
		if !slices.Contains(ep.Errors, code) {
			t.Errorf("errors = %v, want %d among them", ep.Errors, code)
		}
	}
}

// The three spellings that would produce a request nobody could send.
//
// Each is refused rather than resolved, for the reason a reserved query
// parameter is: a part the server binds to something else, two parts under one
// name where only the last would arrive, and a form on a method whose body half
// the intermediaries between here and the handler would drop.
func TestADeclaredFilePartIsRefusedWhereItCouldNotWork(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		method string
		parts  []tableconf.FilePart
		code   string
	}{
		{"the body's own part name", "POST",
			[]tableconf.FilePart{{Name: "JSON"}}, "RIG"},
		{"two parts under one name", "POST",
			[]tableconf.FilePart{{Name: "Cover"}, {Name: "Cover"}}, "RIG"},
		{"a method with no body", "GET",
			[]tableconf.FilePart{{Name: "Cover"}}, "RIG"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, diags := applyEndpoint(tc.method, tableconf.EndpointRequest{FileParts: tc.parts})
			if !diags.HasErrors() {
				t.Fatal("this should not compile")
			}
			if !strings.Contains(diags.String(), tc.code) {
				t.Errorf("expected a diagnostic, got:\n%s", diags.String())
			}
		})
	}
}

// declaredEndpoint compiles one endpoint on `team` and hands it back.
func declaredEndpoint(t *testing.T, req tableconf.EndpointRequest) *ir.Endpoint {
	t.Helper()

	ep, diags := applyEndpoint("POST", req)
	if diags.HasErrors() {
		t.Fatal(diags)
	}
	if ep == nil {
		t.Fatal("the endpoint did not survive compilation")
	}
	return ep
}

func applyEndpoint(method string, req tableconf.EndpointRequest) (*ir.Endpoint, diag.List) {
	schema, _ := compile.Normalize(cascadeSchema(), compile.NormalizeOptions{})
	api, _ := compile.Project(schema, compile.ProjectOptions{Name: "Demo", BasePath: "/api/v1"})

	set := tableconf.NewSet()
	set.Add(&tableconf.Loaded{File: &tableconf.File{
		Table: "team",
		Endpoints: []tableconf.Endpoint{{
			Name:    "Submit",
			Method:  method,
			Path:    "/_submit",
			Request: &req,
			Responses: []tableconf.EndpointResponse{{
				Status: 201, BodyObject: "Team", Description: "The team.",
			}},
		}},
	}})

	out, _, diags := compile.ApplyConfig(api, schema, set, compile.ConfigOptions{})
	for i := range out.Resources {
		if out.Resources[i].Name != "Team" {
			continue
		}
		for j := range out.Resources[i].Endpoints {
			if ep := &out.Resources[i].Endpoints[j]; ep.Name == "Submit" {
				return ep, diags
			}
		}
	}
	return nil, diags
}

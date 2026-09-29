package compile_test

import (
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/naming"
	"github.com/simonjanss/rig/pkg/ir"
)

// An unexposed table's rows, where an endpoint hands them out on purpose.
//
// `expose: false` means no routes and no wire shape, which is what keeps
// rig_account_token out of every document. But a hand-written endpoint may name
// another table's rows in a body, and then the document refers to a shape that
// was never projected — an SDK naming a type nobody declared, which is rig#175
// read from the other side.
//
// So the entity is projected on demand: for a resource a body names, and for no
// other. Both halves are asserted here, because a fix that projected every
// unexposed table would pass the first assertion and be the bug it replaced.
func TestAnUnexposedResourceNamedInABodyIsProjected(t *testing.T) {
	t.Parallel()

	api := ir.API{
		Version: "v1",
		Resources: []ir.Resource{
			{
				Name:    "Issue",
				Plural:  "Issues",
				Storage: &ir.ResourceStorage{Table: "issue"},
				Fields: []ir.ResourceField{{
					Field:      ir.Field{Name: "ID", Wire: "id", Type: ir.TypeUUID},
					Operations: []string{ir.FieldOpRead},
				}},
				Endpoints: []ir.Endpoint{{
					Name:   "Report",
					Method: "GET",
					Path:   "/_report",
					Responses: []ir.EndpointResponse{{
						StatusCode: 200,
						BodyFields: []ir.Field{{
							Name: "Processes", Wire: "processes",
							Type:      "Process",
							Modifiers: []string{ir.ModifierArray},
						}},
					}},
					Impl: ir.EndpointImpl{Kind: ir.EndpointCustom, ServiceMethod: "Report"},
				}},
			},
			{
				// Named by the endpoint above, and otherwise invisible.
				Name:      "Process",
				Plural:    "Processes",
				Unexposed: true,
				Storage:   &ir.ResourceStorage{Table: "process"},
				Fields: []ir.ResourceField{{
					Field:      ir.Field{Name: "Name", Wire: "name", Type: ir.TypeString},
					Operations: []string{ir.FieldOpRead},
				}},
			},
			{
				// Unexposed and named by nothing. This is the row that must stay
				// out: it is every `rig_account_token` in every document.
				Name:      "Secret",
				Plural:    "Secrets",
				Unexposed: true,
				Storage:   &ir.ResourceStorage{Table: "secret"},
				Fields: []ir.ResourceField{{
					Field:      ir.Field{Name: "Token", Wire: "token", Type: ir.TypeString},
					Operations: []string{ir.FieldOpRead},
				}},
			},
		},
	}

	out, diags := compile.Expand(api, compile.ExpandOptions{
		Namer: naming.New(naming.Config{JSONCase: naming.CaseCamel}),
	})
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	declared := map[string]ir.Object{}
	for _, o := range out.Objects {
		declared[o.Name] = o
	}

	process, ok := declared["Process"]
	if !ok {
		t.Fatal("an endpoint names Process in a body, so the document has to declare it")
	}
	if len(process.Fields) != 1 || process.Fields[0].Name != "Name" {
		t.Errorf("the projection is the table's readable columns, got %v", process.Fields)
	}
	if process.Origin != ir.OriginProjected {
		t.Errorf("Origin = %q, want %q", process.Origin, ir.OriginProjected)
	}

	if _, ok := declared["Secret"]; ok {
		t.Error("an unexposed table nothing names stays out of the document")
	}
}

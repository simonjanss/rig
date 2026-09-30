package servergo_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/pkg/ir"
)

// A form on an endpoint that is not a create.
//
// The multipart branch used to be written only for ir.OpCreate, which made an
// endpoint collecting attachments something no configuration could express: the
// document could say the request carried files and the handler would read the
// body as JSON, refuse the form's content type, and never call the file service
// at all. So such an endpoint had to be mounted by hand, outside the router,
// with its own claims, error mapping and idempotency record.
//
// What the handler has to do is here in one test because the parts are read in
// one pass and the order is the whole of it: the body first, each file as it
// arrives, an unknown part refused, and only then the check that a required part
// was there.
func TestADeclaredEndpointReadsTheFilesItSaysItCarries(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "files.ir.json"))
	attach(t, doc)

	routes := collapse(find(t, gentest.Run(t, servergo.New(), doc, opts()),
		"profile_attachment_routes.gen.go"))

	for _, want := range []string{
		// The form is read rather than the body.
		"if filehttp.IsMultipart(r) {",
		"form, err := filehttp.ReadForm(r)",
		// The body travels in the part the client package sends it in, through
		// the decoder the JSON path uses, so an unknown key is refused alike.
		"case filehttp.JSONPart:",
		"if err := decodeReader(part.Body, &body); err != nil",
		// Each file is stored as it arrives, because a part's body is only
		// valid until the next one is asked for.
		`case "attachments":`,
		`case "cover":`,
		"p, err := svc.Files().Prepare(ctx, part.Name, files.AttachRequest{",
		"pending = append(pending, p)",
		// A part nobody claimed is a file uploaded into nowhere, and a 201
		// would tell the caller that worked.
		"fail(s, w, r, rc, filehttp.ErrUnknownPart(part.Name))",
		// Only then the required part, and the JSON path still decodes.
		`if !hasPart(pending, "cover") {`,
		"} else if err := decodeBody(r, &body); err != nil {",
		// And what was carried reaches the service beside the body.
		"svc.Submit(ctx, req, pending)",
	} {
		if !strings.Contains(routes, want) {
			t.Errorf("the handler should contain %q:\n%s", want, routes)
		}
	}

	// The part that may repeat is not special here: the loop appends one
	// pending record per part that arrives, so a second `attachments` is a
	// second file rather than a second case.
	if n := strings.Count(routes, `case "attachments":`); n != 1 {
		t.Errorf("a repeating part is one case, got %d", n)
	}
}

// attach appends the endpoint under test to ProfileAttachment, which is the
// fixture's resource with no file column of its own — so this also asserts that
// a resource reaches the file service through an endpoint rather than only
// through a column.
func attach(t *testing.T, doc *ir.Document) {
	t.Helper()

	res := doc.Resource("ProfileAttachment")
	if res == nil {
		t.Fatal("the files fixture has no ProfileAttachment resource")
	}
	res.Endpoints = append(res.Endpoints, submitEndpoint())
	doc.Reindex()
}

// submitEndpoint is what `file_parts:` on a declared endpoint compiles to: a
// body, a part that may repeat and may be left out, and one that has to be
// there exactly once.
func submitEndpoint() ir.Endpoint {
	return ir.Endpoint{
		Name:        "Submit",
		Method:      "POST",
		Path:        "/_submit",
		Pattern:     "POST /api/v1/profile-attachments/_submit",
		OperationID: "submitProfileAttachment",
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
			BodyObject: "ProfileAttachment", Description: "The attachment.",
		}},
		Errors: []int{400, 401, 403, 413, 415, 429, 500},
		Impl: ir.EndpointImpl{
			Kind:          ir.EndpointCustom,
			ServiceMethod: "Submit",
			HandlerName:   "SubmitProfileAttachment",
		},
	}
}

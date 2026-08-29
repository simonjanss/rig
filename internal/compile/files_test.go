// What a file column turns into, asserted directly.
//
// The golden document already contains every byte of this, and that is exactly
// the problem: a four-thousand-line file is where a wrong permission key or a
// missing part name goes unnoticed, because updating the golden is one flag and
// reading it is not. These name the properties instead.
package compile_test

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/pkg/ir"
)

// filesDoc compiles the fixture that has file columns in every shape worth
// having: two single-column keys, one composite key carrying the tenant, and a
// column named like a file column that points somewhere else.
func filesDoc(t *testing.T) *ir.Document {
	t.Helper()

	doc, diags := compileFixture(t, filepath.Join("testdata", "files"))
	for _, d := range diags {
		if d.Severity == "error" && d.Code.ID != "RIG6010" && d.Code.ID != "RIG6030" {
			// Those two are the fixture's own point: an uncovered foreign key
			// and the control column's naming advice.
			t.Fatalf("unexpected error: %s", d.Message)
		}
	}
	return doc
}

func resourceNamed(t *testing.T, doc *ir.Document, name string) *ir.Resource {
	t.Helper()

	for i := range doc.API.Resources {
		if doc.API.Resources[i].Name == name {
			return &doc.API.Resources[i]
		}
	}
	t.Fatalf("no resource named %q", name)
	return nil
}

// The recognition rule, both halves of it.
//
// A composite key is the shape rig recommends — it is what puts the tenant
// inside the constraint — and it lives on the table rather than on the column,
// so a recognition that read only the denormalized field would miss precisely
// the schema this convention exists to encourage. The control is the other
// half: a column named like a file column and pointing at something else is an
// ordinary foreign key.
func TestAFileColumnIsRecognizedThroughACompositeKey(t *testing.T) {
	t.Parallel()

	doc := filesDoc(t)

	att := resourceNamed(t, doc, "ProfileAttachment")
	var cols []string
	for _, f := range att.Files {
		cols = append(cols, f.Column)
	}
	if want := []string{"document_file_id"}; !slices.Equal(cols, want) {
		t.Errorf("file columns = %v, want %v — document_file_id is reached only "+
			"through the table's constraints, and thumbnail_file_id points at profile", cols, want)
	}

	if got := att.Files[0]; !got.Required {
		t.Error("document_file_id is not null, so its part is required")
	}
}

// Everything a generator needs, derived once. Five identifiers follow from one
// column, and this is where they are settled.
func TestAFileColumnResolvesEveryNameItImplies(t *testing.T) {
	t.Parallel()

	doc := filesDoc(t)
	profile := resourceNamed(t, doc, "Profile")

	var got *ir.FileColumn
	for i := range profile.Files {
		if profile.Files[i].Column == "profile_image_file_id" {
			got = &profile.Files[i]
		}
	}
	if got == nil {
		t.Fatal("profile_image_file_id is not a file column")
	}

	for _, c := range []struct{ what, got, want string }{
		{"role", got.Role, "profileImage"},
		{"field", got.Field, "ProfileImageFileID"},
		{"part", got.Part, "profileImageFile"},
		{"segment", got.Segment, "profile-image-file"},
		{"stem", got.GoName(), "ProfileImageFile"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.what, c.got, c.want)
		}
	}
	if got.Required {
		t.Error("the column is nullable, so the part is not required")
	}
}

// Three endpoints per file column, under the row that owns them.
//
// The nesting is what makes them tenant-scoped and owner-scoped for free: the
// handler has to resolve the owning row through the generated repository before
// it touches a byte. A flat route could not.
func TestEachFileColumnGetsThreeNestedEndpoints(t *testing.T) {
	t.Parallel()

	doc := filesDoc(t)
	profile := resourceNamed(t, doc, "Profile")

	want := map[string]string{
		"UploadProfileImageFile":   "POST /api/v1/profiles/{id}/profile-image-file",
		"DownloadProfileImageFile": "GET /api/v1/profiles/{id}/profile-image-file/{fileId}/{filename}",
		"DeleteProfileImageFile":   "DELETE /api/v1/profiles/{id}/profile-image-file",
		"UploadBannerFile":         "POST /api/v1/profiles/{id}/banner-file",
		"DownloadBannerFile":       "GET /api/v1/profiles/{id}/banner-file/{fileId}/{filename}",
		"DeleteBannerFile":         "DELETE /api/v1/profiles/{id}/banner-file",
	}
	for name, pattern := range want {
		ep := profile.Endpoint(name)
		if ep == nil {
			t.Errorf("no %s endpoint", name)
			continue
		}
		if ep.Pattern != pattern {
			t.Errorf("%s routes %q, want %q", name, ep.Pattern, pattern)
		}
		if ep.File == nil {
			t.Errorf("%s does not say which column it acts on", name)
		}
	}
}

// The upload announces a form and names its one part, and answers with the file
// shape. The last of those is load-bearing beyond documentation: a client
// generator walks out from responses to decide which types to emit, so a body
// object left unnamed is a type nothing can reach.
func TestTheUploadNamesItsPartAndItsResponse(t *testing.T) {
	t.Parallel()

	doc := filesDoc(t)
	ep := resourceNamed(t, doc, "Profile").Endpoint("UploadProfileImageFile")

	if got := ep.Request.ContentTypes; !slices.Equal(got, []string{ir.MediaMultipart}) {
		t.Errorf("content types = %v, want only %s", got, ir.MediaMultipart)
	}
	if len(ep.Request.FileParts) != 1 || ep.Request.FileParts[0].Name != "profileImageFile" {
		t.Errorf("file parts = %+v, want one named profileImageFile", ep.Request.FileParts)
	}
	if got := ep.Responses[0].BodyObject; got != "RigFile" {
		t.Errorf("the success response names %q, want RigFile", got)
	}
	for _, code := range []int{413, 415} {
		if !slices.Contains(ep.Errors, code) {
			t.Errorf("an upload can answer %d and does not say so", code)
		}
	}
}

// The create takes a form as well, which is the only way a not-null file column
// is reachable at all: the row would otherwise have to exist before the upload
// had anywhere to go.
//
// JSON stays first, because a generator that wants "the" content type takes the
// first one and every such generator predates this.
func TestTheCreateAlsoTakesAForm(t *testing.T) {
	t.Parallel()

	doc := filesDoc(t)

	create := resourceNamed(t, doc, "ProfileAttachment").Endpoint(ir.OpCreate)
	if got := create.Request.ContentTypes; !slices.Equal(got, []string{ir.MediaJSON, ir.MediaMultipart}) {
		t.Fatalf("content types = %v, want JSON first and multipart second", got)
	}
	if len(create.Request.FileParts) != 1 || create.Request.FileParts[0].Name != "documentFile" {
		t.Errorf("file parts = %+v, want one named documentFile", create.Request.FileParts)
	}

	// And a table with no file column is untouched, which is the property that
	// keeps the most-called handler rig emits out of this milestone.
	plain := resourceNamed(t, doc, "RigFile").Endpoint(ir.OpCreate)
	if got := plain.Request.ContentTypes; !slices.Equal(got, []string{ir.MediaJSON}) {
		t.Errorf("a table with no file column accepts %v, want only JSON", got)
	}
	if plain.Request.FileParts != nil {
		t.Errorf("file parts = %+v, want none", plain.Request.FileParts)
	}
}

// Two keys per file column, and neither implied by the resource's own write.
//
// The tempting alternative makes the common case a single grant and means a
// role quietly gains the ability to replace an image the day somebody adds a
// file column to a table it could already edit.
func TestAFileColumnAddsTwoPermissionsAndNothingImpliesThem(t *testing.T) {
	t.Parallel()

	doc := filesDoc(t)

	keys := make(map[string]bool, len(doc.API.Permissions))
	for _, p := range doc.API.Permissions {
		keys[p.Key] = true
	}
	for _, want := range []string{
		"profile.profile_image_file.read", "profile.profile_image_file.write",
		"profile.banner_file.read", "profile.banner_file.write",
		"profile_attachment.document_file.read", "profile_attachment.document_file.write",
	} {
		if !keys[want] {
			t.Errorf("no permission %q in the catalogue", want)
		}
	}

	profile := resourceNamed(t, doc, "Profile")
	for name, want := range map[string]string{
		"UploadProfileImageFile":   "profile.profile_image_file.write",
		"DownloadProfileImageFile": "profile.profile_image_file.read",
		"DeleteProfileImageFile":   "profile.profile_image_file.write",
	} {
		if got := profile.Endpoint(name).Permission; got != want {
			t.Errorf("%s requires %q, want %q", name, got, want)
		}
	}

	// The row's own keys are still exactly three. A fourth derived from a file
	// endpoint would mean the file check had folded into the resource's.
	if got := profile.Endpoint(ir.OpUpdate).Permission; got != "profile.write" {
		t.Errorf("Update requires %q, want profile.write", got)
	}
}

// A file endpoint is shadowed by a hand-written one of the same name, like every
// other endpoint rig synthesizes. Files are not a second kind of route with
// rules of their own.
func TestAHandWrittenEndpointShadowsAFileEndpoint(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	in.Resources[0].Files = []ir.FileColumn{{
		Role: "cover", Column: "cover_file_id", Field: "CoverFileID",
		Part: "coverFile", Segment: "cover-file",
	}}
	in.Resources[0].Endpoints = []ir.Endpoint{{
		Name: "UploadCoverFile", Method: "POST", Path: "/{id}/cover-file",
		Summary: "hand written",
		Impl: ir.EndpointImpl{
			Kind: ir.EndpointCustom, ServiceMethod: "UploadCoverFile",
			HandlerName: "UploadLessonCoverFile",
		},
	}}

	api, diags := compile.Expand(in, compile.ExpandOptions{})
	res := api.Resources[0]

	var uploads int
	for _, e := range res.Endpoints {
		if e.Name == "UploadCoverFile" {
			uploads++
		}
	}
	if uploads != 1 {
		t.Fatalf("got %d UploadCoverFile endpoints, want exactly 1", uploads)
	}
	if res.Endpoint("UploadCoverFile").Summary != "hand written" {
		t.Error("the hand-written endpoint should have won")
	}
	// The other two are unaffected: shadowing one is not opting out of files.
	for _, name := range []string{"DownloadCoverFile", "DeleteCoverFile"} {
		if res.Endpoint(name) == nil {
			t.Errorf("%s should still be generated", name)
		}
	}
	if !strings.Contains(diags.String(), "hand-written") {
		t.Errorf("shadowing should be reported:\n%s", diags.String())
	}
	if diags.HasErrors() {
		t.Errorf("shadowing is not an error:\n%s", diags.String())
	}
}

// The file shape exists once. Where the table is projected, the projection wins,
// because it carries the column bindings the persistence and sync generators
// read and a builtin has no way to know.
func TestTheFileShapeIsNotDuplicated(t *testing.T) {
	t.Parallel()

	doc := filesDoc(t)

	var found []ir.Origin
	for _, o := range doc.API.Objects {
		if o.Name == "RigFile" {
			found = append(found, o.Origin)
		}
	}
	if len(found) != 1 {
		t.Fatalf("found %d objects named RigFile, want exactly one", len(found))
	}
	// This fixture sets files.expose, so the projection is the one that should
	// have survived.
	if found[0] != ir.OriginProjected {
		t.Errorf("RigFile origin = %s, want %s: an exposed file table projects "+
			"its own object and the builtin is the fallback for a project that "+
			"does not", found[0], ir.OriginProjected)
	}
}

// And the other side of that: without the projection there is still a shape, or
// the upload would answer with a type the document does not contain.
//
// It is injected only where something has a file column. A shape nothing
// references is a type every client carries and nobody can obtain.
func TestTheFileShapeIsInjectedWhereTheTableIsNotProjected(t *testing.T) {
	t.Parallel()

	bare, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{})
	if hasObject(bare, "RigFile") {
		t.Error("a project with no file column should not carry a RigFile type")
	}

	in := simpleAPI()
	in.Resources[0].Files = []ir.FileColumn{{
		Role: "cover", Column: "cover_file_id", Field: "CoverFileID",
		Part: "coverFile", Segment: "cover-file",
	}}

	api, _ := compile.Expand(in, compile.ExpandOptions{})
	if !hasObject(api, "RigFile") {
		t.Fatal("a file column with no projected file table still needs the shape")
	}

	for _, o := range api.Objects {
		if o.Name != "RigFile" {
			continue
		}
		if o.Origin != ir.OriginBuiltin {
			t.Errorf("origin = %s, want %s", o.Origin, ir.OriginBuiltin)
		}
		var wire []string
		for _, f := range o.Fields {
			wire = append(wire, f.Wire)
		}
		want := []string{"id", "url", "fileName", "contentType", "sizeBytes"}
		if !slices.Equal(wire, want) {
			t.Errorf("fields = %v, want %v — the storage key, the checksum and the "+
				"tenant are the server's bookkeeping and never leave it", wire, want)
		}
	}
}

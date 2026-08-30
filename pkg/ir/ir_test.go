package ir_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/pkg/ir"
)

// sample builds a small but structurally complete document: a table with an
// enum column and a foreign key, projected into a resource with one endpoint.
func sample() *ir.Document {
	lessonStatus := ir.ColumnRef{
		Table: "lesson", Name: "status", SQLType: "lesson_status", Scan: ir.ScanEnumText,
	}
	return &ir.Document{
		IRVersion: ir.CurrentVersion,
		Tool:      "rig test",
		Valid:     true,
		Schema: ir.Schema{
			Name: "public",
			Tables: []ir.Table{{
				Name: "lesson",
				Kind: ir.TableKindBase,
				Columns: []ir.Column{
					{Name: "id", SQLType: "uuid", Ordinal: 1, IsPrimaryKey: true, CommentSource: ir.CommentSourceAuto, Comment: "Unique identifier."},
					{Name: "fixture_id", SQLType: "uuid", Ordinal: 2, CommentSource: ir.CommentSourceConfig,
						ForeignKey: &ir.FKRef{Table: "fixture", Column: "id"}},
					{Name: "status", SQLType: "lesson_status", UDTName: "lesson_status", Ordinal: 3,
						EnumType: "lesson_status", CommentSource: ir.CommentSourceConfig},
				},
				PrimaryKey: []string{"id"},
				Indexes: []ir.Index{
					{Name: "lesson_pkey", Columns: []string{"id"}, Unique: true, Method: "btree"},
					{Name: "lesson_fixture_idx", Columns: []string{"fixture_id", "status"}, Method: "btree"},
				},
			}},
			Enums: []ir.PgEnum{{
				Name:   "lesson_status",
				Values: []ir.PgEnumValue{{Value: "planned"}, {Value: "in_progress"}},
			}},
		},
		API: ir.API{
			Name: "Demo", Version: "v1", BasePath: "/api/v1",
			Enums: []ir.Enum{{
				Name: "LessonStatus", PgType: "lesson_status",
				Values: []ir.EnumValue{
					{Name: "Planned", Wire: "planned"},
					{Name: "InProgress", Wire: "in_progress"},
				},
			}},
			Objects: []ir.Object{{Name: "Error", Origin: ir.OriginBuiltin}},
			Resources: []ir.Resource{{
				Name: "Lesson", Plural: "Lessons", PathSegment: "lessons",
				Operations: []string{ir.OpGet, ir.OpList},
				Fields: []ir.ResourceField{{
					Field: ir.Field{
						Name: "Status", Wire: "status", Type: "LessonStatus",
						TypeKind: ir.TypeKindEnum, GoType: "LessonStatus", Column: &lessonStatus,
					},
					Operations: []string{ir.FieldOpRead, ir.FieldOpUpdate},
				}},
				Endpoints: []ir.Endpoint{{
					Name: "Get", Method: "GET", Path: "/{id}",
					Pattern:     "GET /api/v1/lessons/{id}",
					OperationID: "getLesson",
					Responses:   []ir.EndpointResponse{{StatusCode: 200, BodyObject: "Lesson"}},
					Impl:        ir.EndpointImpl{Kind: ir.EndpointGenerated, ServiceMethod: "Get", HandlerName: "GetLesson"},
				}},
				Storage: &ir.ResourceStorage{Table: "lesson", PrimaryKey: []string{"id"}},
			}},
		},
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	t.Parallel()

	first, err := ir.Marshal(sample())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := range 5 {
		got, err := ir.Marshal(sample())
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		if string(got) != string(first) {
			t.Fatalf("marshal %d differs from the first encoding", i)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	encoded, err := ir.Marshal(sample())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	decoded, err := ir.Unmarshal(encoded)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	reencoded, err := ir.Marshal(decoded)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	if string(reencoded) != string(encoded) {
		t.Fatalf("round trip changed the encoding:\n--- first ---\n%s\n--- second ---\n%s", encoded, reencoded)
	}
}

func TestHashTracksContent(t *testing.T) {
	t.Parallel()

	base := sample()
	h1, err := base.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if !strings.HasPrefix(h1, "sha256:") {
		t.Fatalf("hash %q is missing its algorithm prefix", h1)
	}

	// An identical document hashes identically.
	h2, err := sample().Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("identical documents hashed differently: %s vs %s", h1, h2)
	}

	// A meaningful change moves the hash.
	changed := sample()
	changed.API.Resources[0].Fields[0].Wire = "lessonStatus"
	h3, err := changed.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if h3 == h1 {
		t.Fatal("changing a field's wire name did not change the hash")
	}
}

// A release of rig is not a change to anybody's API. The tool that produced a
// document is written into it and must not be compared by it, or upgrading the
// generator would move every project's revision and tell every client the API
// had changed on a day it did not.
func TestHashIgnoresTheTool(t *testing.T) {
	t.Parallel()

	doc := sample()
	doc.Tool = "rig v0.1.0"
	before, err := doc.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	doc.Tool = "rig v0.2.0"
	after, err := doc.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if after != before {
		t.Fatalf("a new rig moved the hash: %s then %s", before, after)
	}
	if doc.Tool != "rig v0.2.0" {
		t.Fatalf("Tool = %q, want it left on the document", doc.Tool)
	}
}

// The revision is derived from the hash, so a hash that saw it would never
// settle: stamping would move the hash, which would move the revision.
func TestHashIgnoresTheRevision(t *testing.T) {
	t.Parallel()

	doc := sample()
	before, err := doc.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}

	doc.SetRevision("2026-08-18")
	after, err := doc.Hash()
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if after != before {
		t.Fatalf("stamping a revision moved the hash: %s then %s", before, after)
	}
	if doc.API.Revision != "2026-08-18" {
		t.Fatalf("revision = %q, want it left on the document", doc.API.Revision)
	}

	// It still travels in the document, though — the hash is the only thing
	// that does not see it.
	b, err := ir.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"revision": "2026-08-18"`) {
		t.Fatal("the marshalled document does not carry the revision")
	}
}

func TestUnmarshalRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	_, err := ir.Unmarshal([]byte(`{"ir_version":1,"generated_by":"x","future_field":true}`))
	if err == nil {
		t.Fatal("expected an error for an unknown field")
	}
	if !strings.Contains(err.Error(), "future_field") {
		t.Fatalf("error should name the offending field, got: %v", err)
	}
}

func TestUnmarshalRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	_, err := ir.Unmarshal([]byte(`{"ir_version":99,"generated_by":"x","api":{"name":"","version":"","base_path":"","enums":null,"objects":null,"resources":null},"schema":{"name":"","tables":null,"enums":null},"valid":true}`))
	if err == nil {
		t.Fatal("expected an error for an unsupported version")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Fatalf("error should name the version it saw, got: %v", err)
	}
}

func TestAccessors(t *testing.T) {
	t.Parallel()

	d := sample()

	if got := d.Table("lesson"); got == nil || got.Name != "lesson" {
		t.Fatalf("Table(lesson) = %v", got)
	}
	if got := d.Table("nope"); got != nil {
		t.Fatalf("Table(nope) = %v, want nil", got)
	}
	if got := d.Column("lesson", "status"); got == nil || got.EnumType != "lesson_status" {
		t.Fatalf("Column(lesson, status) = %v", got)
	}
	if got := d.Column("lesson", "nope"); got != nil {
		t.Fatalf("Column(lesson, nope) = %v, want nil", got)
	}
	if got := d.Resource("Lesson"); got == nil || got.PathSegment != "lessons" {
		t.Fatalf("Resource(Lesson) = %v", got)
	}
	if got := d.ResourceForTable("lesson"); got == nil || got.Name != "Lesson" {
		t.Fatalf("ResourceForTable(lesson) = %v", got)
	}
	if got := d.Enum("LessonStatus"); got == nil || got.PgType != "lesson_status" {
		t.Fatalf("Enum(LessonStatus) = %v", got)
	}
	if got := d.PgEnum("lesson_status"); got == nil || len(got.Values) != 2 {
		t.Fatalf("PgEnum(lesson_status) = %v", got)
	}
	if got := d.Object("Error"); got == nil || got.Origin != ir.OriginBuiltin {
		t.Fatalf("Object(Error) = %v", got)
	}
}

func TestResolveReachesTheFullColumn(t *testing.T) {
	t.Parallel()

	d := sample()
	ref := d.API.Resources[0].Fields[0].Column

	col := d.Resolve(ref)
	if col == nil {
		t.Fatal("Resolve returned nil for a valid reference")
	}
	// The reference carries only what generators need inline; the resolved
	// column carries everything else.
	if col.Ordinal != 3 || col.EnumType != "lesson_status" {
		t.Fatalf("resolved the wrong column: %+v", col)
	}
	if d.Resolve(nil) != nil {
		t.Fatal("Resolve(nil) should be nil")
	}
	if d.Resolve(&ir.ColumnRef{Table: "lesson", Name: "gone"}) != nil {
		t.Fatal("Resolve of a dangling reference should be nil")
	}
}

func TestTypeKindOf(t *testing.T) {
	t.Parallel()

	d := sample()
	for _, tc := range []struct {
		name string
		want ir.TypeKind
		ok   bool
	}{
		{"String", ir.TypeKindPrimitive, true},
		{"UUID", ir.TypeKindPrimitive, true},
		{"LessonStatus", ir.TypeKindEnum, true},
		{"Lesson", ir.TypeKindResource, true},
		{"Error", ir.TypeKindObject, true},
		{"Nonexistent", "", false},
	} {
		got, ok := d.TypeKindOf(tc.name)
		if got != tc.want || ok != tc.ok {
			t.Errorf("TypeKindOf(%q) = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func TestIndexHelpers(t *testing.T) {
	t.Parallel()

	idx := ir.Index{Name: "i", Columns: []string{"tenant_id", "starts_at"}}
	if !idx.LeadsWith("tenant_id") {
		t.Error("LeadsWith(tenant_id) should be true")
	}
	if idx.LeadsWith("starts_at") {
		t.Error("LeadsWith(starts_at) should be false: it is not the first column")
	}
	if !idx.Covers("starts_at") {
		t.Error("Covers(starts_at) should be true")
	}
	if idx.Covers("nope") {
		t.Error("Covers(nope) should be false")
	}

	// A partial index cannot serve a general lookup, so it never leads.
	partial := ir.Index{Name: "p", Columns: []string{"tenant_id"}, Partial: "deleted_at IS NULL"}
	if partial.LeadsWith("tenant_id") {
		t.Error("a partial index should not count as leading with a column")
	}
}

func TestFieldModifiers(t *testing.T) {
	t.Parallel()

	f := ir.Field{Modifiers: []string{ir.ModifierNullable}}
	if !f.IsNullable() || f.IsArray() {
		t.Fatalf("nullable field misread: nullable=%v array=%v", f.IsNullable(), f.IsArray())
	}

	arr := ir.Field{Modifiers: []string{ir.ModifierArray}}
	if arr.IsNullable() || !arr.IsArray() {
		t.Fatalf("array field misread: nullable=%v array=%v", arr.IsNullable(), arr.IsArray())
	}
}

func TestEmbeddedResourceFieldFlattensInJSON(t *testing.T) {
	t.Parallel()

	// ResourceField embeds Field. The wire format must be flat, not nested
	// under a "Field" key, or every consumer of the IR has to special-case it.
	encoded, err := ir.Marshal(sample())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), `"Field"`) {
		t.Fatalf("ResourceField did not flatten:\n%s", encoded)
	}
	if !strings.Contains(string(encoded), `"wire": "status"`) {
		t.Fatalf("embedded field keys missing from the encoding:\n%s", encoded)
	}
}

func TestHasEndpointDrivesShadowing(t *testing.T) {
	t.Parallel()

	r := sample().Resource("Lesson")
	if !r.HasEndpoint("Get") {
		t.Error("HasEndpoint(Get) should be true")
	}
	if r.HasEndpoint("Publish") {
		t.Error("HasEndpoint(Publish) should be false")
	}
	if !r.Supports(ir.OpList) || r.Supports(ir.OpDelete) {
		t.Error("Supports misread the operation set")
	}
}

func TestReindexAfterMutation(t *testing.T) {
	t.Parallel()

	d := sample()
	_ = d.Table("lesson") // force the index to build

	d.Schema.Tables = append(d.Schema.Tables, ir.Table{Name: "fixture", Kind: ir.TableKindBase})
	if d.Table("fixture") != nil {
		t.Fatal("a stale index should not see a table appended after indexing")
	}

	d.Reindex()
	if d.Table("fixture") == nil {
		t.Fatal("Reindex did not pick up the appended table")
	}
}

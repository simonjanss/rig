package compile_test

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/pkg/ir"
)

// simpleAPI is a hand-built projection, so the expansion tests do not depend on
// the projection stage being right.
func simpleAPI() ir.API {
	return ir.API{
		Name: "Demo", Version: "v1", BasePath: "/api/v1",
		Resources: []ir.Resource{{
			Name: "Lesson", Plural: "Lessons", PathSegment: "lessons",
			Operations: []string{ir.OpCreate, ir.OpGet, ir.OpList, ir.OpSearch, ir.OpUpdate, ir.OpDelete},
			Fields: []ir.ResourceField{
				{
					Field: ir.Field{
						Name: "Title", Wire: "title", Type: ir.TypeString, GoType: "string",
						Column: &ir.ColumnRef{Table: "lesson", Name: "title", SQLType: "text"},
					},
					Operations: []string{ir.FieldOpRead, ir.FieldOpCreate, ir.FieldOpUpdate},
				},
				{
					Field: ir.Field{
						Name: "StartsAt", Wire: "startsAt", Type: ir.TypeTimestamp, GoType: "time.Time",
						Immutable: true,
						Column:    &ir.ColumnRef{Table: "lesson", Name: "starts_at", SQLType: "timestamptz"},
					},
					Operations: []string{ir.FieldOpRead, ir.FieldOpCreate},
				},
				{
					Field: ir.Field{
						Name: "CreatedAt", Wire: "createdAt", Type: ir.TypeTimestamp, GoType: "time.Time",
						ReadOnly: true,
						Column:   &ir.ColumnRef{Table: "lesson", Name: "created_at", SQLType: "timestamptz"},
					},
					Operations: []string{ir.FieldOpRead},
				},
			},
			Storage: &ir.ResourceStorage{Table: "lesson", PrimaryKey: []string{"id"}},
		}},
	}
}

// TestExpandIsIdempotent is the property that lets expansion run wherever it is
// convenient without anyone having to track whether it already has.
func TestExpandIsIdempotent(t *testing.T) {
	t.Parallel()

	once, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{})
	twice, _ := compile.Expand(once, compile.ExpandOptions{})

	if diff := cmp.Diff(once, twice); diff != "" {
		t.Fatalf("expanding twice differs from expanding once (-once +twice):\n%s", diff)
	}
}

// TestExpandDoesNotMutateItsInput guards against the aliasing bug this kind of
// code invites: copying a struct duplicates its slice headers, so appending to
// a copy can write through into the original whenever capacity allows.
func TestExpandDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	before := cmp.Diff(simpleAPI(), in) // sanity: identical to a fresh copy
	if before != "" {
		t.Fatalf("test setup is not reproducible:\n%s", before)
	}

	_, _ = compile.Expand(in, compile.ExpandOptions{})

	if diff := cmp.Diff(simpleAPI(), in); diff != "" {
		t.Fatalf("Expand modified its input (-want +got):\n%s", diff)
	}
}

func TestExpandGeneratesCRUD(t *testing.T) {
	t.Parallel()

	api, diags := compile.Expand(simpleAPI(), compile.ExpandOptions{})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}

	res := api.Resources[0]
	want := []string{ir.OpCreate, ir.OpGet, ir.OpList, ir.OpSearch, ir.OpUpdate, ir.OpDelete}
	for _, op := range want {
		if !res.HasEndpoint(op) {
			t.Errorf("no %s endpoint was generated", op)
		}
	}

	for _, name := range []string{"Error", "Pagination", "Lesson", "LessonListResponse", "LessonFilter"} {
		if !hasObject(api, name) {
			t.Errorf("object %q was not generated", name)
		}
	}
	if !hasEnum(api, "ErrorCode") {
		t.Error("the ErrorCode enum was not generated")
	}
}

// TestImmutableFieldIsAbsentFromUpdate is the whole point of the immutable
// flag: it is not a runtime check, it is a field that does not exist.
func TestImmutableFieldIsAbsentFromUpdate(t *testing.T) {
	t.Parallel()

	api, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{})
	res := api.Resources[0]

	create := res.Endpoint(ir.OpCreate)
	if !hasParam(create.Request.BodyParams, "StartsAt") {
		t.Error("an immutable field should still be settable on create")
	}

	update := res.Endpoint(ir.OpUpdate)
	if hasParam(update.Request.BodyParams, "StartsAt") {
		t.Error("an immutable field must not appear in the update body at all")
	}
	if !hasParam(update.Request.BodyParams, "Title") {
		t.Error("an ordinary field should be updatable")
	}
	if hasParam(update.Request.BodyParams, "CreatedAt") {
		t.Error("a read-only field must not be writable")
	}
}

// TestHandWrittenEndpointShadowsGenerated is what makes the configuration an
// escape hatch rather than a suggestion.
func TestHandWrittenEndpointShadowsGenerated(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	in.Resources[0].Endpoints = []ir.Endpoint{{
		Name: ir.OpGet, Method: "GET", Path: "/{id}",
		Summary: "hand written",
		Impl:    ir.EndpointImpl{Kind: ir.EndpointCustom, ServiceMethod: "Get", HandlerName: "GetLesson"},
	}}

	api, diags := compile.Expand(in, compile.ExpandOptions{})
	res := api.Resources[0]

	var gets int
	for _, e := range res.Endpoints {
		if e.Name == ir.OpGet {
			gets++
		}
	}
	if gets != 1 {
		t.Fatalf("got %d Get endpoints, want exactly 1", gets)
	}
	if res.Endpoint(ir.OpGet).Summary != "hand written" {
		t.Error("the hand-written endpoint should have won")
	}

	// The shadowing is reported, so it is visible rather than mysterious.
	if !strings.Contains(diags.String(), "hand-written") {
		t.Errorf("shadowing should be reported as a note:\n%s", diags.String())
	}
	if diags.HasErrors() {
		t.Errorf("shadowing is not an error:\n%s", diags.String())
	}
}

func TestSearchMethod(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		method      string
		wantMethod  string
		wantPath    string
		wantAliases int
	}{
		{"both", "QUERY", "", 1},
		{"query", "QUERY", "", 0},
		{"post", "POST", "/_search", 0},
		{"", "QUERY", "", 1},
	} {
		api, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{SearchMethod: tc.method})
		e := api.Resources[0].Endpoint(ir.OpSearch)

		if e.Method != tc.wantMethod || e.Path != tc.wantPath {
			t.Errorf("search_method %q gave %s %q, want %s %q",
				tc.method, e.Method, e.Path, tc.wantMethod, tc.wantPath)
		}
		if len(e.AliasPatterns) != tc.wantAliases {
			t.Errorf("search_method %q gave %d aliases, want %d", tc.method, len(e.AliasPatterns), tc.wantAliases)
		}
	}
}

func TestFreezeComputesRoutes(t *testing.T) {
	t.Parallel()

	api, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{})
	doc, diags := compile.Freeze(api, ir.Schema{Name: "public"}, compile.Meta{Tool: "test"})
	// The schema is empty here, so column references cannot resolve; that is
	// checked on its own below.
	_ = diags

	res := doc.Resource("Lesson")
	for _, tc := range []struct{ name, want string }{
		{ir.OpGet, "GET /api/v1/lessons/{id}"},
		{ir.OpList, "GET /api/v1/lessons"},
		{ir.OpCreate, "POST /api/v1/lessons"},
		{ir.OpUpdate, "PATCH /api/v1/lessons/{id}"},
		{ir.OpDelete, "DELETE /api/v1/lessons/{id}"},
		{ir.OpSearch, "QUERY /api/v1/lessons"},
	} {
		e := res.Endpoint(tc.name)
		if e == nil {
			t.Errorf("no %s endpoint", tc.name)
			continue
		}
		if e.Pattern != tc.want {
			t.Errorf("%s pattern = %q, want %q", tc.name, e.Pattern, tc.want)
		}
	}

	// The alias is expanded to a full route, like the endpoint's own pattern.
	search := res.Endpoint(ir.OpSearch)
	if len(search.AliasPatterns) != 1 || search.AliasPatterns[0] != "POST /api/v1/lessons/_search" {
		t.Errorf("alias = %v, want [POST /api/v1/lessons/_search]", search.AliasPatterns)
	}

	// Operation ids follow the handler names, so they carry the same
	// cardinality rather than inventing a second pluralization rule.
	if got := res.Endpoint(ir.OpList).OperationID; got != "listLessons" {
		t.Errorf("list operation id = %q, want listLessons", got)
	}
	if got := res.Endpoint(ir.OpGet).OperationID; got != "getLesson" {
		t.Errorf("get operation id = %q, want getLesson", got)
	}
}

// TestFreezeCatchesColumnDrift is the check that makes the document's two views
// structurally unable to disagree.
func TestFreezeCatchesColumnDrift(t *testing.T) {
	t.Parallel()

	schema := ir.Schema{
		Name: "public",
		Tables: []ir.Table{{
			Name: "lesson", Kind: ir.TableKindBase,
			Columns: []ir.Column{
				{Name: "title", SQLType: "text", Nullable: false, Ordinal: 1},
			},
		}},
	}

	api := ir.API{
		Name: "Demo", BasePath: "/api/v1",
		Objects: []ir.Object{{
			Name: "Lesson", Origin: ir.OriginProjected,
			Fields: []ir.Field{{
				Name: "Title", Wire: "title", Type: ir.TypeString,
				// Claims the column is nullable; the schema says otherwise.
				Column: &ir.ColumnRef{Table: "lesson", Name: "title", SQLType: "text", Nullable: true},
			}},
		}},
	}

	_, diags := compile.Freeze(api, schema, compile.Meta{Tool: "test"})
	if !diags.HasErrors() {
		t.Fatal("a reference that disagrees with the schema should be an error")
	}
	msg := diags.String()
	if !strings.Contains(msg, "RIG9001") {
		t.Errorf("drift should report the internal-consistency code:\n%s", msg)
	}
	// It is rig's bug, not the project's, and the message should say so.
	if !strings.Contains(msg, "bug in rig") {
		t.Errorf("the hint should name it as a rig bug:\n%s", msg)
	}
}

func TestFreezeRejectsDuplicateRoutes(t *testing.T) {
	t.Parallel()

	api := simpleAPI()
	api.Resources[0].Operations = nil
	api.Resources[0].Endpoints = []ir.Endpoint{
		{Name: "A", Method: "GET", Path: "/{id}", Impl: ir.EndpointImpl{HandlerName: "A"}},
		{Name: "B", Method: "GET", Path: "/{id}", Impl: ir.EndpointImpl{HandlerName: "B"}},
	}

	_, diags := compile.Freeze(api, ir.Schema{Name: "public"}, compile.Meta{Tool: "test"})
	if !strings.Contains(diags.String(), "already served by") {
		t.Fatalf("two endpoints on one route should be rejected:\n%s", diags.String())
	}
}

func TestNormalizeSortsAndLinksColumns(t *testing.T) {
	t.Parallel()

	schema, diags := compile.Normalize(ir.Schema{
		Tables: []ir.Table{
			{Name: "b", Kind: ir.TableKindBase, Columns: []ir.Column{{Name: "id", SQLType: "uuid", Ordinal: 1}}, PrimaryKey: []string{"id"}},
			{
				Name: "a", Kind: ir.TableKindBase,
				Columns: []ir.Column{
					{Name: "b_id", SQLType: "uuid", Ordinal: 2},
					{Name: "id", SQLType: "uuid", Ordinal: 1},
				},
				PrimaryKey: []string{"id"},
				ForeignKeys: []ir.ForeignKey{{
					Name: "a_b_fk", Columns: []string{"b_id"},
					ForeignTable: "b", ForeignColumns: []string{"id"},
				}},
			},
		},
	}, compile.NormalizeOptions{})

	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}

	if schema.Tables[0].Name != "a" || schema.Tables[1].Name != "b" {
		t.Errorf("tables were not sorted: %s, %s", schema.Tables[0].Name, schema.Tables[1].Name)
	}
	a := schema.Tables[0]
	if a.Columns[0].Name != "id" {
		t.Errorf("columns were not ordered by ordinal: %v", columnNames(a))
	}
	if !a.Columns[0].IsPrimaryKey {
		t.Error("the primary key flag was not applied")
	}
	// A single-column foreign key is denormalized onto its column, so a
	// generator does not have to search the constraint list for it.
	bID := a.Column("b_id")
	if bID.ForeignKey == nil || bID.ForeignKey.Table != "b" {
		t.Errorf("foreign key was not linked onto the column: %+v", bID.ForeignKey)
	}
}

func TestNormalizeIgnoresTables(t *testing.T) {
	t.Parallel()

	schema, _ := compile.Normalize(ir.Schema{
		Tables: []ir.Table{
			{Name: "keep", Kind: ir.TableKindBase},
			{Name: "rig_migrations", Kind: ir.TableKindBase},
		},
	}, compile.NormalizeOptions{IgnoreTables: []string{"rig_migrations"}})

	if len(schema.Tables) != 1 || schema.Tables[0].Name != "keep" {
		t.Fatalf("ignored table was projected: %v", tableNames(schema))
	}
}

func TestNormalizeRejectsUnmappableType(t *testing.T) {
	t.Parallel()

	_, diags := compile.Normalize(ir.Schema{
		Tables: []ir.Table{{
			Name: "doc", Kind: ir.TableKindBase,
			Columns: []ir.Column{{Name: "vec", SQLType: "tsvector", UDTName: "tsvector", Ordinal: 1}},
		}},
	}, compile.NormalizeOptions{})

	if !diags.HasErrors() {
		t.Fatal("an unmapped type should be refused rather than guessed at")
	}
	if !strings.Contains(diags.String(), "tsvector") {
		t.Errorf("the message should name the type:\n%s", diags.String())
	}
}

func TestNormalizeDoesNotMutateItsInput(t *testing.T) {
	t.Parallel()

	build := func() ir.Schema {
		return ir.Schema{
			Tables: []ir.Table{{
				Name: "t", Kind: ir.TableKindBase,
				Columns:    []ir.Column{{Name: "id", SQLType: "uuid", Ordinal: 1}},
				PrimaryKey: []string{"id"},
			}},
		}
	}

	in := build()
	_, _ = compile.Normalize(in, compile.NormalizeOptions{})
	if diff := cmp.Diff(build(), in); diff != "" {
		t.Fatalf("Normalize modified its input (-want +got):\n%s", diff)
	}
}

func TestAutoCommentsCoverManagedColumns(t *testing.T) {
	t.Parallel()

	schema, _ := compile.Normalize(ir.Schema{
		Tables: []ir.Table{{
			Name: "lesson", Kind: ir.TableKindBase,
			Columns: []ir.Column{
				{Name: "id", SQLType: "uuid", Ordinal: 1},
				{Name: "created_at", SQLType: "timestamptz", Ordinal: 2},
				{Name: "title", SQLType: "text", Ordinal: 3},
				{Name: "snapshot_from_lesson_at", SQLType: "timestamptz", Nullable: true, Ordinal: 4},
			},
			PrimaryKey: []string{"id"},
		}},
	}, compile.NormalizeOptions{})

	t0 := schema.Tables[0]
	for _, name := range []string{"id", "created_at", "snapshot_from_lesson_at"} {
		c := t0.Column(name)
		if c.Comment == "" {
			t.Errorf("column %q should have a generated comment", name)
		}
		if c.CommentSource != ir.CommentSourceAuto {
			t.Errorf("column %q comment source = %q, want auto", name, c.CommentSource)
		}
	}

	// A column rig knows nothing about is left alone, so the missing-comment
	// rule can still ask for one.
	if c := t0.Column("title"); c.Comment != "" || c.CommentSource != ir.CommentSourceNone {
		t.Errorf("an ordinary column should not be auto-commented: %+v", c)
	}
}

func TestLinkTableIsNotAResource(t *testing.T) {
	t.Parallel()

	schema, _ := compile.Normalize(linkSchema(), compile.NormalizeOptions{})

	link := schema.Tables[len(schema.Tables)-1]
	if link.Name != "team_player" {
		t.Fatalf("unexpected table order: %v", tableNames(schema))
	}
	if link.LinkTable == nil {
		t.Fatal("a two-key join table should be recognized as a link")
	}
	if link.LinkTable.Table != "team_player" {
		t.Errorf("the link should name the join table itself, got %q", link.LinkTable.Table)
	}

	api, _ := compile.Project(schema, compile.ProjectOptions{Name: "Demo", BasePath: "/api/v1"})
	for _, r := range api.Resources {
		if r.Name == "TeamPlayer" {
			t.Error("a join table must not become a resource: it would give clients CRUD over a join row")
		}
	}
}

// TestLinkTableWithDataIsAResource: a join row that carries its own columns is
// an entity, not a link, and hiding it would lose those columns entirely.
func TestLinkTableWithDataIsAResource(t *testing.T) {
	t.Parallel()

	raw := linkSchema()
	last := len(raw.Tables) - 1
	raw.Tables[last].Columns = append(raw.Tables[last].Columns,
		ir.Column{Name: "shirt_number", SQLType: "int4", Ordinal: 3})

	schema, _ := compile.Normalize(raw, compile.NormalizeOptions{})
	for _, t0 := range schema.Tables {
		if t0.Name == "team_player" && t0.LinkTable != nil {
			t.Fatal("a join table carrying data should stay a resource")
		}
	}
}

func linkSchema() ir.Schema {
	return ir.Schema{
		Tables: []ir.Table{
			{
				Name: "team", Kind: ir.TableKindBase,
				Columns:    []ir.Column{{Name: "id", SQLType: "uuid", Ordinal: 1}},
				PrimaryKey: []string{"id"},
			},
			{
				Name: "player", Kind: ir.TableKindBase,
				Columns:    []ir.Column{{Name: "id", SQLType: "uuid", Ordinal: 1}},
				PrimaryKey: []string{"id"},
			},
			{
				Name: "team_player", Kind: ir.TableKindBase,
				Columns: []ir.Column{
					{Name: "team_id", SQLType: "uuid", Ordinal: 1, ForeignKey: &ir.FKRef{Table: "team", Column: "id"}},
					{Name: "player_id", SQLType: "uuid", Ordinal: 2, ForeignKey: &ir.FKRef{Table: "player", Column: "id"}},
				},
				PrimaryKey: []string{"team_id", "player_id"},
			},
		},
	}
}

func hasObject(api ir.API, name string) bool {
	for _, o := range api.Objects {
		if o.Name == name {
			return true
		}
	}
	return false
}

func hasEnum(api ir.API, name string) bool {
	for _, e := range api.Enums {
		if e.Name == name {
			return true
		}
	}
	return false
}

func hasParam(fields []ir.Field, name string) bool {
	for _, f := range fields {
		if f.Name == name {
			return true
		}
	}
	return false
}

func columnNames(t ir.Table) []string {
	out := make([]string, 0, len(t.Columns))
	for _, c := range t.Columns {
		out = append(out, c.Name)
	}
	return out
}

func tableNames(s ir.Schema) []string {
	out := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		out = append(out, t.Name)
	}
	return out
}

package compile_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
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

// An enum's Go name is one line of configuration, and every field that referred
// to the derived name has to move with it. Leaving a reference behind produces
// a document whose fields have a type nothing declares, which surfaces two
// stages later as an unresolvable type rather than as the rename it was.
func TestRenamingAnEnumMovesEveryReferenceToIt(t *testing.T) {
	t.Parallel()

	schema := readSchema(t, filepath.Join("testdata", "lifecycle", "schema.json"))

	p, pdiags := project.Parse("rig.yaml", []byte(defaultProject))
	if pdiags.HasErrors() {
		t.Fatal(pdiags.String())
	}

	raw, err := os.ReadFile(filepath.Join("testdata", "lifecycle", "tables", "lesson.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(string(raw), "name: LessonStatus", "name: Stage", 1)
	if renamed == string(raw) {
		t.Fatal("the fixture no longer names LessonStatus")
	}

	loaded, ldiags := tableconf.Parse("lesson.yaml", []byte(renamed))
	if ldiags.HasErrors() {
		t.Fatalf("the renamed configuration does not parse:\n%s", ldiags.String())
	}
	set := tableconf.NewSet()
	set.Add(loaded)

	doc, _ := compile.Compile(schema, set, compile.Options{Project: p, Tool: "rig (test)"})
	if doc == nil {
		t.Fatal("no document")
	}

	if doc.Enum("Stage") == nil {
		t.Error("the enum should be declared under its configured name")
	}
	if doc.Enum("LessonStatus") != nil {
		t.Error("the derived name should be gone, not declared twice")
	}

	res := doc.Resource("Lesson")
	if res == nil {
		t.Fatal("no Lesson resource")
	}
	status := res.Field("Status")
	if status == nil {
		t.Fatal("no Status field")
	}
	if status.Type != "Stage" {
		t.Errorf("Type = %q, want Stage", status.Type)
	}
	// The Go type is the same name behind whatever pointer or slice the
	// column's nullability added, so only the name part moves.
	if !strings.HasSuffix(status.GoType, "Stage") {
		t.Errorf("GoType = %q, want it to end in Stage", status.GoType)
	}

	// And nothing anywhere in the document still names the old one — Freeze
	// resolves every field's type against the declared enums, so a reference
	// left behind is a document that cannot be generated from.
	marshalled, err := ir.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(marshalled), `"LessonStatus"`) {
		t.Error("a reference to the derived name survived the rename")
	}
}

// snapshotAPI is [simpleAPI] with the storage that makes a resource versioned.
func snapshotAPI() ir.API {
	api := simpleAPI()
	api.Resources[0].Storage.Snapshot = &ir.Snapshot{
		VersionType: &ir.ColumnRef{Table: "lesson", Name: "version_type"},
		FromID:      &ir.ColumnRef{Table: "lesson", Name: "snapshot_from_lesson_id"},
		FromAt:      &ir.ColumnRef{Table: "lesson", Name: "snapshot_from_lesson_at"},
	}
	return api
}

// The snapshot columns are in the table, so the resource has a history and
// there is a route to read it. Nothing in the configuration asks for these:
// they follow from the schema, the same way soft delete does.
func TestASnapshotTableGetsItsHistoryEndpoints(t *testing.T) {
	t.Parallel()

	api, diags := compile.Expand(snapshotAPI(), compile.ExpandOptions{})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}
	res := api.Resources[0]

	versions := res.Endpoint(ir.OpVersions)
	if versions == nil {
		t.Fatal("no Versions endpoint")
	}
	if versions.Method != "GET" || versions.Path != "/{id}/_versions" {
		t.Errorf("Versions is %s %s", versions.Method, versions.Path)
	}
	// The same envelope a list is answered with, rather than a second shape for
	// the same thing.
	if got := versions.Responses[0].BodyObject; got != "LessonListResponse" {
		t.Errorf("Versions answers with %q", got)
	}
	if versions.Impl.RepoMethod != "ListSnapshots" {
		t.Errorf("Versions calls %q", versions.Impl.RepoMethod)
	}

	revert := res.Endpoint(ir.OpRevert)
	if revert == nil {
		t.Fatal("no Revert endpoint")
	}
	if revert.Method != "POST" || revert.Path != "/{id}/_revert" {
		t.Errorf("Revert is %s %s", revert.Method, revert.Path)
	}
	if !hasParam(revert.Request.BodyParams, "VersionID") {
		t.Errorf("Revert takes %+v, want a version to put back", revert.Request.BodyParams)
	}
	if got := revert.Responses[0].BodyObject; got != "Lesson" {
		t.Errorf("Revert answers with %q", got)
	}

	// A table with no snapshot columns has no history to offer.
	plain, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{})
	if plain.Resources[0].HasEndpoint(ir.OpVersions) || plain.Resources[0].HasEndpoint(ir.OpRevert) {
		t.Error("a table that keeps no versions should get neither endpoint")
	}
}

// Each one follows from the operation it depends on. Offering the history of a
// row a client cannot fetch is an odd door to leave open, and reverting is an
// update — a resource that does not expose updates must not gain a second way
// to write a row with different permissions.
func TestTheHistoryEndpointsFollowTheOperationsTheyDependOn(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		operations []string
		versions   bool
		revert     bool
	}{
		"the full set":   {[]string{ir.OpGet, ir.OpUpdate}, true, true},
		"read only":      {[]string{ir.OpGet, ir.OpList}, true, false},
		"no get":         {[]string{ir.OpList, ir.OpUpdate}, false, false},
		"nothing at all": {[]string{ir.OpCreate}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			in := snapshotAPI()
			in.Resources[0].Operations = tc.operations

			api, _ := compile.Expand(in, compile.ExpandOptions{})
			res := api.Resources[0]

			if got := res.HasEndpoint(ir.OpVersions); got != tc.versions {
				t.Errorf("Versions = %v, want %v", got, tc.versions)
			}
			if got := res.HasEndpoint(ir.OpRevert); got != tc.revert {
				t.Errorf("Revert = %v, want %v", got, tc.revert)
			}
		})
	}
}

// The list envelope is emitted for the history even when nothing else lists,
// or Versions would answer with a type the document never declares.
func TestTheHistoryEnvelopeExistsWithoutAList(t *testing.T) {
	t.Parallel()

	in := snapshotAPI()
	in.Resources[0].Operations = []string{ir.OpGet}

	api, _ := compile.Expand(in, compile.ExpandOptions{})
	if !hasObject(api, "LessonListResponse") {
		t.Error("the response object Versions answers with was not generated")
	}
}

// Generated endpoints are skipped when one of the same name already exists, and
// these are no exception: a project whose history needs a permission check or a
// different shape writes its own.
func TestAHandWrittenVersionsEndpointStillWins(t *testing.T) {
	t.Parallel()

	in := snapshotAPI()
	in.Resources[0].Endpoints = []ir.Endpoint{{
		Name: ir.OpVersions, Method: "GET", Path: "/{id}/history",
		Impl: ir.EndpointImpl{Kind: ir.EndpointCustom, ServiceMethod: ir.OpVersions},
	}}

	api, diags := compile.Expand(in, compile.ExpandOptions{})
	res := api.Resources[0]

	if got := res.Endpoint(ir.OpVersions); got == nil || got.Path != "/{id}/history" {
		t.Errorf("the hand-written endpoint should have survived: %+v", got)
	}
	if len(res.Endpoints) != len(api.Resources[0].Endpoints) {
		t.Error("the generated one should not have been added alongside it")
	}
	if diags.Len() == 0 {
		t.Error("shadowing should be reported, so it is a choice rather than a surprise")
	}
}

// The filter is not only Search's request body: it is how anything selects
// rows, and the repository takes one whether or not the API exposes a search.
// Emitting it only for searchable resources left the model with no shape to
// declare and List with nothing to pass.
func TestTheFilterExistsWithoutASearch(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	in.Resources[0].Operations = []string{ir.OpGet}

	api, _ := compile.Expand(in, compile.ExpandOptions{})
	for _, name := range []string{
		"LessonFilter", "LessonFilterEquals", "LessonFilterRange",
		"LessonFilterContains", "LessonFilterLike", "LessonFilterNull",
	} {
		if !hasObject(api, name) {
			t.Errorf("%s was not generated", name)
		}
	}

	// A resource with no storage has no rows to select, so it has no filter.
	none := simpleAPI()
	none.Resources[0].Storage = nil
	if bare, _ := compile.Expand(none, compile.ExpandOptions{}); hasObject(bare, "LessonFilter") {
		t.Error("a resource backed by no table should have no filter")
	}

	// An unexposed table has no API at all and still has a repository, and that
	// repository still lists. Nothing routes to it, so nothing in the document
	// references the shape — which is what keeps it out of the OpenAPI.
	hidden := simpleAPI()
	hidden.Resources[0].Unexposed = true

	internal, _ := compile.Expand(hidden, compile.ExpandOptions{})
	if !hasObject(internal, "LessonFilter") {
		t.Error("an unexposed table still needs the filter its repository takes")
	}
	if hasObject(internal, "LessonListResponse") || len(internal.Resources[0].Endpoints) != 0 {
		t.Error("an unexposed table should gain no wire shape and no route")
	}
}

// The filter is the wire shape, so what it can name is what a client may read.
// A column excluded from the API would otherwise still be filterable, and a
// filter is enough to binary-search a value nobody was meant to see.
func TestAFilterNamesOnlyReadableFields(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	// Readable everywhere except here: a column somebody decided clients should
	// not see.
	in.Resources[0].Fields = append(in.Resources[0].Fields, ir.ResourceField{
		Field: ir.Field{
			Name: "InternalRevision", Wire: "internalRevision", Type: ir.TypeInt, GoType: "int",
			Column: &ir.ColumnRef{Table: "lesson", Name: "internal_revision", SQLType: "integer"},
		},
		Operations: []string{ir.FieldOpUpdate},
	})

	api, _ := compile.Expand(in, compile.ExpandOptions{})

	equals := objectNamed(api, "LessonFilterEquals")
	if equals == nil {
		t.Fatal("no LessonFilterEquals")
	}
	for _, f := range equals.Fields {
		if f.Name == "InternalRevision" {
			t.Error("a field a client cannot read should not be one it can filter on")
		}
	}
	if !hasParam(equals.Fields, "Title") {
		t.Error("an ordinary readable field should be filterable")
	}
}

func objectNamed(api ir.API, name string) *ir.Object {
	for i := range api.Objects {
		if api.Objects[i].Name == name {
			return &api.Objects[i]
		}
	}
	return nil
}

// A soft delete retires the row rather than removing it, so there has to be
// somewhere it went. The trash follows from the deleted_at column, the same way
// the history follows from the snapshot ones.
func TestASoftDeletableTableGetsItsTrash(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	in.Resources[0].Storage.SoftDelete = &ir.SoftDelete{
		Column:            &ir.ColumnRef{Table: "lesson", Name: "deleted_at"},
		RestoreWindowDays: 30,
	}

	api, diags := compile.Expand(in, compile.ExpandOptions{})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}

	ep := api.Resources[0].Endpoint(ir.OpListDeleted)
	if ep == nil {
		t.Fatal("no ListDeleted endpoint")
	}
	if ep.Method != "GET" || ep.Path != "/_deleted" {
		t.Errorf("ListDeleted is %s %s", ep.Method, ep.Path)
	}
	if got := ep.Responses[0].BodyObject; got != "LessonListResponse" {
		t.Errorf("it answers with %q, want the same envelope a list does", got)
	}
	if !hasParam(ep.Request.QueryParams, "Limit") {
		t.Errorf("the trash pages like any other listing: %+v", ep.Request.QueryParams)
	}
	// The window is in the description, because "deleted" and "still
	// restorable" are not the same set and a client cannot tell from the shape.
	if !strings.Contains(ep.Description, "30-day") {
		t.Errorf("the description should say the window: %q", ep.Description)
	}

	// The way back out of it.
	restore := api.Resources[0].Endpoint(ir.OpRestore)
	if restore == nil {
		t.Fatal("no Restore endpoint")
	}
	if restore.Method != "POST" || restore.Path != "/{id}/_restore" {
		t.Errorf("Restore is %s %s", restore.Method, restore.Path)
	}
	if got := restore.Responses[0].BodyObject; got != "Lesson" {
		t.Errorf("it answers with %q, want the row it brought back", got)
	}
	if !slices.Contains(restore.Errors, 409) {
		t.Errorf("a row past the window is a conflict: %v", restore.Errors)
	}

	// It carries no fields. What to do when the world has moved on is the
	// application's decision, made in the hook, not something a caller sends.
	if len(restore.Request.BodyParams) != 0 {
		t.Errorf("a restore takes no body: %+v", restore.Request.BodyParams)
	}

	// A table nothing retires has neither.
	plain, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{})
	if plain.Resources[0].HasEndpoint(ir.OpListDeleted) || plain.Resources[0].HasEndpoint(ir.OpRestore) {
		t.Error("a table without a deleted_at column should get neither")
	}
}

// Each follows the operation it is a variety of. Listing the trash is a
// listing; bringing a row back is a write, and a resource that does not expose
// writes must not gain one through the back door.
func TestTheTrashEndpointsFollowTheOperationsTheyDependOn(t *testing.T) {
	t.Parallel()

	soft := func() ir.API {
		api := simpleAPI()
		api.Resources[0].Storage.SoftDelete = &ir.SoftDelete{
			Column:            &ir.ColumnRef{Table: "lesson", Name: "deleted_at"},
			RestoreWindowDays: 30,
		}
		return api
	}

	for name, tc := range map[string]struct {
		operations  []string
		listDeleted bool
		restore     bool
	}{
		"the full set":   {[]string{ir.OpList, ir.OpUpdate, ir.OpDelete}, true, true},
		"read only":      {[]string{ir.OpList, ir.OpGet}, true, false},
		"no list":        {[]string{ir.OpGet, ir.OpUpdate}, false, true},
		"nothing at all": {[]string{ir.OpCreate}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			in := soft()
			in.Resources[0].Operations = tc.operations

			api, _ := compile.Expand(in, compile.ExpandOptions{})
			res := api.Resources[0]

			if got := res.HasEndpoint(ir.OpListDeleted); got != tc.listDeleted {
				t.Errorf("ListDeleted = %v, want %v", got, tc.listDeleted)
			}
			if got := res.HasEndpoint(ir.OpRestore); got != tc.restore {
				t.Errorf("Restore = %v, want %v", got, tc.restore)
			}
		})
	}
}

// The tenant is not a condition anybody gets to write.
//
// Every read is already scoped to the caller's, ANDed above whatever filter
// they sent, so a condition on it could only be a no-op or a contradiction. A
// field that cannot filter should not be in a filter: it documents a control
// that does nothing and invites somebody to think it might.
func TestTheTenantIsNotFilterable(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	in.Resources[0].Storage.Tenant = &ir.ColumnRef{Table: "lesson", Name: "tenant_id"}
	in.Resources[0].Fields = append(in.Resources[0].Fields, ir.ResourceField{
		Field: ir.Field{
			Name: "TenantID", Wire: "tenantId", Type: ir.TypeUUID, GoType: "uuid.UUID",
			Column: &ir.ColumnRef{Table: "lesson", Name: "tenant_id", SQLType: "uuid"},
		},
		Operations: []string{ir.FieldOpRead},
	})

	api, _ := compile.Expand(in, compile.ExpandOptions{})

	for _, name := range []string{
		"LessonFilterEquals", "LessonFilterContains", "LessonFilterRange", "LessonFilterNull",
	} {
		obj := objectNamed(api, name)
		if obj == nil {
			continue
		}
		if hasParam(obj.Fields, "TenantID") {
			t.Errorf("%s offers a condition on the tenant", name)
		}
	}

	// It stays on the resource itself: which tenant a row belongs to is worth
	// answering with, it is just not worth asking about.
	entity := objectNamed(api, "Lesson")
	if entity == nil || !hasParam(entity.Fields, "TenantID") {
		t.Error("the tenant should still be readable on the row")
	}
}

// An operation named in `public:` answers without a credential. Both halves of
// the namespace are covered — a generated operation and a custom endpoint share
// it, since a custom endpoint called Get replaces the generated one.
func TestPublicOperationsAnswerWithoutACredential(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	in.Resources[0].Public = []string{ir.OpGet, "Publish"}
	in.Resources[0].Endpoints = []ir.Endpoint{{
		Name: "Publish", Method: "POST", Path: "/{id}/_publish",
		Impl: ir.EndpointImpl{Kind: ir.EndpointCustom, ServiceMethod: "Publish"},
	}}

	api, diags := compile.Expand(in, compile.ExpandOptions{})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}
	res := api.Resources[0]

	for _, name := range []string{ir.OpGet, "Publish"} {
		if ep := res.Endpoint(name); ep == nil || !ep.Public {
			t.Errorf("%s should be public: %+v", name, ep)
		}
	}
	// Everything not named still needs one. That is the direction the default
	// has to run in: a list somebody forgot to add to is a protected endpoint,
	// not an open one.
	for _, name := range []string{ir.OpCreate, ir.OpList, ir.OpUpdate, ir.OpDelete} {
		if ep := res.Endpoint(name); ep != nil && ep.Public {
			t.Errorf("%s was not named and should not be public", name)
		}
	}
}

// A name that matches nothing reads as "this is open" while meaning nothing at
// all, which is the worst way for a typo in a security setting to fail.
func TestAPublicNameThatMatchesNothingIsAnError(t *testing.T) {
	t.Parallel()

	in := simpleAPI()
	in.Resources[0].Public = []string{"Lst"}

	_, diags := compile.Expand(in, compile.ExpandOptions{})
	if !diags.HasErrors() {
		t.Fatal("a name matching no operation should be refused")
	}
	if !strings.Contains(diags.String(), "Lst") {
		t.Errorf("the diagnostic should name it:\n%s", diags.String())
	}
}

// A relation is a condition beside the ones around it: it sits in the operator
// object, typed as the far side's object of the same kind, so a condition on a
// related row is written where a condition on this row would be.
func TestARelationIsFilterable(t *testing.T) {
	t.Parallel()

	in := relatedAPI()
	api, _ := compile.Expand(in, compile.ExpandOptions{})

	// Every operator, because a relation is a condition of whatever kind the
	// object it sits in is for. Only Equals would leave a relation filterable
	// by exact value and by nothing else.
	for _, suffix := range []string{
		"FilterEquals", "FilterRange", "FilterContains", "FilterLike", "FilterNull",
	} {
		obj := objectNamed(api, "Lesson"+suffix)
		if obj == nil {
			t.Errorf("no Lesson%s", suffix)
			continue
		}

		for _, rel := range []string{"Fixture", "Players"} {
			f := fieldNamed(obj.Fields, rel)
			if f == nil {
				t.Errorf("Lesson%s has no %s condition; got %v", suffix, rel, paramNames(obj.Fields))
				continue
			}
			// The far side's object of the same kind, so the recursion is
			// between objects that ask the same sort of question.
			want := strings.TrimSuffix(rel, "s") + suffix
			if rel == "Players" {
				want = "Player" + suffix
			}
			if f.Type != want {
				t.Errorf("Lesson%s.%s should be a %s, got %s", suffix, rel, want, f.Type)
			}
			if f.TypeKind != ir.TypeKindObject {
				t.Errorf("Lesson%s.%s should be an object, got %s", suffix, rel, f.TypeKind)
			}
			if f.GoType != "*"+want {
				t.Errorf("Lesson%s.%s should be optional, got %s", suffix, rel, f.GoType)
			}
		}
	}

	// And not at the top level, where it would be a second way to ask the same
	// thing with different semantics.
	filter := objectNamed(api, "LessonFilter")
	if filter == nil {
		t.Fatal("no LessonFilter")
	}
	if fieldNamed(filter.Fields, "Fixture") != nil {
		t.Errorf("the filter itself should carry operators, not relations; got %v",
			paramNames(filter.Fields))
	}
}

// Only a resource the API exposes can be filtered through. A relation to a
// table nobody can read would name a filter type no generator emits.
func TestARelationToAnUnexposedResourceIsNotFilterable(t *testing.T) {
	t.Parallel()

	in := relatedAPI()
	in.Resources[1].Unexposed = true
	api, _ := compile.Expand(in, compile.ExpandOptions{})

	obj := objectNamed(api, "LessonFilterEquals")
	if obj != nil && fieldNamed(obj.Fields, "Fixture") != nil {
		t.Errorf("a filter reaches through an unexposed resource; got %v", paramNames(obj.Fields))
	}
}

// The self-reference a snapshot table carries is bookkeeping, not a relation
// worth asking about: filtering a row by the row it is a copy of would offer
// the same filter one level down forever.
func TestTheSnapshotSelfReferenceIsNotFilterable(t *testing.T) {
	t.Parallel()

	in := relatedAPI()
	res := &in.Resources[0]
	res.Storage.Snapshot = &ir.Snapshot{
		VersionType: &ir.ColumnRef{Table: "lesson", Name: "version_type"},
		FromID:      &ir.ColumnRef{Table: "lesson", Name: "snapshot_from_lesson_id"},
		FromAt:      &ir.ColumnRef{Table: "lesson", Name: "snapshot_from_lesson_at"},
	}
	res.Storage.Relations = append(res.Storage.Relations, ir.Relation{
		Name: "SnapshotOf", Kind: ir.RelationBelongsTo, Target: "Lesson",
		LocalColumn: "snapshot_from_lesson_id", ForeignTable: "lesson", ForeignColumn: "id",
	})

	api, _ := compile.Expand(in, compile.ExpandOptions{})

	obj := objectNamed(api, "LessonFilterEquals")
	if obj != nil && fieldNamed(obj.Fields, "SnapshotOf") != nil {
		t.Errorf("a filter reaches through the snapshot self-reference; got %v", paramNames(obj.Fields))
	}
}

// relatedAPI is simpleAPI with a resource on each side of it: one it points at
// and one that points back.
func relatedAPI() ir.API {
	in := simpleAPI()
	in.Resources[0].Storage.Relations = []ir.Relation{
		{
			Name: "Fixture", Kind: ir.RelationBelongsTo, Target: "Fixture",
			LocalColumn: "fixture_id", ForeignTable: "fixture", ForeignColumn: "id",
		},
		{
			Name: "Players", Kind: ir.RelationHasMany, Target: "Player",
			ForeignTable: "player", ForeignColumn: "lesson_id",
		},
	}
	in.Resources = append(in.Resources,
		relatedResource("Fixture", "fixture"),
		relatedResource("Player", "player"),
	)
	return in
}

func relatedResource(name, table string) ir.Resource {
	return ir.Resource{
		Name: name, Plural: name + "s", PathSegment: table + "s",
		Operations: []string{ir.OpGet, ir.OpList, ir.OpSearch},
		Fields: []ir.ResourceField{{
			Field: ir.Field{
				Name: "Name", Wire: "name", Type: ir.TypeString, GoType: "string",
				Column: &ir.ColumnRef{Table: table, Name: "name", SQLType: "text"},
			},
			Operations: []string{ir.FieldOpRead},
		}},
		Storage: &ir.ResourceStorage{Table: table, PrimaryKey: []string{"id"}},
	}
}

func fieldNamed(fields []ir.Field, name string) *ir.Field {
	for i := range fields {
		if fields[i].Name == name {
			return &fields[i]
		}
	}
	return nil
}

func paramNames(fields []ir.Field) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Name)
	}
	return out
}

// Absence is the one question the operator objects cannot ask, so it gets an
// object of its own — carrying the far side's whole filter, because a negation
// is one condition about the far side and it matters whether the conditions
// inside it are ANDed or ORed.
func TestARelationCanBeAskedForItsAbsence(t *testing.T) {
	t.Parallel()

	api, _ := compile.Expand(relatedAPI(), compile.ExpandOptions{})

	obj := objectNamed(api, "LessonFilterWithout")
	if obj == nil {
		t.Fatal("no LessonFilterWithout")
	}
	for _, tc := range []struct{ field, typ string }{
		{"Fixture", "FixtureFilter"},
		{"Players", "PlayerFilter"},
	} {
		f := fieldNamed(obj.Fields, tc.field)
		if f == nil {
			t.Errorf("no %s; got %v", tc.field, paramNames(obj.Fields))
			continue
		}
		if f.Type != tc.typ || f.GoType != "*"+tc.typ {
			t.Errorf("%s should be an optional %s, got %s / %s", tc.field, tc.typ, f.Type, f.GoType)
		}
	}

	// Only columns can be null, so a relation has no business in the presence
	// operators pretending to be one.
	filter := objectNamed(api, "LessonFilter")
	if filter == nil || fieldNamed(filter.Fields, "Without") == nil {
		t.Error("the filter should offer Without")
	}
}

// A table with no relations has nothing to be without, and an object with no
// fields is a condition nobody can write.
func TestAResourceWithNoRelationsHasNoWithout(t *testing.T) {
	t.Parallel()

	api, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{})

	if objectNamed(api, "LessonFilterWithout") != nil {
		t.Error("LessonFilterWithout should not exist for a table with no relations")
	}
	if filter := objectNamed(api, "LessonFilter"); filter != nil &&
		fieldNamed(filter.Fields, "Without") != nil {
		t.Error("the filter should not offer a Without it has no object for")
	}
}

// A name ending in _at claims the column records when something happened, and a
// bare timestamp cannot: it is a clock reading with no anchor.
func TestAnInstantMustBeATimestamptz(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		sqlType string
		want    string
	}{
		{"timestamptz is right", "timestamp with time zone", ""},
		{"a bare timestamp is not", "timestamp without time zone", "must be `timestamptz`"},
		{"nor is a date", "date", "not a timestamp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			doc, p, set := docWithColumn(t, ir.Column{
				Name: "starts_at", SQLType: tc.sqlType, Nullable: true, Ordinal: 2,
			})
			diags := compile.Validate(doc, set, p)
			got := diags.String()

			switch {
			case tc.want == "" && strings.Contains(got, "starts_at"):
				t.Errorf("timestamptz should be accepted:\n%s", got)
			case tc.want != "" && !strings.Contains(got, tc.want):
				t.Errorf("want a diagnostic containing %q:\n%s", tc.want, got)
			}
		})
	}
}

// docWithColumn builds the smallest document a convention rule can run against:
// one table with an id and the column under test.
func docWithColumn(t *testing.T, c ir.Column) (*ir.Document, *project.Project, *tableconf.Set) {
	t.Helper()

	schema := ir.Schema{
		Name: "public",
		Tables: []ir.Table{{
			Name: "lesson", Kind: ir.TableKindBase, Comment: "A lesson.",
			Columns: []ir.Column{
				{Name: "id", SQLType: "uuid", Ordinal: 1, IsPrimaryKey: true, Comment: "The identifier."},
				c,
			},
			PrimaryKey: []string{"id"},
		}},
	}

	p := &project.Project{Config: &project.Config{
		Validate: project.Validate{
			// Only the rule under test, so an unrelated convention cannot make a
			// passing case look like a failing one.
			TimestampSuffix:      "error",
			UnmentionedColumn:    "off",
			MissingComment:       "off",
			ForeignKeyNotIndexed: "off",
			TenantIndexLeading:   "off",
			BooleanPrefix:        "off",
			DateSuffix:           "off",
			ForeignKeyNaming:     "off",
			CascadeDelete:        "off",
			MigrationFilename:    "off",
		},
	}}

	doc, diags := compile.Freeze(ir.API{Name: "Demo", BasePath: "/api/v1"}, schema,
		compile.Meta{Tool: "test"})
	if diags.HasErrors() {
		t.Fatalf("the fixture itself should compile:\n%s", diags.String())
	}
	return doc, p, &tableconf.Set{}
}

// ownerScopedAPI is the simple API with the table asking to be read narrowly.
func ownerScopedAPI() ir.API {
	in := simpleAPI()
	in.Resources[0].Storage.Owner = &ir.ColumnRef{
		Table: "lesson", Name: "created_by_account_id", SQLType: "uuid", Nullable: true,
	}
	return in
}

// TestAnOwnerScopedReadOffersTheScopeParameter checks the shape a client sees:
// one parameter, on the reads, defaulting to the narrow answer.
func TestAnOwnerScopedReadOffersTheScopeParameter(t *testing.T) {
	t.Parallel()

	api, diags := compile.Expand(ownerScopedAPI(), compile.ExpandOptions{})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}
	res := api.Resources[0]

	for _, name := range []string{ir.OpGet, ir.OpList, ir.OpSearch} {
		ep := res.Endpoint(name)
		if ep == nil {
			t.Fatalf("%s is missing", name)
		}
		var found *ir.Field
		for i := range ep.Request.QueryParams {
			if ep.Request.QueryParams[i].Wire == ir.ScopeParam {
				found = &ep.Request.QueryParams[i]
			}
		}
		if found == nil {
			t.Fatalf("%s has no %s parameter", name, ir.ScopeParam)
		}
		// Narrow unless asked otherwise: the failure mode of forgetting the
		// parameter has to be too few rows, never too many.
		if found.Default != "own" {
			t.Errorf("%s.%s defaults to %q, want own", name, ir.ScopeParam, found.Default)
		}
		if ep.WidePermission != "lesson.read.all" {
			t.Errorf("%s widens on %q, want lesson.read.all", name, ep.WidePermission)
		}
	}
}

// TestAWriteHasNoScopeParameter is the other half of the design. An owner-scoped
// table refuses to change somebody else's row outright, and offering a parameter
// that cannot widen a write would describe behaviour that does not exist.
func TestAWriteHasNoScopeParameter(t *testing.T) {
	t.Parallel()

	api, _ := compile.Expand(ownerScopedAPI(), compile.ExpandOptions{})
	res := api.Resources[0]

	for _, name := range []string{ir.OpCreate, ir.OpUpdate, ir.OpDelete} {
		ep := res.Endpoint(name)
		if ep == nil {
			continue
		}
		if ep.WidePermission != "" {
			t.Errorf("%s carries a widening permission %q", name, ep.WidePermission)
		}
		for _, p := range ep.Request.QueryParams {
			if p.Wire == ir.ScopeParam {
				t.Errorf("%s offers the %s parameter", name, ir.ScopeParam)
			}
		}
	}
}

// TestATableThatIsNotOwnerScopedHasNoScopeParameter guards against the parameter
// leaking onto every read in every project.
func TestATableThatIsNotOwnerScopedHasNoScopeParameter(t *testing.T) {
	t.Parallel()

	api, _ := compile.Expand(simpleAPI(), compile.ExpandOptions{})
	for _, ep := range api.Resources[0].Endpoints {
		for _, p := range ep.Request.QueryParams {
			if p.Wire == ir.ScopeParam {
				t.Errorf("%s offers the %s parameter", ep.Name, ir.ScopeParam)
			}
		}
	}
}

// TestAPublicReadGetsNoScopeParameter covers the combination that has no meaning:
// with no caller there is nothing to own, and a parameter nothing can authorize
// would only ever produce a refusal.
func TestAPublicReadGetsNoScopeParameter(t *testing.T) {
	t.Parallel()

	in := ownerScopedAPI()
	in.Resources[0].Public = []string{ir.OpList}

	api, diags := compile.Expand(in, compile.ExpandOptions{})
	if diags.HasErrors() {
		t.Fatalf("unexpected errors:\n%s", diags.String())
	}

	ep := api.Resources[0].Endpoint(ir.OpList)
	if ep.WidePermission != "" {
		t.Errorf("a public read widens on %q", ep.WidePermission)
	}
	for _, p := range ep.Request.QueryParams {
		if p.Wire == ir.ScopeParam {
			t.Error("a public read offers the scope parameter")
		}
	}
}

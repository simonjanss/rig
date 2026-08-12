package ir_test

import (
	"testing"

	"github.com/simonjanss/rig/pkg/ir"
)

// The accessors are what every generator reaches through, so what they answer
// is what gets emitted. Most of them return nil for a name that is not there,
// and a lookup that quietly returns the wrong thing would be worse than one
// that fails.

func TestLookupsMissTheirMisses(t *testing.T) {
	t.Parallel()

	doc := &ir.Document{
		Schema: ir.Schema{
			Tables: []ir.Table{{Name: "lesson", Columns: []ir.Column{{Name: "id"}}}},
			Enums:  []ir.PgEnum{{Name: "lesson_status"}},
		},
		API: ir.API{
			Enums:     []ir.Enum{{Name: "LessonStatus"}},
			Objects:   []ir.Object{{Name: "Pagination"}},
			Resources: []ir.Resource{{Name: "Lesson", Storage: &ir.ResourceStorage{Table: "lesson"}}},
		},
	}

	if doc.Table("lesson") == nil || doc.Table("nothing") != nil {
		t.Error("Table")
	}
	if doc.Column("lesson", "id") == nil {
		t.Error("Column should find a column that is there")
	}
	if doc.Column("lesson", "nothing") != nil || doc.Column("nothing", "id") != nil {
		t.Error("Column should miss both a bad column and a bad table")
	}
	if doc.PgEnum("lesson_status") == nil || doc.PgEnum("nothing") != nil {
		t.Error("PgEnum")
	}
	if doc.Enum("LessonStatus") == nil || doc.Enum("Nothing") != nil {
		t.Error("Enum")
	}
	if doc.Object("Pagination") == nil || doc.Object("Nothing") != nil {
		t.Error("Object")
	}
	if doc.Resource("Lesson") == nil || doc.Resource("Nothing") != nil {
		t.Error("Resource")
	}
	if doc.ResourceForTable("lesson") == nil || doc.ResourceForTable("nothing") != nil {
		t.Error("ResourceForTable")
	}
}

// Resolve is the hop from the reference carried on a field to the full column,
// and a field with no column at all is the ordinary case for a computed one.
func TestResolveFollowsAReferenceAndToleratesNone(t *testing.T) {
	t.Parallel()

	doc := &ir.Document{Schema: ir.Schema{Tables: []ir.Table{{
		Name:    "lesson",
		Columns: []ir.Column{{Name: "title", SQLType: "text"}},
	}}}}

	got := doc.Resolve(&ir.ColumnRef{Table: "lesson", Name: "title"})
	if got == nil || got.SQLType != "text" {
		t.Errorf("Resolve = %+v, want the full column", got)
	}
	if doc.Resolve(nil) != nil {
		t.Error("a field with no column resolves to nothing, not a panic")
	}
	if doc.Resolve(&ir.ColumnRef{Table: "lesson", Name: "gone"}) != nil {
		t.Error("a reference to a column that is not there resolves to nothing")
	}
}

// LIKE against a uuid is a type error, not a filter, and an array of text is
// not a pattern either. Both layers have to answer this the same way — the last
// time they each decided, one generated a filter field the other could not
// place.
func TestIsTextualIsAboutWhatLikeCanMatch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		field ir.Field
		want  bool
	}{
		{ir.Field{Type: ir.TypeString, GoType: "string"}, true},
		{ir.Field{Type: ir.TypeString, GoType: "*string"}, true},
		{ir.Field{Type: ir.TypeString, GoType: "[]string",
			Modifiers: []string{ir.ModifierArray}}, false},
		{ir.Field{Type: ir.TypeUUID, GoType: "uuid.UUID"}, false},
		{ir.Field{Type: ir.TypeString, GoType: "uuid.UUID"}, false},
		{ir.Field{Type: ir.TypeInt, GoType: "int"}, false},
	} {
		if got := tc.field.IsTextual(); got != tc.want {
			t.Errorf("%s/%s: IsTextual = %v, want %v",
				tc.field.Type, tc.field.GoType, got, tc.want)
		}
	}
}

func TestAFieldKnowsWhichOperationsItIsIn(t *testing.T) {
	t.Parallel()

	f := ir.ResourceField{Operations: []string{ir.FieldOpCreate, ir.FieldOpRead}}

	if !f.In(ir.FieldOpCreate) || !f.In(ir.FieldOpRead) {
		t.Error("the operations it was given")
	}
	if f.In(ir.FieldOpUpdate) {
		t.Error("and not the one it was not")
	}
}

func TestResourceFieldLookup(t *testing.T) {
	t.Parallel()

	res := &ir.Resource{Fields: []ir.ResourceField{
		{Field: ir.Field{Name: "Title"}},
		{Field: ir.Field{Name: "Notes"}},
	}}

	if got := res.Field("Notes"); got == nil || got.Name != "Notes" {
		t.Errorf("Field = %+v", got)
	}
	if res.Field("Nothing") != nil {
		t.Error("a field that is not there is nil, not the zero value")
	}
}

// The two lifecycle questions every generator branches on. They are methods on
// a pointer that is nil for a resource with no storage at all, so answering
// without panicking is the requirement.
func TestStorageLifecycleOnANilStorage(t *testing.T) {
	t.Parallel()

	var none *ir.ResourceStorage
	if none.IsSoftDeletable() || none.IsSnapshotable() {
		t.Error("a resource with no storage has neither")
	}

	soft := &ir.ResourceStorage{SoftDelete: &ir.SoftDelete{}}
	if !soft.IsSoftDeletable() || soft.IsSnapshotable() {
		t.Error("soft delete is not snapshotting")
	}

	snap := &ir.ResourceStorage{Snapshot: &ir.Snapshot{}}
	if snap.IsSoftDeletable() || !snap.IsSnapshotable() {
		t.Error("snapshotting is not soft delete")
	}
}

// missing_comment distinguishes a comment somebody wrote from one rig or
// Postgres supplied, which is the whole reason the source is recorded.
func TestOnlyAConfiguredCommentIsAuthored(t *testing.T) {
	t.Parallel()

	if !ir.CommentSourceConfig.Authored() {
		t.Error("a comment in the table configuration was written by a person")
	}
	for _, source := range []ir.CommentSource{
		ir.CommentSourceNone, ir.CommentSourceAuto, ir.CommentSourceDatabase,
	} {
		if source.Authored() {
			t.Errorf("%s is not somebody's writing", source)
		}
	}
}

// A column the database fills in is not one a client can supply, and offering
// it in a create input would be offering to be ignored.
func TestWritableExcludesWhatTheDatabaseOwns(t *testing.T) {
	t.Parallel()

	if !(ir.Column{Name: "title"}).Writable() {
		t.Error("an ordinary column is writable")
	}
	if (ir.Column{Name: "search", Generated: true}).Writable() {
		t.Error("a generated column is not")
	}
	if (ir.Column{Name: "id", Identity: true}).Writable() {
		t.Error("an identity column is not")
	}
}

func TestTableAccessors(t *testing.T) {
	t.Parallel()

	table := &ir.Table{
		Name:       "lesson",
		Columns:    []ir.Column{{Name: "id"}, {Name: "title"}},
		PrimaryKey: []string{"id"},
	}

	if got := table.Column("title"); got == nil || got.Name != "title" {
		t.Errorf("Column = %+v", got)
	}
	if table.Column("nothing") != nil {
		t.Error("a column that is not there is nil")
	}
	if !table.HasColumn("id") || table.HasColumn("nothing") {
		t.Error("HasColumn")
	}

	if pk, ok := table.SinglePrimaryKey(); !ok || pk != "id" {
		t.Errorf("SinglePrimaryKey = %q, %v", pk, ok)
	}

	// A composite key has no single column, and the caller has to handle that
	// rather than be handed the first one.
	composite := &ir.Table{PrimaryKey: []string{"lesson_id", "player_id"}}
	if _, ok := composite.SinglePrimaryKey(); ok {
		t.Error("a two-column key is not a single one")
	}
	if _, ok := (&ir.Table{}).SinglePrimaryKey(); ok {
		t.Error("no key at all is not a single one")
	}

	if table.IsLinkTable() {
		t.Error("an ordinary table is not a join")
	}
	if !(&ir.Table{LinkTable: &ir.LinkTable{}}).IsLinkTable() {
		t.Error("a table with a link description is one")
	}
}

// The enum's own membership check, which is what validation of an incoming
// value comes down to.
func TestPgEnumMembership(t *testing.T) {
	t.Parallel()

	e := ir.PgEnum{Values: []ir.PgEnumValue{{Value: "planned"}, {Value: "completed"}}}

	if !e.HasValue("planned") || !e.HasValue("completed") {
		t.Error("the labels it has")
	}
	if e.HasValue("Planned") {
		t.Error("Postgres enum labels are case-sensitive")
	}
	if e.HasValue("nothing") {
		t.Error("and not one it does not")
	}
}

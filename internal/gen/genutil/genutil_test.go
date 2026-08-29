package genutil_test

import (
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/internal/gen/gobuf"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// The type text carries its own qualifier, so the import has to be recognised
// from it. Three generators emit the same field, and this is the only reason
// they agree about what it is.
func TestGoTypeImportsWhatItNames(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		goType  string
		want    string
		import_ string
	}{
		{"string", "string", ""},
		{"", "any", ""}, // a field the compiler could not type
		{"time.Time", "time.Time", "time"},
		{"*time.Time", "*time.Time", "time"},
		{"uuid.UUID", "uuid.UUID", "github.com/google/uuid"},
		{"*uuid.UUID", "*uuid.UUID", "github.com/google/uuid"},
		{"[]string", "[]string", ""},
		{"[]uuid.UUID", "[]uuid.UUID", "github.com/google/uuid"},
		{"*[]byte", "*[]byte", ""},
		{"pgtype.Numeric", "pgtype.Numeric", "github.com/jackc/pgx/v5/pgtype"},
		{"json.RawMessage", "json.RawMessage", "encoding/json"},
		{"netip.Prefix", "netip.Prefix", "net/netip"},
	} {
		b := gobuf.New("x")
		got := genutil.GoType(b, ir.Field{GoType: tc.goType}, nil)

		if got != tc.want {
			t.Errorf("GoType(%q) = %q, want %q", tc.goType, got, tc.want)
		}

		rendered := render(t, b)
		if tc.import_ != "" && !strings.Contains(rendered, `"`+tc.import_+`"`) {
			t.Errorf("GoType(%q) should have imported %s:\n%s", tc.goType, tc.import_, rendered)
		}
		if tc.import_ == "" && strings.Contains(rendered, "import") {
			t.Errorf("GoType(%q) imported something it did not need:\n%s", tc.goType, rendered)
		}
	}
}

// A qualifier nothing recognises is a type the application declared in the
// package being written, so it needs no import and must not get one.
func TestAnUnknownQualifierIsLocal(t *testing.T) {
	t.Parallel()

	b := gobuf.New("store")
	got := genutil.GoType(b, ir.Field{GoType: "ScoringPayload"}, nil)

	if got != "ScoringPayload" {
		t.Errorf("got %q", got)
	}
	if strings.Contains(render(t, b), "import") {
		t.Error("a local type needs no import")
	}
}

// An enum is declared by the model, so everywhere else has to qualify it — and
// only there, because a file whose types are all builtin would otherwise end up
// with an import it never uses, which does not compile.
func TestTheModelIsImportedOnlyWhenSomethingNeedsIt(t *testing.T) {
	t.Parallel()

	asked := 0
	model := func() string { asked++; return "model" }

	b := gobuf.New("store")
	enum := genutil.GoType(b, ir.Field{GoType: "LessonStatus", TypeKind: ir.TypeKindEnum}, model)

	if enum != "model.LessonStatus" {
		t.Errorf("enum = %q, want it qualified", enum)
	}
	if asked != 1 {
		t.Errorf("the model was asked for %d times, want once", asked)
	}

	// A plain field asks for nothing.
	before := asked
	if got := genutil.GoType(b, ir.Field{GoType: "string"}, model); got != "string" {
		t.Errorf("got %q", got)
	}
	if asked != before {
		t.Error("a builtin type should not have reached for the model package")
	}

	// Inside the model itself the names are local, and nil says so.
	local := genutil.GoType(gobuf.New("model"),
		ir.Field{GoType: "LessonStatus", TypeKind: ir.TypeKindEnum}, nil)
	if local != "LessonStatus" {
		t.Errorf("inside the model the enum is %q, want it unqualified", local)
	}
}

// A nullable enum is a pointer to a qualified name, which is the case that gets
// the prefix and the qualifier in the wrong order if it is done by hand.
func TestAPointerToAnEnumKeepsBoth(t *testing.T) {
	t.Parallel()

	got := genutil.GoType(gobuf.New("api"),
		ir.Field{GoType: "*LessonStatus", TypeKind: ir.TypeKindEnum},
		func() string { return "model" })

	if got != "*model.LessonStatus" {
		t.Errorf("got %q, want *model.LessonStatus", got)
	}
}

func TestElemTypeStripsOnePointer(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{
		"*string":   "string",
		"string":    "string",
		"*[]string": "[]string",
		"**string":  "*string", // one, not all: the wrapper holds what is left
	} {
		if got := genutil.ElemType(in); got != want {
			t.Errorf("ElemType(%q) = %q, want %q", in, got, want)
		}
	}
}

// Every exported type carries documentation even before anybody writes any,
// which is what keeps a generated package readable on the first day.
func TestDescribeFallsBackWithoutOverriding(t *testing.T) {
	t.Parallel()

	if got := genutil.Describe("What needs doing.", "fallback"); got != "What needs doing." {
		t.Errorf("a written comment should win: %q", got)
	}
	if got := genutil.Describe("   ", "fallback"); got != "fallback" {
		t.Errorf("whitespace is not a comment: %q", got)
	}
	if got := genutil.Describe("", "fallback"); got != "fallback" {
		t.Errorf("got %q", got)
	}
}

// A computed field has no column, and the persistence layer has nothing to do
// with it.
func TestStoredFieldsAreTheOnesWithColumns(t *testing.T) {
	t.Parallel()

	res := &ir.Resource{Fields: []ir.ResourceField{
		{Field: ir.Field{Name: "ID", Column: &ir.ColumnRef{Name: "id"}}},
		{Field: ir.Field{Name: "Computed"}},
		{Field: ir.Field{Name: "Title", Column: &ir.ColumnRef{Name: "title"}}},
	}}

	got := names(genutil.StoredFields(res))
	if len(got) != 2 || got[0] != "ID" || got[1] != "Title" {
		t.Errorf("stored = %v, want [ID Title] in column order", got)
	}
}

// What a caller may write differs per operation, and the three exclusions are
// three different reasons: it is not a column, it is the database's to set, or
// it may be set once and never changed.
func TestWritableFieldsExcludeForThreeReasons(t *testing.T) {
	t.Parallel()

	column := func(name string) *ir.ColumnRef { return &ir.ColumnRef{Name: name} }
	res := &ir.Resource{Fields: []ir.ResourceField{
		{Field: ir.Field{Name: "Title", Column: column("title")},
			Operations: []string{ir.FieldOpCreate, ir.FieldOpUpdate}},
		{Field: ir.Field{Name: "StartsAt", Column: column("starts_at"), Immutable: true},
			Operations: []string{ir.FieldOpCreate, ir.FieldOpUpdate}},
		{Field: ir.Field{Name: "CreatedAt", Column: column("created_at"), ReadOnly: true},
			Operations: []string{ir.FieldOpCreate, ir.FieldOpUpdate}},
		{Field: ir.Field{Name: "Status", Column: column("status")},
			Operations: []string{ir.FieldOpUpdate}},
		{Field: ir.Field{Name: "Computed"},
			Operations: []string{ir.FieldOpCreate, ir.FieldOpUpdate}},
	}}

	create := names(genutil.WritableFields(res, ir.FieldOpCreate))
	if len(create) != 2 || create[0] != "Title" || create[1] != "StartsAt" {
		t.Errorf("create = %v, want [Title StartsAt]", create)
	}

	// Immutable is absent from update by not being there, not by being
	// rejected: there is no way to ask for it.
	update := names(genutil.WritableFields(res, ir.FieldOpUpdate))
	if len(update) != 2 || update[0] != "Title" || update[1] != "Status" {
		t.Errorf("update = %v, want [Title Status]", update)
	}
}

// A buffer that cannot be formatted is a generator bug, and the error has to
// name the file or it is a needle in a haystack of forty.
func TestArtifactNamesTheFileItCouldNotWrite(t *testing.T) {
	t.Parallel()

	good := gobuf.New("x")
	art, err := genutil.Artifact("lesson.gen.go", good, gen.Overwrite)
	if err != nil {
		t.Fatal(err)
	}
	if art.Path != "lesson.gen.go" || art.Mode != gen.Overwrite {
		t.Errorf("artifact = %+v", art)
	}
	if !strings.Contains(string(art.Content), "package x") {
		t.Errorf("content = %q", art.Content)
	}

	broken := gobuf.New("x")
	broken.L("func (")

	if _, err := genutil.Artifact("broken.gen.go", broken, gen.Overwrite); err == nil {
		t.Fatal("unformattable output should be an error")
	} else if !strings.Contains(err.Error(), "broken.gen.go") {
		t.Errorf("the error should name the file: %v", err)
	}
}

// A create or an update is named from the operation and not from the field
// list. The model declares LessonCreateInput whether or not the table left any
// column for a caller to fill in, so answering "nothing to name" for the empty
// one would have the client emit a parameter with no type.
func TestAModelInputIsNamedEvenWithNoFields(t *testing.T) {
	t.Parallel()

	res := &ir.Resource{Name: "Lesson"}
	generated := ir.EndpointImpl{Kind: ir.EndpointGenerated}

	for _, tc := range []struct {
		name string
		ep   *ir.Endpoint
		want string
	}{
		{"empty create", &ir.Endpoint{Name: ir.OpCreate, Impl: generated}, "LessonCreateInput"},
		{"empty update", &ir.Endpoint{Name: ir.OpUpdate, Impl: generated}, "LessonUpdateInput"},
		{
			"create with fields",
			&ir.Endpoint{Name: ir.OpCreate, Impl: generated, Request: ir.EndpointRequest{
				BodyParams: []ir.Field{{Name: "Title"}},
			}},
			"LessonCreateInput",
		},
		{
			"a custom body is named from the endpoint",
			&ir.Endpoint{Name: "Publish", Impl: generated, Request: ir.EndpointRequest{
				BodyParams: []ir.Field{{Name: "NotifyGuardians"}},
			}},
			"LessonPublishBody",
		},
		// Nothing to name: no body at all, or one the document already named.
		{"no body", &ir.Endpoint{Name: "Publish", Impl: generated}, ""},
		{
			"an object the document named",
			&ir.Endpoint{Name: "Publish", Impl: generated, Request: ir.EndpointRequest{
				BodyObject: "PublishRequest",
				BodyParams: []ir.Field{{Name: "NotifyGuardians"}},
			}},
			"",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := genutil.BodyShapeName(res, tc.ep); got != tc.want {
				t.Errorf("BodyShapeName = %q, want %q", got, tc.want)
			}
		})
	}
}

func names(fields []ir.ResourceField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Name)
	}
	return out
}

func render(t *testing.T, b *gobuf.Buf) string {
	t.Helper()

	out, err := b.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

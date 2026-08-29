package modelgo_test

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/pkg/gen"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const fixture = "lifecycle.ir.json"

func opts() gen.Options {
	return gen.Options{OutDir: ".", Raw: map[string]any{"package": "model"}}
}

// The model is where a description first becomes something a developer reads,
// and the same sentence has to reach the OpenAPI document and the TypeScript
// client when those generators land. Guarding it here is what makes that a
// convention with a test behind it rather than a hope.
func TestDescriptionsReachTheOutput(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, modelgo.New(), doc, opts())

	gentest.DescriptionsSurvive(t, doc, artifacts, func(name string) string {
		return "type " + name + " struct"
	})
}

func TestGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, modelgo.New(), doc, opts())

	gentest.Golden(t, filepath.Join("testdata", "lifecycle"), artifacts, *update)
}

func TestDeterministic(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	gentest.Deterministic(t, modelgo.New(), doc, opts())
}

func TestGeneratedCodeCompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	gentest.MustCompile(t, gentest.Run(t, modelgo.New(), doc, opts()), "model")
}

// The two wrappers are the whole point of the update input: a column that
// cannot hold null must have no way to be given one.
func TestUpdateInputSeparatesNullableFromNotNull(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	body, ok := between(src, "type LessonUpdateInput struct {", "\n}")
	if !ok {
		t.Fatal("no LessonUpdateInput")
	}
	for _, want := range []string{
		"Title patch.Optional[string]", // title text NOT NULL
		"Notes patch.Nullable[string]", // notes text
		"Capacity patch.Nullable[int]", // capacity int
	} {
		if !strings.Contains(collapse(body), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	// An immutable column may be set once. It is not rejected by the update
	// input; it is simply not in it.
	if strings.Contains(body, "StartsAt") {
		t.Errorf("an immutable field has no place in an update input:\n%s", body)
	}

	// A create has nothing to leave alone, so it takes plain values.
	create, _ := between(src, "type LessonCreateInput struct {", "\n}")
	if strings.Contains(create, "patch.") {
		t.Errorf("a create input should be plain:\n%s", create)
	}
}

// Validation runs against the row the update would produce, which is the only
// state a rule about two fields can be checked against.
func TestUpdateValidatesTheMergedRow(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	validate, ok := between(src, "func (i *LessonUpdateInput) Validate(prev *Lesson) error {", "\n}")
	if !ok {
		t.Fatal("no update Validate")
	}
	if !strings.Contains(validate, "merged := i.Merged(prev)") {
		t.Errorf("update validation should run against the merged row:\n%s", validate)
	}

	// Merged must not write back into the input: the repository builds its
	// UPDATE from the patches, and a filled-in input writes every column.
	merged, _ := between(src, "func (i LessonUpdateInput) Merged(prev *Lesson) Lesson {", "\n}")
	if strings.Contains(merged, "i.") && strings.Contains(merged, "= prev.") {
		t.Errorf("Merged should copy prev and apply the patches, not fill in the input:\n%s", merged)
	}
	if !strings.Contains(merged, "out := *prev") {
		t.Errorf("Merged should work on a copy:\n%s", merged)
	}
}

// The failure mirrors the input, which is what lets a client attach each
// message to the control it belongs to.
func TestTheInputErrorMirrorsTheInput(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	failure, ok := between(src, "type LessonCreateInputError struct {", "\n}")
	if !ok {
		t.Fatal("no LessonCreateInputError")
	}
	input, _ := between(src, "type LessonCreateInput struct {", "\n}")

	for _, field := range []string{"Title", "Notes", "ManagerEmailAddress"} {
		if !strings.Contains(input, field+" ") {
			t.Fatalf("the fixture no longer has a %s field", field)
		}
		if !strings.Contains(collapse(failure), field+" *rigerr.FieldError") {
			t.Errorf("%s is missing from the failure:\n%s", field, failure)
		}
	}

	// A field that was fine is absent from the body rather than present and
	// null, so a client can test for presence.
	if !strings.Contains(failure, `json:"title,omitempty"`) {
		t.Errorf("a field error should be omitted when there is none:\n%s", failure)
	}

	// A rule about the row rather than a field still has somewhere to land.
	if !strings.Contains(failure, "Entity *rigerr.FieldError") {
		t.Errorf("the failure should carry what belongs to no field:\n%s", failure)
	}

	// The field name is the member, so carrying it again inside would be a
	// second place for it to be wrong.
	fieldError, _ := between(src, "type FieldError struct {", "\n}")
	_ = fieldError

	// It has to reach the client as a 422 with its structure intact.
	for _, want := range []string{
		"func (e *LessonCreateInputError) ErrorCode() rigerr.Code { return rigerr.CodeUnprocessableEntity }",
		"func (e *LessonCreateInputError) ErrorFields() any { return e }",
	} {
		if !strings.Contains(collapse(src), collapse(want)) {
			t.Errorf("missing %s", want)
		}
	}

	// And validation must return it, not a sentence.
	if !strings.Contains(src, "return &failed") {
		t.Error("Validate should return the typed failure")
	}
}

// The field error is the same shape for every project, so it is rigerr's and
// not generated. What the model still owns is the struct that arranges them.
func TestTheFieldErrorIsNotGenerated(t *testing.T) {
	t.Parallel()

	base := find(t, "model.gen.go")

	for _, gone := range []string{"type FieldError struct", "type FieldCode string", "type FieldErrors"} {
		if strings.Contains(base, gone) {
			t.Errorf("%s should come from rigerr, not be generated:\n%s", gone, base)
		}
	}

	// And the generated checks say which kind of wrong they found, in rigerr's
	// vocabulary. The fixture has no length-limited column, so TooLong has no
	// example here; the code exists for the ones that do.
	input := find(t, "lesson_input.gen.go")
	for _, want := range []string{
		"rigerr.NewFieldError(rigerr.FieldCodeCannotBeEmpty,",
		"rigerr.NewFieldError(rigerr.FieldCodeInvalidValue,",
	} {
		if !strings.Contains(input, want) {
			t.Errorf("a generated check should carry its code: %s", want)
		}
	}
}

// A rule that could not be run is not a rule that failed: one is the caller's
// problem, the other is ours, and a 422 saying "title: connection refused"
// would be a lie about whose fault it is.
func TestARuleThatCannotRunIsNotAFieldError(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	run, ok := between(src, "func (v LessonCreateValidator) run(", "\n}")
	if !ok {
		t.Fatal("no create run method")
	}
	for _, want := range []string{
		"field, ok := rigerr.AsFieldError(err)",
		"if !ok { return nil, rigerr.Wrap(err, \"validate title\") }",
		"failed.Title = field",
	} {
		if !strings.Contains(collapse(run), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, run)
		}
	}

	// Nothing found means nothing returned, so a caller tests one thing.
	if !strings.Contains(collapse(run), "if failed.Empty() { return nil, nil }") {
		t.Errorf("an empty failure should come back as nil:\n%s", run)
	}
}

// Enums live in the model because both layers name them, and a client sends
// the label rather than the Go identifier.
func TestEnumsParseCaseInsensitively(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_status.gen.go")

	if !strings.Contains(src, "func ParseLessonStatus(") {
		t.Fatalf("no parser:\n%s", src)
	}
	if !strings.Contains(src, "strings.ToLower(strings.TrimSpace(s))") {
		t.Error("IN_PROGRESS and in_progress are the same value, and one of them must not be a 422")
	}
	if !strings.Contains(src, "func (v LessonStatus) Valid() bool") {
		t.Error("validation needs to ask whether a value is a member")
	}
}

func find(t *testing.T, name string) string {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for _, a := range gentest.Run(t, modelgo.New(), doc, opts()) {
		if filepath.Base(a.Path) == name {
			return string(a.Content)
		}
	}
	t.Fatalf("no artifact named %s", name)
	return ""
}

func collapse(s string) string { return strings.Join(strings.Fields(s), " ") }

func between(s, start, end string) (string, bool) {
	i := strings.Index(s, start)
	if i < 0 {
		return "", false
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		return "", false
	}
	return rest[:j], true
}

// Create and update are different questions asked about different fields, so
// they get different validators. The fixture has one of each asymmetry: a
// column that may be set once and never changed, and one that may be changed
// but not set at creation.
func TestTheTwoValidatorsCoverDifferentFields(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	create, ok := between(src, "type LessonCreateValidator struct {", "\n}")
	if !ok {
		t.Fatal("no LessonCreateValidator")
	}
	update, ok := between(src, "type LessonUpdateValidator struct {", "\n}")
	if !ok {
		t.Fatal("no LessonUpdateValidator")
	}

	// StartsAt is immutable: settable once, so a rule about setting it belongs
	// to create and there is nothing for an update rule to be about.
	if !strings.Contains(create, "StartsAt func(") {
		t.Errorf("create should be able to rule on an immutable field:\n%s", create)
	}
	if strings.Contains(update, "StartsAt func(") {
		t.Errorf("an update cannot touch StartsAt, so it should have no hook for it:\n%s", update)
	}

	// Status is the other way round: not writable at creation, changeable
	// afterwards.
	if strings.Contains(create, "Status func(") {
		t.Errorf("Status is not settable at creation:\n%s", create)
	}
	if !strings.Contains(update, "Status func(") {
		t.Errorf("Status should be rulable on update:\n%s", update)
	}

	// Each carries its own row-level hook and its own runner.
	for _, want := range []string{
		"func (v LessonCreateValidator) RunCreate(ctx context.Context, claims tenancy.Claims, i *LessonCreateInput) error",
		"func (v LessonUpdateValidator) RunUpdate(ctx context.Context, claims tenancy.Claims, i *LessonUpdateInput, prev *Lesson) error",
	} {
		if !strings.Contains(collapse(src), collapse(want)) {
			t.Errorf("missing %s", want)
		}
	}
	if !strings.Contains(create, "Entity func(") || !strings.Contains(update, "Entity func(") {
		t.Error("both should be able to rule on the row as a whole")
	}
}

// The filter is one type: what a client sends in a search body and what a
// repository takes are the same struct, so it carries wire tags and lives here
// rather than being mirrored in the api package.
func TestTheFilterIsTheWireShape(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_query.gen.go")

	filter, ok := between(src, "type LessonFilter struct {", "\n}")
	if !ok {
		t.Fatalf("no LessonFilter:\n%s", src)
	}
	for _, want := range []string{
		`Equals *LessonFilterEquals ` + "`" + `json:"equals,omitempty"` + "`",
		`NotEquals *LessonFilterEquals ` + "`" + `json:"notEquals,omitempty"` + "`",
		`Contains *LessonFilterContains ` + "`" + `json:"contains,omitempty"` + "`",
		`Like *LessonFilterLike ` + "`" + `json:"like,omitempty"` + "`",
		`Null *LessonFilterNull ` + "`" + `json:"null,omitempty"` + "`",
		`NestedFilters []LessonFilter ` + "`" + `json:"nestedFilters,omitempty"` + "`",
	} {
		if !strings.Contains(collapse(filter), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, filter)
		}
	}

	// The leaves are tagged too, or half the filter would arrive unnamed.
	equals, _ := between(src, "type LessonFilterEquals struct {", "\n}")
	if !strings.Contains(collapse(equals), `Title *string `+"`"+`json:"title,omitempty"`+"`") {
		t.Errorf("the operand fields should carry their wire names:\n%s", equals)
	}
}

// Splitting operators into separate structs is what keeps the filter typed: a
// range struct only carries orderable columns, so "createdAt contains 3" is not
// expressible rather than merely rejected.
func TestEachOperatorCarriesOnlyTheFieldsItCanCompare(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_query.gen.go")

	ranged, _ := between(src, "type LessonFilterRange struct {", "\n}")
	if strings.Contains(ranged, "Title ") {
		t.Errorf("text is not orderable:\n%s", ranged)
	}
	if !strings.Contains(ranged, "StartsAt ") {
		t.Errorf("a timestamp is:\n%s", ranged)
	}

	like, _ := between(src, "type LessonFilterLike struct {", "\n}")
	if strings.Contains(like, "Capacity ") {
		t.Errorf("a number has no pattern to match:\n%s", like)
	}
	if !strings.Contains(like, "Title ") {
		t.Errorf("text does:\n%s", like)
	}

	null, _ := between(src, "type LessonFilterNull struct {", "\n}")
	if strings.Contains(null, "Title ") {
		t.Errorf("a NOT NULL column is never null:\n%s", null)
	}
	if !strings.Contains(null, "Notes ") {
		t.Errorf("a nullable one can be:\n%s", null)
	}
}

// Pagination and ordering are not conditions, and the filter is a wire shape:
// leaving them in it let a client ask for a page inside a body and an ordering
// it was never offered.
func TestThePageIsSeparateFromTheFilter(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_query.gen.go")

	page, ok := between(src, "type LessonPage struct {", "\n}")
	if !ok {
		t.Fatalf("no LessonPage:\n%s", src)
	}
	for _, want := range []string{"Limit int", "Offset int", "OrderBy []LessonOrder"} {
		if !strings.Contains(collapse(page), want) {
			t.Errorf("missing %s:\n%s", want, page)
		}
	}

	filter, _ := between(src, "type LessonFilter struct {", "\n}")
	for _, gone := range []string{"Limit", "Offset", "OrderBy"} {
		if strings.Contains(filter, gone) {
			t.Errorf("%s is not a condition:\n%s", gone, filter)
		}
	}

	// And the old shape is gone rather than left beside the new one.
	if strings.Contains(src, "type LessonQuery struct") {
		t.Error("LessonQuery should have become LessonFilter, not gained a sibling")
	}
}

// A restore is a row re-entering the live set, so the same rules apply — and
// all of them do. An update may skip a rule for a field the request did not
// mention, because that value is already live and already passed; on a restore
// nothing was live, so nothing has been checked against the world the row is
// coming back to.
func TestRestoreValidationRunsEveryRule(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	restore, ok := between(src, ") RunRestore(", "\n}")
	if !ok {
		t.Fatalf("no RunRestore:\n%s", src)
	}
	// The same input an update takes, because a restore may have to change the
	// row to be allowed back at all.
	if !strings.Contains(collapse(restore), "claims tenancy.Claims, i *LessonUpdateInput, prev *Lesson") {
		t.Errorf("RunRestore should take the update input:\n%s", restore)
	}
	if strings.Contains(restore, ".IsSet()") || strings.Contains(restore, ".Touched()") {
		t.Errorf("a restore does not ask what the request mentioned:\n%s", restore)
	}
	if !strings.Contains(restore, "c.changed[ColumnLessonTitle] = true") {
		t.Errorf("every field should count as changed:\n%s", restore)
	}

	// And an update still asks, or every rule would run on every PATCH.
	update, _ := between(src, ") RunUpdate(", "\n}")
	if !strings.Contains(update, "c.changed[ColumnLessonTitle] = i.Title.IsSet()") {
		t.Errorf("an update should still skip what it did not touch:\n%s", update)
	}

	// One validator serves both, so a service wires the same value to each.
	if strings.Contains(src, "type LessonRestoreValidator") {
		t.Error("a restore is judged by the update validator, not a second one")
	}
	// And the input it used to take is gone rather than left beside the new one.
	if strings.Contains(src, "type LessonRestoreInput") {
		t.Error("LessonRestoreInput should be gone: the restore takes the update input")
	}
}

// A rule is handed who is asking, on the context struct it already takes.
//
// On the struct rather than as a parameter, because every field rule would
// otherwise grow one; and as a value rather than something to fetch, because a
// rule that has to look them up is a rule that can forget to — and there is no
// case where they are missing. A write without a caller is refused by the
// repository before any rule runs.
func TestARuleIsHandedWhoIsAsking(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	ctxStruct, ok := between(src, "type LessonValidatorContext struct {", "\n}")
	if !ok {
		t.Fatalf("no validator context:\n%s", src)
	}
	if !strings.Contains(collapse(ctxStruct), "Claims tenancy.Claims") {
		t.Errorf("the context should carry the caller:\n%s", ctxStruct)
	}

	// Every entry point takes them and puts them there, so a rule reads one
	// field whichever operation it was reached by.
	for _, name := range []string{"RunCreate", "RunUpdate", "RunRestore"} {
		body, found := between(src, ") "+name+"(", "\n}")
		if !found {
			t.Errorf("no %s", name)
			continue
		}
		if !strings.Contains(body, "claims tenancy.Claims") {
			t.Errorf("%s should take the caller:\n%s", name, body)
		}
		if !strings.Contains(collapse(body), "Claims:   claims,") &&
			!strings.Contains(collapse(body), "Claims: claims,") {
			t.Errorf("%s should pass them on:\n%s", name, body)
		}
	}

	// And nothing in the model reaches for them itself.
	if strings.Contains(src, "tenancy.FromContext") {
		t.Errorf("a rule is handed its caller, not left to find one:\n%s", src)
	}
}

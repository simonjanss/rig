package servicego_test

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/internal/gen/servicego"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const fixture = "lifecycle.ir.json"

func opts() gen.Options {
	return gen.Options{
		OutDir: ".",
		Raw: map[string]any{
			"package":      "api",
			"model_import": "rigtest/model",
			"store_import": "rigtest/store",
		},
	}
}

// The API layer emits the envelopes — the list response, the search body — and
// the same rule applies to them: whatever the document says about a field is
// what the output says about it.
func TestDescriptionsReachTheOutput(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "lifecycle.ir.json"))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	gentest.DescriptionsSurvive(t, doc, artifacts, func(name string) string {
		return "type " + name + " struct"
	})
}

func TestGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	gentest.Golden(t, filepath.Join("testdata", "lifecycle"), artifacts, *update)
}

func TestDeterministic(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	gentest.Deterministic(t, servicego.New(), doc, opts())
}

// The parent hooks are one field pair per foreign key, and the relations fixture
// is the one with any: fixture points at team twice, so its ParentHooks has to
// carry HomeTeam and AwayTeam rather than one Team that swallowed the other.
func TestCascadeGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "relations.ir.json"))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	gentest.Golden(t, filepath.Join("testdata", "relations"), artifacts, *update)
}

// TestGeneratedCodeCompiles builds the API layer against the persistence layer
// it calls. Compiling either alone would miss the mismatches worth catching.
func TestGeneratedCodeCompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))

	gentest.MustCompileAll(t, layers(t, doc, gentest.Package{
		Dir:       "api",
		Artifacts: gentest.Run(t, servicego.New(), doc, opts()),
	})...)
}

// TestStubCompilesAgainstTheAPI is the arrangement the whole design rests on: a
// resource with no business logic needs nothing but a constructor, because the
// embedded default already satisfies the interface.
func TestStubCompilesAgainstTheAPI(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))

	withStub := gen.Options{
		OutDir: ".",
		Raw: map[string]any{
			"package":      "api",
			"model_import": "rigtest/model",
			"store_import": "rigtest/store",
			"api_import":   "rigtest/api",
			"stub_dir":     "services/{table}",
		},
	}
	all := gentest.Run(t, servicego.New(), doc, withStub)

	var api, stubs []gen.Artifact
	for _, a := range all {
		if a.Mode == gen.CreateOnce {
			stubs = append(stubs, a)
			continue
		}
		api = append(api, a)
	}
	if len(stubs) == 0 {
		t.Fatal("no stub was emitted")
	}

	gentest.MustCompileAll(t, append(layers(t, doc, gentest.Package{Dir: "api", Artifacts: api}),
		gentest.Package{Dir: "services/lesson", Artifacts: stubs})...)
}

func TestStubIsWrittenOnce(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servicego.New(), doc, gen.Options{
		Root: "/project",
		Raw: map[string]any{
			"package":      "api",
			"model_import": "rigtest/model",
			"store_import": "rigtest/store",
			"api_import":   "rigtest/api",
			"stub_dir":     "services/{table}",
		},
	})

	var stub *gen.Artifact
	for i := range artifacts {
		if artifacts[i].Mode == gen.CreateOnce {
			stub = &artifacts[i]
		}
	}
	if stub == nil {
		t.Fatal("no hand-owned file was emitted")
	}

	// The stub belongs where the developer works, not beside the code it
	// implements, so its path is absolute rather than relative to OutDir.
	if want := filepath.Join("/project", "services", "lesson", "lesson.go"); stub.Path != want {
		t.Errorf("stub path = %q, want %q", stub.Path, want)
	}
	if strings.Contains(stub.Path, ".gen.go") {
		t.Error("a hand-owned file must not be named .gen.go: it would be gitignored and overwritten")
	}

	// Editing it is the entire point, and the banner would tell every tool that
	// reads it — and every developer — the opposite.
	if strings.Contains(string(stub.Content), "DO NOT EDIT") {
		t.Error("a hand-owned file must not claim to be generated")
	}
}

func TestServiceInterfaceCoversEveryEndpoint(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson_service.gen.go")

	iface, ok := between(src, "type LessonService interface {", "\n}")
	if !ok {
		t.Fatal("no LessonService interface")
	}
	for _, method := range []string{"Create", "Get", "List", "Search", "Update", "Delete", "Publish"} {
		if !strings.Contains(iface, method+"(ctx context.Context") {
			t.Errorf("%s is missing from the interface:\n%s", method, iface)
		}
	}

	// An empty slot is struct{} rather than a named empty type, so the
	// signature says what the operation takes and nothing more.
	if !strings.Contains(collapse(iface), "Get(ctx context.Context, r Request[LessonGetPath, struct{}, struct{}]) (*model.Lesson, error)") {
		t.Errorf("Get should take only a path:\n%s", iface)
	}
	if !strings.Contains(collapse(iface), "Delete(ctx context.Context, r Request[LessonDeletePath, struct{}, struct{}]) error") {
		t.Errorf("Delete returns no body, so it returns only an error:\n%s", iface)
	}
}

// A custom endpoint has no sensible default, so the default hands it to the
// contract rather than inventing one — and the contract names the whole set as
// an interface, so declaring an endpoint and not writing it fails the build.
func TestCustomEndpointsBelongToTheContract(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson_service.gen.go")

	for _, generated := range []string{"Create", "Get", "List", "Search", "Update", "Delete"} {
		if !strings.Contains(src, "func (s DefaultLessonService) "+generated+"(") {
			t.Errorf("the default service should implement %s", generated)
		}
	}

	endpoints, ok := between(src, "type LessonEndpoints interface {", "\n}")
	if !ok {
		t.Fatal("no LessonEndpoints interface")
	}
	if !strings.Contains(endpoints, "Publish(ctx context.Context") {
		t.Errorf("Publish should be part of the set:\n%s", endpoints)
	}
	for _, generated := range []string{"Create(", "List("} {
		if strings.Contains(endpoints, generated) {
			t.Errorf("%s has a default, so it is not the service layer's to write:\n%s", generated, endpoints)
		}
	}

	// The default answers the route by handing it over, so a resource whose
	// custom endpoints are all written needs no method of its own.
	publish, ok := between(src, "func (s DefaultLessonService) Publish(", "\n}")
	if !ok {
		t.Fatal("the default should answer the route")
	}
	if !strings.Contains(publish, "s.contract.Endpoints.Publish(ctx, r)") {
		t.Errorf("Publish should be handed to the contract:\n%s", publish)
	}

	// And a nil set is refused where it can still be fixed.
	ctor, _ := between(src, "func NewDefaultLessonService(", "\n}")
	if !strings.Contains(ctor, "Contract.Endpoints is required") {
		t.Errorf("a resource with custom endpoints should refuse a nil set:\n%s", ctor)
	}
}

// The create and update bodies are the model's input types, not copies of them.
// A wire struct beside them would be a third definition of the same fields.
func TestTheBodyIsTheModelsInput(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson_service.gen.go")

	iface, _ := between(src, "type LessonService interface {", "\n}")
	for _, want := range []string{
		"Request[struct{}, struct{}, model.LessonCreateInput]",
		"Request[LessonUpdatePath, struct{}, model.LessonUpdateInput]",
	} {
		if !strings.Contains(collapse(iface), want) {
			t.Errorf("missing %s:\n%s", want, iface)
		}
	}

	// And there is no wire type shadowing them.
	types := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson.gen.go")
	for _, gone := range []string{"type LessonCreateBody", "type LessonUpdateBody"} {
		if strings.Contains(types, gone) {
			t.Errorf("%s should not exist: the model's input is the body", gone)
		}
	}
}

// The entity is returned as it is stored. The copy that used to sit here was a
// field-by-field transcription whose only possible deviation was a missing one.
func TestThereIsNoConversion(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for _, a := range gentest.Run(t, servicego.New(), doc, opts()) {
		if strings.Contains(string(a.Content), "func toLesson(") {
			t.Errorf("%s still converts between two shapes of one entity", a.Path)
		}
	}
}

// A rule nobody attached is a rule that does not run, and a zero-valued field
// says nothing at the call site. The constructor takes the business logic, so
// "this resource has no rules" is something somebody wrote down.
func TestTheServiceIsHandedItsRules(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson_service.gen.go")

	want := "func NewDefaultLessonService(repo store.LessonRepository, " +
		"contract LessonContract) DefaultLessonService"
	if !strings.Contains(collapse(src), collapse(want)) {
		t.Errorf("the constructor should demand the contract:\n%s", want)
	}

	// And there must be no way to build one without it. An exported field is
	// exactly that way.
	def, ok := between(src, "type DefaultLessonService struct {", "\n}")
	if !ok {
		t.Fatal("no DefaultLessonService")
	}
	for _, exported := range []string{"Repo ", "Validator ", "Hooks ", "Endpoints "} {
		if strings.Contains(def, exported) {
			t.Errorf("%s is settable after construction, so it can be forgotten:\n%s", exported, def)
		}
	}
}

// The stub is where a developer decides what the rules are, so it lists every
// field the validator has — nil included. An empty literal would say nothing
// about which fields could have had one.
func TestTheStubSpellsOutEveryRule(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	all := gentest.Run(t, servicego.New(), doc, gen.Options{
		OutDir: ".",
		Raw: map[string]any{
			"package":      "api",
			"model_import": "rigtest/model",
			"store_import": "rigtest/store",
			"api_import":   "rigtest/api",
			"stub_dir":     "services/{table}",
		},
	})

	var stub string
	for _, a := range all {
		if a.Mode == gen.CreateOnce {
			stub = string(a.Content)
		}
	}
	if stub == "" {
		t.Fatal("no stub was emitted")
	}

	contract, ok := between(stub, "func (s *rules) Hooks() api.LessonHooks {", "\n}")
	if !ok {
		t.Fatalf("the stub should say what the rules are:\n%s", stub)
	}
	for _, field := range []string{"Notes", "ManagerEmailAddress", "Capacity", "Entity"} {
		if !strings.Contains(contract, field+":") {
			t.Errorf("%s should be listed, even as nil:\n%s", field, contract)
		}
	}
	// Both validators, each with its own fields: StartsAt can only be ruled on
	// at creation, Status only afterwards.
	for _, want := range []string{
		"Validator: model.LessonCreateValidator{",
		"Validator: model.LessonUpdateValidator{",
		"StartsAt:", "Status:",
	} {
		if !strings.Contains(contract, want) {
			t.Errorf("%s should be listed:\n%s", want, contract)
		}
	}
	for _, hook := range []string{"Create:", "Update:", "Delete:", "Restore:", "AfterCommit:"} {
		if !strings.Contains(contract, hook) {
			t.Errorf("%s should be listed:\n%s", hook, contract)
		}
	}
	// The custom endpoints are methods on the same value, and the constructor
	// asks for the whole set as an interface — so one that is declared and not
	// written does not compile.
	if !strings.Contains(stub, "func (s *rules) Publish(") {
		t.Errorf("a custom endpoint is implemented on the type carrying the rules:\n%s", stub)
	}
	if !strings.Contains(collapse(stub), "var _ api.LessonRules = (*rules)(nil)") {
		t.Errorf("the stub should assert it satisfies what the constructor wants:\n%s", stub)
	}

	// New is one line, and nothing in the rules mentions the service: that is
	// what removes the value which had to exist before it could describe itself.
	if !strings.Contains(collapse(stub), "return api.NewLessonService(repo, &rules{repo: repo})") {
		t.Errorf("the stub should build itself in one line:\n%s", stub)
	}
	// The only Service in the file is the one the doc comment shows you writing
	// if you ever override something — not a declaration.
	if strings.Contains(stub, "\ntype Service struct") {
		t.Errorf("there is no service type to write any more:\n%s", stub)
	}
	// The writer arrives rather than being assembled, so it cannot be built
	// from the wrong hooks.
	if !strings.Contains(collapse(stub), "func (s *rules) Bind(w api.LessonWriter) { s.write = w }") {
		t.Errorf("the writer should be handed over:\n%s", stub)
	}
	if strings.Contains(stub, "api.NewLessonWriter") {
		t.Errorf("the writer should not be assembled by hand:\n%s", stub)
	}
}

func TestTheImportsAreRequired(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for _, missing := range []string{"store_import", "model_import"} {
		raw := map[string]any{
			"package":      "api",
			"store_import": "rigtest/store",
			"model_import": "rigtest/model",
		}
		delete(raw, missing)

		_, err := servicego.New().Generate(t.Context(), doc, gen.Options{Raw: raw})
		if err == nil {
			t.Fatalf("%s is required", missing)
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("the error should name the missing option: %v", err)
		}
	}
}

// The revision is what a client says it was built against, so both the value
// and the header carrying it come from the document rather than from a literal
// in the generator.
func TestRevisionComesFromTheDocument(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.API.RevisionHeader = "X-Demo-Revision"
	doc.SetRevision("2026-08-01")

	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "api.gen.go")

	for _, want := range []string{
		`const Revision = "2026-08-01"`,
		`const RevisionHeader = "X-Demo-Revision"`,
		"ClientRevision string",
		"func (rc RequestContext) Client() apirev.Revision",
		"func (rc RequestContext) BuiltBefore(rev apirev.Revision) bool",
		"func (rc RequestContext) Stale() (time.Duration, bool)",
	} {
		if !strings.Contains(collapse(src), collapse(want)) {
			t.Errorf("api.gen.go is missing %s", want)
		}
	}
}

// A validator and a hook are handed a context and nothing else, so the request
// metadata has to travel on one or it stops at the service layer.
func TestTheRequestContextTravelsOnTheContext(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := collapse(find(t, gentest.Run(t, servicego.New(), doc, opts()), "api.gen.go"))

	for _, want := range []string{
		"type requestContextKey struct{}",
		"func NewContext(ctx context.Context, rc RequestContext) context.Context",
		"return context.WithValue(ctx, requestContextKey{}, rc)",
		"func RequestContextFrom(ctx context.Context) (RequestContext, bool)",
		"rc, ok := ctx.Value(requestContextKey{}).(RequestContext)",
	} {
		if !strings.Contains(src, collapse(want)) {
			t.Errorf("api.gen.go is missing %s", want)
		}
	}
}

// The shim in the example the method's own documentation gives has to be
// writable where the request is, which is the service method.
func TestARequestAnswersWhatItWasBuiltBefore(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := collapse(find(t, gentest.Run(t, servicego.New(), doc, opts()), "api.gen.go"))

	want := "func (r Request[Path, Query, Body]) BuiltBefore(rev apirev.Revision) bool " +
		"{ return r.ctx.BuiltBefore(rev) }"
	if !strings.Contains(src, collapse(want)) {
		t.Errorf("api.gen.go is missing %s", want)
	}
}

// A project that has never generated a revision still compiles, and still says
// so honestly rather than inventing a date.
func TestRevisionIsEmptyUntilOneIsRecorded(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "api.gen.go")

	if !strings.Contains(src, `const Revision = ""`) {
		t.Error("an unstamped document should generate an empty revision")
	}
	// The header still has a name: it is what the next client will send.
	if !strings.Contains(src, `const RevisionHeader = "API-Revision"`) {
		t.Error("the header should fall back to the default")
	}
}

// The set is closed and the compiler owns it. A second copy in the generator is
// a copy that is one commit away from being wrong.
func TestErrorCodesComeFromTheDocument(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "api.gen.go")

	codes := doc.Enum("ErrorCode")
	if codes == nil {
		t.Fatal("the fixture has no ErrorCode enumeration")
	}
	for _, v := range codes.Values {
		want := "ErrorCode" + v.Name + " ErrorCode = rigerr.Code" + v.Name
		if !strings.Contains(collapse(src), collapse(want)) {
			t.Errorf("api.gen.go is missing %s", want)
		}
	}
}

func find(t *testing.T, artifacts []gen.Artifact, name string) string {
	t.Helper()
	for _, a := range artifacts {
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

// layers builds the two packages the API layer sits on, plus whatever the
// caller wants compiled with them.
func layers(t *testing.T, doc *ir.Document, extra ...gentest.Package) []gentest.Package {
	t.Helper()

	out := []gentest.Package{
		{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		{
			Dir: "store",
			Artifacts: gentest.Run(t, persistgo.New(), doc, gen.Options{Raw: map[string]any{
				"package": "store", "model_import": "rigtest/model",
			}}),
		},
	}
	return append(out, extra...)
}

// The contract carries both, so a service can say different things about
// creating and changing.
func TestTheRulesTravelWithTheOperation(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson_service.gen.go")

	contract, ok := between(src, "type LessonContract struct {", "\n}")
	if !ok {
		t.Fatal("no LessonContract")
	}
	if !strings.Contains(collapse(contract), "Hooks LessonHooks") {
		t.Errorf("the contract should carry the hooks:\n%s", contract)
	}

	// The rules travel with the operation they belong to, so a write is handed
	// one value and cannot be given the other operation's validator.
	hooks, ok := between(src, "type LessonHooks struct {", "\n}")
	if !ok {
		t.Fatal("no LessonHooks")
	}
	for _, want := range []string{
		"Create dbhook.CreateHooks[model.LessonCreateInput, model.Lesson]",
		"Update dbhook.UpdateHooks[model.LessonUpdateInput, model.Lesson]",
	} {
		if !strings.Contains(collapse(hooks), want) {
			t.Errorf("missing %s:\n%s", want, hooks)
		}
	}
	// And the writer is what pairs them with the repository, once, so a
	// generated operation and a hand-written endpoint cannot be given different
	// ones.
	writer, ok := between(src, ") Create(ctx context.Context, in model.LessonCreateInput)", "\n}")
	if !ok {
		t.Fatal("no writer Create")
	}
	if !strings.Contains(collapse(writer), "Hooks: w.hooks.Create") {
		t.Errorf("a create should be handed the create side of the contract:\n%s", writer)
	}
	update, _ := between(src, ") Update(ctx context.Context, id uuid.UUID, in model.LessonUpdateInput)", "\n}")
	if !strings.Contains(collapse(update), "Hooks: w.hooks.Update") {
		t.Errorf("an update should be handed the update side:\n%s", update)
	}
}

// The filter is the model's, not a copy of it in the api package. The copy was
// the largest piece of boilerplate an API layer carried — twelve operators,
// each transcribing every applicable field, plus a recursive pass for nested
// filters — and the only thing it could ever do differently was miss one.
func TestTheSearchBodyIsTheModelsFilter(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	types := find(t, artifacts, "lesson.gen.go")
	body, _ := between(types, "type LessonSearchBody struct {", "\n}")
	if !strings.Contains(collapse(body), "Filter model.LessonFilter") {
		t.Errorf("the search body should carry the model's filter:\n%s", body)
	}

	// Neither the shapes nor the conversion between them exist any more.
	for _, gone := range []string{
		"type LessonFilter struct",
		"type LessonFilterEquals struct",
		"type LessonFilterRange struct",
		"func applyLessonFilter",
		"func LessonParams",
	} {
		for _, a := range artifacts {
			if strings.Contains(string(a.Content), gone) {
				t.Errorf("%s still has %s", a.Path, gone)
			}
		}
	}
}

// A custom endpoint refuses a field the same way a create does, or its client
// has to parse prose for the one endpoint the generator did not cover. Nothing
// generated returns one — only the service knows what its own body means — so
// what is emitted is the shape and the four methods that let it be returned.
func TestACustomBodySaysWhatWasWrongWithIt(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	types := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson.gen.go")

	fields, ok := between(types, "type LessonPublishBodyError struct {", "\n}")
	if !ok {
		t.Fatalf("no LessonPublishBodyError:\n%s", types)
	}
	for _, want := range []string{
		"NotifyGuardians *rigerr.FieldError `json:\"notifyGuardians,omitempty\"`",
		"Entity *rigerr.FieldError `json:\"entity,omitempty\"`",
	} {
		if !strings.Contains(collapse(fields), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, fields)
		}
	}

	// The four that make it an error the HTTP layer can answer with. Without
	// ErrorFields the 422 carries a sentence and nothing else, which is the
	// failure this whole shape exists to avoid.
	for _, want := range []string{
		"func (e *LessonPublishBodyError) Empty() bool",
		"func (e *LessonPublishBodyError) Error() string",
		"func (e *LessonPublishBodyError) ErrorCode() rigerr.Code",
		"func (e *LessonPublishBodyError) ErrorFields() any",
	} {
		if !strings.Contains(collapse(types), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, types)
		}
	}

	// The model's inputs have their own, in the model package. A second one here
	// would be two structs for one body.
	for _, gone := range []string{"type LessonCreateBodyError", "type LessonUpdateBodyError"} {
		if strings.Contains(types, gone) {
			t.Errorf("%s should not exist: the model's input error covers it", gone)
		}
	}
}

// Pagination arrives as query parameters and the filter as a body, so they are
// separate arguments to the repository. Reading them off the request rather
// than out of the filter is what lets a client page a search it did not write.
func TestTheDefaultReadsPassTheirPageSeparately(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson_service.gen.go")

	list, _ := between(src, ") List(ctx context.Context", "\n}")
	if !strings.Contains(collapse(list), "model.LessonPage{Limit: r.Query.Limit, Offset: r.Query.Offset}") {
		t.Errorf("List should build a page from the query parameters:\n%s", list)
	}
	// A list with no filter still passes one: an empty filter matches every row
	// the caller may see, which is what a list is.
	if !strings.Contains(collapse(list), "asked := model.NewLessonFilter()") {
		t.Errorf("List should pass an empty filter:\n%s", list)
	}

	search, _ := between(src, ") Search(ctx context.Context", "\n}")
	if !strings.Contains(collapse(search), "asked := r.Body.Filter") {
		t.Errorf("Search should hand the body's filter straight over:\n%s", search)
	}
	if !strings.Contains(collapse(search), "model.LessonPage{Limit: r.Query.Limit, Offset: r.Query.Offset}") {
		t.Errorf("Search should page from the query parameters too:\n%s", search)
	}
}

// Read hooks run in the service, not the repository, and every read goes
// through the same two helpers rather than repeating them.
func TestEveryReadGoesThroughTheReadHooks(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson_service.gen.go")

	// Narrowing is a nested AND rather than an edit to the caller's filter: a
	// search whose own filter is an OR must not be able to widen its way out.
	helper, ok := between(src, ") readFilter(", "\n}")
	if !ok {
		t.Fatalf("no readFilter helper:\n%s", src)
	}
	if !strings.Contains(collapse(helper), "model.LessonFilter{NestedFilters: []model.LessonFilter{*narrowed, asked}}") {
		t.Errorf("the two filters should be ANDed:\n%s", helper)
	}

	for name, want := range map[string]string{
		"List":        "readFilter",
		"Search":      "readFilter",
		"ListDeleted": "readFilter",
	} {
		body, _ := between(src, ") "+name+"(ctx context.Context", "\n}")
		if !strings.Contains(body, want) {
			t.Errorf("%s does not narrow:\n%s", name, body)
		}
	}

	// Every read reports what it found, Get included — it has no filter to
	// narrow, so the row hook is the only place a rule about it can go.
	for _, name := range []string{"Get", "List", "Search", "ListDeleted", "Versions"} {
		body, _ := between(src, ") "+name+"(ctx context.Context", "\n}")
		if !strings.Contains(body, "readRows") {
			t.Errorf("%s does not run the row hook:\n%s", name, body)
		}
	}
	get, _ := between(src, ") Get(ctx context.Context", "\n}")
	if strings.Contains(get, "readFilter") {
		t.Errorf("Get fetches by primary key; there is no filter to narrow:\n%s", get)
	}
}

// Claims arrive as an argument, and their type says whether there is a caller.
//
// A write cannot happen without one — the repository refuses before any hook
// runs — so those take a value. A read marked public is reached by somebody who
// presented nothing, so those take a pointer and nil is that somebody. Handing
// a zero-valued Claims to a read hook would be a tenant of all zeroes that
// reads like a real one.
func TestReadHooksTakeTheirCallerAsAPointer(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	hooks, _ := between(find(t, artifacts, "lesson_service.gen.go"), "type LessonHooks struct {", "\n}")
	if !strings.Contains(collapse(hooks), "Read dbhook.ReadHooks[model.LessonFilter, model.Lesson]") {
		t.Errorf("unexpected read hook type:\n%s", hooks)
	}

	// The claims come off the request rather than out of the context, and the
	// one place that decides "was there a caller" is shared.
	src := find(t, artifacts, "lesson_service.gen.go")
	for _, want := range []string{"readFilter(ctx, r.Claims,", "readRows(ctx, r.Claims,"} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q — claims should be passed, not looked up", want)
		}
	}
	if strings.Contains(src, "tenancy.FromContext") {
		t.Errorf("the generated service should not fish claims out of the context:\n%s", src)
	}

	base, ok := between(find(t, artifacts, "api.gen.go"), "func caller(", "\n}")
	if !ok {
		t.Fatal("no caller helper")
	}
	if !strings.Contains(collapse(base), "if !claims.Valid() { return nil }") {
		t.Errorf("nil is how a public read says there was nobody:\n%s", base)
	}
}

// One write path. A custom endpoint that reached for the repository would have
// to pass the hooks by hand, and one that forgot would be a second way into the
// table where the rules do not run — so the pairing is made once, in the
// writer, and everything goes through it.
func TestEveryWriteGoesThroughTheWriter(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())
	src := find(t, artifacts, "lesson_service.gen.go")

	// The generated operations delegate rather than assembling an envelope.
	for name, want := range map[string]string{
		"Create":  "s.write.Create(ctx, r.Body)",
		"Update":  "s.write.Update(ctx, r.Path.ID, r.Body)",
		"Delete":  "s.write.Delete(ctx, model.LessonDeleteInput{ID: r.Path.ID})",
		"Restore": "s.write.Restore(ctx, r.Path.ID)",
		"Revert":  "s.write.Revert(ctx, r.Path.ID, r.Body.VersionID)",
	} {
		body, ok := between(src, ") "+name+"(ctx context.Context, r Request", "\n}")
		if !ok {
			t.Errorf("no %s on the default service", name)
			continue
		}
		if !strings.Contains(collapse(body), want) {
			t.Errorf("%s should write through the writer:\n%s", name, body)
		}
	}

	// Nothing in the default builds one of the envelopes itself any more, so
	// there is nowhere left for the two paths to differ.
	for _, gone := range []string{"dbhook.Create[", "dbhook.Update[", "dbhook.Delete[", "dbhook.Restore["} {
		defaults, _ := between(src, "type DefaultLessonService struct {", "\n// LessonWriter")
		if strings.Contains(defaults, gone) {
			t.Errorf("the default still assembles %s itself", gone)
		}
	}

}

// The front door asks the service layer what it is, rather than being handed a
// half-built value that then has to be told what it is part of.
//
// That is the whole point of the shape: the rules never mention the service, so
// there is no cycle for the constructor to break in two statements.
func TestTheConstructorAsksTheRulesWhatTheyAre(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servicego.New(), doc, opts()), "lesson_service.gen.go")

	iface, ok := between(src, "type LessonRules interface {", "\n}")
	if !ok {
		t.Fatalf("no LessonRules:\n%s", src)
	}
	for _, want := range []string{
		// The endpoints are part of it, so one that is declared and not written
		// fails at the constructor rather than on the route.
		"LessonEndpoints",
		"Hooks() LessonHooks",
		"Bind(LessonWriter)",
	} {
		if !strings.Contains(collapse(iface), want) {
			t.Errorf("missing %s:\n%s", want, iface)
		}
	}

	ctor, ok := between(src, "func NewLessonService(repo store.LessonRepository, rules LessonRules)", "\n}")
	if !ok {
		t.Fatalf("no NewLessonService:\n%s", src)
	}
	// Asked once, then the writer built from the answer is handed back — before
	// anything can reach a hook, because the constructor has not returned.
	if !strings.Contains(collapse(ctor), "Hooks: rules.Hooks(), Endpoints: rules") {
		t.Errorf("the rules should supply both halves:\n%s", ctor)
	}
	if !strings.Contains(collapse(ctor), "rules.Bind(svc.Writer())") {
		t.Errorf("the writer should be handed back:\n%s", ctor)
	}
	// And it returns the concrete default, so overriding is embedding it.
	if !strings.Contains(collapse(src), "rules LessonRules) DefaultLessonService {") {
		t.Errorf("the constructor should return the concrete default:\n%s", ctor)
	}
}

// TestTheDefaultServicePassesTheScopeOn checks the link between what the handler
// validated and what the repository applies. The mapping is one function, so
// "all" means exactly one thing.
func TestTheDefaultServicePassesTheScopeOn(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	base := collapse(find(t, artifacts, "api.gen.go"))
	want := "func readScope(s tenancy.Scope) []readopt.Option { " +
		"if s == tenancy.ScopeAll { return []readopt.Option{readopt.WithoutOwnerScope()} } return nil }"
	if !strings.Contains(base, want) {
		t.Errorf("the scope helper is missing or changed shape:\n%s", want)
	}

	svc := collapse(find(t, artifacts, "memo_service.gen.go"))
	for _, want := range []string{
		"s.repo.Get(ctx, r.Path.ID, readScope(r.Query.Scope)...)",
		"s.repo.List(ctx, filter, page, readScope(r.Query.Scope)...)",
		"s.repo.ListDeleted(ctx, filter, page, readScope(r.Query.Scope)...)",
	} {
		if !strings.Contains(svc, want) {
			t.Errorf("missing:\n%s", want)
		}
	}
}

// TestATableThatIsNotOwnerScopedGetsNoScopeHelper keeps the generated package free
// of a function nothing calls.
func TestATableThatIsNotOwnerScopedGetsNoScopeHelper(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	if strings.Contains(find(t, artifacts, "api.gen.go"), "func readScope(") {
		t.Error("readScope was emitted for a project with no owner-scoped table")
	}
}

// TestFileEndpointsCompile is the check that matters most for the file half:
// three methods per column, a create that commits a row and its bytes together,
// and a conversion to the shape a client sees — none of which a golden would
// notice was referring to a method that does not exist.
func TestFileEndpointsCompile(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "files.ir.json"))

	gentest.MustCompileAll(t, layers(t, doc, gentest.Package{
		Dir:       "api",
		Artifacts: gentest.Run(t, servicego.New(), doc, opts()),
	})...)
}

// A notifiable table owes rig two answers, and an ordinary one owes it nothing.
//
// Both halves matter. The methods are required — that is the whole mechanism, a
// build failure rather than a 501 — and requiring them of a table that is not
// notifiable would make every project answer a question it was never asked.
func TestNotifiableTablesOweTwoMethodsAndOthersOweNone(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	post := find(t, artifacts, "blog_post_service.gen.go")
	for _, want := range []string{
		"type BlogPostNotify interface {",
		"NotifyAt(row *model.BlogPost, kind string) (time.Time, bool)",
		"NotifyWho(ctx context.Context, n *notify.Notification, row *model.BlogPost) ([]uuid.UUID, error)",
		// Required at the constructor, not discovered in a background job.
		"if contract.Notify == nil {",
		// And the adapter that carries the answers to the dispatcher.
		"func (s BlogPostSubject) Audience(",
		`func NotifyAboutBlogPost(id uuid.UUID) notify.Subject {`,
	} {
		if !strings.Contains(collapse(post), collapse(want)) {
			t.Errorf("blog_post should carry %s", want)
		}
	}

	// The read that answers the audience runs without a caller, so the owner
	// narrowing has to be turned off explicitly — the one trap in this design,
	// and the generator should not leave it to the application.
	if !strings.Contains(collapse(post), collapse("readopt.WithoutOwnerScope()")) {
		t.Error("the dispatcher's read should not be narrowed to a caller that does not exist")
	}

	account := find(t, artifacts, "rig_account_service.gen.go")
	for _, unwanted := range []string{"NotifyAt(", "NotifyWho(", "AccountNotify"} {
		if strings.Contains(account, unwanted) {
			t.Errorf("an ordinary table should not be asked %s", unwanted)
		}
	}
}

// The inbox's generated output, kept where it can be read. The wiring is small
// and every line of it is a decision the milestone argued for; a golden is how
// those stay reviewable rather than being re-derived from the emitter.
func TestNotifyGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	artifacts := gentest.Run(t, servicego.New(), doc, opts())

	gentest.Golden(t, filepath.Join("testdata", "notify"), artifacts, *update)
}

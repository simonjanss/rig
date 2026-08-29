package goclient_test

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/goclient"
	"github.com/simonjanss/rig/pkg/gen"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const fixture = "lifecycle.ir.json"

func opts() gen.Options {
	return gen.Options{OutDir: ".", Raw: map[string]any{"package": "client"}}
}

func TestGolden(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, goclient.New(), doc, opts())

	gentest.Golden(t, filepath.Join("testdata", "lifecycle"), artifacts, *update)
}

func TestDeterministic(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	gentest.Deterministic(t, goclient.New(), doc, opts())
}

func TestGeneratedCodeCompiles(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	gentest.MustCompile(t, gentest.Run(t, goclient.New(), doc, opts()), "client")
}

// The client is where a description reaches whoever is calling the API rather
// than whoever wrote it, which is the reader who has nothing else to go on.
func TestDescriptionsReachTheOutput(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, goclient.New(), doc, opts())

	gentest.DescriptionsSurvive(t, doc, artifacts, func(name string) string {
		return "type " + name + " struct"
	})
}

// The failure this guards is silent and expensive: patch.Optional encodes
// absence as null, because a marshaler cannot remove its own field. Without
// omitzero every untouched member of an update would arrive as an explicit
// null — refused on the columns that cannot hold one, and a silent clearing of
// the columns that can.
func TestAnUntouchedUpdateSendsNothing(t *testing.T) {
	t.Parallel()

	body, ok := between(find(t, "lesson_input.gen.go"), "type LessonUpdateInput struct {", "\n}")
	if !ok {
		t.Fatal("no LessonUpdateInput")
	}

	for _, line := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.Contains(line, "`json:") {
			continue
		}
		if !strings.Contains(line, ",omitzero") {
			t.Errorf("an update member without omitzero sends null when it is untouched:\n%s", line)
		}
	}

	// And the wrappers still say which columns can be cleared at all.
	for _, want := range []string{
		"Title patch.Optional[string]", // title text NOT NULL
		"Notes patch.Nullable[string]", // notes text
		"Capacity patch.Nullable[int]", // capacity int
		"Status patch.Optional[LessonStatus]",
	} {
		if !strings.Contains(collapse(body), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	// A create has nothing to leave alone, so it takes plain values.
	create, _ := between(find(t, "lesson_input.gen.go"), "type LessonCreateInput struct {", "\n}")
	if strings.Contains(create, "patch.") {
		t.Errorf("a create input should be plain:\n%s", create)
	}
}

// The server applies a parameter's default only when the parameter is absent, so
// a client that sent limit=0 would be asking for an empty page.
func TestQueryParametersAreOptional(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	query, ok := between(src, "type LessonListQuery struct {", "\n}")
	if !ok {
		t.Fatal("no LessonListQuery")
	}
	for _, want := range []string{"Limit *int", "Offset *int"} {
		if !strings.Contains(collapse(query), want) {
			t.Errorf("missing %s:\n%s", want, query)
		}
	}
	// And the default is named where a caller will read it.
	if !strings.Contains(collapse(query), "the server uses 50") {
		t.Errorf("the server's default should be in the comment:\n%s", query)
	}

	// Nothing is written for a nil member.
	list, _ := between(find(t, "lesson_client.gen.go"), "func (c *LessonClient) List(", "\n}")
	if !strings.Contains(collapse(list), `rigclient.SetInt(query, "limit", q.Limit)`) {
		t.Errorf("the query should be written through the pointer-taking setters:\n%s", list)
	}
}

// QUERY is the primary form and the alias is the accommodation, in that order:
// an intermediary that refuses the method is worked around once and remembered.
func TestSearchCarriesItsFallback(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_client.gen.go")

	search, ok := between(src, "func (c *LessonClient) Search(", "\n}")
	if !ok {
		t.Fatal("no Search")
	}
	if !strings.Contains(search, "rigclient.MethodQuery") {
		t.Errorf("a search should issue QUERY:\n%s", search)
	}
	if !strings.Contains(search, `Fallback: "/lessons/_search"`) {
		t.Errorf("a search should carry the alias to fall back to:\n%s", search)
	}
	// The filter is its own argument: a struct with one member called Filter is
	// a wrapper nobody wants to type.
	if !strings.Contains(collapse(search), "filter LessonFilter") {
		t.Errorf("Search should take the filter directly:\n%s", search)
	}
}

// The three shapes of method, which is the whole surface a caller sees.
func TestTheMethodSignatures(t *testing.T) {
	t.Parallel()

	src := collapse(find(t, "lesson_client.gen.go"))

	for _, want := range []string{
		"func (c *LessonClient) Create(ctx context.Context, in LessonCreateInput, " +
			"opts ...rigclient.CallOption) (*Lesson, error)",
		"func (c *LessonClient) Get(ctx context.Context, id uuid.UUID, " +
			"opts ...rigclient.CallOption) (*Lesson, error)",
		"func (c *LessonClient) Update(ctx context.Context, id uuid.UUID, in LessonUpdateInput, " +
			"opts ...rigclient.CallOption) (*Lesson, error)",
		// A delete answers with nothing, so there is nothing to give back but
		// the failure.
		"func (c *LessonClient) Delete(ctx context.Context, id uuid.UUID, " +
			"opts ...rigclient.CallOption) error",
		// A custom endpoint is a method like any other, named after the service
		// method the server generated.
		"func (c *LessonClient) Publish(ctx context.Context, id uuid.UUID, in LessonPublishBody, " +
			"opts ...rigclient.CallOption) (*Lesson, error)",
		// And the iterator over a list.
		"func (c *LessonClient) All(ctx context.Context, q LessonListQuery, " +
			"opts ...rigclient.CallOption) iter.Seq2[Lesson, error]",
	} {
		if !strings.Contains(src, collapse(want)) {
			t.Errorf("missing:\n%s", want)
		}
	}
}

// An identifier that arrived from somewhere else can be anything at all, and a
// slash in one would address a different route entirely.
func TestPathParametersAreEscaped(t *testing.T) {
	t.Parallel()

	get, _ := between(find(t, "lesson_client.gen.go"), "func (c *LessonClient) Get(", "\n}")
	if !strings.Contains(get, "rigclient.PathValue(id.String())") {
		t.Errorf("a path parameter should be escaped:\n%s", get)
	}
	if !strings.Contains(get, `"/lessons/{id}"`) {
		t.Errorf("the route should come from the document's pattern:\n%s", get)
	}
}

// A 422 is worth having a type for: it is the one failure whose body says
// something about each field the caller sent.
func TestAValidationFailureHasAShape(t *testing.T) {
	t.Parallel()

	src := find(t, "lesson_input.gen.go")

	fields, ok := between(src, "type LessonCreateFields struct {", "\n}")
	if !ok {
		t.Fatalf("no LessonCreateFields:\n%s", src)
	}
	for _, want := range []string{
		"Title *rigerr.FieldError `json:\"title,omitempty\"`",
		"Entity *rigerr.FieldError `json:\"entity,omitempty\"`",
	} {
		if !strings.Contains(collapse(fields), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, fields)
		}
	}

	// A custom endpoint's body can be refused the same way, and used not to be —
	// which left the one endpoint a project wrote itself as the one whose client
	// had to parse prose.
	body, ok := between(src, "type LessonPublishFields struct {", "\n}")
	if !ok {
		t.Fatalf("no LessonPublishFields:\n%s", src)
	}
	if !strings.Contains(collapse(body), collapse(
		"NotifyGuardians *rigerr.FieldError `json:\"notifyGuardians,omitempty\"`")) {
		t.Errorf("the custom body's shape is not mirrored:\n%s", body)
	}
}

// The shape alone still has to be named by hand, and naming the wrong one is not
// an error: every member is optional, so the wrong shape decodes into an empty
// one. A reader per call is what removes the choice, in one line at the call
// site.
func TestEveryCallWithABodySaysHowItFails(t *testing.T) {
	t.Parallel()

	input := find(t, "lesson_input.gen.go")
	for _, want := range []string{
		"func LessonCreateError(err error) (*rigclient.Failure[LessonCreateFields], bool) { " +
			"return rigclient.As[LessonCreateFields](err) }",
		"func LessonUpdateError(err error) (*rigclient.Failure[LessonUpdateFields], bool) { " +
			"return rigclient.As[LessonUpdateFields](err) }",
		"func LessonPublishError(err error) (*rigclient.Failure[LessonPublishFields], bool) { " +
			"return rigclient.As[LessonPublishFields](err) }",
	} {
		if !strings.Contains(collapse(input), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, input)
		}
	}

	// The methods are untouched: reading a refusal is a second way to look at the
	// error they already returned, not a different error.
	client := find(t, "lesson_client.gen.go")
	for _, want := range []string{
		"return rigclient.Do[Lesson](ctx, c.rt, op, opts...)",
		"return rigclient.Do[LessonListResponse](ctx, c.rt, op, opts...)",
	} {
		if !strings.Contains(collapse(client), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, client)
		}
	}

	// A read sends no body, so there is nothing for it to be wrong about, and a
	// search's body is a filter nothing validates — a reader for either would be
	// a function per resource that can only ever answer nil.
	//
	// A revert is the one that would have been worse than useless. It has a body
	// of its own and a 422 to go with it, but the refusal comes from the update
	// rules it replays and arrives in LessonUpdateFields — so a reader named after
	// the revert body would decode those into a shape with one member nobody
	// complained about and hand back a struct that is entirely nil, which is the
	// wrong-shape read this whole file exists to make unwritable.
	for _, gone := range []string{
		"LessonGetError", "LessonGetFields", "LessonListFields",
		"LessonSearchError", "LessonSearchFields",
		"LessonRevertError", "LessonRevertFields",
	} {
		if strings.Contains(input, gone) || strings.Contains(client, gone) {
			t.Errorf("%s exists, and nothing can produce what it reads", gone)
		}
	}
}

// The error vocabulary is rigerr's, so a program talking to two rig APIs
// switches on one set of constants.
func TestTheErrorVocabularyIsShared(t *testing.T) {
	t.Parallel()

	src := find(t, "client.gen.go")

	for _, want := range []string{
		"type Error = rigclient.Error",
		"type ErrorCode = rigerr.Code",
		"ErrorCodeNotFound ErrorCode = rigerr.CodeNotFound",
	} {
		if !strings.Contains(collapse(src), collapse(want)) {
			t.Errorf("missing %s:\n%s", want, src)
		}
	}
	if strings.Contains(src, "type Error struct") {
		t.Error("the client should not declare a second Error type")
	}
}

// The base path is the document's. A client that could be pointed at a different
// one would be a client for a different API.
func TestTheClientIsBuiltFromTheDocument(t *testing.T) {
	t.Parallel()

	src := collapse(find(t, "client.gen.go"))

	for _, want := range []string{
		`const BasePath = "/api/v1"`,
		"Lessons *LessonClient",
		"func New(cfg rigclient.Config) (*Client, error)",
		"rt, err := rigclient.New(cfg, rigclient.API{ BasePath: BasePath, " +
			"Revision: Revision, RevisionHeader: RevisionHeader, })",
	} {
		if !strings.Contains(src, collapse(want)) {
			t.Errorf("missing %s", want)
		}
	}
}

// A client that does not say what it was built against is a client the server's
// logs cannot age, which is the one thing this is for. It comes from the
// document, so nobody has to remember to pass it.
func TestTheClientCarriesTheRevision(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.API.RevisionHeader = "X-Demo-Revision"
	doc.SetRevision("2026-08-01")

	var src string
	for _, a := range gentest.Run(t, goclient.New(), doc, opts()) {
		if filepath.Base(a.Path) == "client.gen.go" {
			src = collapse(string(a.Content))
		}
	}
	if src == "" {
		t.Fatal("no client.gen.go")
	}

	for _, want := range []string{
		`const Revision = "2026-08-01"`,
		`const RevisionHeader = "X-Demo-Revision"`,
	} {
		if !strings.Contains(src, collapse(want)) {
			t.Errorf("missing %s", want)
		}
	}
}

// A project with no authentication has no profile, and nothing that reads one.
func TestNoAuthNoProfile(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for _, a := range gentest.Run(t, goclient.New(), doc, opts()) {
		if filepath.Base(a.Path) == "auth.gen.go" {
			t.Fatal("a project with no auth block should get no auth file")
		}
	}
}

func find(t *testing.T, name string) string {
	t.Helper()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for _, a := range gentest.Run(t, goclient.New(), doc, opts()) {
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

// TestFileMethodsCompile builds a client for a project with three file columns
// in three shapes: one reached through a composite key, one nullable, one not.
//
// The upload and the download are the two the ordinary method shape cannot
// express, and CreateWithFiles is the one whose whole point is that leaving out
// a required file does not compile.
func TestFileMethodsCompile(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "files.ir.json"))

	gentest.MustCompile(t, gentest.Run(t, goclient.New(), doc, opts()), "client")
}

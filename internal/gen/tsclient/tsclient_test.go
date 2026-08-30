package tsclient_test

import (
	"flag"
	"path/filepath"
	"strings"
	"testing"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/tsclient"
	"github.com/simonjanss/rig/pkg/gen"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const (
	fixture = "lifecycle.ir.json"
	// A second document, because file columns are the one part of a resource the
	// ordinary method shape cannot express and the lifecycle schema has none. It
	// carries all three cases: an optional file column, a required one, and a
	// table with two.
	filesFixture = "files.ir.json"
	// A third, because neither of the above has a table that streams without
	// declaring params, and a factory with nothing to forward binds its params
	// differently. This one has two: the notification tables are rig's own,
	// unexposed, and subscribed to rather than read — which is the shape a
	// project meets first, since it arrives with the module rather than being
	// asked for.
	notifyFixture = "notify.ir.json"
)

func opts() gen.Options {
	return gen.Options{OutDir: ".", Raw: map[string]any{}}
}

func load(t *testing.T, name string) []gen.Artifact {
	t.Helper()
	doc := gentest.LoadDocument(t, filepath.Join("testdata", name))
	return gentest.Run(t, tsclient.New(), doc, opts())
}

func TestGolden(t *testing.T) {
	t.Parallel()

	gentest.Golden(t, filepath.Join("testdata", "lifecycle"), load(t, fixture), *update)
}

func TestGoldenFiles(t *testing.T) {
	t.Parallel()

	gentest.Golden(t, filepath.Join("testdata", "files"), load(t, filesFixture), *update)
}

func TestGoldenNotify(t *testing.T) {
	t.Parallel()

	gentest.Golden(t, filepath.Join("testdata", "notify"), load(t, notifyFixture), *update)
}

func TestDeterministic(t *testing.T) {
	t.Parallel()

	for _, name := range []string{fixture, filesFixture, notifyFixture} {
		doc := gentest.LoadDocument(t, filepath.Join("testdata", name))
		gentest.Deterministic(t, tsclient.New(), doc, opts())
	}
}

// The client is where a description reaches whoever is calling the API rather
// than whoever wrote it, which is the reader who has nothing else to go on.
func TestDescriptionsReachTheOutput(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	gentest.DescriptionsSurvive(t, doc, gentest.Run(t, tsclient.New(), doc, opts()),
		func(name string) string { return "export type " + name + " = {" })
}

// The finding that shaped this generator, kept as a test because it is the one
// mistake a reader of the output would most naturally make.
//
// A row reaches a front end two ways and they are not the same shape: REST sends
// the keys `json_case` produced, and a stream sends what Postgres printed. One
// type for both would compile and be wrong about every key.
func TestAStreamedRowIsItsOwnTypeKeyedByColumn(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, fixture), "lesson.gen.ts")

	rest, ok := between(src, "export type Lesson = {", "\n};")
	if !ok {
		t.Fatal("no Lesson type in the emitted file")
	}
	row, ok := between(src, "export type LessonRow = {", "\n};")
	if !ok {
		t.Fatal("no LessonRow type in the emitted file")
	}

	if !strings.Contains(rest, "createdAt:") {
		t.Errorf("the REST type should carry the wire key createdAt:\n%s", rest)
	}
	if strings.Contains(rest, "created_at:") {
		t.Errorf("the REST type should not carry a column name:\n%s", rest)
	}
	if !strings.Contains(row, "created_at:") {
		t.Errorf("the stream row should carry the column name created_at:\n%s", row)
	}
	if strings.Contains(row, "createdAt:") {
		t.Errorf("the stream row should not carry a wire key:\n%s", row)
	}
}

// A stream row is never absent, only null. The sync service sends every column
// of the projection on every row, so an optional member would make a caller
// narrow a value that is always there.
func TestAStreamedRowIsNullableRatherThanOptional(t *testing.T) {
	t.Parallel()

	row, ok := between(fileOf(t, load(t, fixture), "lesson.gen.ts"),
		"export type LessonRow = {", "\n};")
	if !ok {
		t.Fatal("no LessonRow type in the emitted file")
	}

	if !strings.Contains(row, "deleted_at: string | null;") {
		t.Errorf("a nullable column should be `T | null` and not optional:\n%s", row)
	}
}

// The columns decide which shapes exist, which is the rule the server already
// follows. Asking a second time here would create a way for the two to disagree.
func TestTheExtraStreamsComeFromTheColumns(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, fixture), "electric.gen.ts")

	for _, want := range []string{
		"export const createLessonStream",
		"export const createLessonDeletedStream",
		"export const createLessonVersionsStream",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %s\n%s", want, src)
		}
	}
}

// The history route takes the row's identifier as a path segment, so it must not
// also travel on the query string.
func TestTheHistoryStreamSplicesTheIdIntoTheRoute(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, fixture), "electric.gen.ts")

	if !strings.Contains(src, "_versions/_stream`") {
		t.Errorf("the history route should be a template literal:\n%s", src)
	}
	if strings.Contains(src, "id: params.id") {
		t.Errorf("the identifier is a path segment and should not be sent as a param:\n%s", src)
	}
}

// Nothing is emitted for live sync until a table asks for it, which is the
// promise server-go makes on the server side. It is what keeps the streaming
// package out of a project that streams nothing.
func TestNoStreamFileWithoutAStreamedTable(t *testing.T) {
	t.Parallel()

	for _, a := range load(t, filesFixture) {
		if a.Path == "electric.gen.ts" {
			t.Fatal("a document with no electric table produced electric.gen.ts")
		}
	}
}

// A search is issued as QUERY and falls back to the alias the router mounts
// beside it, which is the one behaviour a proxy in front of a deployment can
// take away without anybody noticing.
func TestSearchCarriesItsFallback(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, fixture), "lesson_client.gen.ts")

	if !strings.Contains(src, "METHOD_QUERY") {
		t.Errorf("a search should be issued as QUERY:\n%s", src)
	}
	if !strings.Contains(src, `fallback: "/lessons/_search"`) {
		t.Errorf("a search should carry the alias to fall back to:\n%s", src)
	}
}

// An update says its three states in the type: absent leaves the field alone,
// null clears it, and a column that cannot hold null has no way to be given one.
func TestAnUpdateOnlyOffersNullWhereTheColumnTakesIt(t *testing.T) {
	t.Parallel()

	input, ok := between(fileOf(t, load(t, fixture), "lesson_input.gen.ts"),
		"export type LessonUpdateInput = {", "\n};")
	if !ok {
		t.Fatal("no LessonUpdateInput in the emitted file")
	}

	if !strings.Contains(input, "notes?: string | null;") {
		t.Errorf("a nullable column should accept null:\n%s", input)
	}
	if !strings.Contains(input, "title?: string;") {
		t.Errorf("a not-null column should not accept null:\n%s", input)
	}
}

// Every generated file has to be importable from one place, or a caller has to
// know which file the generator happened to put a type in.
func TestTheBarrelExportsEveryFile(t *testing.T) {
	t.Parallel()

	artifacts := load(t, fixture)
	index := fileOf(t, artifacts, "index.ts")

	for _, a := range artifacts {
		if a.Path == "index.ts" {
			continue
		}
		want := `"./` + strings.TrimSuffix(a.Path, ".ts") + `.js"`
		if !strings.Contains(index, want) {
			t.Errorf("the barrel does not export %s", a.Path)
		}
	}
}

// An upload is the one method the ordinary shape cannot express, and getting it
// wrong is silent: a JSON-only method for a multipart route compiles, calls the
// right path, and sends nothing at all.
func TestAnUploadCarriesItsFileAsAForm(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, filesFixture), "profile_client.gen.ts")

	if !strings.Contains(src,
		"uploadBannerFile(iD: string, file: Upload, options?: CallOptions)") {
		t.Errorf("an upload should take the file it sends:\n%s", src)
	}
	if !strings.Contains(src, `const form = multipart(undefined, [["bannerFile", file]]);`) {
		t.Errorf("an upload should send the part the document declares:\n%s", src)
	}
	// No row: an upload binds bytes to a row that already exists.
	if strings.Contains(src, "body: input,\n            form,") {
		t.Errorf("an upload should send a form and not a JSON body too:\n%s", src)
	}
}

// The multipart create is beside the JSON one rather than instead of it, so that
// adding a file column to a table somebody already had does not break a caller.
func TestAMultipartCreateSitsBesideTheJsonOne(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, filesFixture), "profile_client.gen.ts")

	if !strings.Contains(src, "create(input: ProfileCreateInput, options?: CallOptions)") {
		t.Errorf("the JSON create should still be there:\n%s", src)
	}
	if !strings.Contains(src,
		"createWithFiles(input: ProfileCreateInput, files: ProfileCreateFiles, options?: CallOptions)") {
		t.Errorf("the multipart create should take the files shape:\n%s", src)
	}
	// The row goes first, because the server reads the parts in order and wants
	// it before the bytes have anywhere to go.
	if !strings.Contains(src, "const form = multipart(input, [") {
		t.Errorf("the row should be the form's first part:\n%s", src)
	}
}

// The whole point of the files shape: leaving out a file the schema requires is
// a compile error rather than a 422.
func TestAFilesShapeIsOptionalOnlyWhereTheColumnIsNullable(t *testing.T) {
	t.Parallel()

	artifacts := load(t, filesFixture)

	optional, ok := between(fileOf(t, artifacts, "profile_input.gen.ts"),
		"export type ProfileCreateFiles = {", "\n};")
	if !ok {
		t.Fatal("no ProfileCreateFiles in the emitted file")
	}
	if !strings.Contains(optional, "bannerFile?: Upload;") {
		t.Errorf("a nullable file column should be optional:\n%s", optional)
	}

	required, ok := between(fileOf(t, artifacts, "profile_attachment_input.gen.ts"),
		"export type ProfileAttachmentCreateFiles = {", "\n};")
	if !ok {
		t.Fatal("no ProfileAttachmentCreateFiles in the emitted file")
	}
	if !strings.Contains(required, "documentFile: Upload;") {
		t.Errorf("a not-null file column should not be optional:\n%s", required)
	}
}

// A table with no file columns gets neither the shape nor the second create, so
// the ordinary resource reads exactly as it did.
func TestNoFilesShapeWithoutAFileColumn(t *testing.T) {
	t.Parallel()

	artifacts := load(t, fixture)
	for _, a := range artifacts {
		if strings.Contains(string(a.Content), "CreateFiles") {
			t.Errorf("%s mentions a files shape for a schema with no file column", a.Path)
		}
		if strings.Contains(string(a.Content), "createWithFiles") {
			t.Errorf("%s has a multipart create for a schema with no file column", a.Path)
		}
	}
}

// A streamed table need not be exposed, and its row type is emitted from the
// columns rather than from an object — so the types those columns carry have to
// be reached from the stream as well as from the endpoints.
func TestAnUnexposedStreamedRowDeclaresItsEnums(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for i := range doc.API.Resources {
		doc.API.Resources[i].Endpoints = nil
	}
	artifacts := gentest.Run(t, tsclient.New(), doc, opts())

	src := fileOf(t, artifacts, "electric.gen.ts")
	if !strings.Contains(src, "status: LessonStatus;") {
		t.Fatalf("the row should carry its enum column:\n%s", src)
	}
	if !strings.Contains(src, `import type { LessonStatus } from "./lesson_status.gen.js";`) {
		t.Errorf("the enum should be imported where it is named:\n%s", src)
	}
	fileOf(t, artifacts, "lesson_status.gen.ts")
}

// A factory's second parameter is bound whether or not the body reads it, so
// that `createCollectionCache` keeps inferring the params type from that
// position. Where nothing reads it the name is underscored, because rig writes
// no tsconfig and the templates a front end starts from turn
// `noUnusedParameters` on — the alternative is a TS6133 in a file whose banner
// says not to edit it.
func TestAParamlessStreamUnderscoresTheBindingNothingReads(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, notifyFixture), "electric.gen.ts")

	if n := strings.Count(src, "_params: Record<string, never>"); n == 0 {
		t.Errorf("a factory with nothing to forward should underscore it:\n%s", src)
	}
	if strings.Contains(src, " params: Record<string, never>") {
		t.Errorf("no factory should bind a params it never reads:\n%s", src)
	}
}

// The history route is the exception, and it is not about the declared params:
// the identifier is a path segment, so that factory reads the binding even when
// the resource declares nothing. Asserted on a lifecycle document with its
// params taken away, because no committed schema has a snapshotable table that
// streams without them.
func TestTheHistoryStreamBindsParamsWithNoneDeclared(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	for i := range doc.API.Resources {
		if doc.API.Resources[i].Electric != nil {
			doc.API.Resources[i].Electric.Params = nil
		}
	}
	src := fileOf(t, gentest.Run(t, tsclient.New(), doc, opts()), "electric.gen.ts")

	if !strings.Contains(src, "(runtime: Runtime, params: { id: string }) =>") {
		t.Errorf("the history factory should still bind params:\n%s", src)
	}
	if !strings.Contains(src, "pathValue(params.id)") {
		t.Errorf("the history route should splice the identifier:\n%s", src)
	}
	// And its siblings, which have nothing to read, should not.
	if n := strings.Count(src, "(runtime: Runtime, _params: Record<string, never>) =>"); n != 2 {
		t.Errorf("want the live and trash factories underscored, got %d:\n%s", n, src)
	}
}

// The same document with its params declared reads the binding in every
// factory, so nothing there is underscored. Without this the underscore could
// be applied to all of them and only a golden would notice.
func TestAStreamWithParamsBindsThemPlainly(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, fixture), "electric.gen.ts")

	if strings.Contains(src, "_params") {
		t.Errorf("a factory that reads its params should not underscore them:\n%s", src)
	}
}

func fileOf(t *testing.T, artifacts []gen.Artifact, path string) string {
	t.Helper()
	for _, a := range artifacts {
		if a.Path == path {
			return string(a.Content)
		}
	}
	t.Fatalf("no %s among the emitted files", path)
	return ""
}

func between(src, open, close string) (string, bool) {
	_, rest, ok := strings.Cut(src, open)
	if !ok {
		return "", false
	}
	body, _, ok := strings.Cut(rest, close)
	return body, ok
}

// The fallback that makes the constant mean something — and the one character
// in it that matters.
//
// `??` and not `||`: the empty string is falsy and is the same-origin answer a
// browser served by this API wants, so `||` would quietly repoint a front end
// that passed `""` at whatever the project called its default. That is not a
// hypothetical — examples/linearlite/web and the typecheck fixture both pass it.
func TestTheDefaultBaseUrlIsUsedWithoutSwallowingTheEmptyString(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, filesFixture), "client.gen.ts")

	if !strings.Contains(src, "baseUrl: config.baseUrl ?? defaultBaseUrl,") {
		t.Errorf("createClient does not fall back to defaultBaseUrl:\n%s", src)
	}
	if strings.Contains(src, "config.baseUrl || defaultBaseUrl") {
		t.Error(`|| would take the same-origin "" away from a browser; use ??`)
	}
	if !strings.Contains(src, "export const defaultBaseUrl = servers.production;") {
		t.Error("the default is not the deployment rig.yaml marked")
	}
	if !strings.Contains(src, `export type ClientConfig = Omit<Config, "baseUrl"> & { baseUrl?: string };`) {
		t.Error("baseUrl did not become optional, so the fallback is unreachable")
	}
}

// A project that named no deployment keeps the client it had: no constants, and
// a createClient that goes on requiring a baseUrl.
func TestAClientForAProjectWithNoDeploymentsAsksForABaseUrl(t *testing.T) {
	t.Parallel()

	src := fileOf(t, load(t, fixture), "client.gen.ts")
	if strings.Contains(src, "defaultBaseUrl") || strings.Contains(src, "ClientConfig") {
		t.Error("a project that named nowhere got a default anyway")
	}
	if !strings.Contains(src, "export function createClient(config: Config): Client {") {
		t.Error("a client with no default stopped requiring a baseUrl")
	}
}

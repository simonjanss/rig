package openapigen_test

import (
	"encoding/json"
	"flag"
	"net/http"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pb33f/libopenapi"
	validator "github.com/pb33f/libopenapi-validator/schema_validation"
	"github.com/pb33f/libopenapi/datamodel/high/base"
	v3 "github.com/pb33f/libopenapi/datamodel/high/v3"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/openapigen"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// fixtures are the compiled documents this generator is held to. Each is a copy
// of the compiler's own golden, and gentest's fixture test enforces that.
var fixtures = []string{"lifecycle", "files", "authwired", "ownerscope", "notify"}

// primary is the fixture the golden files and most assertions are written
// against: it has a QUERY search with its alias, soft delete, snapshots, a
// custom endpoint, enums, a formatted field, an immutable field and live sync —
// and no auth block, which makes it the no-security case as well.
const primary = "lifecycle"

func opts() gen.Options {
	return gen.Options{OutDir: "."}
}

// deprecatedServersOpts is the superseded per-generator option, which is read
// only by a project that named no deployment of its own.
func deprecatedServersOpts() gen.Options {
	return gen.Options{OutDir: ".", Raw: map[string]any{
		"servers": []any{map[string]any{"url": "https://api.example.test"}},
	}}
}

func load(t *testing.T, fixture string) *ir.Document {
	t.Helper()
	return gentest.LoadDocument(t, filepath.Join("testdata", fixture+".ir.json"))
}

func run(t *testing.T, fixture string) []gen.Artifact {
	t.Helper()
	return gentest.Run(t, openapigen.New(), load(t, fixture), opts())
}

// yamlOf is the rendered YAML, which is what most assertions read: it is the
// same model as the JSON and it is the one a person would open.
func yamlOf(t *testing.T, artifacts []gen.Artifact) string {
	t.Helper()
	for _, a := range artifacts {
		if a.Path == "openapi.gen.yaml" {
			return string(a.Content)
		}
	}
	t.Fatal("no openapi.gen.yaml among the artifacts")
	return ""
}

// model parses an emitted document back into a high-level model, which is the
// only honest way to assert about what was produced: reading the bytes with a
// regexp tests the renderer, not the document.
func model(t *testing.T, artifacts []gen.Artifact) *v3.Document {
	t.Helper()
	doc, err := libopenapi.NewDocument([]byte(yamlOf(t, artifacts)))
	if err != nil {
		t.Fatalf("the emitted document does not parse: %v", err)
	}
	built, err := doc.BuildV3Model()
	if err != nil {
		t.Fatalf("the emitted document is not a v3 model: %v", err)
	}
	return &built.Model
}

func TestGolden(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			gentest.Golden(t, filepath.Join("testdata", fixture), run(t, fixture), *update)
		})
	}
}

func TestDeterministic(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			gentest.Deterministic(t, openapigen.New(), load(t, fixture), opts())
		})
	}
}

// TestTheDocumentIsValid is the in-process half of the verification: the
// emitted bytes are parsed back and checked against the OpenAPI meta-schema.
// The external linter in `make openapi-lint` is the other half, and catches
// what a valid-but-poor document gets wrong.
func TestTheDocumentIsValid(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			artifacts := run(t, fixture)
			doc, err := libopenapi.NewDocument([]byte(yamlOf(t, artifacts)))
			if err != nil {
				t.Fatalf("does not parse: %v", err)
			}
			if _, err := doc.BuildV3Model(); err != nil {
				t.Fatalf("does not build: %v", err)
			}

			ok, errs := validator.ValidateOpenAPIDocument(doc)
			if !ok {
				for _, e := range errs {
					t.Errorf("%s: %s", e.Message, e.Reason)
					for _, se := range e.SchemaValidationErrors {
						t.Errorf("  %s at %s", se.Reason, se.FieldPath)
					}
				}
			}
		})
	}
}

// TestDescriptionsReachTheOutput holds the generator to the document's own
// words. ir.Field.Description is the single copy of them, and an output with
// somewhere to put one has to put that one there.
//
// It reads the YAML and not the JSON, and the reason is not cosmetic.
// gentest's matcher collapses whitespace, but a paragraph break inside a JSON
// string is the two characters backslash-n, which is not whitespace and does
// not collapse — so every multi-paragraph description in the fixture would fail
// against the JSON. YAML renders the same string as a block scalar with real
// newlines. Both artifacts come from one model, so checking one checks both.
func TestDescriptionsReachTheOutput(t *testing.T) {
	t.Parallel()

	doc := load(t, primary)
	artifacts := run(t, primary)

	var yamlOnly []gen.Artifact
	for _, a := range artifacts {
		if a.Path == "openapi.gen.yaml" {
			yamlOnly = append(yamlOnly, a)
		}
	}

	gentest.DescriptionsSurvive(t, doc, yamlOnly, func(name string) string {
		// A component key. The colon is what keeps Lesson from matching
		// LessonFilter, and a $ref value has none.
		return name + ":"
	})
}

// TestAMultiParagraphDescriptionIsABlockScalar is why the check above reads the
// YAML. If this ever stops holding, that test is silently weaker than it looks.
func TestAMultiParagraphDescriptionIsABlockScalar(t *testing.T) {
	t.Parallel()

	if got := yamlOf(t, run(t, primary)); !strings.Contains(got, "description: |-") {
		t.Error("no block scalar in the document; TestDescriptionsReachTheOutput " +
			"is relying on one")
	}
}

// TestEveryEndpointReachesTheDocument is the invariant the whole generator is
// for. A golden file cannot catch a missing endpoint family: one that was never
// emitted still matches a golden that never had it.
func TestEveryEndpointReachesTheDocument(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			doc := load(t, fixture)
			ids := operationIDs(model(t, run(t, fixture)))

			for _, res := range doc.API.Resources {
				if res.Unexposed {
					continue
				}
				for i := range res.Endpoints {
					ep := &res.Endpoints[i]
					// A QUERY with no POST alias has no route 3.1 can describe.
					// Its absence is deliberate and the tag says so.
					if ep.Method == "QUERY" && len(ep.AliasPatterns) == 0 {
						continue
					}
					if !slices.Contains(ids, ep.OperationID) {
						t.Errorf("%s.%s (%s) reached no operation",
							res.Name, ep.Name, ep.Pattern)
					}
				}
			}
		})
	}
}

func operationIDs(m *v3.Document) []string {
	var out []string
	for pair := m.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		for _, op := range pair.Value().GetOperations().FromOldest() {
			out = append(out, op.OperationId)
		}
	}
	return out
}

func TestOperationIdsAreUnique(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			seen := map[string]bool{}
			for _, id := range operationIDs(model(t, run(t, fixture))) {
				if seen[id] {
					t.Errorf("operationId %q is used more than once", id)
				}
				seen[id] = true
			}
		})
	}
}

var pathParam = regexp.MustCompile(`\{([^}]*)\}`)

// TestEveryPathParameterIsDeclared compares both directions: a template naming
// something no operation declares, and an operation declaring a path parameter
// the template does not contain. Either one is a document that fails its own
// validation, and the live-sync history route — whose {id} the IR does not
// declare — is the case that makes this worth having.
func TestEveryPathParameterIsDeclared(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			m := model(t, run(t, fixture))
			for pair := m.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
				path := pair.Key()

				var inTemplate []string
				for _, match := range pathParam.FindAllStringSubmatch(path, -1) {
					inTemplate = append(inTemplate, match[1])
				}
				slices.Sort(inTemplate)

				for method, op := range pair.Value().GetOperations().FromOldest() {
					var declared []string
					for _, p := range op.Parameters {
						if p.In == "path" {
							declared = append(declared, p.Name)
						}
					}
					slices.Sort(declared)

					if !slices.Equal(inTemplate, declared) {
						t.Errorf("%s %s: template has %v, operation declares %v",
							strings.ToUpper(method), path, inTemplate, declared)
					}
				}
			}
		})
	}
}

// TestSearchIsDocumentedAsItsPostAlias holds the one decision 3.1 forced.
func TestSearchIsDocumentedAsItsPostAlias(t *testing.T) {
	t.Parallel()

	artifacts := run(t, primary)
	m := model(t, artifacts)

	alias, ok := m.Paths.PathItems.Get("/api/v1/lessons/_search")
	if !ok {
		t.Fatal("no POST alias path for the search")
	}
	if alias.Post == nil || alias.Post.OperationId != "searchLessons" {
		t.Error("the alias does not carry the search's own operationId")
	}
	if !strings.Contains(alias.Post.Description, "QUERY /api/v1/lessons") {
		t.Error("the alias does not say what the primary form is")
	}

	collection, ok := m.Paths.PathItems.Get("/api/v1/lessons")
	if !ok {
		t.Fatal("no collection path")
	}
	if collection.Query != nil {
		t.Error("a query operation was emitted; the 3.1 meta-schema rejects one")
	}
	// The model would drop a key it could not parse, so the bytes are checked
	// too: libopenapi has a Query field that renders happily.
	if regexp.MustCompile(`(?m)^\s{8}query:`).MatchString(yamlOf(t, artifacts)) {
		t.Error("a query: path-item key reached the rendered document")
	}
}

// TestUnexposedShapesStayOut proves the reachability walk is doing work.
//
// AuthLogEntry is the sharpest case: the compiler injects it for a consumer
// that does not exist yet, nothing references it, and it must not appear.
func TestUnexposedShapesStayOut(t *testing.T) {
	t.Parallel()

	for _, fixture := range []string{"authwired", "notify"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			m := model(t, run(t, fixture))
			for _, unwanted := range []string{
				"AuthLogEntry", "RigAccountToken", "RigSession",
				"RigNotification", "RigNotificationFilter",
			} {
				if _, found := m.Components.Schemas.Get(unwanted); found {
					t.Errorf("%s is in components/schemas and no operation carries it",
						unwanted)
				}
			}
		})
	}
}

// TestNoSQLDefaultLeaksIntoTheDocument guards the trap in ir.Field.Default: on a
// query parameter it is a wire literal, on a column-backed field it is the
// Postgres default expression. Nothing else in the repository catches this.
func TestNoSQLDefaultLeaksIntoTheDocument(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			got := yamlOf(t, run(t, fixture))
			for _, expr := range []string{"default: now()", "gen_random_uuid()", "default: '''"} {
				if strings.Contains(got, expr) {
					t.Errorf("a SQL default expression reached the document: %s", expr)
				}
			}
		})
	}

	m := model(t, run(t, primary))
	lesson, ok := m.Components.Schemas.Get("Lesson")
	if !ok {
		t.Fatal("no Lesson schema")
	}
	createdAt, ok := lesson.Schema().Properties.Get("createdAt")
	if !ok {
		t.Fatal("no createdAt on Lesson")
	}
	if createdAt.Schema().Default != nil {
		t.Error("createdAt carries a default; a response field is always sent")
	}

	list := findOperation(t, m, "listLessons")
	for _, p := range list.Parameters {
		if p.Name != "limit" {
			continue
		}
		if p.Schema.Schema().Default == nil {
			t.Error("limit lost its default, which is the kind that should be documented")
		}
	}
}

// TestNullableIsAUnionNotAKeyword: 3.1 is JSON Schema 2020-12, where null is a
// type. The nullable keyword is 3.0's and means nothing here.
func TestNullableIsAUnionNotAKeyword(t *testing.T) {
	t.Parallel()

	artifacts := run(t, primary)
	if strings.Contains(yamlOf(t, artifacts), "nullable:") {
		t.Error("the 3.0 nullable keyword reached a 3.1 document")
	}

	lesson := schemaOf(t, model(t, artifacts), "Lesson")

	notes := propertyOf(t, lesson, "notes")
	if !slices.Equal(notes.Type, []string{"string", "null"}) {
		t.Errorf("notes type = %v, want [string null]", notes.Type)
	}

	tags := propertyOf(t, lesson, "tags")
	if !slices.Equal(tags.Type, []string{"array", "null"}) {
		t.Errorf("tags type = %v, want [array null] — the array is nullable, not its "+
			"elements", tags.Type)
	}
	if items := tags.Items; items == nil || !items.IsA() {
		t.Fatal("tags has no items schema")
	} else if got := items.A.Schema().Type; !slices.Equal(got, []string{"string"}) {
		t.Errorf("tags items type = %v, want [string]", got)
	}
}

// TestImmutableIsExpressedByShape: OpenAPI has no keyword for it, and writeOnly
// means the opposite. The paths say it — present on create, absent on update.
func TestImmutableIsExpressedByShape(t *testing.T) {
	t.Parallel()

	m := model(t, run(t, primary))

	if strings.Contains(yamlOf(t, run(t, primary)), "writeOnly:") {
		t.Error("writeOnly was emitted; it means request-only, not immutable")
	}

	create := schemaOf(t, m, "LessonCreateInput")
	if _, ok := create.Properties.Get("startsAt"); !ok {
		t.Error("the immutable field is missing from the create body")
	}
	update := schemaOf(t, m, "LessonUpdateInput")
	if _, ok := update.Properties.Get("startsAt"); ok {
		t.Error("the immutable field is in the update body, which cannot change it")
	}
	if len(update.Required) != 0 {
		t.Errorf("the update body requires %v; a PATCH leaves an absent field alone",
			update.Required)
	}

	entity := schemaOf(t, m, "Lesson")
	id := propertyOf(t, entity, "id")
	if id.ReadOnly == nil || !*id.ReadOnly {
		t.Error("a read-only field is not marked readOnly on the entity")
	}
}

// TestErrorResponsesNameTheirCode: ir.Endpoint.Errors is bare statuses, and the
// pairing with the code a client switches on has to survive into the document.
func TestErrorResponsesNameTheirCode(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()

			doc := load(t, fixture)
			m := model(t, run(t, fixture))

			for _, res := range doc.API.Resources {
				for i := range res.Endpoints {
					for _, status := range res.Endpoints[i].Errors {
						if _, ok := m.Components.Responses.Get(
							responseName(status)); !ok {
							t.Errorf("status %d has no shared response", status)
						}
					}
				}
			}

			for pair := m.Components.Responses.First(); pair != nil; pair = pair.Next() {
				if pair.Value().Description == "" {
					t.Errorf("%s has an empty description", pair.Key())
				}
			}
		})
	}
}

// responseName mirrors the generator's own naming, from the compiler's table.
func responseName(status int) string {
	for _, c := range errorCodeTable {
		if c.status == status {
			return c.name
		}
	}
	return ""
}

var errorCodeTable = []struct {
	name   string
	status int
}{
	{"BadRequest", 400}, {"Unauthorized", 401}, {"Forbidden", 403}, {"NotFound", 404},
	{"TooLarge", 413}, {"UnsupportedMediaType", 415}, {"Conflict", 409},
	{"UnprocessableEntity", 422}, {"UpgradeRequired", 426}, {"RateLimited", 429},
	{"Internal", 500},
}

// TestTheScopeParameterIsAnEnumeration: the scope parameter's Go type is
// tenancy.Scope rather than an IR enum, so without a special case the document
// would say `string` and leave the two values to prose.
func TestTheScopeParameterIsAnEnumeration(t *testing.T) {
	t.Parallel()

	m := model(t, run(t, "ownerscope"))

	var checked int
	for pair := m.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		for _, op := range pair.Value().GetOperations().FromOldest() {
			for _, p := range op.Parameters {
				if p.Name != ir.ScopeParam {
					continue
				}
				checked++
				s := p.Schema.Schema()
				if len(s.Enum) != 2 {
					t.Errorf("%s: scope has %d enum values, want own and all",
						op.OperationId, len(s.Enum))
				}
				if s.Default == nil || s.Default.Value != "own" {
					t.Errorf("%s: scope does not default to own", op.OperationId)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no scope parameter found; this test is watching nothing")
	}
}

// TestMultipartCarriesItsParts: a create on a table with file columns takes the
// row and its bytes together, because a not-null file column is otherwise
// unreachable.
func TestMultipartCarriesItsParts(t *testing.T) {
	t.Parallel()

	m := model(t, run(t, "files"))

	var found bool
	for pair := m.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		op := pair.Value().Post
		if op == nil || op.RequestBody == nil {
			continue
		}
		form, ok := op.RequestBody.Content.Get(ir.MediaMultipart)
		if !ok {
			continue
		}
		found = true

		if form.Encoding == nil {
			t.Errorf("%s: a multipart body with no encoding block", op.OperationId)
			continue
		}
		for enc := form.Encoding.First(); enc != nil; enc = enc.Next() {
			if enc.Value().ContentType == "" {
				t.Errorf("%s: part %q has no content type", op.OperationId, enc.Key())
			}
		}
	}
	if !found {
		t.Fatal("no multipart request body found; this test is watching nothing")
	}
}

// TestSecurityComesFromTheAuthBlock, and its absence from its absence.
func TestSecurityComesFromTheAuthBlock(t *testing.T) {
	t.Parallel()

	m := model(t, run(t, "authwired"))
	if m.Components.SecuritySchemes == nil {
		t.Fatal("a document with an auth block declares no security scheme")
	}
	scheme, ok := m.Components.SecuritySchemes.Get("bearerAuth")
	if !ok {
		t.Fatal("no bearerAuth scheme")
	}
	if scheme.Type != "http" || scheme.Scheme != "bearer" {
		t.Errorf("scheme = %s/%s, want http/bearer", scheme.Type, scheme.Scheme)
	}
	if m.Components.SecuritySchemes.Len() != 1 {
		t.Error("more than one scheme; both credentials arrive the same way and " +
			"are told apart by a prefix, which OpenAPI cannot express")
	}
	if len(m.Security) == 0 {
		t.Error("no document-level security requirement")
	}

	plain := model(t, run(t, primary))
	if plain.Components.SecuritySchemes != nil && plain.Components.SecuritySchemes.Len() > 0 {
		t.Error("a project with no auth block got a security scheme")
	}
	if len(plain.Security) != 0 {
		t.Error("a project with no auth block got a security requirement")
	}
}

// TestPublicEndpointsAllowButDoNotIgnoreACredential.
//
// ir.Endpoint.Public means the claims lookup need not succeed, not that a
// credential is ignored — a caller who presents one is still identified by it
// and may be shown more than a stranger. An empty security list would say the
// opposite, so the encoding is an empty requirement object beside the real one.
//
// No compiler fixture has a public endpoint, so this one is made rather than
// found. Skipping instead would be a test that reads as covering the branch and
// never executes it.
func TestPublicEndpointsAllowButDoNotIgnoreACredential(t *testing.T) {
	t.Parallel()

	doc := load(t, "authwired")

	var opened string
	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Unexposed || len(res.Endpoints) == 0 {
			continue
		}
		for j := range res.Endpoints {
			ep := &res.Endpoints[j]
			if len(routesFor(ep)) == 0 {
				continue
			}
			ep.Public = true
			opened = ep.OperationID
			break
		}
		break
	}
	if opened == "" {
		t.Fatal("no endpoint to open; this test is watching nothing")
	}

	m := model(t, gentest.Run(t, openapigen.New(), doc, opts()))
	op := findOperation(t, m, opened)
	if op == nil {
		t.Fatalf("%s is not in the document", opened)
	}

	if len(op.Security) != 2 {
		t.Fatalf("security has %d entries, want an empty requirement beside the real one",
			len(op.Security))
	}
	if !op.Security[0].ContainsEmptyRequirement {
		t.Error("the first requirement is not the empty one, so a caller presenting " +
			"nothing is not described as served")
	}
	if _, ok := op.Security[1].Requirements.Get("bearerAuth"); !ok {
		t.Error("the credential is not offered, so a caller presenting one is " +
			"described as ignored")
	}
}

// routesFor mirrors the generator's own rule for whether an endpoint can be
// described in 3.1 at all.
func routesFor(ep *ir.Endpoint) []string {
	var out []string
	if ep.Method != "QUERY" {
		out = append(out, ep.Pattern)
	}
	return append(out, ep.AliasPatterns...)
}

// TestAnUnexposedResourceStillStreams: a shape is its own read surface. The
// notification recipient table has no endpoints and is not exposed, and the mux
// still serves its shape route — server-go gates only on the resource having
// an Electric block — so a document that left it out would be describing fewer
// routes than the server answers on.
func TestAnUnexposedResourceStillStreams(t *testing.T) {
	t.Parallel()

	doc := load(t, "notify")
	m := model(t, run(t, "notify"))

	var checked int
	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if !res.Unexposed || res.Electric == nil {
			continue
		}
		checked++
		if _, ok := m.Paths.PathItems.Get(res.Electric.Path); !ok {
			t.Errorf("%s streams on %s and the document does not say so",
				res.Name, res.Electric.Path)
		}
		// The row itself stays out: the compiler emits no wire object for an
		// unexposed table, and a shape route answers with the sync protocol's
		// own body rather than the row.
		if _, found := m.Components.Schemas.Get(res.Name); found {
			t.Errorf("%s is in components/schemas and no operation carries it", res.Name)
		}
	}
	if checked == 0 {
		t.Fatal("no unexposed streaming resource in the fixture; this test is watching nothing")
	}
}

// TestNoComponentIsDeclaredUnused. vacuum reports one as a warning and
// `make openapi-lint` fails on warnings, so this is the in-process half of a
// check the pipeline makes anyway — and it runs on the shapes a fixture does
// not have, which is where the pipeline has nothing to say.
func TestNoComponentIsDeclaredUnused(t *testing.T) {
	t.Parallel()

	for _, fixture := range fixtures {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			assertNoUnusedComponents(t, run(t, fixture))
		})
	}

	// An API of nothing but reads: no Idempotency-Key to send, and no
	// Idempotency-Replayed to come back.
	t.Run("reads only", func(t *testing.T) {
		t.Parallel()

		doc := load(t, primary)
		for i := range doc.API.Resources {
			res := &doc.API.Resources[i]
			var reads []ir.Endpoint
			for _, ep := range res.Endpoints {
				if ep.Method == http.MethodGet {
					reads = append(reads, ep)
				}
			}
			res.Endpoints = reads
		}

		artifacts := gentest.Run(t, openapigen.New(), doc, opts())
		assertNoUnusedComponents(t, artifacts)
		if strings.Contains(yamlOf(t, artifacts), "Idempotency") {
			t.Error("an idempotency component survived into a document with no writes")
		}
	})
}

// assertNoUnusedComponents fails for any component nothing refers to.
//
// It reads the rendered JSON rather than the model, because a $ref is the one
// thing the model resolves away: two schemas that point at a third are
// indistinguishable from two that inline it once it is built.
func assertNoUnusedComponents(t *testing.T, artifacts []gen.Artifact) {
	t.Helper()

	var rendered string
	for _, a := range artifacts {
		if a.Path == "openapi.gen.json" {
			rendered = string(a.Content)
		}
	}
	if rendered == "" {
		t.Fatal("no openapi.gen.json among the artifacts")
	}

	var doc struct {
		Components map[string]map[string]json.RawMessage `json:"components"`
	}
	if err := json.Unmarshal([]byte(rendered), &doc); err != nil {
		t.Fatalf("the emitted document is not JSON: %v", err)
	}

	for kind, items := range doc.Components {
		// A security scheme is named by a security requirement rather than
		// referred to, so it has no $ref to look for.
		if kind == "securitySchemes" {
			continue
		}
		for name := range items {
			ref := "#/components/" + kind + "/" + name
			if !strings.Contains(rendered, `"`+ref+`"`) {
				t.Errorf("%s is declared and nothing refers to it", ref)
			}
		}
	}
}

// TestSecurityIsOnlyClaimedAgainstADeclaredScheme. ir.Endpoint.Public does not
// depend on the auth foundation being wired, so the combination is reachable —
// and an operation naming a scheme the document does not declare is an error a
// linter reports rather than a document a reader can use.
func TestSecurityIsOnlyClaimedAgainstADeclaredScheme(t *testing.T) {
	t.Parallel()

	doc := load(t, primary)
	if doc.API.Auth != nil {
		t.Fatal("the fixture has an auth block; this test needs one without")
	}

	var opened string
	for i := range doc.API.Resources {
		res := &doc.API.Resources[i]
		if res.Unexposed || len(res.Endpoints) == 0 {
			continue
		}
		for j := range res.Endpoints {
			ep := &res.Endpoints[j]
			if len(routesFor(ep)) == 0 {
				continue
			}
			ep.Public = true
			opened = ep.OperationID
			break
		}
		break
	}
	if opened == "" {
		t.Fatal("no endpoint to open; this test is watching nothing")
	}

	artifacts := gentest.Run(t, openapigen.New(), doc, opts())
	if strings.Contains(yamlOf(t, artifacts), securitySchemeName) {
		t.Errorf("%s is claimed by an operation and declared nowhere", securitySchemeName)
	}

	m := model(t, artifacts)
	if op := findOperation(t, m, opened); op != nil && len(op.Security) != 0 {
		t.Error("a public endpoint in a project with no credential describes one")
	}
}

// securitySchemeName is the key the generator declares its one scheme under.
const securitySchemeName = "bearerAuth"

func TestOptionsAreValidated(t *testing.T) {
	t.Parallel()

	doc := load(t, primary)

	cases := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{"unknown key", map[string]any{"verson": "3.1"}, "verson"},
		{"unsupported version", map[string]any{"version": "3.2"}, "3.2"},
		{"unknown format", map[string]any{"formats": []any{"toml"}}, "toml"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := openapigen.New().Generate(t.Context(), doc,
				gen.Options{OutDir: ".", Raw: tc.raw})
			if err == nil {
				t.Fatalf("no error for %v", tc.raw)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not name %q: %v", tc.want, err)
			}
		})
	}
}

func TestElectricShapesAreDocumentedUnlessTurnedOff(t *testing.T) {
	t.Parallel()

	m := model(t, run(t, primary))
	if _, ok := m.Paths.PathItems.Get("/api/v1/lesson/_stream"); !ok {
		t.Error("the live shape route is missing")
	}

	off := gentest.Run(t, openapigen.New(), load(t, primary),
		gen.Options{OutDir: ".", Raw: map[string]any{"electric": false}})
	if strings.Contains(yamlOf(t, off), "_stream") {
		t.Error("electric: false still emitted the shape routes")
	}
}

// servedDoc is the primary fixture with the document turned into a route.
//
// Set here rather than in a fixture of its own: it is an `api:` key and reaches
// the IR through Freeze, which internal/compile tests directly. The paths are
// the ones Freeze computes from this fixture's base path.
func servedDoc(t *testing.T) *ir.Document {
	t.Helper()

	return serveDoc(t, load(t, primary))
}

// serveDoc turns any fixture's document into one that serves itself.
//
// Import and Package are what the compiler joins out of the module path and the
// openapi generator's out_dir; there is no option for either, so there is
// nothing here for a project to state or a test to configure.
func serveDoc(t *testing.T, doc *ir.Document) *ir.Document {
	t.Helper()

	doc.API.OpenAPI = &ir.OpenAPI{
		JSONPath: "/api/v1/openapi.json",
		YAMLPath: "/api/v1/openapi.yaml",
		Import:   "example.com/demo/docs",
		Package:  "docs",
	}
	return doc
}

// TestOpenAPIEmbedGolden is the Go file this generator writes beside the
// document for a project that serves it.
//
// Only that file, because the two renderings are already goldened five times
// over and what is new here is one declaration and the directive above it.
func TestOpenAPIEmbedGolden(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, openapigen.New(), servedDoc(t),
		gen.Options{OutDir: filepath.Join("project", "docs")})

	var only []gen.Artifact
	for _, a := range artifacts {
		if a.Path == "openapi.gen.go" {
			only = append(only, a)
		}
	}
	if len(only) != 1 {
		t.Fatalf("want one openapi.gen.go, got %d", len(only))
	}

	gentest.Golden(t, filepath.Join("testdata", "openapi"), only, *update)
}

// The embed is the mechanism, and the reason the document is served with no line
// in anybody's main.go: a go:embed directive cannot climb out of the directory
// of the file it is written in, so it is written beside the renderings and the
// generated router imports the package.
func TestTheEmbedNamesEveryRenderingWritten(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		formats []any
		want    string
	}{
		{nil, "//go:embed openapi.gen.json openapi.gen.yaml"},
		{[]any{"json"}, "//go:embed openapi.gen.json"},
		{[]any{"yaml"}, "//go:embed openapi.gen.yaml"},
	} {
		raw := map[string]any{}
		if tc.formats != nil {
			raw["formats"] = tc.formats
		}
		artifacts := gentest.Run(t, openapigen.New(), servedDoc(t),
			gen.Options{OutDir: filepath.Join("project", "docs"), Raw: raw})

		src := ""
		for _, a := range artifacts {
			if a.Path == "openapi.gen.go" {
				src = string(a.Content)
			}
		}
		if src == "" {
			t.Fatalf("formats %v: no openapi.gen.go", tc.formats)
		}
		if !strings.Contains(src, tc.want+"\n") {
			t.Errorf("formats %v: want the directive %q, got:\n%s", tc.formats, tc.want, src)
		}
	}
}

// The package the embed file declares is the one the router will import it as.
// Where that name comes from is internal/project's — a directory name Go could
// not use is refused when rig.yaml is read — so what is left here is that the
// document's answer is the one written down.
func TestTheEmbedDeclaresThePackageTheDocumentNames(t *testing.T) {
	t.Parallel()

	doc := servedDoc(t)
	doc.API.OpenAPI.Package = "apispec"

	artifacts := gentest.Run(t, openapigen.New(), doc,
		gen.Options{OutDir: filepath.Join("project", "docs")})

	var src string
	for _, a := range artifacts {
		if a.Path == "openapi.gen.go" {
			src = string(a.Content)
		}
	}
	// Not "docs", which is what the output directory is called: the name has to
	// be the one the router qualifies its reference with, and only the compiled
	// document knows that.
	if !strings.Contains(src, "package apispec") {
		t.Errorf("want package apispec, got:\n%s", src)
	}
}

// A document that is served describes the routes it is served on. Anything else
// would be this generator's one claim failing on itself: the specification is
// supposed to describe every route the server answers, and these two are routes
// the same mux answers.
func TestTheServedDocumentDescribesItsOwnRoutes(t *testing.T) {
	t.Parallel()

	m := model(t, gentest.Run(t, openapigen.New(), servedDoc(t), opts()))

	for _, tc := range []struct{ path, id, mediaType string }{
		{"/api/v1/openapi.json", "getOpenAPIJSON", "application/json"},
		{"/api/v1/openapi.yaml", "getOpenAPIYAML", "application/yaml"},
	} {
		item, ok := m.Paths.PathItems.Get(tc.path)
		if !ok {
			t.Errorf("no path item for %s", tc.path)
			continue
		}
		op := item.Get
		if op == nil {
			t.Errorf("%s has no GET", tc.path)
			continue
		}
		if op.OperationId != tc.id {
			t.Errorf("%s: operationId = %q, want %q", tc.path, op.OperationId, tc.id)
		}
		if !slices.Contains(op.Tags, "openapi") {
			t.Errorf("%s: tags = %v, want the openapi tag", tc.path, op.Tags)
		}
		ok2, found := op.Responses.Codes.Get("200")
		if !found {
			t.Errorf("%s: no 200", tc.path)
			continue
		}
		if _, found := ok2.Content.Get(tc.mediaType); !found {
			t.Errorf("%s: 200 does not answer %s", tc.path, tc.mediaType)
		}
		// The conditional request is the whole reason the runtime hashes the
		// document, so a reader has to be told the header exists.
		if _, found := ok2.Headers.Get("ETag"); !found {
			t.Errorf("%s: 200 declares no ETag", tc.path)
		}
		if _, found := op.Responses.Codes.Get("304"); !found {
			t.Errorf("%s: no 304, so If-None-Match is undocumented", tc.path)
		}
	}

	// A tag nothing defines is a lint finding, and this one is referenced twice.
	var described bool
	for _, tag := range m.Tags {
		if tag.Name == "openapi" {
			described = tag.Description != ""
		}
	}
	if !described {
		t.Error("the openapi tag is referenced and not defined with a description")
	}
}

// One route per rendering the generator writes, which is what keeps the document
// and the server in step without either half of the wiring being told what the
// other was configured with: the runtime mounts a route per document it finds
// embedded, and only the formats listed here are ever written.
func TestOnlyTheFormatsWrittenAreDescribed(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, openapigen.New(), servedDoc(t),
		gen.Options{OutDir: filepath.Join("project", "docs"),
			Raw: map[string]any{"formats": []any{"json"}}})

	var body string
	for _, a := range artifacts {
		switch a.Path {
		case "openapi.gen.json":
			body = string(a.Content)
		case "openapi.gen.go":
		default:
			t.Fatalf("unexpected artifact %s", a.Path)
		}
	}
	if !strings.Contains(body, "/api/v1/openapi.json") {
		t.Error("the JSON route is not described")
	}
	if strings.Contains(body, "/api/v1/openapi.yaml") {
		t.Error("a YAML route is described and no YAML document is written")
	}
}

// A project with an auth block requires a credential document-wide. These two
// routes read none, and inheriting the default would describe one that is never
// checked.
func TestTheServedDocumentNeedsNoCredential(t *testing.T) {
	t.Parallel()

	m := model(t, gentest.Run(t, openapigen.New(), serveDoc(t, load(t, "authwired")), opts()))
	if m.Security == nil {
		t.Fatal("authwired no longer requires a credential document-wide")
	}

	item, ok := m.Paths.PathItems.Get("/api/v1/openapi.json")
	if !ok {
		t.Fatal("no path item for the document's own route")
	}
	// One empty requirement, and nothing beside it: OpenAPI's spelling of "no
	// authentication here". An absent key would inherit the default instead.
	if n := len(item.Get.Security); n != 1 {
		t.Fatalf("security has %d requirements, want 1 (the empty one)", n)
	}
	if got := item.Get.Security[0].Requirements; got != nil && got.Len() != 0 {
		t.Errorf("the requirement names %d schemes, want none", got.Len())
	}
}

// The negative: a project that keeps the document a file has no routes for it,
// no tag for routes that do not exist, and no Go file at all — its out_dir stays
// a directory of documents rather than becoming a package.
func TestADocumentThatIsNotServedDescribesNoRoutes(t *testing.T) {
	t.Parallel()

	artifacts := run(t, primary)
	for _, a := range artifacts {
		if a.Path == "openapi.gen.go" {
			t.Error("a project that does not serve the document got an embed file")
		}
	}

	body := yamlOf(t, artifacts)
	for _, absent := range []string{"openapi.json:", "openapi.yaml:", "getOpenAPIJSON"} {
		if strings.Contains(body, absent) {
			t.Errorf("emitted %q for a project that does not serve the document", absent)
		}
	}
}

func TestBothFormatsAreEmitted(t *testing.T) {
	t.Parallel()

	var paths []string
	for _, a := range run(t, primary) {
		paths = append(paths, a.Path)
	}
	slices.Sort(paths)
	if !slices.Equal(paths, []string{"openapi.gen.json", "openapi.gen.yaml"}) {
		t.Errorf("artifacts = %v", paths)
	}
}

func schemaOf(t *testing.T, m *v3.Document, name string) *base.Schema {
	t.Helper()
	proxy, ok := m.Components.Schemas.Get(name)
	if !ok {
		t.Fatalf("no %s schema", name)
	}
	return proxy.Schema()
}

func propertyOf(t *testing.T, s *base.Schema, name string) *base.Schema {
	t.Helper()
	proxy, ok := s.Properties.Get(name)
	if !ok {
		t.Fatalf("no %s property", name)
	}
	return proxy.Schema()
}

func findOperation(t *testing.T, m *v3.Document, id string) *v3.Operation {
	t.Helper()
	for pair := m.Paths.PathItems.First(); pair != nil; pair = pair.Next() {
		for _, op := range pair.Value().GetOperations().FromOldest() {
			if op.OperationId == id {
				return op
			}
		}
	}
	return nil
}

// The project's servers block is what the document lists, and the deployment it
// marks as the default comes first — because that is where a viewer sends its
// trial request, and a document whose "try it" went somewhere the SDK beside it
// does not default to is the disagreement the block exists to prevent.
//
// The authwired fixture marks its default on the second of three entries, so
// the order below cannot come from the file's order.
func TestTheProjectsServersReachTheDocumentDefaultFirst(t *testing.T) {
	t.Parallel()

	m := model(t, run(t, "authwired"))
	var got []string
	for _, s := range m.Servers {
		got = append(got, s.URL)
	}

	want := []string{
		"https://api.example.com",
		"http://localhost:8080",
		"https://staging.eu.example.com",
	}
	if !slices.Equal(got, want) {
		t.Errorf("servers = %v, want %v", got, want)
	}
	if d := m.Servers[2].Description; d != "The staging_eu deployment of this API." {
		t.Errorf("an entry with no description said %q rather than naming itself", d)
	}
}

// A project that names no deployment gets the relative server, which is true of
// every deployment and is what keeps the document usable in a viewer for a
// project that has not named a host.
func TestAProjectThatNamesNoDeploymentGetsARelativeServer(t *testing.T) {
	t.Parallel()

	m := model(t, run(t, primary))
	if len(m.Servers) != 1 || m.Servers[0].URL != "/" {
		t.Fatalf("want a single relative server, got %#v", m.Servers)
	}
}

// The superseded option still works, so a project that has not migrated is not
// broken by an upgrade it did not ask for. It is read only when the project
// named no deployment of its own; the refusal to set both lives in
// internal/project, which is the only place that can see both.
func TestTheDeprecatedServersOptionStillWorks(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, openapigen.New(), load(t, primary), deprecatedServersOpts())
	m := model(t, artifacts)
	if len(m.Servers) != 1 || m.Servers[0].URL != "https://api.example.test" {
		t.Fatalf("the deprecated option was ignored: %#v", m.Servers)
	}
}

// And the project's block wins over it, so the two can never both describe the
// document.
func TestTheProjectsServersWinOverTheDeprecatedOption(t *testing.T) {
	t.Parallel()

	artifacts := gentest.Run(t, openapigen.New(), load(t, "authwired"), deprecatedServersOpts())
	m := model(t, artifacts)
	if len(m.Servers) != 3 {
		t.Fatalf("want the project's three deployments, got %#v", m.Servers)
	}
}

package servergo_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/internal/gen/gentest"
	"github.com/simonjanss/rig/internal/gen/modelgo"
	"github.com/simonjanss/rig/internal/gen/persistgo"
	"github.com/simonjanss/rig/internal/gen/servergo"
	"github.com/simonjanss/rig/internal/gen/servicego"
	"github.com/simonjanss/rig/pkg/gen"
	"github.com/simonjanss/rig/pkg/ir"
)

// stubOpts is opts with the hand-owned scoping files turned on.
func stubOpts() gen.Options {
	return gen.Options{Raw: map[string]any{
		"package":      "api",
		"model_import": "rigtest/model",
		"electric_url": "http://electric:3000",
		"api_import":   "rigtest/api",
		"stub_dir":     "shapes/{table}",
	}}
}

// A shape is answered from the database with what is on the Shape literal, so
// the three of those that describe the rows have to agree with each other: the
// columns streamed, the key each row is named by, and the type a subscriber
// reads each column as. Nothing at runtime can check it — by then they are three
// constants that were emitted separately.
func TestWhatDescribesAShapesRowsAgreesWithItself(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	columns, ok := between(src, "var LessonShapeColumns = []string{", "\n}")
	if !ok {
		t.Fatal("no column list")
	}
	key, ok := between(src, "var LessonShapeKey = []string{", "\n}")
	if !ok {
		t.Fatal("no key list")
	}
	schema, ok := between(src, "const LessonShapeSchema = ", "\n")
	if !ok {
		t.Fatal("no schema")
	}

	names := regexp.MustCompile(`"([a-z_]+)"`)

	// Every streamed column has a type. Without one a subscriber leaves the value
	// as the string it arrived as, which decodes differently from the same column
	// read over the API.
	want := names.FindAllStringSubmatch(columns, -1)
	if len(want) == 0 {
		t.Fatal("no columns to check")
	}
	for _, m := range want {
		if !strings.Contains(schema, `\"`+m[1]+`\":`) {
			t.Errorf("%s is streamed but has no type in the schema", m[1])
		}
	}

	// The other direction: nothing typed that is not streamed. A column excluded
	// from the API is excluded here, and describing it would be describing a
	// column the shape does not carry.
	// Followed by an opening brace: a column's entry is an object, and the keys
	// inside one describe the column rather than being one.
	for _, m := range regexp.MustCompile(`\\"([a-z_]+)\\":\{`).FindAllStringSubmatch(schema, -1) {
		if !strings.Contains(columns, `"`+m[1]+`"`) {
			t.Errorf("%s has a type in the schema and is not streamed", m[1])
		}
	}

	// And the key is the primary key the schema names by pk_index, in that order.
	// Two answers to what identifies a row would be one too many.
	for i, m := range names.FindAllStringSubmatch(key, -1) {
		if !strings.Contains(schema, `\"`+m[1]+`\":{`) {
			t.Errorf("%s keys a row and is not in the schema", m[1])
		}
		if !strings.Contains(schema, `\"pk_index\":`+strconv.Itoa(i)) {
			t.Errorf("%s is key column %d and the schema does not say so", m[1], i)
		}
	}
	if len(names.FindAllStringSubmatch(key, -1)) == 0 {
		t.Error("a shape with no key is a shape whose rows cannot be named")
	}
}

// Every shape is answered from the database unless the table said otherwise, and
// off has to be written on the shape rather than left to a deployment: a project
// that sets Config.DB should not have to know which of its shapes wanted it.
func TestOnlyAShapeThatOptedOutSaysSo(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	if strings.Contains(src, "NoFallback") {
		t.Error("a shape nobody opted out of refuses to answer a sync outage")
	}

	// And with the key off, every one of the table's shapes says so — the live
	// one, its trash and its history all carry the same decision.
	off := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	off.Resource("Lesson").Electric.Fallback = false

	src = find(t, gentest.Run(t, servergo.New(), off, opts()), "lesson_shape.gen.go")
	if got := strings.Count(src, "NoFallback: true"); got != 3 {
		t.Errorf("%d of the table's three shapes opted out, want all of them", got)
	}
}

// The filter is the whole point of a shape. One whose tenant condition can be
// left off is a subscription to somebody else's data.
func TestTheFilterIsBuiltBeforeAnythingElseRuns(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	body, ok := between(src, "func handleLessonShape(", "\n}")
	if !ok {
		t.Fatal("no handler")
	}
	collapsed := collapse(body)

	for _, want := range []string{
		`where.Eq("tenant_id", claims.TenantID.String())`,
		`where.IsNull("deleted_at")`,
		`where.EqText("version_type", "Original")`,
	} {
		if !strings.Contains(collapsed, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	// Order matters: the scope runs last, and every condition is joined with
	// AND, so there is nothing a scope can do to widen the shape.
	tenant := strings.Index(collapsed, "tenant_id")
	scope := strings.Index(collapsed, "sh.Lesson(ctx,")
	if tenant < 0 || scope < 0 || tenant > scope {
		t.Error("the tenant condition should be added before the application's scope runs")
	}

	// And the proxy call comes after both.
	if serve := strings.Index(collapsed, "sh.Proxy.Serve("); serve < scope {
		t.Error("the shape should not be served before the scope has run")
	}
}

// A shape route is an API route, so it identifies its caller the way every
// other one does. It used to have a prepare of its own — a thinner copy that
// knew nothing about the revision gate, the rate limiter or the request
// identifier, and whose refusals reached no log at all.
func TestAShapeGoesThroughTheSharedRequestPipeline(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	body, ok := between(src, "func handleLessonShape(", "\n}")
	if !ok {
		t.Fatal("no handler")
	}
	collapsed := collapse(body)

	// The same call every generated route opens with, so the claims, the
	// request context and the refusal are one implementation rather than two.
	if !strings.Contains(collapsed, "ctx, claims, rc, ok := prepare(s, w, r)") {
		t.Errorf("a shape should prepare like every other route:\n%s", body)
	}

	// And every refusal carries the request context, which is what puts the
	// identifier on the body and the failure in the log.
	if strings.Contains(collapsed, "fail(s, w, r, err)") {
		t.Errorf("a refusal without the request context reaches no log:\n%s", body)
	}
	if !strings.Contains(collapsed, "fail(s, w, r, rc, err)") {
		t.Errorf("a refusal should carry the request context:\n%s", body)
	}

	// The scope receives the prepared context rather than the raw one, so what
	// a scope reads about the caller is what every service method reads.
	if !strings.Contains(collapsed, "sh.Lesson(ctx, r, claims, params, where)") {
		t.Errorf("the scope should be given the prepared context:\n%s", body)
	}
}

// Sharing that pipeline is not sharing all of it. prepare refuses a credential
// GetClaims rejected and nothing else, which is enough for every other route
// because the one after it ends at a repository, where tenancy.FromContext
// refuses claims that name no tenant a second time. A subscription ends at the
// sync service instead, so nothing downstream asks — and a GetClaims that
// answers an anonymous caller with empty claims rather than an error would
// otherwise reach a table with no tenant column and stream all of it.
func TestAShapeRefusesClaimsThatNameNoTenant(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	// Every one of the table's shapes, not the live one alone: the trash and
	// the history carry the same rows and are filtered by the same tenant.
	for _, handler := range []string{
		"handleLessonShape(", "handleLessonDeletedShape(", "handleLessonVersionsShape(",
	} {
		body, ok := between(src, "func "+handler, "\n}")
		if !ok {
			t.Fatalf("no %s", handler)
		}
		collapsed := collapse(body)

		if !strings.Contains(collapsed, "if !claims.Valid() {") {
			t.Errorf("%s streams for a caller with no tenant:\n%s", handler, body)
		}

		// Before the filter is built, since the filter is made of the claims.
		refuse := strings.Index(collapsed, "claims.Valid()")
		where := strings.Index(collapsed, "where := &electric.Where{}")
		if refuse < 0 || where < 0 || refuse > where {
			t.Errorf("%s builds its filter before checking there is one to build:\n%s", handler, body)
		}
	}
}

// An owner-scoped table streams the caller's own rows and nothing else.
//
// This was a hole rather than a decision: the shape builder emitted the tenant,
// soft-delete and snapshot predicates and ignored the owner the IR has carried
// since owner scoping shipped, so an owner-scoped table with a shape streamed
// the whole tenant unless the application remembered to narrow it in the stub —
// the one narrowing a stub should never have been responsible for, because the
// repository does not make anybody remember it.
func TestAnOwnerScopedShapeNarrowsToTheCaller(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "memo_shape.gen.go")

	body, ok := between(src, "func handleMemoShape(", "\n}")
	if !ok {
		t.Fatal("no handler")
	}
	collapsed := collapse(body)

	if want := `where.Eq("created_by_account_id", claims.AccountID.String())`; !strings.Contains(collapsed, collapse(want)) {
		t.Errorf("missing %s:\n%s", want, body)
	}

	// Before the stub, so the stub can still only narrow.
	owner := strings.Index(collapsed, "created_by_account_id")
	scope := strings.Index(collapsed, "sh.Memo(ctx,")
	if owner < 0 || scope < 0 || owner > scope {
		t.Error("the owner condition should be added before the application's scope runs")
	}

	// Refused rather than narrowed. An API key and a system credential both
	// have a nil identifier, and Eq against one matches nothing *silently* — a
	// subscriber handed an empty stream cannot tell it from having no rows.
	if !strings.Contains(collapsed, "if claims.AccountID == uuid.Nil {") {
		t.Errorf("a caller with no account should be refused:\n%s", body)
	}
	if refuse := strings.Index(collapsed, "uuid.Nil"); refuse < 0 || refuse > owner {
		t.Error("the refusal should come before the predicate it stands in for")
	}
}

func TestATableWithNoTenantIsStillFiltered(t *testing.T) {
	t.Parallel()

	// A shape on a table with no tenant column has no tenant condition to add,
	// which is correct — but it must not silently lose the lifecycle ones.
	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	res := doc.Resource("Lesson")
	res.Storage.Tenant = nil

	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")
	body, _ := between(src, "func handleLessonShape(", "\n}")

	if strings.Contains(body, "tenant_id") {
		t.Error("there is no tenant column to filter by")
	}
	if !strings.Contains(collapse(body), `where.IsNull("deleted_at")`) {
		t.Errorf("the lifecycle conditions should still be there:\n%s", body)
	}
}

// A shape carries every column it names to every subscriber, forever. A column
// excluded from the API has no business in a live stream either.
func TestOnlyReadableColumnsAreStreamed(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	columns, ok := between(src, "var LessonShapeColumns = []string{", "\n}")
	if !ok {
		t.Fatal("no column list")
	}
	if !strings.Contains(columns, `"title"`) {
		t.Errorf("a readable column is missing:\n%s", columns)
	}
	// row_version is excluded in the fixture's configuration.
	if strings.Contains(columns, "row_version") {
		t.Errorf("an excluded column reached the stream:\n%s", columns)
	}
}

func TestDeclaredParametersAreTyped(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	params, ok := between(src, "type LessonShapeParams struct {", "\n}")
	if !ok {
		t.Fatal("no params type")
	}
	if !strings.Contains(collapse(params), "Matchday int") {
		t.Errorf("a declared Int parameter should be an int:\n%s", params)
	}
	// An optional parameter needs a way to tell a zero value from an absent
	// one, or "matchday=0" and "no matchday" are the same request.
	if !strings.Contains(collapse(params), "HasMatchday bool") {
		t.Errorf("an optional parameter needs a presence flag:\n%s", params)
	}
	if strings.Contains(collapse(params), "HasStatus bool") {
		t.Error("a required parameter is always present and needs no flag")
	}

	parse, _ := between(src, "func parseLessonShapeParams(", "\n}")
	if !strings.Contains(collapse(parse), `httpx.QueryRequired(r, "status")`) {
		t.Errorf("a required parameter should be required:\n%s", parse)
	}
	if !strings.Contains(collapse(parse), `httpx.QueryOptional(r, "matchday")`) {
		t.Errorf("an optional one should not be:\n%s", parse)
	}
}

// Mounting the routes and registering their ending are one call, because they
// were two and the second was the one a main function forgot.
func TestRegisterMountsAndDrainsTheShapesTogether(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	register := collapse(routes(t, find(t, gentest.Run(t, servergo.New(), doc, opts()), "server.gen.go")))

	// A nil proxy mounts nothing: a project that generated its shapes and has
	// not built a front end for them yet serves no route rather than one that
	// answers.
	if !strings.Contains(register, "if h.Shapes.Proxy != nil {") {
		t.Errorf("a nil proxy should mount nothing:\n%s", register)
	}

	// The drain is registered where the proxy is named, rather than travelling
	// back to rig as a second thing to remember about the same object.
	if !strings.Contains(register, `h.Shapes.App.DrainWithin("shapes", shapesShutdown, h.Shapes.Proxy.Drain)`) {
		t.Errorf("Register should register the drain:\n%s", register)
	}

	// A caller with no App owns the ending itself, or serves no route at all,
	// which is allowed and said out loud — the same treatment Mount gives every
	// other part it cannot tell an omission from a decision about.
	if !strings.Contains(register, "} else if h.Server.Logger != nil {") {
		t.Errorf("a nil App should be reported rather than refused:\n%s", register)
	}
	if !strings.Contains(register, "live-sync shapes are mounted with no App to drain them") {
		t.Errorf("nothing is said about shapes nobody will drain:\n%s", register)
	}
}

// An admin-only shape with no way to check for an administrator must refuse
// everybody, not admit everybody.
func TestAdminShapesRefuseWhenUnconfigured(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.Resource("Lesson").Electric.Auth = ir.ElectricAuthAdmin

	artifacts := gentest.Run(t, servergo.New(), doc, opts())
	shape := collapse(find(t, artifacts, "lesson_shape.gen.go"))

	if !strings.Contains(shape, "if sh.IsAdmin == nil || !sh.IsAdmin(claims) {") {
		t.Error("an unconfigured IsAdmin should refuse rather than admit")
	}

	// A shape nobody marked administrative asks nothing of IsAdmin, so the
	// check is generated rather than paid for on every subscription.
	plain := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	if src := find(t, gentest.Run(t, servergo.New(), plain, opts()), "lesson_shape.gen.go"); strings.Contains(src, "IsAdmin") {
		t.Error("a shape that is not admin-only should not consult IsAdmin")
	}
}

// Nothing has asked for live sync, so there is nothing to mount, no struct to
// fill in, and no shape file — the same rule the auth, files and presence
// blocks follow.
func TestNothingIsEmittedWithoutAShape(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	doc.Resource("Lesson").Electric = nil

	artifacts := gentest.Run(t, servergo.New(), doc, opts())
	for _, a := range artifacts {
		if base := filepath.Base(a.Path); base == "electric.gen.go" || strings.HasSuffix(base, "_shape.gen.go") {
			t.Errorf("nothing asked for live sync and %s was written anyway", base)
		}
	}

	if src := find(t, artifacts, "server.gen.go"); strings.Contains(src, "Shapes") {
		t.Error("a project with no shapes should have no Shapes field to fill in")
	}
}

func TestUnknownShapeOptionIsRejected(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	_, err := servergo.New().Generate(t.Context(), doc, gen.Options{Raw: map[string]any{
		"package": "api", "model_import": "rigtest/model", "electric_ur": "http://electric:3000",
	}})

	if err == nil {
		t.Fatal("a mistyped option should be rejected")
	}
	if !strings.Contains(err.Error(), "electric_ur") {
		t.Errorf("the error should name the key: %v", err)
	}
}

// The trash shape is the live one inverted. A row deleted while somebody holds
// both subscriptions leaves one stream and arrives in the other, which is the
// whole reason to have it rather than to poll GET /_deleted.
func TestTheTrashShapeWantsWhatTheLiveShapeExcludes(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	body, ok := between(src, "func handleLessonDeletedShape(", "\n}")
	if !ok {
		t.Fatal("no trash handler")
	}
	collapsed := collapse(body)

	for _, want := range []string{
		`where.Eq("tenant_id", claims.TenantID.String())`,
		`where.NotNull("deleted_at")`,
		// Still the live generation of the row: the trash is what was deleted,
		// not the history of what was deleted.
		`where.EqText("version_type", "Original")`,
	} {
		if !strings.Contains(collapsed, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	if strings.Contains(collapsed, collapse(`where.IsNull("deleted_at")`)) {
		t.Errorf("the trash shape should not exclude the rows it exists to carry:\n%s", body)
	}

	// Same as everywhere else: the scope runs last and can only narrow.
	deleted := strings.Index(collapsed, "deleted_at")
	scope := strings.Index(collapsed, "sh.LessonDeleted(ctx,")
	if deleted < 0 || scope < 0 || deleted > scope {
		t.Error("the lifecycle condition should be added before the application's scope runs")
	}
}

// History is per row, matching GET /{id}/_versions and the ListSnapshots it
// calls. A table-wide stream of every version of everything would be a
// different and much larger thing than the endpoint it is named after.
func TestAHistoryShapeIsScopedToOneRow(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "lesson_shape.gen.go")

	body, ok := between(src, "func handleLessonVersionsShape(", "\n}")
	if !ok {
		t.Fatal("no history handler")
	}
	collapsed := collapse(body)

	for _, want := range []string{
		`id, err := httpx.PathUUID(r, "id")`,
		`where.Eq("tenant_id", claims.TenantID.String())`,
		`where.EqText("version_type", "Snapshot")`,
		`where.Eq("snapshot_from_lesson_id", id.String())`,
	} {
		if !strings.Contains(collapsed, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	// Never the live row. That one is what the live shape is for.
	if strings.Contains(collapsed, collapse(`where.EqText("version_type", "Original")`)) {
		t.Errorf("the history shape should not carry the live row:\n%s", body)
	}

	// The row is bound before the scope runs, so there is nothing a scope can
	// do to point the shape at somebody else's history.
	row := strings.Index(collapsed, "snapshot_from_lesson_id")
	scope := strings.Index(collapsed, "sh.LessonVersions(ctx,")
	if row < 0 || scope < 0 || row > scope {
		t.Error("the row condition should be added before the application's scope runs")
	}
	if !strings.Contains(collapsed, "sh.LessonVersions(ctx, r, claims, id, params, where)") {
		t.Errorf("the scope should receive the row it is the history of:\n%s", body)
	}
}

// Which shapes a table has is not configured. The columns decide, the same way
// they decide whether the API has a GET /_deleted — asking the schema twice is
// how the two answers get to disagree.
func TestTheExtraShapesComeFromTheColumns(t *testing.T) {
	t.Parallel()

	// Memo retires its rows and keeps no previous versions, so it has a trash
	// shape and no history one.
	doc := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	base := routes(t, find(t, gentest.Run(t, servergo.New(), doc, opts()), "server.gen.go"))

	if !strings.Contains(base, `"GET /api/v1/memo/_stream"`) {
		t.Error("the live shape should be mounted")
	}
	if !strings.Contains(base, `"GET /api/v1/memo/_deleted/_stream"`) {
		t.Errorf("a soft-deletable table should have a trash shape:\n%s", base)
	}
	if strings.Contains(base, "_versions") {
		t.Errorf("a table that keeps no versions has no history to stream:\n%s", base)
	}

	// And a table with neither has the live shape and nothing else.
	plain := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	plain.Resource("Memo").Storage.SoftDelete = nil
	only := routes(t, find(t, gentest.Run(t, servergo.New(), plain, opts()), "server.gen.go"))

	if !strings.Contains(only, `"GET /api/v1/memo/_stream"`) {
		t.Error("the live shape should still be mounted")
	}
	if strings.Contains(only, "_deleted") || strings.Contains(only, "_versions") {
		t.Errorf("nothing in the schema asks for an extra shape here:\n%s", only)
	}
}

// The owner predicate went missing from shapes once already, and it went
// missing because somebody wrote the tenant, soft-delete and snapshot ones and
// stopped. Adding two more routes is exactly the shape of that mistake, so
// every one of them is checked rather than the first.
func TestEveryShapeCarriesTheTenantAndOwnerConditions(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "ownerscope.ir.json"))
	src := find(t, gentest.Run(t, servergo.New(), doc, opts()), "memo_shape.gen.go")

	for _, handler := range []string{"handleMemoShape(", "handleMemoDeletedShape("} {
		body, ok := between(src, "func "+handler, "\n}")
		if !ok {
			t.Fatalf("no %s", handler)
		}
		collapsed := collapse(body)

		for _, want := range []string{
			`where.Eq("tenant_id", claims.TenantID.String())`,
			`where.Eq("created_by_account_id", claims.AccountID.String())`,
			"if claims.AccountID == uuid.Nil {",
		} {
			if !strings.Contains(collapsed, collapse(want)) {
				t.Errorf("%s is missing %s:\n%s", handler, want, body)
			}
		}
	}
}

// The columns decide, and the resource's operations deliberately do not — the
// API needs List for a GET /_deleted and Get for a GET /{id}/_versions, but a
// shape is its own read surface. rig_notification_recipient is the case that
// settles it: an unexposed table with no operations at all, subscribed to
// through a shape because there is no endpoint to read it with. Reading the
// operations here would give it a live shape and no trash.
func TestTheExtraShapesIgnoreTheOperations(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", "notify.ir.json"))
	if ops := doc.Resource("RigNotificationRecipient").Operations; len(ops) != 0 {
		t.Fatalf("this test needs a resource that exposes no endpoints, got %v", ops)
	}

	body := routes(t, find(t, gentest.Run(t, servergo.New(), doc, opts()), "server.gen.go"))

	for _, want := range []string{
		`"GET /api/v1/rig_notification_recipient/_stream"`,
		`"GET /api/v1/rig_notification_recipient/_deleted/_stream"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}
}

// A project that regenerates gains these routes without a line of its own code
// changing, and a new field on a struct it fills in by name is not a compile
// error. Defaulting them to no scope would mean whatever narrowing its live
// shape had — the membership check rig cannot express as a column — stops
// applying, silently, on two routes that carry the same table's rows.
func TestADerivedShapeInheritsTheLiveScope(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servergo.New(), doc, opts())
	body := collapse(routes(t, find(t, artifacts, "server.gen.go")))

	for _, want := range []string{
		"if h.Shapes.LessonDeleted == nil { h.Shapes.LessonDeleted = LessonDeletedScope(h.Shapes.Lesson) }",
		"if h.Shapes.LessonVersions == nil { h.Shapes.LessonVersions = versionsFromLiveLesson(h.Shapes.Lesson) }",
	} {
		if !strings.Contains(body, collapse(want)) {
			t.Errorf("missing %s:\n%s", want, body)
		}
	}

	// Before the routes are mounted, because a handler is given the struct as
	// it stands and cannot be told about a scope later.
	fallback := strings.Index(body, "h.Shapes.LessonDeleted =")
	mount := strings.Index(body, `mux.HandleFunc("GET /api/v1/lesson/_stream"`)
	if fallback < 0 || mount < 0 || fallback > mount {
		t.Error("the fallback should be wired before the routes are mounted")
	}

	// And a scope nobody wrote stays nil rather than becoming a closure that
	// calls one, which would panic on the first subscription.
	shape := collapse(find(t, artifacts, "lesson_shape.gen.go"))
	if !strings.Contains(shape, collapse("if live == nil { return nil }")) {
		t.Errorf("versionsFromLiveLesson should pass nil through:\n%s", shape)
	}
}

// A stub is written once and then belongs to the developer, so a shape that
// shared one with another would have no way to be scoped separately — and a
// project that regenerates after these shapes existed would find its own file
// rewritten, which is the one thing CreateOnce promises never happens.
func TestEachShapeGetsItsOwnStub(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	artifacts := gentest.Run(t, servergo.New(), doc, stubOpts())

	stubs := map[string]string{}
	for _, a := range artifacts {
		if a.Mode == gen.CreateOnce {
			stubs[filepath.Base(a.Path)] = string(a.Content)
		}
	}

	for file, want := range map[string]string{
		"lesson_shape.go":          ".LessonScope = Shape",
		"lesson_deleted_shape.go":  ".LessonDeletedScope = DeletedShape",
		"lesson_versions_shape.go": ".LessonVersionsScope = VersionsShape",
	} {
		src, ok := stubs[file]
		if !ok {
			t.Errorf("no stub named %s", file)
			continue
		}
		if !strings.Contains(src, want) {
			t.Errorf("%s should assert %q:\n%s", file, want, src)
		}
	}
}

// The stub names a scope type and a params struct the API package declares, and
// nothing but a compile proves the two agree — which is most of what folding
// these files into that package was for.
func TestShapeStubsCompileAgainstTheAPIPackage(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))

	api := gentest.Run(t, servicego.New(), doc, gen.Options{Raw: map[string]any{
		"package": "api", "model_import": "rigtest/model", "store_import": "rigtest/store",
	}})

	var stubs []gen.Artifact
	for _, a := range gentest.Run(t, servergo.New(), doc, stubOpts()) {
		if a.Mode == gen.CreateOnce {
			stubs = append(stubs, a)
			continue
		}
		api = append(api, a)
	}
	if len(stubs) == 0 {
		t.Fatal("no scoping stub was emitted")
	}

	gentest.MustCompileAll(t,
		gentest.Package{
			Dir: "model",
			Artifacts: gentest.Run(t, modelgo.New(), doc,
				gen.Options{Raw: map[string]any{"package": "model"}}),
		},
		gentest.Package{
			Dir: "store",
			Artifacts: gentest.Run(t, persistgo.New(), doc, gen.Options{Raw: map[string]any{
				"package": "store", "model_import": "rigtest/model",
			}}),
		},
		gentest.Package{Dir: "api", Artifacts: api},
		gentest.Package{Dir: "shapes/lesson", Artifacts: stubs},
	)
}

// http.ServeMux panics on conflicting patterns, and it does it at registration
// — so a shape whose route overlaps another's is not a bad response, it is an
// application that will not start. The generated patterns are mounted here for
// real rather than reasoned about, because "/x/_deleted" and "/x/{id}/_thing"
// are exactly the pair where the reasoning is easy to get wrong.
func TestTheGeneratedShapeRoutesMountOnARealMux(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	body := routes(t, find(t, gentest.Run(t, servergo.New(), doc, opts()), "server.gen.go"))

	patterns := mountedShapes(body)
	if len(patterns) != 3 {
		t.Fatalf("got %d routes %v, want the live, trash and history shapes", len(patterns), patterns)
	}

	mux := http.NewServeMux()
	for _, p := range patterns {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, p)
		})
	}

	// And each one answers for itself: registering without a panic proves only
	// that the patterns can coexist, not that they mean different things.
	for path, want := range map[string]string{
		"/api/v1/lesson/_stream":                                    "GET /api/v1/lesson/_stream",
		"/api/v1/lesson/_deleted/_stream":                           "GET /api/v1/lesson/_deleted/_stream",
		"/api/v1/lesson/" + uuid.NewString() + "/_versions/_stream": "GET /api/v1/lesson/{id}/_versions/_stream",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))

		if got := rec.Body.String(); got != want {
			t.Errorf("%s dispatched to %q, want %q", path, got, want)
		}
	}
}

// mountedShapes is the pattern of every mux.HandleFunc in a Register body. The
// resource routes are mounted by a registerX call rather than here, so what
// this finds is the shapes.
func mountedShapes(body string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`mux\.HandleFunc\("([^"]+)"`).FindAllStringSubmatch(body, -1) {
		out = append(out, m[1])
	}
	return out
}

// routes is Register's body: what it mounts and nothing else. The doc comments
// around it talk about _deleted in prose, and a test that grepped the whole
// file would find that and call it a route.
func routes(t *testing.T, src string) string {
	t.Helper()
	body, ok := between(src, "func Register(", "\n}")
	if !ok {
		t.Fatal("no Register function")
	}
	return body
}

// A stub names the scope type this package declares, so it needs a path to it.
// Emitting one without would write a file that does not build — and CreateOnce
// means the run after it would leave that file exactly as it is.
func TestStubDirWithoutAnAPIImportIsRefused(t *testing.T) {
	t.Parallel()

	doc := gentest.LoadDocument(t, filepath.Join("testdata", fixture))
	_, err := servergo.New().Generate(t.Context(), doc, gen.Options{Raw: map[string]any{
		"package": "api", "model_import": "rigtest/model", "stub_dir": "shapes/{table}",
	}})

	if err == nil {
		t.Fatal("a stub with nowhere to import its scope type from should be refused")
	}
	if !strings.Contains(err.Error(), "api_import") {
		t.Errorf("the error should name the missing option: %v", err)
	}
}

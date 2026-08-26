package genutil_test

import (
	"testing"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/pkg/ir"
)

// The two exclusions are the whole content of FieldsTypeName, and both SDKs
// depend on them landing the same way: a name emitted here with nothing on the
// server to fill it in is a shape that decodes to an empty one rather than
// failing.
func TestFieldsTypeNameLeavesOutTheBodiesNothingValidates(t *testing.T) {
	t.Parallel()

	res := &ir.Resource{Name: "Todo"}
	body := []ir.Field{{Name: "Title", Wire: "title"}}

	for _, tc := range []struct {
		name string
		ep   ir.Endpoint
		want string
	}{
		{
			name: "a hand-written body gets a shape",
			ep:   ir.Endpoint{Name: "Publish", Request: ir.EndpointRequest{BodyParams: body}},
			want: "TodoPublishFields",
		},
		{
			name: "a create's body is the model's input, which is validated",
			ep: ir.Endpoint{
				Name:    ir.OpCreate,
				Impl:    ir.EndpointImpl{Kind: ir.EndpointGenerated},
				Request: ir.EndpointRequest{BodyParams: body},
			},
			want: "TodoCreateFields",
		},
		{
			name: "a search is generated and is refused in some other shape",
			ep: ir.Endpoint{
				Name:    "Search",
				Impl:    ir.EndpointImpl{Kind: ir.EndpointGenerated},
				Request: ir.EndpointRequest{BodyParams: body},
			},
			want: "",
		},
		{
			name: "a named body is shared, so its failure shape would have to be",
			ep: ir.Endpoint{
				Name:    "Import",
				Request: ir.EndpointRequest{BodyObject: "TodoImport", BodyParams: body},
			},
			want: "",
		},
		{
			name: "no body, nothing to be wrong about",
			ep:   ir.Endpoint{Name: "Get"},
			want: "",
		},
	} {
		if got := genutil.FieldsTypeName(res, &tc.ep); got != tc.want {
			t.Errorf("%s: FieldsTypeName = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The claimed seed is the one thing the two SDK generators answer differently —
// the Go client declares Pagination in its base file and the TypeScript client
// does not — so it is a parameter, and passing nil has to mean "claim nothing".
func TestUnclaimedObjectsHonoursTheSeedAndTheResources(t *testing.T) {
	t.Parallel()

	doc := &ir.Document{API: ir.API{
		Resources: []ir.Resource{
			{Name: "Todo", Endpoints: []ir.Endpoint{{Name: "List"}}},
			{Name: "Session", Unexposed: true, Endpoints: []ir.Endpoint{{Name: "List"}}},
		},
		Objects: []ir.Object{
			{Name: "Error"},
			{Name: "Pagination"},
			{Name: "Todo"},
			{Name: "TodoListResponse"},
			{Name: "TodoFilter", Origin: ir.OriginFilter},
			{Name: "ScoringPayload"},
			{Name: "Session"},
		},
	}}
	reachable := map[string]bool{
		"Error": true, "Pagination": true, "Todo": true, "TodoListResponse": true,
		"TodoFilter": true, "ScoringPayload": true,
		// Session is declared and unreachable: it must never be emitted.
	}

	names := func(objs []*ir.Object) []string {
		var out []string
		for _, o := range objs {
			out = append(out, o.Name)
		}
		return out
	}

	got := names(genutil.UnclaimedObjects(doc, reachable,
		map[string]bool{"Error": true, "Pagination": true}))
	if len(got) != 1 || got[0] != "ScoringPayload" {
		t.Errorf("with Error and Pagination claimed: got %v, want [ScoringPayload]", got)
	}

	got = names(genutil.UnclaimedObjects(doc, reachable, map[string]bool{"Error": true}))
	if len(got) != 2 || got[0] != "Pagination" || got[1] != "ScoringPayload" {
		t.Errorf("with only Error claimed: got %v, want [Pagination ScoringPayload]", got)
	}

	got = names(genutil.UnclaimedObjects(doc, reachable, nil))
	if len(got) != 3 || got[0] != "Error" {
		t.Errorf("with nothing claimed: got %v, want Error first of three", got)
	}
}

// A resource that is unexposed, or has no endpoints, has no client surface —
// and both SDKs have to agree about that or one of them emits a client the
// other does not.
func TestExposedSkipsBothWaysOfHavingNoSurface(t *testing.T) {
	t.Parallel()

	doc := &ir.Document{API: ir.API{Resources: []ir.Resource{
		{Name: "Todo", Endpoints: []ir.Endpoint{{Name: "List"}}},
		{Name: "Session", Unexposed: true, Endpoints: []ir.Endpoint{{Name: "List"}}},
		{Name: "AuthLogEntry"},
	}}}

	got := genutil.Exposed(doc)
	if len(got) != 1 || got[0].Name != "Todo" {
		t.Fatalf("Exposed = %v, want [Todo]", got)
	}
}

func TestRoutePathDropsTheMethod(t *testing.T) {
	t.Parallel()

	for pattern, want := range map[string]string{
		"GET /v1/todos/{id}":     "/v1/todos/{id}",
		"QUERY /v1/todos/search": "/v1/todos/search",
		"/v1/todos":              "/v1/todos", // no method: returned whole
	} {
		if got := genutil.RoutePath(pattern); got != want {
			t.Errorf("RoutePath(%q) = %q, want %q", pattern, got, want)
		}
	}
}

// The filter member is found in the body rather than assumed, so a rename in
// the compiler cannot leave a client sending the wrong key.
func TestSearchFilterFieldIsFoundInTheBody(t *testing.T) {
	t.Parallel()

	ep := &ir.Endpoint{Request: ir.EndpointRequest{BodyParams: []ir.Field{
		{Name: "Limit", TypeKind: ir.TypeKindPrimitive, Type: ir.TypeInt},
		{Name: "Where", TypeKind: ir.TypeKindObject, Type: "TodoFilter"},
	}}}

	got, ok := genutil.SearchFilterField(ep)
	if !ok || got.Name != "Where" {
		t.Fatalf("SearchFilterField = %v, %v; want the Where member", got.Name, ok)
	}

	bare := &ir.Endpoint{Request: ir.EndpointRequest{BodyParams: []ir.Field{
		{Name: "Limit", TypeKind: ir.TypeKindPrimitive, Type: ir.TypeInt},
	}}}
	if _, ok := genutil.SearchFilterField(bare); ok {
		t.Error("a body with no filter member reported one")
	}
}

// The walk's job is to stop: a document with a cycle in it, and an object that
// nothing reaches, are both ordinary.
func TestWalkFollowsFieldsAndTerminatesOnACycle(t *testing.T) {
	t.Parallel()

	doc := &ir.Document{API: ir.API{Objects: []ir.Object{
		{Name: "Todo", Fields: []ir.Field{
			{Name: "Status", TypeKind: ir.TypeKindEnum, Type: "TodoStatus"},
			{Name: "Parent", TypeKind: ir.TypeKindObject, Type: "Todo"},
			{Name: "Title", TypeKind: ir.TypeKindPrimitive, Type: ir.TypeString},
		}},
		{Name: "AuthLogEntry"},
	}}}

	w := genutil.NewWalk(doc)
	w.Follow("Todo")

	seen := w.Seen()
	for _, want := range []string{"Todo", "TodoStatus"} {
		if !seen[want] {
			t.Errorf("Walk did not reach %s", want)
		}
	}
	if seen["AuthLogEntry"] {
		t.Error("Walk reached AuthLogEntry, which nothing references")
	}
}

// Headers are walked in every output at once. No compiled document fills them
// yet; the first one to carry an enum should not reach it in two generators out
// of three.
func TestWalkEndpointReachesHeadersAndResponses(t *testing.T) {
	t.Parallel()

	doc := &ir.Document{API: ir.API{Objects: []ir.Object{{Name: "Todo"}}}}
	ep := &ir.Endpoint{
		Request: ir.EndpointRequest{
			Headers: []ir.Field{{Name: "Scope", TypeKind: ir.TypeKindEnum, Type: "Scope"}},
		},
		Responses: []ir.EndpointResponse{{
			BodyObject: "Todo",
			Headers:    []ir.Field{{Name: "Stage", TypeKind: ir.TypeKindEnum, Type: "Stage"}},
		}},
	}

	w := genutil.NewWalk(doc)
	w.Endpoint(ep)

	seen := w.Seen()
	for _, want := range []string{"Scope", "Todo", "Stage"} {
		if !seen[want] {
			t.Errorf("Walk.Endpoint did not reach %s", want)
		}
	}
}

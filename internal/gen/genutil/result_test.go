package genutil_test

import (
	"testing"

	"github.com/simonjanss/rig/internal/gen/genutil"
	"github.com/simonjanss/rig/pkg/ir"
)

// Which of the two ways a configuration can describe a success body ends up
// with which name. Every generator asks this one function, and before it existed
// each of them answered for itself by reading BodyObject alone — so a body
// spelled out reached the specification and no SDK.
func TestResultShapeNameCoversBothWaysOfDescribingASuccessBody(t *testing.T) {
	t.Parallel()

	res := &ir.Resource{Name: "Issue"}
	fields := []ir.Field{{Name: "Numbers", Wire: "numbers"}}

	for _, tc := range []struct {
		name string
		ep   ir.Endpoint
		want string
	}{
		{
			name: "a named object keeps its own name",
			ep: ir.Endpoint{Name: "Report", Responses: []ir.EndpointResponse{
				{StatusCode: 200, BodyObject: "Issue"},
			}},
			want: "Issue",
		},
		{
			name: "fields spelled out get a name rig makes up",
			ep: ir.Endpoint{Name: "Report", Responses: []ir.EndpointResponse{
				{StatusCode: 200, BodyFields: fields},
			}},
			want: "IssueReportResult",
		},
		{
			name: "a named object wins over fields beside it",
			ep: ir.Endpoint{Name: "Report", Responses: []ir.EndpointResponse{
				{StatusCode: 200, BodyObject: "Issue", BodyFields: fields},
			}},
			want: "Issue",
		},
		{
			name: "a success with no body is not a shape",
			ep: ir.Endpoint{Name: "Report", Responses: []ir.EndpointResponse{
				{StatusCode: 204},
			}},
			want: "",
		},
		{
			name: "only the success counts, not the refusals",
			ep: ir.Endpoint{Name: "Report", Responses: []ir.EndpointResponse{
				{StatusCode: 204},
				{StatusCode: 422, BodyObject: "Error"},
			}},
			want: "",
		},
		{
			name: "no responses at all",
			ep:   ir.Endpoint{Name: "Report"},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := genutil.ResultShapeName(res, &tc.ep); got != tc.want {
				t.Errorf("ResultShapeName = %q, want %q", got, tc.want)
			}
		})
	}
}

// InlineResultFields is what separates the shape somebody else declares from the
// one the naming generator also has to emit. Reading it wrongly is either a type
// declared twice or a type declared nowhere.
func TestInlineResultFieldsAreOnlyTheOnesNobodyElseDeclares(t *testing.T) {
	t.Parallel()

	fields := []ir.Field{{Name: "Numbers", Wire: "numbers"}}

	spelled := ir.Endpoint{Responses: []ir.EndpointResponse{
		{StatusCode: 200, BodyFields: fields},
	}}
	if got := genutil.InlineResultFields(&spelled); len(got) != 1 {
		t.Errorf("a body spelled out has fields to emit, got %d", len(got))
	}

	named := ir.Endpoint{Responses: []ir.EndpointResponse{
		{StatusCode: 200, BodyObject: "Issue", BodyFields: fields},
	}}
	if got := genutil.InlineResultFields(&named); got != nil {
		t.Errorf("a named body is declared elsewhere, got %v", got)
	}

	none := ir.Endpoint{Responses: []ir.EndpointResponse{{StatusCode: 204}}}
	if got := genutil.InlineResultFields(&none); got != nil {
		t.Errorf("no body, nothing to emit, got %v", got)
	}
}

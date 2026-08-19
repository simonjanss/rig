package compile_test

import (
	"net/http"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// TestErrorCodesMatchRigerr is the guard on a fact two packages hold and neither
// owns. `compile.ErrorCodes` is what every generated enum, every OpenAPI schema
// and every TypeScript union is rendered from; `rigerr` is what the handler
// actually returns. A code in one and not the other is a client switching on a
// value the server never sends, or a server sending one the client cannot name —
// and nothing else in the build would say so.
//
// The set is closed on purpose, which is what makes adding to it worth a commit
// of its own: the day it is baked into a generated client, widening it is a
// breaking change rather than golden churn.
func TestErrorCodesMatchRigerr(t *testing.T) {
	for _, c := range compile.ErrorCodes {
		if got := rigerr.Code(c.Name).HTTPStatus(); got != c.Status {
			t.Errorf("%s: compile says %d, rigerr says %d", c.Name, c.Status, got)
		}
	}
}

// TestErrorCodesCoverRigerr catches the other direction, which the status
// comparison cannot: `HTTPStatus` answers 500 for a code it has never heard of,
// so a code catalogued here and missing from `rigerr` would pass the test above
// as long as somebody wrote 500 beside it.
func TestErrorCodesCoverRigerr(t *testing.T) {
	catalogued := make(map[rigerr.Code]bool, len(compile.ErrorCodes))
	for _, c := range compile.ErrorCodes {
		catalogued[rigerr.Code(c.Name)] = true
	}

	// Every code rigerr defines. Listed by hand because Go cannot enumerate the
	// members of a string type, which is the same reason this test exists.
	declared := []rigerr.Code{
		rigerr.CodeBadRequest,
		rigerr.CodeUnauthorized,
		rigerr.CodeForbidden,
		rigerr.CodeNotFound,
		rigerr.CodeConflict,
		rigerr.CodeUnprocessableEntity,
		rigerr.CodeRateLimited,
		rigerr.CodeTooLarge,
		rigerr.CodeUnsupportedMediaType,
		rigerr.CodeUpgradeRequired,
		rigerr.CodeInternal,
	}

	for _, code := range declared {
		if !catalogued[code] {
			t.Errorf("rigerr.%s is not in compile.ErrorCodes, so no generated client knows about it", code)
		}
	}
	if len(compile.ErrorCodes) != len(declared) {
		t.Errorf("compile.ErrorCodes has %d entries, rigerr declares %d",
			len(compile.ErrorCodes), len(declared))
	}
}

// TestErrorCodeStatusesAreReal keeps a typo in the catalogue from becoming a
// status no client library recognises.
func TestErrorCodeStatusesAreReal(t *testing.T) {
	for _, c := range compile.ErrorCodes {
		if http.StatusText(c.Status) == "" {
			t.Errorf("%s: %d is not a registered HTTP status", c.Name, c.Status)
		}
	}
}

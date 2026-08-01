package rigerr_test

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/simonjanss/rig/runtime/rigerr"
)

func TestStatuses(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		err  *rigerr.Error
		want int
	}{
		{rigerr.BadRequest("x"), http.StatusBadRequest},
		{rigerr.Unauthorized("x"), http.StatusUnauthorized},
		{rigerr.Forbidden("x"), http.StatusForbidden},
		{rigerr.NotFound("x"), http.StatusNotFound},
		{rigerr.Conflict("x"), http.StatusConflict},
		{rigerr.Invalid("x"), http.StatusUnprocessableEntity},
		{rigerr.RateLimited("x"), http.StatusTooManyRequests},
		{rigerr.Internal(nil, "x"), http.StatusInternalServerError},
	} {
		if got := tc.err.HTTPStatus(); got != tc.want {
			t.Errorf("%s = %d, want %d", tc.err.Code, got, tc.want)
		}
	}
}

// An error that never passed through this package is a bug rather than a
// client mistake, and reporting it as anything else tells the caller to fix
// something that is not theirs.
func TestUnknownErrorIsInternal(t *testing.T) {
	t.Parallel()

	if got := rigerr.CodeOf(errors.New("boom")); got != rigerr.CodeInternal {
		t.Errorf("code = %q, want Internal", got)
	}
	if got := rigerr.StatusOf(errors.New("boom")); got != 500 {
		t.Errorf("status = %d, want 500", got)
	}
}

func TestWrappingIsTransparent(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection reset")
	err := rigerr.Internal(cause, "read lesson")

	if !errors.Is(err, cause) {
		t.Error("the cause should be reachable through errors.Is")
	}
	if !rigerr.Is(err, rigerr.CodeInternal) {
		t.Error("the code should survive")
	}

	// And through another layer of wrapping.
	outer := fmt.Errorf("handling request: %w", err)
	if !rigerr.Is(outer, rigerr.CodeInternal) {
		t.Error("the code should survive being wrapped again")
	}
}

func TestFormatting(t *testing.T) {
	t.Parallel()

	err := rigerr.NotFound("no lesson with id %s", "abc")
	if err.Message != "no lesson with id abc" {
		t.Errorf("message = %q", err.Message)
	}
}

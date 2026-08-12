package rigerr_test

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
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

// An error that names its own code keeps it. This is what lets a generated
// validation failure be returned as itself — with its per-field detail intact
// — instead of being flattened into prose.
func TestCodeOfAskstheError(t *testing.T) {
	t.Parallel()

	err := selfCoded{}
	if got := rigerr.CodeOf(err); got != rigerr.CodeUnprocessableEntity {
		t.Errorf("CodeOf = %q, want %q", got, rigerr.CodeUnprocessableEntity)
	}
	if got := rigerr.StatusOf(err); got != http.StatusUnprocessableEntity {
		t.Errorf("StatusOf = %d, want 422", got)
	}

	fields, ok := rigerr.FieldsOf(err)
	if !ok {
		t.Fatal("the detail should be reachable")
	}
	if fields != "the detail" {
		t.Errorf("ErrorFields = %v", fields)
	}

	if _, ok := rigerr.FieldsOf(rigerr.Invalid("no detail")); ok {
		t.Error("an error with no per-field detail should say so")
	}
}

type selfCoded struct{}

func (selfCoded) Error() string          { return "invalid" }
func (selfCoded) ErrorCode() rigerr.Code { return rigerr.CodeUnprocessableEntity }
func (selfCoded) ErrorFields() any       { return "the detail" }

// Wrap keeps a code that was already decided and invents Internal for one that
// was not. The distinction is whose fault the failure is: a rule that refused
// with a Conflict is still a conflict once the layer above says which rule it
// was, and a bare error nobody classified is the server's problem until
// somebody classifies it.
func TestWrapKeepsTheCodeOrDecidesInternal(t *testing.T) {
	t.Parallel()

	cause := errors.New("connection refused")

	for _, tc := range []struct {
		name string
		err  error
		want rigerr.Code
	}{
		{"nothing said", cause, rigerr.CodeInternal},
		{"a conflict stays one", rigerr.Conflict("already published"), rigerr.CodeConflict},
		{"not found stays one", rigerr.NotFound("no such lesson"), rigerr.CodeNotFound},
		{"an error that names its own code", selfCoded{}, rigerr.CodeUnprocessableEntity},
	} {
		got := rigerr.Wrap(tc.err, "validate title")

		if code := rigerr.CodeOf(got); code != tc.want {
			t.Errorf("%s: code = %q, want %q", tc.name, code, tc.want)
		}
		if !errors.Is(got, tc.err) {
			t.Errorf("%s: the cause should survive wrapping", tc.name)
		}
		if !strings.Contains(got.Error(), "validate title") {
			t.Errorf("%s: the context should be in the message: %v", tc.name, got)
		}
	}

	if rigerr.Wrap(nil, "nothing happened") != nil {
		t.Error("wrapping nothing should be nothing")
	}
}

// A rule returns a field error to say the input is wrong, and anything else to
// say it could not decide. Telling them apart is the whole of that boundary.
func TestAsFieldError(t *testing.T) {
	t.Parallel()

	field := rigerr.NewFieldError(rigerr.FieldCodeTooLong, "cannot be longer than 5 characters")

	got, ok := rigerr.AsFieldError(field)
	if !ok {
		t.Fatal("a field error should be recognised as one")
	}
	if got.Code != rigerr.FieldCodeTooLong || got.Message == "" {
		t.Errorf("got %+v", got)
	}

	// Still reachable once something has wrapped it.
	if _, ok := rigerr.AsFieldError(fmt.Errorf("checking the title: %w", field)); !ok {
		t.Error("wrapping should not hide it")
	}

	if _, ok := rigerr.AsFieldError(errors.New("connection refused")); ok {
		t.Error("an ordinary error is not about a field")
	}
	if _, ok := rigerr.AsFieldError(nil); ok {
		t.Error("nothing is not a field error")
	}
}

// A field error that escapes on its own is a bug in the service: it says what
// was wrong without saying what it was about, and no client can act on that.
// Answering 422 with an empty body would blame the caller for it.
func TestALooseFieldErrorIsInternal(t *testing.T) {
	t.Parallel()

	loose := rigerr.NewFieldError(rigerr.FieldCodeCannotBeEmpty, "cannot be empty")

	if got := rigerr.CodeOf(loose); got != rigerr.CodeInternal {
		t.Errorf("CodeOf = %q, want Internal", got)
	}
	if got := rigerr.StatusOf(loose); got != http.StatusInternalServerError {
		t.Errorf("StatusOf = %d, want 500", got)
	}

	// Wrapping one keeps it a bug rather than promoting it.
	if got := rigerr.CodeOf(rigerr.Wrap(loose, "validate title")); got != rigerr.CodeInternal {
		t.Errorf("wrapped code = %q, want Internal", got)
	}

	// Attached to the field it belongs to, it is the caller's problem again.
	// That is what the generated input errors do, and they say so themselves.
	if got := rigerr.CodeOf(selfCoded{}); got != rigerr.CodeUnprocessableEntity {
		t.Errorf("an input error should still be 422, got %q", got)
	}
}

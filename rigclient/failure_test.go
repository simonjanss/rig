// The typed failure: what a generated method returns when the server refuses it.
//
// The shapes here stand in for what the go-client generator emits — a struct
// with one *rigerr.FieldError per member of the body — so these cases exercise
// the same thing a real client does without needing one.
package rigclient_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// todoCreateFields is what the generator emits beside a TodoCreateInput.
type todoCreateFields struct {
	Title  *rigerr.FieldError `json:"title,omitempty"`
	Notes  *rigerr.FieldError `json:"notes,omitempty"`
	Entity *rigerr.FieldError `json:"entity,omitempty"`
}

// todoCreateError is the alias the generator emits beside the shape.
type todoCreateError = rigclient.Failure[todoCreateFields]

// refuses answers every request with the given status and body.
func refuses(status int, body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
}

const invalidTitle = `{
	"code": "UnprocessableEntity",
	"message": "todo is not valid: title CannotBeEmpty: cannot be empty",
	"requestId": "req-7",
	"fields": {"title": {"code": "CannotBeEmpty", "message": "cannot be empty"}}
}`

func TestARefusalArrivesAsTheCallsOwnError(t *testing.T) {
	rt := newClient(t, refuses(http.StatusUnprocessableEntity, invalidTitle), rigclient.Config{})

	_, err := rigclient.DoTyped[todo, todoCreateFields](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: map[string]any{},
	})

	var refused *todoCreateError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want the call's own error type", err)
	}
	if refused.Fields.Title == nil {
		t.Fatal("title carried no failure, and it is the one that failed")
	}
	if got := refused.Fields.Title.Code; got != rigerr.FieldCodeCannotBeEmpty {
		t.Errorf("title code = %q, want CannotBeEmpty", got)
	}
	if refused.Fields.Notes != nil {
		t.Errorf("notes = %+v, want nothing: it was not what failed", refused.Fields.Notes)
	}

	// The envelope is on the same value, which is the point of it being one.
	if refused.Code != rigerr.CodeUnprocessableEntity {
		t.Errorf("code = %q, want UnprocessableEntity", refused.Code)
	}
	if refused.Status != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", refused.Status)
	}
	if refused.RequestID != "req-7" {
		t.Errorf("request id = %q, want req-7", refused.RequestID)
	}
	if !strings.Contains(refused.Error(), "cannot be empty") {
		t.Errorf("Error() = %q, want the server's message in it", refused.Error())
	}
}

// The typed error is an addition, not a replacement: everything written against
// *rigclient.Error before it existed has to keep answering.
func TestATypedFailureIsStillARefusal(t *testing.T) {
	rt := newClient(t, refuses(http.StatusUnprocessableEntity, invalidTitle), rigclient.Config{})

	_, err := rigclient.DoTyped[todo, todoCreateFields](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: map[string]any{},
	})

	var e *rigclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want errors.As to still reach *rigclient.Error", err)
	}
	if !rigclient.IsInvalid(err) {
		t.Error("IsInvalid says no")
	}
	if got := rigclient.CodeOf(err); got != rigerr.CodeUnprocessableEntity {
		t.Errorf("CodeOf = %q, want UnprocessableEntity", got)
	}

	fields, ok := rigclient.FieldsAs[todoCreateFields](err)
	if !ok || fields.Title == nil {
		t.Error("FieldsAs no longer reaches the detail through the typed error")
	}
}

// A method's error type does not change with the status. A 404 from a call that
// has a body is still that call's error, with nothing in Fields — otherwise a
// caller would have to match twice to find out which failure it got.
func TestAFailureWithNoFieldsIsStillTyped(t *testing.T) {
	rt := newClient(t, refuses(http.StatusNotFound,
		`{"code":"NotFound","message":"no such todo"}`), rigclient.Config{})

	_, err := rigclient.DoTyped[todo, todoCreateFields](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: map[string]any{},
	})

	var refused *todoCreateError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want the call's own error type", err)
	}
	if refused.Fields.Title != nil || refused.Fields.Entity != nil {
		t.Errorf("fields = %+v, want the zero value", refused.Fields)
	}
	if !rigclient.IsNotFound(err) {
		t.Error("IsNotFound says no")
	}
}

// A request that never reached the server has no envelope to type. Wrapping it
// anyway would produce a failure with no refusal in it, and the first thing to
// print it would panic.
func TestSomethingThatNeverReachedTheServerIsNotTyped(t *testing.T) {
	rt, err := rigclient.New(rigclient.Config{BaseURL: "http://127.0.0.1:1"},
		rigclient.API{BasePath: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = rigclient.DoTyped[todo, todoCreateFields](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: map[string]any{},
	})
	if err == nil {
		t.Fatal("a call to a closed port succeeded")
	}

	var refused *todoCreateError
	if errors.As(err, &refused) {
		t.Errorf("err = %v, want it left alone: there was no envelope to type", err)
	}
	_ = fmt.Sprint(err) // the panic this is guarding against
}

// Skew between a client and a server loses the detail. It must not also lose the
// code and the message, which is what a strict decode would do.
func TestFieldsThatDoNotFitKeepTheCodeAndTheMessage(t *testing.T) {
	rt := newClient(t, refuses(http.StatusUnprocessableEntity,
		`{"code":"UnprocessableEntity","message":"not valid","fields":["title"]}`),
		rigclient.Config{})

	_, err := rigclient.DoTyped[todo, todoCreateFields](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: map[string]any{},
	})

	var refused *todoCreateError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want the call's own error type", err)
	}
	if refused.Fields.Title != nil {
		t.Errorf("title = %+v, want nothing: the body did not fit the shape",
			refused.Fields.Title)
	}
	if refused.Code != rigerr.CodeUnprocessableEntity || refused.Message != "not valid" {
		t.Errorf("code = %q, message = %q, want both kept", refused.Code, refused.Message)
	}
	if len(refused.Refusal.Fields) == 0 {
		t.Error("the raw bytes are gone, and they are what would explain the skew")
	}
}

// What a caller stubbing a client writes. It has no cause, so Unwrap has to fall
// back to the refusal or the value answers nothing.
func TestAFailureBuiltByHandIsStillARefusal(t *testing.T) {
	err := error(&todoCreateError{
		Refusal: &rigclient.Error{Status: http.StatusNotFound, Code: rigerr.CodeNotFound},
		Fields:  todoCreateFields{},
	})

	if !rigclient.IsNotFound(err) {
		t.Error("IsNotFound says no")
	}
	var e *rigclient.Error
	if !errors.As(err, &e) {
		t.Error("errors.As does not reach the refusal")
	}
}

// The 401 rewind joins two facts, and typing the failure must not drop one.
func TestATypedUploadThatCannotSeekKeepsBothFacts(t *testing.T) {
	cred := &refresher{token: "stale"}
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"Unauthorized","message":"expired"}`))
	}), rigclient.Config{Credential: cred})

	err := rigclient.DoNoContentTyped[todoCreateFields](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile", rigclient.Upload{
				Name: "c.png", ContentType: "image/png",
				Body: struct{ io.Reader }{strings.NewReader("x")},
			}),
		}},
	})

	var refused *todoCreateError
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want the call's own error type", err)
	}
	if !errors.Is(err, rigclient.ErrCannotRetry) {
		t.Error("the failure no longer answers ErrCannotRetry, which typing it dropped")
	}
	if !rigclient.IsUnauthorized(err) {
		t.Error("IsUnauthorized says no")
	}
}

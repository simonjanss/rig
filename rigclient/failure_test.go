// Reading a refusal back: the envelope and the per-field detail in one value.
//
// The shapes here stand in for what the go-client generator emits — a struct
// with one *rigerr.FieldError per member of the body, and a function per call
// that reads it — so these cases exercise the same thing a real client does
// without needing one.
package rigclient_test

import (
	"encoding/json"
	"errors"
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

// todoCreateError is the function the generator emits beside the shape: one
// line, and the shape is not the caller's to name.
func todoCreateError(err error) (*rigclient.Failure[todoCreateFields], bool) {
	return rigclient.As[todoCreateFields](err)
}

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

// create is the call the rest of this file refuses in one way or another.
func create(t *testing.T, h http.Handler) error {
	t.Helper()

	rt := newClient(t, h, rigclient.Config{})
	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: map[string]any{},
	})
	return err
}

func TestARefusalIsReadBackAsTheCallsOwnShape(t *testing.T) {
	err := create(t, refuses(http.StatusUnprocessableEntity, invalidTitle))

	refused, ok := todoCreateError(err)
	if !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if refused.Fields == nil {
		t.Fatal("no fields, and the server sent some")
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

// Nothing about the error a call returns changed, so everything written against
// it keeps answering. This reads it a second way rather than instead.
func TestTheErrorItselfIsUntouched(t *testing.T) {
	err := create(t, refuses(http.StatusUnprocessableEntity, invalidTitle))

	var e *rigclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *rigclient.Error", err)
	}
	if !rigclient.IsInvalid(err) {
		t.Error("IsInvalid says no")
	}
	if fields, ok := rigclient.FieldsAs[todoCreateFields](err); !ok || fields.Title == nil {
		t.Error("FieldsAs no longer reaches the detail")
	}
}

// A 404 is a refusal and has a code worth reading, but there is nothing to put
// beside a control — so Fields is nil rather than a shape nobody complained
// about, which is the difference between "fine" and "not asked".
func TestARefusalWithNoFieldsCarriesNone(t *testing.T) {
	err := create(t, refuses(http.StatusNotFound,
		`{"code":"NotFound","message":"no such todo"}`))

	refused, ok := todoCreateError(err)
	if !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if refused.Fields != nil {
		t.Errorf("fields = %+v, want nil", refused.Fields)
	}
	if refused.Code != rigerr.CodeNotFound {
		t.Errorf("code = %q, want NotFound", refused.Code)
	}
}

// A request that never reached the server has no envelope, so there is nothing
// for a code or a field to have come from and the answer is no.
func TestSomethingThatNeverReachedTheServerIsNotARefusal(t *testing.T) {
	rt, err := rigclient.New(rigclient.Config{BaseURL: "http://127.0.0.1:1"},
		rigclient.API{BasePath: "/api/v1"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: map[string]any{},
	})
	if err == nil {
		t.Fatal("a call to a closed port succeeded")
	}

	if refused, ok := todoCreateError(err); ok {
		t.Errorf("refused = %+v, want false: there was no envelope", refused)
	}
}

// A validation failure grows with the request, and the widest inputs used to be
// the ones whose refusals said the least: a body over the read limit was
// truncated into JSON that would not parse, which loses the code and the message
// along with the fields it was too big for.
func TestALargeValidationFailureSurvives(t *testing.T) {
	long := strings.Repeat("far too long. ", 4000) // ~56 KiB, past the plain limit
	body, err := json.Marshal(map[string]any{
		"code":      "UnprocessableEntity",
		"message":   "todo is not valid",
		"requestId": "req-9",
		"fields": map[string]any{
			"title": map[string]string{"code": "TooLong", "message": long},
			"notes": map[string]string{"code": "CannotBeEmpty", "message": "cannot be empty"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	refused, ok := todoCreateError(create(t, refuses(http.StatusUnprocessableEntity, string(body))))
	if !ok {
		t.Fatal("a large refusal did not read back at all")
	}
	if refused.Code != rigerr.CodeUnprocessableEntity {
		t.Errorf("code = %q, want UnprocessableEntity: the body was too big to parse", refused.Code)
	}
	if refused.Message != "todo is not valid" || refused.RequestID != "req-9" {
		t.Errorf("message = %q, request = %q, want both kept", refused.Message, refused.RequestID)
	}
	if refused.Fields == nil {
		t.Fatal("no fields, and they are what made the body large")
	}
	if refused.Fields.Title == nil || len(refused.Fields.Title.Message) != len(long) {
		t.Error("the long message did not survive whole")
	}
	if refused.Fields.Notes == nil {
		t.Error("the field after the long one is gone, so the body was cut")
	}

	// The excerpt is bounded even though the read was not: whatever the body
	// carried is in Fields by now.
	if len(refused.Body) > 8<<10 {
		t.Errorf("Body is %d bytes, want it cut to the plain limit", len(refused.Body))
	}
}

// The widened limit follows the content type, which is a claim rather than a
// promise — so anything that is not the server's envelope is read as narrowly as
// it ever was.
func TestSomethingThatIsNotJSONIsStillReadNarrowly(t *testing.T) {
	page := strings.Repeat("<html>502 Bad Gateway</html>", 4000) // ~112 KiB
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, page)
	}), rigclient.Config{Retry: rigclient.Retry{Attempts: 1}}) // about the read, not the retry

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	var e *rigclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a *rigclient.Error", err)
	}
	if e.Status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", e.Status)
	}
	if len(e.Body) != 8<<10 {
		t.Errorf("read %d bytes of the page, want it stopped at the plain limit", len(e.Body))
	}
}

// Skew between a client and a server loses the detail. It must not also lose the
// code and the message, which is what a strict decode would do.
func TestFieldsThatDoNotFitKeepTheCodeAndTheMessage(t *testing.T) {
	err := create(t, refuses(http.StatusUnprocessableEntity,
		`{"code":"UnprocessableEntity","message":"not valid","fields":["title"]}`))

	refused, ok := todoCreateError(err)
	if !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if refused.Fields != nil {
		t.Errorf("fields = %+v, want nil: the body did not fit the shape", refused.Fields)
	}
	if refused.Code != rigerr.CodeUnprocessableEntity || refused.Message != "not valid" {
		t.Errorf("code = %q, message = %q, want both kept", refused.Code, refused.Message)
	}
	if len(refused.Refusal.Fields) == 0 {
		t.Error("the raw bytes are gone, and they are what would explain the skew")
	}
}

// The 401 rewind joins two facts. Reading one of them back must not cost the
// other, which is what unwrapping to the refusal alone would have done.
func TestAnUploadThatCannotSeekIsStillReadable(t *testing.T) {
	cred := &refresher{token: "stale"}
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"Unauthorized","message":"expired"}`))
	}), rigclient.Config{Credential: cred})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			rigclient.Part("coverFile", rigclient.Upload{
				Name: "c.png", ContentType: "image/png",
				Body: struct{ io.Reader }{strings.NewReader("x")},
			}),
		}},
	})

	refused, ok := todoCreateError(err)
	if !ok {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if refused.Code != rigerr.CodeUnauthorized {
		t.Errorf("code = %q, want Unauthorized", refused.Code)
	}
	if !errors.Is(err, rigclient.ErrCannotRetry) {
		t.Error("the caller needs both facts, and one of them is gone")
	}
}

// A Failure re-returned as an error is still one, so a wrapper that reads a
// refusal and hands it on does not cost its caller the predicates.
func TestAFailureIsStillARefusal(t *testing.T) {
	err := create(t, refuses(http.StatusNotFound, `{"code":"NotFound","message":"gone"}`))

	refused, _ := todoCreateError(err)
	if !rigclient.IsNotFound(error(refused)) {
		t.Error("IsNotFound says no about a value that plainly is one")
	}
}

package httpx_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/simonjanss/rig/runtime/httpx"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"

	"github.com/google/uuid"
)

// The envelope is flat, and this is the test that says so. A nested
// `{"error":{…}}` — which two of the three packages that now share this used to
// write — decodes into an all-zero struct in both of rig's client libraries, so
// every error predicate answers false and the caller is left with the status.
func TestTheEnvelopeIsFlat(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.WriteError(rec, "req-7", rigerr.NotFound("no such thing"))

	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, nested := raw["error"]; nested {
		t.Error(`the envelope has an "error" key`)
	}
	for k, want := range map[string]any{
		"code": string(rigerr.CodeNotFound), "message": "no such thing", "requestId": "req-7",
	} {
		if raw[k] != want {
			t.Errorf("%s = %v, want %v", k, raw[k], want)
		}
	}
}

// camelCase, always. A project's `api.json_case` renames the fields of its own
// resources and the generated server's error envelope goes through it; this one
// does not, because these routes are identical in every project and the browser
// packages are compiled against them once.
func TestTheKeysAreCamelCase(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.WriteError(rec, "req-7", rigerr.NotFound("gone"))

	if body := rec.Body.String(); strings.Contains(body, "request_id") {
		t.Errorf("the envelope uses snake_case: %s", body)
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, ok := raw["requestId"]; !ok {
		t.Errorf("no requestId in %v", raw)
	}
}

// A request id nobody has is absent rather than empty, so a client can tell "no
// identifier" from "an identifier that is the empty string". The hand-written
// routes have no request context to read one from.
func TestAnAbsentRequestIDIsOmitted(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.WriteError(rec, "", rigerr.Forbidden("no"))

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["requestId"]; present {
		t.Errorf("requestId is present with nothing in it: %v", raw)
	}
}

// An internal failure's detail never reaches the client. It is exactly the kind
// of thing that carries a table name, a constraint, or a connection string, and
// the request id is what finds the rest in the logs.
func TestAnInternalFailureIsRedacted(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.WriteError(rec, "req-7",
		rigerr.Internal(nil, "dial tcp 10.0.0.4:5432: password authentication failed for \"rig\""))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, leak := range []string{"10.0.0.4", "password", "5432"} {
		if strings.Contains(body, leak) {
			t.Errorf("the response carries %q: %s", leak, body)
		}
	}
	if !strings.Contains(body, "something went wrong") {
		t.Errorf("no redacted message: %s", body)
	}
	if !strings.Contains(body, "req-7") {
		t.Errorf("the request id is the only way back to the detail, and it is missing: %s", body)
	}
}

// fieldError is the shape a generated input error has: it reports per-field
// detail so a client can attach each message to the control the person is looking
// at, rather than parsing one sentence for field names.
type fieldError struct {
	Email string `json:"email"`
}

func (e *fieldError) Error() string          { return "the input is not valid" }
func (e *fieldError) ErrorCode() rigerr.Code { return rigerr.CodeUnprocessableEntity }
func (e *fieldError) ErrorFields() any       { return e }

// The per-field detail survives to the wire.
//
// This is what `authhttp.fail` dropped: it wrote a `map[string]string`, so there
// was nowhere for `fields` to go. Nothing in `auth` produces a field-carrying
// error today, so it was a latent divergence rather than a live bug — but it is
// the kind that is only found by somebody's client, and the fix is this test.
func TestPerFieldDetailSurvives(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.WriteError(rec, "", &fieldError{Email: "that address is already in use"})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
	var body struct {
		Fields struct{ Email string } `json:"fields"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Fields.Email != "that address is already in use" {
		t.Errorf("fields.email = %q", body.Fields.Email)
	}
}

// An error carrying no field detail has no `fields` member at all, rather than a
// null one for a client to branch on.
func TestNoFieldDetailMeansNoFieldsMember(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.WriteError(rec, "", rigerr.BadRequest("nope"))

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["fields"]; present {
		t.Errorf("fields is present with nothing in it: %v", raw)
	}
}

// A nil body is a status and no bytes, which is what a 204 needs.
func TestANilBodyWritesNothing(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.WriteJSON(rec, http.StatusNoContent, nil)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("wrote %q", rec.Body.String())
	}
}

func decodeInto(body string, limit int64) error {
	var into struct {
		Name string `json:"name"`
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	return httpx.Decode(r, limit, &into)
}

// An empty body is the most common client mistake by a wide margin, and
// "unexpected end of JSON input" is not what the person reading it needs to know.
func TestAnEmptyBodySaysSo(t *testing.T) {
	t.Parallel()

	err := decodeInto("", 0)
	if err == nil {
		t.Fatal("an empty body was accepted")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("err = %v, and it should say the body was empty", err)
	}
	if got := rigerr.CodeOf(err); got != rigerr.CodeBadRequest {
		t.Errorf("code = %q, want %q", got, rigerr.CodeBadRequest)
	}
}

// A field the route does not declare is refused rather than ignored: a client
// that sent it believed it meant something, and the field it was reaching for is
// often the one the route exists to make unreachable.
func TestAnUndeclaredFieldIsRefused(t *testing.T) {
	t.Parallel()

	if err := decodeInto(`{"name":"a","accountId":"somebody-else"}`, 0); err == nil {
		t.Fatal("a body naming an undeclared field was accepted")
	}
}

// The limit is the only thing between a stranger and the server's memory on an
// unauthenticated route, so it is enforced rather than advisory.
func TestABodyOverTheLimitIsRefused(t *testing.T) {
	t.Parallel()

	huge := `{"name":"` + strings.Repeat("x", 4096) + `"}`
	if err := decodeInto(huge, 64); err == nil {
		t.Fatalf("a %d-byte body was accepted under a 64-byte limit", len(huge))
	}
	// And the same body is fine when the limit allows it, so what refused it was
	// the limit and not the parser.
	if err := decodeInto(huge, 0); err != nil {
		t.Errorf("the same body under the default limit: %v", err)
	}
}

// Caller establishes who is asking, once, so every route under it narrows
// structurally rather than as something each handler remembers.
func TestCallerPutsTheClaimsOnTheContext(t *testing.T) {
	t.Parallel()

	want := tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New(),
		Subject: tenancy.SubjectAccount}

	var got tenancy.Claims
	h := httpx.Caller{Of: func(*http.Request) (tenancy.Claims, error) { return want, nil }}.
		Wrap(func(_ http.ResponseWriter, r *http.Request) {
			got, _ = tenancy.FromContext(r.Context())
		})

	h(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got.TenantID != want.TenantID || got.AccountID != want.AccountID ||
		got.Subject != want.Subject {
		t.Errorf("the handler saw %+v, want %+v", got, want)
	}
}

// A caller who cannot be established never reaches the handler, and the refusal
// goes out through the configured writer.
func TestCallerRefusesBeforeTheHandler(t *testing.T) {
	t.Parallel()

	reached := false
	h := httpx.Caller{Of: func(*http.Request) (tenancy.Claims, error) {
		return tenancy.Claims{}, rigerr.Unauthorized("no session")
	}}.Wrap(func(http.ResponseWriter, *http.Request) { reached = true })

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if reached {
		t.Error("the handler ran for a caller who could not be established")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// A nil Fail is [httpx.Fail], so a Caller is usable with one field set.
func TestCallerDefaultsItsErrorWriter(t *testing.T) {
	t.Parallel()

	custom := false
	h := httpx.Caller{
		Of:   func(*http.Request) (tenancy.Claims, error) { return tenancy.Claims{}, rigerr.Forbidden("no") },
		Fail: func(w http.ResponseWriter, r *http.Request, err error) { custom = true; httpx.Fail(w, r, err) },
	}.Wrap(func(http.ResponseWriter, *http.Request) {})

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !custom {
		t.Error("the supplied Fail was not used")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d", rec.Code)
	}
}

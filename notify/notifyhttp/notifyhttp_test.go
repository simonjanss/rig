package notifyhttp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/notify"
	"github.com/simonjanss/rig/notify/notifyhttp"
	"github.com/simonjanss/rig/runtime/httpx"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// This package had no tests. The 404 that was a 500 is why it has some now: the
// mapping lived in a fallback error writer that every generated project replaces,
// so it was never reached in a real application and nothing said so.
//
// Nothing here needs a database. Every case is either the wire shape, a query
// string refused before the service is called, or the error writer.

func mount(t *testing.T, claims func(*http.Request) (tenancy.Claims, error),
	fail func(http.ResponseWriter, *http.Request, error),
) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	// A nil service is safe for these cases and is the point: none of them
	// reaches it.
	notifyhttp.New(nil, notifyhttp.Options{Claims: claims, Fail: fail}).Mount(mux)
	return mux
}

func someone(*http.Request) (tenancy.Claims, error) {
	return tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New()}, nil
}

// **The reason this file exists.** `notify.ErrNotFound` used to be a bare
// `errors.New`, so anything that classified it read CodeInternal. The 404 was
// applied in this package's own fallback writer — and every generated project
// supplies `Fail`, which bypasses it. So dismissing a notification that was not
// there, or was somebody else's, answered `500 something went wrong`.
//
// Asserted against a writer shaped like the generated one, because that is the
// arrangement every real application is in.
func TestAMissingNotificationIs404ThroughAnyErrorWriter(t *testing.T) {
	t.Parallel()

	generated := func(w http.ResponseWriter, _ *http.Request, err error) {
		answer := httpx.AnswerFor(w, err)
		httpx.WriteJSON(w, answer.Status, httpx.Error{
			Code: answer.Code, Message: answer.Message, RequestID: "req-1",
		})
	}

	for _, tc := range []struct {
		name string
		fail func(http.ResponseWriter, *http.Request, error)
	}{
		{"the generated server's writer", generated},
		{"the package's own fallback", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			// Provoked through the error writer directly: reaching it through a
			// handler would need a database to have a row that is not there.
			fail := tc.fail
			if fail == nil {
				fail = httpx.Fail
			}
			fail(rec, httptest.NewRequest(http.MethodDelete, "/notifications/x", nil),
				notify.ErrNotFound)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404", rec.Code)
			}
			var body httpx.Error
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Code != rigerr.CodeNotFound {
				t.Errorf("code = %q, want %q", body.Code, rigerr.CodeNotFound)
			}
			if body.Message != "no such notification" {
				t.Errorf("message = %q", body.Message)
			}
		})
	}
}

// The envelope is flat. A nested `{"error":{…}}` decodes into an all-zero struct
// in both of rig's client libraries, so every error predicate answers false and
// the caller is left with the status and nothing else.
func TestTheFallbackEnvelopeIsFlat(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.Fail(rec, httptest.NewRequest(http.MethodGet, "/notifications", nil),
		rigerr.Forbidden("not yours"))

	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	var raw map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if _, nested := raw["error"]; nested {
		t.Error(`the envelope has an "error" key, which neither client library reads`)
	}
	if raw["code"] != string(rigerr.CodeForbidden) {
		t.Errorf("code = %v", raw["code"])
	}
	if raw["message"] != "not yours" {
		t.Errorf("message = %v", raw["message"])
	}
}

// An internal failure says nothing about the internals, because the detail is
// exactly the kind of thing that carries a table name or a connection string.
func TestAnInternalFailureIsNotDetailed(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	httpx.Fail(rec, httptest.NewRequest(http.MethodGet, "/notifications", nil),
		rigerr.Internal(nil, "dial tcp 10.0.0.4:5432: connection refused"))

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "10.0.0.4") {
		t.Errorf("the response carries the cause: %s", body)
	}
}

// A claims function that refuses stops every route, which is what makes the
// narrowing structural rather than something each handler remembers.
func TestABadClaimsFunctionRefusesEveryRoute(t *testing.T) {
	t.Parallel()

	mux := mount(t, func(*http.Request) (tenancy.Claims, error) {
		return tenancy.Claims{}, rigerr.Unauthorized("no session")
	}, nil)

	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/notifications"},
		{http.MethodGet, "/notifications/_unread-count"},
		{http.MethodPost, "/notifications/_read-all"},
		{http.MethodPost, "/notifications/" + uuid.New().String() + "/_read"},
		{http.MethodDelete, "/notifications/" + uuid.New().String()},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: status = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

// The query string is refused before the service is reached, so a typo is a 400
// naming the parameter rather than a page of the wrong rows.
func TestAMalformedQueryIsRefused(t *testing.T) {
	t.Parallel()

	mux := mount(t, someone, nil)

	for _, tc := range []struct{ name, query, mentions string }{
		{"limit is not a number", "?limit=lots", "limit"},
		{"before is not RFC 3339", "?before=yesterday", "before"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/notifications"+tc.query, nil))

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.mentions) {
				t.Errorf("the refusal does not name %q: %s", tc.mentions, rec.Body)
			}
		})
	}
}

// A path identifier that is not a UUID is refused the same way, and before the
// service, so nothing looks up a row for a string somebody typed.
func TestAMalformedPathIdentifierIsRefused(t *testing.T) {
	t.Parallel()

	mux := mount(t, someone, nil)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/notifications/not-a-uuid/_read"},
		{http.MethodDelete, "/notifications/not-a-uuid"},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}
}

// A handler with no way to identify its caller would answer every request with
// the same empty inbox, which reads as "you have no notifications" rather than as
// the misconfiguration it is.
func TestNewPanicsWithoutClaims(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("a handler with no Claims was accepted")
		}
	}()
	notifyhttp.New(nil, notifyhttp.Options{})
}

// The same promise presencehttp makes: these routes answer camelCase whatever
// `api.json_case` a project sets, because the browser packages are compiled
// against them once. A test rather than a comment because the failure is silent.
func TestTheWireNamesAreCamelCase(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeOf(notifyhttp.Line{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			t.Errorf("%s.%s has no wire name", typ.Name(), f.Name)
			continue
		}
		if c := name[0]; c < 'a' || c > 'z' {
			t.Errorf("%s.%s is %q on the wire; these routes answer camelCase",
				typ.Name(), f.Name, name)
		}
		if strings.ContainsAny(name, "_-") {
			t.Errorf("%s.%s is %q on the wire; these routes answer camelCase",
				typ.Name(), f.Name, name)
		}
	}
}

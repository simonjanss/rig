package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/httpx"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// The refusals these write are a wire contract. Two generators used to emit
// their own copy of each one, so the sentences below are the thing being pinned
// — not the parsing, which is the standard library's.

func TestARefusalNamesTheParameterAndTheShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		what string
		err  error
		want string
	}{
		{"int", errOf(httpx.ParseInt("limit", "soon")), "limit must be a whole number"},
		{"int64", errOf(httpx.ParseInt64("offset", "soon")), "offset must be a whole number"},
		{"float", errOf(httpx.ParseFloat("ratio", "half")), "ratio must be a number"},
		{"bool", errOf(httpx.ParseBool("done", "maybe")), "done must be true or false"},
		{"uuid", errOf(httpx.ParseUUID("id", "nope")), "id is not a valid identifier"},
		{"time", errOf(httpx.ParseTime("since", "yesterday")), "since must be an RFC 3339 timestamp"},
	} {
		t.Run(tc.what, func(t *testing.T) {
			t.Parallel()

			if tc.err == nil {
				t.Fatal("no refusal")
			}
			if got := rigerr.CodeOf(tc.err); got != rigerr.CodeBadRequest {
				t.Errorf("code is %v, want BadRequest", got)
			}
			// What the client reads, which is the message rather than
			// [rigerr.Error.Error]'s code-prefixed form.
			if got := httpx.AnswerFor(httptest.NewRecorder(), tc.err).Message; got != tc.want {
				t.Errorf("message is %q, want %q", got, tc.want)
			}
		})
	}
}

// Wider than the message says, and on purpose: the message tells a client what
// to send, and the parser accepts what a client is likely to have sent.
func TestABooleanTakesEverythingStrconvDoes(t *testing.T) {
	t.Parallel()

	for _, raw := range []string{"1", "t", "T", "TRUE", "true", "True"} {
		v, err := httpx.ParseBool("done", raw)
		if err != nil || !v {
			t.Errorf("%q read as (%v, %v)", raw, v, err)
		}
	}
}

// The whole reason the parsers take (name, raw) rather than a request: the two
// generators disagree about where raw comes from and agree about the rest.
func TestAPathAndAQueryRefuseInTheSameWords(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/things?id=nope", nil)
	r.SetPathValue("id", "nope")

	_, fromPath := httpx.PathUUID(r, "id")
	_, fromQuery := httpx.QueryUUID(r, "id")

	if fromPath == nil || fromQuery == nil {
		t.Fatal("one of them accepted it")
	}
	if fromPath.Error() != fromQuery.Error() {
		t.Errorf("path says %q and query says %q", fromPath, fromQuery)
	}
}

// An absent parameter and an empty one are the same thing, everywhere. A query
// string has no reading of `?since=` that every client agrees on, so rig picks
// the one that cannot surprise anybody.
func TestAnAbsentParameterIsTheFallbackAndSoIsAnEmptyOne(t *testing.T) {
	t.Parallel()

	for _, query := range []string{"", "?limit=&flag=&name=&id=&since="} {
		r := httptest.NewRequest(http.MethodGet, "/things"+query, nil)

		if n, err := httpx.QueryInt(r, "limit", 50); err != nil || n != 50 {
			t.Errorf("%q: limit read as (%v, %v)", query, n, err)
		}
		if v, err := httpx.QueryBool(r, "flag", true); err != nil || !v {
			t.Errorf("%q: flag read as (%v, %v)", query, v, err)
		}
		if v := httpx.QueryString(r, "name", "anon"); v != "anon" {
			t.Errorf("%q: name read as %q", query, v)
		}
		if v, err := httpx.QueryUUID(r, "id"); err != nil || v != uuid.Nil {
			t.Errorf("%q: id read as (%v, %v)", query, v, err)
		}
		if v, err := httpx.QueryTime(r, "since"); err != nil || !v.IsZero() {
			t.Errorf("%q: since read as (%v, %v)", query, v, err)
		}
		if _, ok := httpx.QueryOptional(r, "name"); ok {
			t.Errorf("%q: an empty optional reported present", query)
		}
		if _, err := httpx.QueryRequired(r, "name"); err == nil {
			t.Errorf("%q: an empty required was accepted", query)
		}
	}
}

func TestAValueThatIsThereIsRead(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	when := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	r := httptest.NewRequest(http.MethodGet,
		"/things?limit=7&flag=false&name=alex&id="+id.String()+
			"&since="+when.Format(time.RFC3339), nil)
	r.SetPathValue("id", id.String())
	r.SetPathValue("n", "3")

	if n, err := httpx.QueryInt(r, "limit", 50); err != nil || n != 7 {
		t.Errorf("limit read as (%v, %v)", n, err)
	}
	if v, err := httpx.QueryBool(r, "flag", true); err != nil || v {
		t.Errorf("flag read as (%v, %v)", v, err)
	}
	if v := httpx.QueryString(r, "name", "anon"); v != "alex" {
		t.Errorf("name read as %q", v)
	}
	if v, err := httpx.QueryUUID(r, "id"); err != nil || v != id {
		t.Errorf("id read as (%v, %v)", v, err)
	}
	if v, err := httpx.QueryTime(r, "since"); err != nil || !v.Equal(when) {
		t.Errorf("since read as (%v, %v)", v, err)
	}
	if v, err := httpx.PathUUID(r, "id"); err != nil || v != id {
		t.Errorf("path id read as (%v, %v)", v, err)
	}
	if v, err := httpx.PathInt(r, "n"); err != nil || v != 3 {
		t.Errorf("path n read as (%v, %v)", v, err)
	}
	if v, err := httpx.PathString(r, "id"); err != nil || v != id.String() {
		t.Errorf("path id read as (%v, %v)", v, err)
	}
	if v, ok := httpx.QueryOptional(r, "name"); !ok || v != "alex" {
		t.Errorf("optional name read as (%v, %v)", v, ok)
	}
	if v, err := httpx.QueryRequired(r, "name"); err != nil || v != "alex" {
		t.Errorf("required name read as (%v, %v)", v, err)
	}
}

// A segment the pattern does not carry is a 400 rather than a 500: a router rig
// did not write may have got there first, and blaming the client is the
// answer that does not claim to know.
func TestAMissingPathSegmentIsRefusedRatherThanPanicked(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/things", nil)

	if _, err := httpx.PathString(r, "id"); err == nil {
		t.Error("a missing segment was accepted")
	} else if got := rigerr.CodeOf(err); got != rigerr.CodeBadRequest {
		t.Errorf("code is %v, want BadRequest", got)
	}
	if _, err := httpx.PathUUID(r, "id"); err == nil {
		t.Error("a missing identifier segment was accepted")
	}
}

func errOf[T any](_ T, err error) error { return err }

// Trying again: which failures are worth it, which calls may be sent twice, and
// what bounds the whole of it.
//
// Hermetic like the rest, and fast on purpose: every case that is about an
// outcome rather than about a wait sets a millisecond base, so the suite spends
// microseconds proving what a production client would spend seconds on. Where a
// case is about the wait itself, it moves the client's own clock instead of
// sleeping.
package rigclient_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/simonjanss/rig/rigclient"
	"github.com/simonjanss/rig/runtime/rigerr"
)

// quick is a retry policy with the waits taken out. Four attempts, as the
// default has, so the cases below are about which failures are repeated rather
// than about how many times.
var quick = rigclient.Retry{Base: time.Millisecond, Cap: time.Millisecond}

// failing answers status the first n times and 200 after that, counting every
// request it saw.
func failing(n int, status int, header map[string]string) (http.Handler, *atomic.Int32) {
	var seen atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if int(seen.Add(1)) <= n {
			for k, v := range header {
				w.Header().Set(k, v)
			}
			w.WriteHeader(status)
			w.Write([]byte(`{"code":"Internal","message":"not now","requestId":"req-1"}`))
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	})
	return h, &seen
}

func TestARetryableFailureIsSentAgain(t *testing.T) {
	h, seen := failing(2, http.StatusServiceUnavailable, nil)
	rt := newClient(t, h, rigclient.Config{Retry: quick})

	got, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if err != nil {
		t.Fatalf("err = %v, want the third attempt to have succeeded", err)
	}
	if got.ID != "1" {
		t.Errorf("got %+v, want the todo the third attempt answered with", got)
	}
	if n := seen.Load(); n != 3 {
		t.Errorf("the server saw %d requests, want 3", n)
	}
}

// The closed set, one row per status. A refusal that will not change is a
// refusal a wait cannot help, and asking again only makes the caller wait for
// the same answer.
func TestARefusalThatWillNotChangeIsSentOnce(t *testing.T) {
	cases := []struct {
		status int
		why    string
	}{
		{http.StatusBadRequest, "the request was malformed and will be malformed again"},
		{http.StatusUnauthorized, "the credential path has its own single retry"},
		{http.StatusForbidden, "the caller is known and still not permitted"},
		{http.StatusNotFound, "it was not there and will not be there"},
		{http.StatusConflict, "the state contradicted the request"},
		{http.StatusRequestEntityTooLarge, "the body is the same size on the second send"},
		{http.StatusUnsupportedMediaType, "and the same type"},
		{http.StatusUnprocessableEntity, "validation does not change its mind"},
		// The QUERY fallback's own signal. A method nobody in the chain has
		// heard of will not have been heard of a second later, and this route
		// has no alias to fall back to.
		{http.StatusNotImplemented, "the method is unknown, not the moment"},
		{http.StatusUpgradeRequired, "nothing the caller does at runtime fixes it"},
		{http.StatusHTTPVersionNotSupported, "the protocol is not a moment either"},
	}

	for _, c := range cases {
		t.Run(http.StatusText(c.status), func(t *testing.T) {
			var seen atomic.Int32
			rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				seen.Add(1)
				w.WriteHeader(c.status)
				w.Write([]byte(`{"code":"NotFound","message":"no"}`))
			}), rigclient.Config{Retry: quick})

			_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
				Method: http.MethodGet, Path: "/todos",
			})

			if err == nil {
				t.Fatal("the refusal did not come back")
			}
			if n := seen.Load(); n != 1 {
				t.Errorf("the server saw %d requests, want 1: %s", n, c.why)
			}
		})
	}
}

// A write is retried, because it goes out named: the server can tell the second
// send of one create from two creates, and answers the second with what it
// answered the first.
func TestAWriteIsRetriedUnderTheKeyItCarries(t *testing.T) {
	var keys []string
	h, seen := failing(2, http.StatusServiceUnavailable, nil)
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		h.ServeHTTP(w, r)
	}), rigclient.Config{Retry: quick})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: todo{Title: "x"},
	}); err != nil {
		t.Fatalf("err = %v, want the third attempt to have succeeded", err)
	}

	if n := seen.Load(); n != 3 {
		t.Errorf("the server saw %d requests, want 3", n)
	}
	// One key across all three, which is the whole of what makes them one write.
	// A fresh name per attempt would be three creates with three names.
	for i, k := range keys {
		if k == "" {
			t.Fatalf("attempt %d carried no idempotency key", i+1)
		}
		if k != keys[0] {
			t.Errorf("attempt %d carried %q, want the first attempt's %q", i+1, k, keys[0])
		}
	}
}

// A client that turned retries off should not be making the server keep a
// record against a retry it will never send.
func TestAWriteThatWillNotBeRetriedIsNotNamed(t *testing.T) {
	var key string
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		key = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"code":"Internal","message":"no"}`))
	}), rigclient.Config{Retry: rigclient.Retry{Attempts: 1}})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: todo{Title: "x"},
	})

	var e *rigclient.Error
	if !errors.As(err, &e) || e.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want the 503 back", err)
	}
	if e.Code != rigerr.CodeInternal {
		t.Errorf("code = %q, want the envelope to have survived", e.Code)
	}
	if seen.Load() != 1 {
		t.Errorf("the server saw %d requests, want 1", seen.Load())
	}
	if key != "" {
		t.Errorf("the call carried the key %q, want none", key)
	}
}

// A caller's own key is worth having and is left alone: one derived from the
// data deduplicates a re-run of a whole job, which a fresh random name cannot.
func TestACallersOwnKeyIsNotReplaced(t *testing.T) {
	var keys []string
	h, _ := failing(1, http.StatusServiceUnavailable, nil)
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		h.ServeHTTP(w, r)
	}), rigclient.Config{Retry: quick})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: todo{Title: "x"},
	}, rigclient.WithIdempotencyKey("import-42-summit")); err != nil {
		t.Fatal(err)
	}

	for i, k := range keys {
		if k != "import-42-summit" {
			t.Errorf("attempt %d carried %q, want the caller's own key", i+1, k)
		}
	}
}

// A search goes out as a POST where an intermediary refuses QUERY, and it is
// still a read: naming it would have the server record a row for a question.
func TestASearchIsNotNamedEvenWhenItGoesOutAsAPost(t *testing.T) {
	var searchKey string
	var posts atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "QUERY" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		posts.Add(1)
		searchKey = r.Header.Get("Idempotency-Key")
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{Retry: quick})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: rigclient.MethodQuery, Path: "/todos", Fallback: "/todos/_search",
		Body: map[string]string{"title": "x"},
	}); err != nil {
		t.Fatal(err)
	}

	if posts.Load() != 1 {
		t.Fatalf("the alias saw %d requests, want 1", posts.Load())
	}
	if searchKey != "" {
		t.Errorf("the search carried the key %q, want none", searchKey)
	}
}

// An upload route is the one write a rig server does not record against a key,
// because its body is still arriving when the handler calls the service. So the
// SDK neither names a form body nor sends one twice: a key on one would name a
// write nobody wrote down, and the retry would store the file again.
func TestAFormBodyIsNeitherNamedNorSentTwice(t *testing.T) {
	var key string
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		key = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"code":"Internal","message":"not now"}`))
	}), rigclient.Config{Retry: quick})

	// A seekable body, so nothing but the rule below stops a second attempt:
	// rewinding this one would succeed.
	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos/1/cover-file",
		Multipart: &rigclient.Multipart{Files: []rigclient.Upload{
			{Field: "coverFile", Name: "cover.png", Body: strings.NewReader("bytes")},
		}},
	})

	var e *rigclient.Error
	if !errors.As(err, &e) || e.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v, want the 503 back", err)
	}
	if n := seen.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want 1", n)
	}
	if key != "" {
		t.Errorf("the upload carried the key %q, want none", key)
	}
}

// A delete is idempotent in what it leaves behind, so it is worth sending
// again.
func TestADeleteIsRetried(t *testing.T) {
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}), rigclient.Config{Retry: quick})

	if err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodDelete, Path: "/todos/1",
	}); err != nil {
		t.Fatalf("err = %v, want the second attempt to have succeeded", err)
	}
	if n := seen.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want 2", n)
	}
}

// The price of the line above, on purpose. If the first attempt worked and its
// answer was lost, the second one is telling the truth: it is not there any
// more. The row is gone either way, which is what makes this a cost worth
// paying and a duplicated create not.
func TestADeleteSentTwiceReportsWhatTheSecondServerSaid(t *testing.T) {
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":"NotFound","message":"already gone"}`))
	}), rigclient.Config{Retry: quick})

	err := rigclient.DoNoContent(t.Context(), rt, rigclient.Op{
		Method: http.MethodDelete, Path: "/todos/1",
	})

	if !rigclient.IsNotFound(err) {
		t.Fatalf("err = %v, want the second attempt's 404", err)
	}
}

// A connection that died before an answer arrived is the commonest retryable
// failure there is, and it carries no status to switch on.
func TestATransportFailureOnAReadIsRetried(t *testing.T) {
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if seen.Add(1) <= 2 {
			hijack(t, w)
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{Retry: quick})

	got, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if err != nil {
		t.Fatalf("err = %v, want the third attempt to have succeeded", err)
	}
	if got.ID != "1" {
		t.Errorf("got %+v, want the todo", got)
	}
	if n := seen.Load(); n != 3 {
		t.Errorf("the server saw %d requests, want 3", n)
	}
}

// The same failure on a write is the ambiguous one: the request may have been
// applied and the answer lost, or never have arrived at all. Nothing the client
// holds tells the two apart — and the key is what makes that stop mattering,
// because the server can tell even though the client cannot. It is the failure
// this whole mechanism exists for.
func TestATransportFailureOnANamedWriteIsRetried(t *testing.T) {
	var keys []string
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys = append(keys, r.Header.Get("Idempotency-Key"))
		if seen.Add(1) <= 2 {
			hijack(t, w)
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{Retry: quick})

	got, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodPost, Path: "/todos", Body: todo{Title: "x"},
	})

	if err != nil {
		t.Fatalf("err = %v, want the third attempt to have succeeded", err)
	}
	if got.ID != "1" {
		t.Errorf("got %+v, want the todo", got)
	}
	if n := seen.Load(); n != 3 {
		t.Errorf("the server saw %d requests, want 3", n)
	}
	// The same name each time, which is what lets the server answer the third
	// one with the row the first may already have written.
	for i, k := range keys {
		if k == "" || k != keys[0] {
			t.Errorf("attempt %d carried %q, want the first attempt's %q", i+1, k, keys[0])
		}
	}
}

// A credential that will not apply is this call's own failure, not the
// network's. Repeating it would spend three more token exchanges on a refresh
// that is not going to start working.
func TestARequestThatNeverWentOutIsNotRetried(t *testing.T) {
	var seen atomic.Int32
	broken := errors.New("no session")
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.Write([]byte(`{"id":"1"}`))
	}), rigclient.Config{Retry: quick, Credential: brokenCredential{broken}})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if !errors.Is(err, broken) {
		t.Fatalf("err = %v, want the credential's own failure", err)
	}
	if n := seen.Load(); n != 0 {
		t.Errorf("the server saw %d requests, want none", n)
	}
}

// The alias is the same request addressed differently, so it does not spend an
// attempt. A search behind a refusing proxy gets the same three retries as the
// read beside it.
func TestTheFallbackDoesNotSpendTheRetryBudget(t *testing.T) {
	var queries, posts atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "QUERY" {
			queries.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if posts.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{Retry: quick})

	got, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: rigclient.MethodQuery, Path: "/todos", Fallback: "/todos/_search",
		Body: map[string]string{"title": "x"},
	})

	if err != nil {
		t.Fatalf("err = %v, want the search to have succeeded", err)
	}
	if got.ID != "1" {
		t.Errorf("got %+v, want the todo", got)
	}
	if q, p := queries.Load(), posts.Load(); q != 1 || p != 3 {
		t.Errorf("saw %d QUERY and %d POST, want 1 and 3", q, p)
	}
}

// A server answering 405 to everything is a refusal, not a loop.
func TestTheAliasIsNotTriedTwice(t *testing.T) {
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}), rigclient.Config{Retry: quick})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: rigclient.MethodQuery, Path: "/todos", Fallback: "/todos/_search",
		Body: map[string]string{"title": "x"},
	})

	var e *rigclient.Error
	if !errors.As(err, &e) || e.Status != http.StatusMethodNotAllowed {
		t.Fatalf("err = %v, want the 405 back", err)
	}
	if n := seen.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want the QUERY and one POST", n)
	}
}

// The credential changed, not the server's mind, so it does not spend an
// attempt either.
func TestAReauthorizationDoesNotSpendTheRetryBudget(t *testing.T) {
	var seen atomic.Int32
	cred := &refresher{token: "stale"}
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"code":"Unauthorized","message":"expired"}`))
			return
		}
		if seen.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{Retry: quick, Credential: cred})

	got, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if err != nil {
		t.Fatalf("err = %v, want the call to have succeeded", err)
	}
	if got.ID != "1" {
		t.Errorf("got %+v, want the todo", got)
	}
	if cred.calls != 1 {
		t.Errorf("the credential refreshed %d times, want 1", cred.calls)
	}
}

// A blind retry on 401 is a way to lock an account out with a wrong password,
// so the second one is the answer.
func TestASecondUnauthorizedIsNotRetried(t *testing.T) {
	var seen atomic.Int32
	cred := &refresher{token: "stale"}
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"Unauthorized","message":"expired"}`))
	}), rigclient.Config{Retry: quick, Credential: cred})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if !rigclient.IsUnauthorized(err) {
		t.Fatalf("err = %v, want the 401 back", err)
	}
	if n := seen.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want 2", n)
	}
	if cred.calls != 1 {
		t.Errorf("the credential refreshed %d times, want 1", cred.calls)
	}
}

// A Reauthorizer that decides it cannot help leaves the 401 as the answer — and
// the answer has to still be a 401 anybody can recognize. It is a refusal read
// from a body that is about to be discarded, which is the ordering this pins.
func TestACredentialThatCannotRefreshStillReportsTheRefusal(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"Unauthorized","message":"expired","requestId":"req-9"}`))
	}), rigclient.Config{Retry: quick, Credential: &declining{}})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if !rigclient.IsUnauthorized(err) {
		t.Fatalf("err = %v, want a refusal that answers IsUnauthorized", err)
	}
	var e *rigclient.Error
	if errors.As(err, &e) && e.RequestID != "req-9" {
		t.Errorf("requestId = %q, want the envelope to have been read before the drain", e.RequestID)
	}
}

// The caller stopped waiting, and both facts are true: the server refused, and
// this call is over. A caller needs both to tell a rate limit they gave up on
// from a 500 they gave up on.
func TestACancelledContextEndsTheWaitAtOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"code":"Internal","message":"draining"}`))
	}), rigclient.Config{
		Retry: rigclient.Retry{Base: time.Minute, Cap: time.Minute},
		// No timeout, so the budget is not what ends this — the cancel is.
		HTTPClient: &http.Client{},
	})

	// A minute of backoff against two requests to a server on this machine, so
	// the cancel lands in the wait rather than in a request. Cancelling from
	// inside the handler cannot do it: the response is not the client's until
	// Do returns, and a context cancelled before that arrives as a transport
	// failure with no refusal on it — which is a different case.
	time.AfterFunc(200*time.Millisecond, cancel)

	start := time.Now()
	_, err := rigclient.Do[todo](ctx, rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})
	elapsed := time.Since(start)

	if elapsed > 5*time.Second {
		t.Errorf("the call took %s, want it to have given up rather than slept a minute", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to answer context.Canceled", err)
	}
	var e *rigclient.Error
	if !errors.As(err, &e) || e.Status != http.StatusServiceUnavailable {
		t.Errorf("err = %v, want the refusal to have come out with it", err)
	}
}

// The budget test. Thirty seconds of client timeout, and a handler that moves
// the client's own clock twenty seconds forward on every request — so the first
// attempt fails with ten seconds left, which the second attempt fits in, and
// then there is nothing left for a third.
func TestTheWholeCallIsBoundedByItsTimeout(t *testing.T) {
	var seen atomic.Int32
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		now = now.Add(20 * time.Second)
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"code":"Internal","message":"draining","requestId":"req-3"}`))
	}), rigclient.Config{Retry: quick, Now: clock})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if n := seen.Load(); n != 2 {
		t.Errorf("the server saw %d requests, want 2: the budget ran out before a third", n)
	}
	// Not a deadline error. Blaming this clock for the server's outage would
	// send somebody to the wrong logs.
	var e *rigclient.Error
	if !errors.As(err, &e) || e.RequestID != "req-3" {
		t.Fatalf("err = %v, want the server's own refusal", err)
	}
}

// Twenty-five seconds is a wait the SDK would agree to in principle — it is
// under MaxRetryAfter — and this call has ten. So the budget is what declines
// it, and the interval comes back on the error for somebody who has longer.
func TestAnIntervalLongerThanTheCallHasIsNotWaitedFor(t *testing.T) {
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.Header().Set("Retry-After", "25")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"code":"RateLimited","message":"slow down"}`))
	}), rigclient.Config{Retry: quick, HTTPClient: &http.Client{Timeout: 10 * time.Second}})

	start := time.Now()
	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the call took %s, want it not to have waited", elapsed)
	}
	if !rigclient.IsRateLimited(err) {
		t.Fatalf("err = %v, want the 429", err)
	}
	var e *rigclient.Error
	if !errors.As(err, &e) || e.RetryAfter != 25*time.Second {
		t.Errorf("RetryAfter = %v, want the interval kept for the caller to decide about", e.RetryAfter)
	}
	if n := seen.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want 1", n)
	}
}

// And with no budget at all — a caller who supplied a client with no timeout —
// MaxRetryAfter is what stops "no timeout" from meaning "the SDK may park your
// request for an hour".
func TestAnAbsurdIntervalIsNotWaitedForEvenWithAllTheTimeInTheWorld(t *testing.T) {
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"code":"RateLimited","message":"slow down"}`))
	}), rigclient.Config{Retry: quick, HTTPClient: &http.Client{}})

	start := time.Now()
	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the call took %s, want it to have refused the hour", elapsed)
	}
	if !rigclient.IsRateLimited(err) {
		t.Fatalf("err = %v, want the 429", err)
	}
	if n := seen.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want 1", n)
	}
}

func TestWithRetryTurnsItOffForOneCall(t *testing.T) {
	h, seen := failing(10, http.StatusServiceUnavailable, nil)
	rt := newClient(t, h, rigclient.Config{Retry: quick})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}, rigclient.WithRetry(1))

	if err == nil {
		t.Fatal("the refusal did not come back")
	}
	if n := seen.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want 1", n)
	}
}

func TestWithRetryRaisesItForOneCall(t *testing.T) {
	h, seen := failing(4, http.StatusServiceUnavailable, nil)
	rt := newClient(t, h, rigclient.Config{Retry: quick})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}, rigclient.WithRetry(6)); err != nil {
		t.Fatalf("err = %v, want the fifth attempt to have succeeded", err)
	}
	if n := seen.Load(); n != 5 {
		t.Errorf("the server saw %d requests, want 5", n)
	}
}

// Nothing about the error changed, which is the point: everything written
// against a refusal before retries existed still reads it.
func TestTheLastAttemptsErrorIsTheRealOne(t *testing.T) {
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"code":"RateLimited","message":"slow down","requestId":"req-7"}`))
	}), rigclient.Config{Retry: quick})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if !rigclient.IsRateLimited(err) {
		t.Error("IsRateLimited says no")
	}
	if code := rigclient.CodeOf(err); code != rigerr.CodeRateLimited {
		t.Errorf("CodeOf = %q, want RateLimited", code)
	}
	var e *rigclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want errors.As to still find it", err)
	}
	if e.RequestID != "req-7" {
		t.Errorf("requestId = %q, want req-7", e.RequestID)
	}
}

func TestRetryAfterIsReadInBothForms(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"the seconds rig's own server sends", "12", 12 * time.Second},
		{"the date something in front of it sends", now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second},
		{"a date already past is no wait, not a wait backwards", now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"negative seconds are the same", "-5", 0},
		{"nothing at all", "", 0},
		{"something that is neither", "soon", 0},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rigclient.RetryAfter(c.value, now); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// The arithmetic, with the randomness handed in. worst returns the whole window
// and best returns none of it, which is how a range gets pinned by two exact
// numbers.
func TestTheWindowDoublesAndIsCapped(t *testing.T) {
	r := rigclient.Retry{Base: time.Second, Cap: 4 * time.Second}
	worst := func(n int64) int64 { return n - 1 }

	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 4 * time.Second}
	for i, w := range want {
		if got := rigclient.Delay(r, i+1, 0, worst); got != w {
			t.Errorf("attempt %d waited %v, want %v", i+1, got, w)
		}
	}
}

// Half the window is fixed and half is random, so a crowd that failed together
// does not come back together.
func TestTheWaitIsJitteredAcrossHalfTheWindow(t *testing.T) {
	r := rigclient.Retry{Base: time.Second, Cap: time.Second}

	best := rigclient.Delay(r, 2, 0, func(int64) int64 { return 0 })
	worst := rigclient.Delay(r, 2, 0, func(n int64) int64 { return n - 1 })

	if best != 500*time.Millisecond {
		t.Errorf("the shortest wait is %v, want half the window", best)
	}
	if worst != time.Second {
		t.Errorf("the longest wait is %v, want the whole window", worst)
	}
}

// The first retry goes out immediately, because the failure it most often fixes
// is a connection that was already closed — unless the server said when to come
// back, which answers the question.
func TestTheFirstRetryIsImmediateUnlessTheServerSaidOtherwise(t *testing.T) {
	r := rigclient.Retry{Base: time.Second, Cap: time.Second}
	never := func(int64) int64 { t.Fatal("the server's interval was jittered"); return 0 }

	if got := rigclient.Delay(r, 1, 0, never); got != 0 {
		t.Errorf("the first retry waited %v, want none", got)
	}
	if got := rigclient.Delay(r, 1, 5*time.Second, never); got != 5*time.Second {
		t.Errorf("the first retry waited %v, want the server's five seconds", got)
	}
	// Whole, not clamped. Clamping would mean going back before the server said
	// to, which is the same request refused twice; declining it is what the two
	// cases above this one check.
	if got := rigclient.Delay(r, 3, time.Hour, never); got != time.Hour {
		t.Errorf("an hour became %v, want it handed back whole for the loop to decline", got)
	}
}

// hijack kills the connection without answering, which is what a client sees as
// a transport failure rather than as a status.
func hijack(t *testing.T, w http.ResponseWriter) {
	t.Helper()

	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Fatalf("hijacking: %v", err)
	}
	conn.(*net.TCPConn).SetLinger(0)
	conn.Close()
}

// brokenCredential fails before the request goes out.
type brokenCredential struct{ err error }

func (c brokenCredential) Apply(context.Context, *http.Request) error { return c.err }

// declining is a Reauthorizer that decides there is nothing it can do.
type declining struct{}

func (declining) Apply(_ context.Context, r *http.Request) error {
	r.Header.Set("Authorization", "Bearer stale")
	return nil
}

func (declining) Reauthorize(context.Context) (bool, error) { return false, nil }

// A 401 for a credential that is not a Reauthorizer never enters the
// reauthorization arm at all: it falls through with its body untouched and is
// read as an ordinary refusal below. The distinction matters because the arm it
// falls past is the one that reads the body — so anything that answered on its
// behalf would hand back a 401 with nothing on it, and IsUnauthorized would say
// no about a 401.
func TestA401ForACredentialThatCannotRefreshIsStillReadInFull(t *testing.T) {
	var seen atomic.Int32
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		seen.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"Unauthorized","message":"expired","requestId":"req-9"}`))
	}), rigclient.Config{Retry: quick, Credential: rigclient.StaticToken("rig_at_stale")})

	_, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	})

	if !rigclient.IsUnauthorized(err) {
		t.Fatalf("err = %v, want a refusal that answers IsUnauthorized", err)
	}
	var e *rigclient.Error
	if !errors.As(err, &e) {
		t.Fatalf("err = %v, want a typed refusal", err)
	}
	if e.RequestID != "req-9" {
		t.Errorf("requestId = %q, want the envelope to have been read", e.RequestID)
	}
	if n := seen.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want 1: a 401 nobody can answer is not retried", n)
	}
}

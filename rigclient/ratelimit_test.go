package rigclient_test

import (
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/simonjanss/rig/rigclient"
)

// budget answers with the RateLimit-* headers a rig server sends, counting down
// and refusing past the limit — the shape a caller watching its budget sees.
// retryAfter is what a refusal asks for. Empty omits the header, which leaves
// the client on its own backoff — the only way to exercise several attempts
// without a test that really sleeps for the interval a server would ask for.
func budget(limit int, retryAfter string) http.HandlerFunc {
	var (
		mu   sync.Mutex
		used int
	)
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		used++
		remaining := max(limit-used, 0)
		over := used > limit
		mu.Unlock()

		w.Header().Set("RateLimit-Limit", strconv.Itoa(limit))
		w.Header().Set("RateLimit-Remaining", strconv.Itoa(remaining))
		if over {
			w.Header().Set("RateLimit-Reset", "30")
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"code":"RateLimited","message":"too many attempts"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"1","title":"x"}`))
	}
}

// The whole point of the callback: the numbers arrive while the calls are still
// succeeding, so a caller can act before it is refused.
func TestOnRateLimitSeesTheBudgetBeforeItIsSpent(t *testing.T) {
	t.Parallel()

	var seen []rigclient.RateLimitStatus
	rt := newClient(t, budget(4, "30"), rigclient.Config{
		OnRateLimit: func(s rigclient.RateLimitStatus) { seen = append(seen, s) },
	})

	for range 3 {
		if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
			Name: "listTodos", Method: http.MethodGet, Path: "/todos",
		}); err != nil {
			t.Fatal(err)
		}
	}

	if len(seen) != 3 {
		t.Fatalf("the callback ran %d times for 3 calls", len(seen))
	}
	for i, s := range seen {
		if s.Limit != 4 {
			t.Errorf("call %d: limit %d", i+1, s.Limit)
		}
		if want := 3 - i; s.Remaining != want {
			t.Errorf("call %d: remaining %d, want %d", i+1, s.Remaining, want)
		}
		if s.Refused {
			t.Errorf("call %d was reported as refused", i+1)
		}
		if s.Op != "listTodos" {
			t.Errorf("call %d: op %q", i+1, s.Op)
		}
	}

	// The number worth alerting on.
	if got := seen[2].Fraction(); got != 0.75 {
		t.Errorf("after 3 of 4, Fraction() = %v, want 0.75", got)
	}
	if got := seen[2].Used(); got != 3 {
		t.Errorf("Used() = %d, want 3", got)
	}
}

func TestOnRateLimitReportsARefusal(t *testing.T) {
	t.Parallel()

	var last rigclient.RateLimitStatus
	rt := newClient(t, budget(1, "30"), rigclient.Config{
		Retry:       rigclient.Retry{Attempts: 1},
		OnRateLimit: func(s rigclient.RateLimitStatus) { last = s },
	})

	for range 2 {
		_, _ = rigclient.Do[todo](t.Context(), rt, rigclient.Op{
			Name: "listTodos", Method: http.MethodGet, Path: "/todos",
		})
	}

	if !last.Refused {
		t.Fatal("the refused response was not reported as refused")
	}
	if last.Remaining != 0 {
		t.Errorf("remaining = %d on a refusal", last.Remaining)
	}
	// Only stated on a refusal, which is why the allowed calls above leave it
	// zero and this one does not.
	if last.ResetAfter != 30*time.Second {
		t.Errorf("ResetAfter = %v, want 30s", last.ResetAfter)
	}
}

// A retried 429 spent the budget too, so it is an observation rather than
// something the retry erases.
func TestEveryAttemptIsObserved(t *testing.T) {
	t.Parallel()

	var calls int
	rt := newClient(t, budget(1, ""), rigclient.Config{
		Retry:       rigclient.Retry{Attempts: 3, Base: time.Millisecond},
		OnRateLimit: func(rigclient.RateLimitStatus) { calls++ },
	})

	_, _ = rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Name: "listTodos", Method: http.MethodGet, Path: "/todos",
	})
	_, _ = rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Name: "listTodos", Method: http.MethodGet, Path: "/todos",
	})

	// One for the call that went through, then one per attempt of the one that
	// did not.
	if calls < 3 {
		t.Fatalf("the callback ran %d times; retried attempts are not being observed", calls)
	}
}

// A server with no throttle block sends none of these headers, and a caller must
// not read a limit of zero out of that — zero remaining of zero would look like
// a budget entirely spent.
func TestAServerWithNoLimitSaysNothing(t *testing.T) {
	t.Parallel()

	var calls int
	rt := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"1","title":"x"}`))
	}), rigclient.Config{
		OnRateLimit: func(rigclient.RateLimitStatus) { calls++ },
	})

	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("the callback ran %d times against a server that sends no headers", calls)
	}
}

func TestFractionOfAnUnsetLimitDoesNotDivideByZero(t *testing.T) {
	t.Parallel()

	var zero rigclient.RateLimitStatus
	if got := zero.Fraction(); got != 0 {
		t.Errorf("Fraction() = %v on the zero value", got)
	}
	if got := zero.Used(); got != 0 {
		t.Errorf("Used() = %d on the zero value", got)
	}
}

// No callback is the ordinary case and must cost nothing and crash nothing.
func TestNoCallbackIsFine(t *testing.T) {
	t.Parallel()

	rt := newClient(t, budget(10, "30"), rigclient.Config{})
	if _, err := rigclient.Do[todo](t.Context(), rt, rigclient.Op{
		Method: http.MethodGet, Path: "/todos",
	}); err != nil {
		t.Fatal(err)
	}
}

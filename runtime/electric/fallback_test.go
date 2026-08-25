package electric_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/runtime/electric"
)

// nowhere is an address nothing answers on, which is what a sync service that
// is not running looks like from here.
const nowhere = "http://127.0.0.1:1"

// snapshotOf is the fallback used throughout: two rows and a schema, so that a
// test can tell an answer from this apart from an answer forwarded upstream.
func snapshotOf(rows ...string) electric.Fallback {
	return func(context.Context) (electric.Snapshot, error) {
		out := make([]electric.Row, 0, len(rows))
		for _, title := range rows {
			out = append(out, electric.Row{
				Key:   electric.RowKey("lesson", title),
				Value: map[string]any{"id": title, "title": title},
			})
		}
		return electric.Snapshot{
			Rows:   out,
			Schema: `{"id":{"type":"uuid"},"title":{"type":"text"}}`,
		}, nil
	}
}

// messages decodes a shape response.
func messages(t *testing.T, res *http.Response) []map[string]any {
	t.Helper()

	body, _ := io.ReadAll(res.Body)
	var out []map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return out
}

// The whole point: a subscriber whose sync service is gone gets rows.
func TestAFallbackAnswersWhenTheSyncServiceIsGone(t *testing.T) {
	t.Parallel()

	p, _ := electric.New(electric.Config{URL: nowhere})
	res := serve(t, p, electric.Shape{
		Table:    "lesson",
		Fallback: snapshotOf("one", "two"),
	}, "offset=-1")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", res.StatusCode)
	}

	out := messages(t, res)
	if len(out) != 3 {
		t.Fatalf("got %d messages, want two rows and a control message", len(out))
	}
	if got := out[0]["key"]; got != `"public"."lesson"/"one"` {
		t.Errorf("key = %v", got)
	}
	if got := out[0]["value"].(map[string]any)["title"]; got != "one" {
		t.Errorf("title = %v", got)
	}
	headers := out[0]["headers"].(map[string]any)
	if headers["operation"] != "insert" {
		t.Errorf("operation = %v, want insert", headers["operation"])
	}
	if got := headers["relation"].([]any); got[0] != "public" || got[1] != "lesson" {
		t.Errorf("relation = %v", got)
	}

	// The control message is what makes a subscriber ready rather than still
	// loading, so its absence would be a collection that never renders.
	last := out[len(out)-1]["headers"].(map[string]any)
	if last["control"] != "up-to-date" {
		t.Errorf("the response does not end up-to-date: %v", last)
	}
}

// A subscriber picks a parser per column from the schema header, so a snapshot
// without one decodes every value as the string it arrived as.
func TestASnapshotCarriesTheCursorAndTheSchema(t *testing.T) {
	t.Parallel()

	p, _ := electric.New(electric.Config{URL: nowhere})
	res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}, "")

	if got := res.Header.Get("electric-schema"); !strings.Contains(got, `"title"`) {
		t.Errorf("electric-schema = %q", got)
	}
	if got := res.Header.Get("electric-offset"); got == "" {
		t.Error("no electric-offset, so a subscriber has nothing to poll from")
	}
	if got := res.Header.Get("electric-handle"); !strings.HasPrefix(got, "rig-fallback-") {
		t.Errorf("electric-handle = %q, want one this proxy can recognise later", got)
	}
	if got := res.Header.Get("X-Rig-Sync-Fallback"); got != "snapshot" {
		t.Errorf("X-Rig-Sync-Fallback = %q", got)
	}
	if got := res.Header.Get("electric-has-data"); got != "true" {
		t.Errorf("electric-has-data = %q", got)
	}
	// Empty, the way the sync service sends it, and present is the whole point:
	// a handle and an offset without it is a response a client's chunk buffer
	// reads as having another chunk behind it, and it asks for one.
	if _, ok := res.Header["Electric-Up-To-Date"]; !ok {
		t.Error("no electric-up-to-date, so a subscriber asks for a chunk that does not exist")
	}
}

// Two subscriptions served during one outage are not the same shape, and a
// handle that said they were would let one of them resume into the other.
func TestEachSnapshotGetsItsOwnHandle(t *testing.T) {
	t.Parallel()

	p, _ := electric.New(electric.Config{URL: nowhere})
	shape := electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}

	first := serve(t, p, shape, "").Header.Get("electric-handle")
	second := serve(t, p, shape, "").Header.Get("electric-handle")
	if first == second {
		t.Errorf("both snapshots claim to be shape %q", first)
	}
}

// A sync service answering with its own failure is as unreachable as one that
// does not answer.
func TestAFiveHundredFallsBackToo(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	up.status = http.StatusInternalServerError
	p, _ := electric.New(electric.Config{URL: up.srv.URL})

	res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}, "")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the snapshot", res.StatusCode)
	}
	if res.Header.Get("X-Rig-Sync-Fallback") == "" {
		t.Error("the answer came from upstream, which had failed")
	}
}

// A 4xx is a decision about this shape. Answering it from somewhere else would
// hide a filter being refused.
func TestAFourHundredIsForwardedRatherThanAnswered(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	up.status = http.StatusForbidden
	p, _ := electric.New(electric.Config{URL: up.srv.URL})

	res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}, "")
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want the refusal forwarded", res.StatusCode)
	}
}

// A live poll asks what changed. A snapshot is not a smaller answer to that
// question; it is a different question answered, with no way to tell.
func TestALivePollIsNeverAnsweredWithASnapshot(t *testing.T) {
	t.Parallel()

	p, _ := electric.New(electric.Config{URL: nowhere})
	for _, query := range []string{
		"offset=0_inf&handle=the-handle&live=true",
		"offset=0_inf&handle=the-handle",
		"offset=0_0&handle=the-handle",
	} {
		res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}, query)
		if res.StatusCode != http.StatusBadGateway {
			t.Errorf("?%s: status = %d, want 502", query, res.StatusCode)
		}
	}
}

// A subscriber holding a snapshot already has the rows another one would send.
// What it is missing is the sync service, and 503 with a Retry-After is how to
// say so without asking it to throw away what it has.
func TestASubscriberHoldingASnapshotIsAskedToWait(t *testing.T) {
	t.Parallel()

	p, _ := electric.New(electric.Config{URL: nowhere})
	shape := electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}

	handle := serve(t, p, shape, "").Header.Get("electric-handle")
	res := serve(t, p, shape, "offset=0_inf&handle="+handle+"&live=true")

	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After, which leaves a subscriber to guess")
	}
}

// And the recovery that follows it is the sync service's own: a handle it never
// issued is a must-refetch, which is what resets a subscriber onto real sync.
// This proxy does nothing but forward it.
func TestARecoveredSyncServiceRefusesTheFallbackHandleItself(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	up.status = http.StatusConflict
	up.body = `[{"headers":{"control":"must-refetch"}}]`
	p, _ := electric.New(electric.Config{URL: up.srv.URL})

	res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf("one")},
		"offset=0_inf&handle=rig-fallback-1&live=true")

	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want the 409 forwarded", res.StatusCode)
	}
	if got := up.query.Get("handle"); got != "rig-fallback-1" {
		t.Errorf("the handle reached upstream as %q", got)
	}
	out := messages(t, res)
	if out[0]["headers"].(map[string]any)["control"] != "must-refetch" {
		t.Errorf("the reset did not come back: %v", out)
	}
}

// A sync service that is running and not answering is the outage a transport
// error does not catch, and the one a fallback is most useful for.
func TestAHungSyncServiceFallsBack(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	up.block = make(chan struct{})
	t.Cleanup(func() { close(up.block) })

	p, _ := electric.New(electric.Config{URL: up.srv.URL, InitialTimeout: 50 * time.Millisecond})
	res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}, "")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the snapshot", res.StatusCode)
	}
	if res.Header.Get("X-Rig-Sync-Fallback") == "" {
		t.Error("the wait was not given up on")
	}
}

// The deadline bounds the wait and not the transfer. A body that takes longer
// than the deadline to arrive is still the sync service answering, and cutting
// one goes out under a 200 already written: half a response, which a subscriber
// cannot tell from a whole one and no fallback can rescue.
func TestASlowInitialBodyIsNotCutInHalf(t *testing.T) {
	t.Parallel()

	// Headers and a first row at once, then a pause longer than the deadline,
	// then the rest — a large shape over a slow connection, in miniature.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"key":"1","value":{"id":"1"}}`))
		_ = http.NewResponseController(w).Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`,{"headers":{"control":"up-to-date"}}]`))
	}))
	t.Cleanup(up.Close)

	p, _ := electric.New(electric.Config{URL: up.URL, InitialTimeout: 100 * time.Millisecond})
	res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf("one")}, "offset=-1")

	if got := res.Header.Get("X-Rig-Sync-Fallback"); got != "" {
		t.Fatalf("the fallback answered a request the sync service was answering: %q", got)
	}
	out := messages(t, res)
	if len(out) != 2 {
		t.Fatalf("got %d messages, want the row and the control message", len(out))
	}
	if last := out[len(out)-1]["headers"].(map[string]any); last["control"] != "up-to-date" {
		t.Errorf("the response was cut before it ended: %v", out)
	}
}

// The deadline is for the one request that is not supposed to hang. A live poll
// hangs on purpose, and cutting it would end every subscription on an interval.
func TestTheDeadlineDoesNotReachALivePoll(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	up.block = make(chan struct{})

	p, _ := electric.New(electric.Config{URL: up.srv.URL, InitialTimeout: 50 * time.Millisecond})

	done := make(chan int, 1)
	go func() {
		done <- serve(t, p, electric.Shape{Table: "lesson"},
			"offset=0_inf&handle=the-handle&live=true").StatusCode
	}()

	select {
	case status := <-done:
		t.Fatalf("the poll was cut short with %d", status)
	case <-time.After(300 * time.Millisecond):
	}
	close(up.block)
	if status := <-done; status != http.StatusOK {
		t.Errorf("status = %d", status)
	}
}

// A fallback that refuses is not a fallback. The subscriber is told the sync
// service is unavailable, which is true, rather than given half an answer.
func TestARefusedFallbackIsABadGateway(t *testing.T) {
	t.Parallel()

	p, _ := electric.New(electric.Config{URL: nowhere})
	res := serve(t, p, electric.Shape{
		Table: "lesson",
		Fallback: func(context.Context) (electric.Snapshot, error) {
			return electric.Snapshot{}, errors.New("the database is gone too")
		},
	}, "")

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
}

// Both failures a shape endpoint hides reach somebody.
func TestOnErrorSeesWhyTheAnswerWasNotTheSyncServices(t *testing.T) {
	t.Parallel()

	var seen []string
	p, _ := electric.New(electric.Config{
		URL:     nowhere,
		OnError: func(_ context.Context, err error) { seen = append(seen, err.Error()) },
	})

	serve(t, p, electric.Shape{
		Table: "lesson",
		Fallback: func(context.Context) (electric.Snapshot, error) {
			return electric.Snapshot{}, errors.New("the database is gone too")
		},
	}, "")

	if len(seen) != 2 {
		t.Fatalf("got %d errors, want the outage and the refusal: %v", len(seen), seen)
	}
	if !strings.Contains(seen[1], "the database is gone too") {
		t.Errorf("the refusal did not reach OnError: %q", seen[1])
	}
}

// While the sync service answers, nothing else is consulted. A fallback that
// ran alongside a working subscription would be a query per poll.
func TestTheFallbackIsNotConsultedWhileTheSyncServiceAnswers(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	p, _ := electric.New(electric.Config{URL: up.srv.URL})

	called := false
	res := serve(t, p, electric.Shape{
		Table: "lesson",
		Fallback: func(context.Context) (electric.Snapshot, error) {
			called = true
			return electric.Snapshot{}, nil
		},
	}, "")

	if res.StatusCode != http.StatusOK || called {
		t.Errorf("status = %d, fallback called = %v", res.StatusCode, called)
	}
}

// An empty shape is an answer, not a failure: a subscriber has to be able to
// tell "no rows" from "no sync service".
func TestAnEmptySnapshotIsStillAnAnswer(t *testing.T) {
	t.Parallel()

	p, _ := electric.New(electric.Config{URL: nowhere})
	res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf()}, "")

	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", res.StatusCode)
	}
	if got := res.Header.Get("electric-has-data"); got != "false" {
		t.Errorf("electric-has-data = %q, want false", got)
	}
	if out := messages(t, res); len(out) != 1 {
		t.Errorf("got %d messages, want only the control message", len(out))
	}
}

func TestRowKey(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		table string
		key   []string
		want  string
	}{
		{"lesson", []string{"1"}, `"public"."lesson"/"1"`},
		{"teaching.lesson", []string{"1"}, `"teaching"."lesson"/"1"`},
		{"lesson", []string{"1", "2"}, `"public"."lesson"/"1"/"2"`},
		{"lesson", nil, `"public"."lesson"`},
		{`odd"name`, []string{"1"}, `"public"."odd""name"/"1"`},
	} {
		if got := electric.RowKey(c.table, c.key...); got != c.want {
			t.Errorf("RowKey(%q, %q) = %s, want %s", c.table, c.key, got, c.want)
		}
	}
}

// A snapshot is built whole and in memory, and every subscriber builds one at
// the same moment, because what they have in common is the outage. Past the
// bound it is refused rather than sent short: a subscriber cannot tell a
// truncated collection from a table that lost half its rows.
func TestASnapshotPastItsBoundIsRefusedRatherThanTruncated(t *testing.T) {
	t.Parallel()

	var seen []string
	p, _ := electric.New(electric.Config{
		URL:             nowhere,
		MaxSnapshotRows: 2,
		OnError:         func(_ context.Context, err error) { seen = append(seen, err.Error()) },
	})

	res := serve(t, p, electric.Shape{
		Table:    "lesson",
		Fallback: snapshotOf("one", "two", "three"),
	}, "")

	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", res.StatusCode)
	}
	if len(seen) != 2 || !strings.Contains(seen[1], "past the 2 it may send") {
		t.Errorf("the bound was not reported: %v", seen)
	}

	// And exactly at the bound it is sent.
	if got := serve(t, p, electric.Shape{
		Table:    "lesson",
		Fallback: snapshotOf("one", "two"),
	}, "").StatusCode; got != http.StatusOK {
		t.Errorf("a snapshot at the bound answered %d", got)
	}
}

// A project whose shapes are large enough to be worth the memory can say so.
func TestTheBoundCanBeTurnedOff(t *testing.T) {
	t.Parallel()

	p, _ := electric.New(electric.Config{URL: nowhere, MaxSnapshotRows: -1})
	res := serve(t, p, electric.Shape{Table: "lesson", Fallback: snapshotOf("one", "two", "three")}, "")

	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d", res.StatusCode)
	}
}

// A failure the sync service chose is forwarded when there is nothing to answer
// with instead. Its status and its Retry-After say more than a 502 substituted
// for them, and this is what every shape did before there were fallbacks.
func TestWithNothingToFallBackToTheSyncServicesOwnFailureIsForwarded(t *testing.T) {
	t.Parallel()

	up := newUpstream(t)
	up.status = http.StatusServiceUnavailable
	p, _ := electric.New(electric.Config{URL: up.srv.URL})

	if got := serve(t, p, electric.Shape{Table: "lesson"}, "").StatusCode; got != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want the 503 forwarded", got)
	}
}

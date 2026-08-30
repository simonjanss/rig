package observe_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/simonjanss/rig/observe"
)

// checks is the checks.json body, decoded.
type checks struct {
	Reason string `json:"reason"`
	Checks []struct {
		Name   string `json:"name"`
		OK     bool   `json:"ok"`
		Detail string `json:"detail"`
		Millis int64  `json:"ms"`
	} `json:"checks"`
}

func readChecks(t *testing.T, mux *http.ServeMux, base string) checks {
	t.Helper()

	res := get(t, mux, base+"/checks.json", true)
	if res.Code != http.StatusOK {
		t.Fatalf("checks.json = %d, want 200", res.Code)
	}
	var out checks
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestChecksReportEachDependency(t *testing.T) {
	mux, base := mount(t, nil, observe.PageConfig{
		ServiceName: "todo",
		Password:    password,
		Checks: []observe.Check{
			{Name: "database", Probe: func(context.Context) error { return nil }},
			{Name: "sync service", Probe: func(context.Context) error {
				return errors.New("electric: the sync service answered 503")
			}},
		},
	})

	out := readChecks(t, mux, base)
	if len(out.Checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(out.Checks))
	}

	byName := map[string]bool{}
	detail := map[string]string{}
	for _, c := range out.Checks {
		byName[c.Name] = c.OK
		detail[c.Name] = c.Detail
	}
	if !byName["database"] {
		t.Error("database should be ok")
	}
	if byName["sync service"] {
		t.Error("the sync service should be failing")
	}
	// The reason, not just the fact. A pill has room for a name; the cause is
	// the half somebody actually needs.
	if detail["sync service"] == "" {
		t.Error("a failing check should carry the error's text")
	}
	if detail["database"] != "" {
		t.Errorf("a passing check should carry no detail, got %q", detail["database"])
	}
}

// The page's answer is a report and never a verdict. serve's ReadinessPath is
// the endpoint an orchestrator acts on, and it has its own opinion about which
// dependencies are worth taking an instance out of rotation for.
func TestChecksAlwaysAnswerOK(t *testing.T) {
	mux, base := mount(t, nil, observe.PageConfig{
		ServiceName: "todo",
		Password:    password,
		Checks: []observe.Check{
			{Name: "database", Probe: func(context.Context) error { return errors.New("gone") }},
		},
	})

	if got := get(t, mux, base+"/checks.json", true).Code; got != http.StatusOK {
		t.Errorf("checks.json = %d with a failing dependency, want 200", got)
	}
}

// A probe that hangs is a probe that has already answered. The page would
// rather show a dependency as failing than show nothing while it waits.
func TestChecksAreBounded(t *testing.T) {
	mux, base := mount(t, nil, observe.PageConfig{
		ServiceName:  "todo",
		Password:     password,
		CheckTimeout: 50 * time.Millisecond,
		Checks: []observe.Check{
			{Name: "slow", Probe: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			}},
			{Name: "quick", Probe: func(context.Context) error { return nil }},
		},
	})

	began := time.Now()
	out := readChecks(t, mux, base)
	if took := time.Since(began); took > 2*time.Second {
		t.Fatalf("took %s: the round was not bounded", took)
	}
	if len(out.Checks) != 2 {
		t.Fatalf("got %d checks, want both: one slow dependency should not lose the others", len(out.Checks))
	}
	// Concurrent, so the quick one is not queued behind the slow one.
	for _, c := range out.Checks {
		if c.Name == "quick" && !c.OK {
			t.Error("the quick dependency should have answered")
		}
	}
}

// And bounded by the budget rather than by the probes, which is the only
// version of it worth having: a dependency whose client takes no context is
// exactly the one that hangs, and waiting for it would hold the page's poll
// open and stop everything else on it refreshing too.
func TestChecksAreBoundedByABudgetAndNotByTheProbe(t *testing.T) {
	blocked := make(chan struct{})
	t.Cleanup(func() { close(blocked) })

	mux, base := mount(t, nil, observe.PageConfig{
		ServiceName:  "todo",
		Password:     password,
		CheckTimeout: 50 * time.Millisecond,
		Checks: []observe.Check{
			// The context is ignored, the way a Ping that never took one does.
			{Name: "deaf", Probe: func(context.Context) error {
				<-blocked
				return nil
			}},
			{Name: "quick", Probe: func(context.Context) error { return nil }},
		},
	})

	began := time.Now()
	out := readChecks(t, mux, base)
	if took := time.Since(began); took > time.Second {
		t.Fatalf("took %s: a probe that ignores its context was waited on", took)
	}
	if len(out.Checks) != 2 {
		t.Fatalf("got %d checks, want both", len(out.Checks))
	}

	byName := map[string]struct {
		ok     bool
		detail string
	}{}
	for _, c := range out.Checks {
		byName[c.Name] = struct {
			ok     bool
			detail string
		}{c.OK, c.Detail}
	}
	if byName["deaf"].ok {
		t.Error("the deaf dependency answered nothing and should not be ok")
	}
	// Answered for rather than left blank: the pill says why it is red.
	if byName["deaf"].detail == "" {
		t.Error("a dependency that ran out of budget should say so")
	}
	if !byName["quick"].ok {
		t.Error("the quick dependency should have answered")
	}
}

// Watch is how rig registers these, because the page is built by NewProcess —
// before there is a pool to ping or a sync proxy to ask.
func TestWatchRegistersAfterThePageIsBuilt(t *testing.T) {
	page, err := (*observe.Provider)(nil).Page(observe.PageConfig{
		ServiceName: "todo",
		Password:    password,
		Addr:        testAddr,
	})
	if err != nil {
		t.Fatal(err)
	}
	page.Watch("database", func(context.Context) error { return nil })
	page.Watch("nothing", nil)

	mux := http.NewServeMux()
	page.Mount(mux)

	out := readChecks(t, mux, observe.DefaultMonitorPath)
	if len(out.Checks) != 1 {
		t.Fatalf("got %d checks, want 1: a nil probe registers nothing", len(out.Checks))
	}
	if out.Checks[0].Name != "database" || !out.Checks[0].OK {
		t.Errorf("check = %+v", out.Checks[0])
	}
}

// Safe on a nil page, so wiring that registers a probe does not first have to
// find out whether this environment armed the page at all.
func TestWatchOnANilPage(t *testing.T) {
	var page *observe.Page
	page.Watch("database", func(context.Context) error { return nil })
}

// A page with nothing registered says so, rather than showing an empty row that
// reads as "no dependencies, all fine".
func TestChecksSayWhenNothingIsRegistered(t *testing.T) {
	mux, base := mount(t, nil, observe.PageConfig{ServiceName: "todo", Password: password})

	out := readChecks(t, mux, base)
	if len(out.Checks) != 0 {
		t.Fatalf("got %d checks, want 0", len(out.Checks))
	}
	if out.Reason == "" {
		t.Error("a page with no checks should say why it is showing none")
	}
}

// The same guard as everything else here: this endpoint names the dependencies
// of the server and how long each takes to answer.
func TestChecksNeedThePassword(t *testing.T) {
	mux, base := mount(t, nil, observe.PageConfig{
		ServiceName: "todo",
		Password:    password,
		Checks:      []observe.Check{{Name: "database", Probe: func(context.Context) error { return nil }}},
	})

	if got := get(t, mux, base+"/checks.json", false).Code; got != http.StatusUnauthorized {
		t.Errorf("checks.json without the password = %d, want 401", got)
	}
}

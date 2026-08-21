//go:build docker

// What rig's monitoring page shows, over spans a real database produced.
//
// The handler is tested in the observe module against files written by hand.
// This is the other end: a write against Postgres, exported through the same
// provider a main builds, read back by the page without anything in between
// agreeing on a format. It is the test that would fail if the exporter and the
// reader ever stopped meaning the same thing by a file.
package store_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/simonjanss/rig/examples/fantasyfootball/internal/model"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/tenancy"
)

const monitorPassword = "correct horse battery"

// A write happens, and it is on the page: one trace, with the stage spans and
// the statement under it.
func TestTheMonitoringPageShowsARealWrite(t *testing.T) {
	w := newTraced(t)

	_, err := w.repos.Teams.Create(w.ctx, dbhook.Create[model.TeamCreateInput, model.Team]{
		Input: model.TeamCreateInput{Name: "Rovers", IsActive: true},
		Hooks: dbhook.CreateHooks[model.TeamCreateInput, model.Team]{
			Before: func(context.Context, tenancy.Claims, *model.TeamCreateInput) error { return nil },
			After:  func(context.Context, tenancy.Claims, *model.Team) error { return nil },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Export is batched; shutting down is what empties the batch, and the page
	// reads a file rather than the provider's buffer.
	w.stop(t)

	page, err2 := w.provider.Page(observe.PageConfig{
		ServiceName: "fantasyfootball", Password: monitorPassword,
	})
	if err2 != nil {
		t.Fatal(err2)
	}
	if why := page.Unarmed(); why != "" {
		t.Fatalf("the page will serve nothing: %s", why)
	}

	mux := http.NewServeMux()
	page.Mount(mux)

	// Without the password there is no page. It lists paths, request ids and
	// the cause of every 500, which is a record of what every caller did.
	bare := httptest.NewRecorder()
	mux.ServeHTTP(bare, httptest.NewRequest(http.MethodGet, observe.DefaultMonitorPath+"/traces.json", nil))
	if bare.Code != http.StatusUnauthorized {
		t.Errorf("the page answered %d without the password, want 401", bare.Code)
	}

	r := httptest.NewRequest(http.MethodGet, observe.DefaultMonitorPath+"/traces.json", nil)
	r.SetBasicAuth("rig", monitorPassword)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, r)
	if res.Code != http.StatusOK {
		t.Fatalf("traces.json = %d, want 200", res.Code)
	}

	var body struct {
		Service string                `json:"service"`
		Reason  string                `json:"reason"`
		Traces  []observe.TraceRecord `json:"traces"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Reason != "" {
		t.Fatalf("the page says it has nothing to show: %s", body.Reason)
	}
	if len(body.Traces) != 1 {
		t.Fatalf("want the one write, got %d traces", len(body.Traces))
	}
	if body.Service != "fantasyfootball" {
		t.Errorf("service = %q", body.Service)
	}

	// The stages and the statement, under the call. The file is flat — one line
	// per finished span — so this is the grouping the page adds over it, and
	// the reason it is worth having a page rather than a grep.
	trace := body.Traces[0]
	byName := map[string]observe.SpanRecord{}
	for _, span := range trace.Spans {
		byName[span.Name] = span
	}

	create, ok := byName["repository.Team.Create"]
	if !ok {
		t.Fatalf("the page is missing the create itself; it has %v", names(byName))
	}
	// No root: this is a repository call rather than a request, so there is no
	// handler span above it. The page shows such a trace anyway, by id, which
	// is also what a trace whose beginning has rotated away looks like.
	if trace.Root != nil && trace.Root.Name != "repository.Team.Create" {
		t.Errorf("the trace's root is %q", trace.Root.Name)
	}

	for _, want := range []string{"repository.Team.Create.Before", "repository.Team.Create.After", "INSERT team"} {
		got, ok := byName[want]
		if !ok {
			t.Errorf("the page is missing %q; it has %v", want, names(byName))
			continue
		}
		if got.ParentID != create.SpanID {
			t.Errorf("%s is not under the create on the page: parent %q, create %q",
				want, got.ParentID, create.SpanID)
		}
	}
	if trace.DurationMS <= 0 {
		t.Errorf("the trace has no duration: %v", trace.DurationMS)
	}
}

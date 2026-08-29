package compile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/simonjanss/rig/internal/compile"
	"github.com/simonjanss/rig/internal/diag"
	"github.com/simonjanss/rig/internal/project"
	"github.com/simonjanss/rig/internal/tableconf"
	"github.com/simonjanss/rig/pkg/ir"
)

// presenceResource is rig_presence out of the fixture, compiled.
func presenceResource(t *testing.T) *ir.Resource {
	t.Helper()

	doc, diags := compileFixture(t, filepath.Join("testdata", "presence"))
	if hasErrors(diags) {
		t.Fatalf("the presence fixture does not compile:\n%s", renderDiagnostics(diags))
	}
	res := doc.ResourceForTable("rig_presence")
	if res == nil {
		t.Fatal("rig_presence is not a resource; presence.enabled is what keeps it in the schema")
	}
	return res
}

// TestPresenceIsNotOwnerScoped is the hole this feature could most easily open,
// and the reason it is a named test rather than a line in a golden file.
//
// On an owner-scoped table server-go emits
// `where.Eq(owner, claims.AccountID)` into the shape before any application scope
// runs, and there is no `?scope=all` for a stream. So a presence table with an
// owner would stream every subscriber nothing but themselves — presence that is
// silently a mirror. There is no error, no refused request and no configuration
// to blame: it presents as "presence does not work".
//
// A golden file would catch a change to this too, and it would catch it as one
// line of JSON moving among two thousand. This says what breaks.
func TestPresenceIsNotOwnerScoped(t *testing.T) {
	t.Parallel()

	res := presenceResource(t)
	if res.Storage == nil {
		t.Fatal("rig_presence has no storage")
	}
	if res.Storage.Owner != nil {
		t.Fatalf("rig_presence is owner-scoped on %q: the live shape would carry "+
			"account_id = the caller's, and every subscriber would see only itself",
			res.Storage.Owner.Name)
	}
	if res.Storage.IsOwnerScoped() {
		t.Fatal("rig_presence reports itself owner-scoped")
	}
}

// TestPresenceGetsAShapeWithoutBeingExposed. The shape is how presence is read at
// all, so it cannot depend on `expose` — and a stream is deliberately not gated
// on operations, which is the carve-out the inbox already relies on.
func TestPresenceGetsAShapeWithoutBeingExposed(t *testing.T) {
	t.Parallel()

	res := presenceResource(t)
	if !res.Unexposed {
		t.Error("the fixture sets expose: false and the resource is exposed anyway")
	}
	if len(res.Operations) != 0 {
		t.Errorf("an unexposed presence table has operations %v", res.Operations)
	}
	if res.Electric == nil {
		t.Fatal("rig_presence has no live shape, which is the only way it is read")
	}
	if res.Electric.Auth != ir.ElectricAuthTenant {
		t.Errorf("the shape is %q-authenticated, want %q", res.Electric.Auth, ir.ElectricAuthTenant)
	}
}

// TestTheShapeCanBeNarrowedToAScreen. A tenant-wide presence stream sends every
// heartbeat of every tab to every tab, which is quadratic in people; these two
// params are how a subscriber pays for one screen instead.
func TestTheShapeCanBeNarrowedToAScreen(t *testing.T) {
	t.Parallel()

	res := presenceResource(t)
	want := map[string]string{"scope": "String", "target_id": "UUID"}
	if len(res.Electric.Params) != len(want) {
		t.Fatalf("the shape declares %d params, want %d: %+v",
			len(res.Electric.Params), len(want), res.Electric.Params)
	}
	for _, p := range res.Electric.Params {
		typ, ok := want[p.Name]
		if !ok {
			t.Errorf("the shape declares an unexpected param %q", p.Name)
			continue
		}
		if p.Type != typ {
			t.Errorf("param %q is %s, want %s", p.Name, p.Type, typ)
		}
		if !p.Optional {
			t.Errorf("param %q is required; a subscriber that wants the whole tenant "+
				"should not have to say so", p.Name)
		}
		if p.Field == "" {
			t.Errorf("param %q has no Go identifier, so the generated struct field "+
				"would be unnamed", p.Name)
		}
	}
}

// TestThePresenceShapeCannotBeConfiguredAFallback. rig decides that presence
// answers a sync outage with nothing rather than with ghosts, and the decision
// has to survive a project having an opinion about this table's shape at all:
// applyElectricConfig replaces the endpoint rather than merging into it, so a
// configuration asking for anything else would otherwise carry the default in
// with it. It fails as a room full of people who left, which is exactly the
// failure nobody reports.
func TestThePresenceShapeCannotBeConfiguredAFallback(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("testdata", "presence")
	src, err := os.ReadFile(filepath.Join(dir, "rig.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, pdiags := project.Parse(filepath.Join(dir, "rig.yaml"), src)
	if pdiags.HasErrors() {
		t.Fatalf("the presence fixture's project does not parse:\n%s", pdiags.String())
	}

	path := filepath.Join(t.TempDir(), "rig_presence.yaml")
	if err := os.WriteFile(path, []byte("table: rig_presence\nelectric:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set, tdiags := tableconf.LoadDir([]string{path})
	if tdiags.HasErrors() {
		t.Fatalf("the table configuration does not load:\n%s", tdiags.String())
	}

	doc, _ := compile.Compile(readSchema(t, filepath.Join(dir, "schema.json")), set, compile.Options{
		Project:    p,
		Tool:       "rig (test)",
		Foundation: readFoundation(t, dir),
	})
	res := doc.ResourceForTable("rig_presence")
	if res == nil || res.Electric == nil {
		t.Fatal("rig_presence has no live shape")
	}
	if res.Electric.Fallback {
		t.Error("a table configuration turned the presence fallback on: a sync outage " +
			"would freeze the room rather than empty it")
	}
}

// TestThePresenceShapeStreamsNoLifecycleColumns is the columns-are-the-rule
// property, read from the other end: rig_presence has no deleted_at and no
// snapshot triple, so it gets one route and not three.
func TestThePresenceShapeStreamsNoLifecycleColumns(t *testing.T) {
	t.Parallel()

	res := presenceResource(t)
	if res.Storage.IsSoftDeletable() {
		t.Error("rig_presence is soft-deletable; a retired presence row is a ghost the " +
			"sync service goes on streaming")
	}
	if res.Storage.IsSnapshotable() {
		t.Error("rig_presence keeps snapshots; a durable history of who looked at which " +
			"field is a surveillance log nobody asked for")
	}
}

// TestTheSweepersNumbersAreNotInTheRevision, and the browser's are.
//
// Two of the four numbers come back on every heartbeat, so a client built when
// the TTL was a minute behaves differently against twenty seconds — that is what
// a revision is for. The sweeper's interval and its grace period appear in no
// response and no specification, so spending a revision on them would tell every
// client it was built against something older than the server, over a change none
// of them can see.
func TestTheSweepersNumbersAreNotInTheRevision(t *testing.T) {
	t.Parallel()

	base := func() *ir.Document {
		doc, diags := compileFixture(t, filepath.Join("testdata", "presence"))
		if hasErrors(diags) {
			t.Fatalf("the presence fixture does not compile:\n%s", renderDiagnostics(diags))
		}
		return doc
	}

	before := base()
	want, err := before.Hash()
	if err != nil {
		t.Fatal(err)
	}

	// Invisible to every client: the hash must not move.
	for _, tc := range []struct {
		name  string
		apply func(*ir.Presence)
	}{
		{"the sweep interval", func(p *ir.Presence) { p.SweepSeconds = 999 }},
		{"the grace period", func(p *ir.Presence) { p.GraceSeconds = 999 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := base()
			tc.apply(doc.API.Presence)
			got, err := doc.Hash()
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("changing %s moved the revision, and no client can tell it "+
					"changed", tc.name)
			}
		})
	}

	// Answered to the browser on every beat: the hash has to move.
	for _, tc := range []struct {
		name  string
		apply func(*ir.Presence)
	}{
		{"the TTL", func(p *ir.Presence) { p.TTLSeconds = 999 }},
		{"the heartbeat interval", func(p *ir.Presence) { p.HeartbeatSeconds = 999 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := base()
			tc.apply(doc.API.Presence)
			got, err := doc.Hash()
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Errorf("changing %s did not move the revision, and every client reads "+
					"it off a heartbeat", tc.name)
			}
		})
	}
}

// TestHashingDoesNotMutateTheDocument. Hash takes a shallow copy, so clearing two
// fields of a pointed-to struct has to go through a copy of that struct as well —
// otherwise asking for a hash erases the caller's sweep interval.
func TestHashingDoesNotMutateTheDocument(t *testing.T) {
	t.Parallel()

	doc, _ := compileFixture(t, filepath.Join("testdata", "presence"))
	sweep, grace := doc.API.Presence.SweepSeconds, doc.API.Presence.GraceSeconds
	if sweep == 0 || grace == 0 {
		t.Fatal("the fixture resolves no sweep or grace, so this test proves nothing")
	}

	if _, err := doc.Hash(); err != nil {
		t.Fatal(err)
	}
	if doc.API.Presence.SweepSeconds != sweep || doc.API.Presence.GraceSeconds != grace {
		t.Errorf("hashing erased the sweeper's numbers: sweep %d→%d, grace %d→%d",
			sweep, doc.API.Presence.SweepSeconds, grace, doc.API.Presence.GraceSeconds)
	}
}

// hasErrors is the severity question this file asks twice, and the fixture list
// is a slice rather than a diag.List so it is asked here.
func hasErrors(entries []diag.Diagnostic) bool {
	for _, d := range entries {
		if d.Severity == diag.SeverityError {
			return true
		}
	}
	return false
}

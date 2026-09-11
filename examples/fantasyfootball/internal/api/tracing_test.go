// What a traced request actually produces, end to end through the generated
// mux.
//
// The generator suites prove that Register registers through the tracing router
// and that what it emits compiles; the runtime suite proves that router names a
// span by the pattern each route was registered under. Only a run proves the
// two halves meet: a real provider, the real generated Register, and a span in
// a file with the route on it.
//
// No database and no Docker. Register wants a pool for the idempotency records
// and the routes here never write one, so a stand-in that would fail if
// anything opened a transaction is exactly the right amount of database.
package api_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/simonjanss/rig/examples/fantasyfootball/internal/api"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/httpx"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// noPool satisfies the pool Register asks for without being one. Nothing these
// routes do reaches it, and if something starts to, this says so.
type noPool struct{}

func (noPool) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("no database in this test")
}

// noTeam registers the generated Team routes without being able to serve one.
// The request below never reaches a method: an identifier that is not a UUID is
// refused while the path is being decoded, which is a route that answers
// without a database behind it.
type noTeam struct{ api.TeamService }

// Link hands every service the children that cascade off it, before any route
// is mounted. Embedding the interface would answer this with a nil panic.
func (noTeam) AdoptChildren(api.TeamChildDeletes) {}

// mountedAuth stands in for rig/auth: it answers who is calling, and it mounts a
// route of its own. That second half is the whole point — a route registered by
// Auth.Mount is the case that used to have no span, because the generator that
// opened the spans never saw it.
type mountedAuth struct{}

func (mountedAuth) Claims(*http.Request) (tenancy.Claims, error) {
	return tenancy.Claims{}, nil
}

func (mountedAuth) Mount(mux httpx.Router) {
	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

func TestEveryRouteOnTheMuxLandsInTheSpanFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spans.jsonl")

	provider, err := observe.Setup(t.Context(), observe.Config{ServiceName: "fantasyfootball", File: path})
	if err != nil {
		t.Fatal(err)
	}

	mux := api.Register(api.Handlers{
		Server: api.Server{Auth: mountedAuth{}, DB: noPool{}},
		Team:   noTeam{},
	})

	for _, r := range []*http.Request{
		// Mounted by Auth.Mount, which is the route this test exists for.
		httptest.NewRequest(http.MethodPost, "/auth/login", nil),
		// And one of the generated ones, which used to open its own span and now
		// gets it from the router — proof the router replaced that rather than
		// joining it.
		// It is refused while its path is decoded, before anything asks the
		// service or the database for anything.
		httptest.NewRequest(http.MethodGet, "/api/v1/teams/not-a-uuid", nil),
	} {
		mux.ServeHTTP(httptest.NewRecorder(), r)
	}

	// Export is batched; shutting the provider down is what empties the batch.
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	spans := readSpans(t, path)

	byName := map[string]map[string]any{}
	for _, s := range spans {
		byName[s.Name] = s.Attributes
	}

	for _, want := range []string{"POST /auth/login", "GET /api/v1/teams/{id}"} {
		attrs, ok := byName[want]
		if !ok {
			t.Errorf("no span named %q; got %v", want, names(byName))
			continue
		}
		if attrs["http.route"] != want {
			t.Errorf("%s carries http.route %v", want, attrs["http.route"])
		}
	}

	// Exactly one span per request. Two would be the per-handler span and the
	// wrapper's both alive, which is what a generated route is most at risk of.
	//
	// Counted over the records rather than over byName: both halves of that pair
	// would be named by the same route, so a map keyed by name would collapse
	// them and report the number this asserts.
	if len(spans) != 2 {
		t.Errorf("%d spans, want 2: %v", len(spans), names(byName))
	}
}

func names(byName map[string]map[string]any) []string {
	out := make([]string, 0, len(byName))
	for n := range byName {
		out = append(out, n)
	}
	return out
}

func readSpans(t *testing.T, path string) []observe.SpanRecord {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []observe.SpanRecord
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var rec observe.SpanRecord
		if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
			t.Fatalf("line is not a span record: %v\n%s", err, scan.Text())
		}
		out = append(out, rec)
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

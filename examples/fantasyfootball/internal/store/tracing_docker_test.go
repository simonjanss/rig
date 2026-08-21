//go:build docker

// What a traced write actually produces, against a real database.
//
// The generator suites prove the spans are emitted and that what they emit
// compiles. Only a run proves the shape: one span per stage, each under the one
// that caused it, and the statement underneath the stage that issued it. The
// span file is what makes that readable without standing up a collector.
package store_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/examples/fantasyfootball/internal/model"
	"github.com/simonjanss/rig/examples/fantasyfootball/internal/store"
	"github.com/simonjanss/rig/observe"
	"github.com/simonjanss/rig/runtime/dbhook"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// traced is a world of its own: a provider writing to a file this test owns,
// and a pool whose connections carry the statement tracer.
type traced struct {
	repos *store.Store
	ctx   context.Context
	path  string
	stop  func(t *testing.T)
}

func newTraced(t *testing.T) *traced {
	t.Helper()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://rig:rig@localhost:55441/rig?sslmode=disable"
	}

	path := filepath.Join(t.TempDir(), "spans.jsonl")
	provider, err := observe.Setup(t.Context(), observe.Config{
		ServiceName: "fantasyfootball", File: path,
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	// The same one line main.go passes to serve.Config.Pool.
	if err := observe.Pool(cfg); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("no database at %s: %v — run `rig db up` first", dsn, err)
	}
	t.Cleanup(pool.Close)

	claims := tenancy.Claims{TenantID: uuid.New(), AccountID: uuid.New()}
	return &traced{
		repos: store.New(pool, store.Config{Tracer: observe.Tracer()}),
		ctx:   tenancy.NewContext(ctx, claims),
		path:  path,
		stop: func(t *testing.T) {
			t.Helper()
			// Export is batched; shutting down is what empties the batch.
			if err := provider.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
		},
	}
}

// spans reads the file back, keyed by name.
func (w *traced) spans(t *testing.T) map[string]observe.SpanRecord {
	t.Helper()
	w.stop(t)

	f, err := os.Open(w.path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	out := map[string]observe.SpanRecord{}
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var rec observe.SpanRecord
		if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
			t.Fatalf("line is not a span record: %v\n%s", err, scan.Text())
		}
		out[rec.Name] = rec
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A create runs four things somebody wrote and one statement, and each of them
// is a span under the create. That is the whole point of the milestone: "the
// create was slow" is not worth collecting, "the validator was slow" is.
func TestAWriteProducesASpanPerStage(t *testing.T) {
	w := newTraced(t)

	ran := map[string]bool{}
	_, err := w.repos.Teams.Create(w.ctx, dbhook.Create[model.TeamCreateInput, model.Team]{
		Input: model.TeamCreateInput{Name: "Rovers", IsActive: true},
		Hooks: dbhook.CreateHooks[model.TeamCreateInput, model.Team]{
			Before: func(context.Context, tenancy.Claims, *model.TeamCreateInput) error {
				ran["Before"] = true
				return nil
			},
			After: func(context.Context, tenancy.Claims, *model.Team) error {
				ran["After"] = true
				return nil
			},
			AfterCommit: func(context.Context, tenancy.Claims, *model.Team) {
				ran["AfterCommit"] = true
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ran) != 3 {
		t.Fatalf("the hooks did not all run: %v", ran)
	}

	spans := w.spans(t)

	create, ok := spans["repository.Team.Create"]
	if !ok {
		t.Fatalf("no span for the create itself; got %v", names(spans))
	}

	for _, stage := range []string{
		"repository.Team.Create.Before",
		"repository.Team.Create.After",
		"repository.Team.Create.AfterCommit",
	} {
		got, ok := spans[stage]
		if !ok {
			t.Errorf("no span for %s; got %v", stage, names(spans))
			continue
		}
		if got.ParentID != create.SpanID {
			t.Errorf("%s is not under the create: parent %q, create %q",
				stage, got.ParentID, create.SpanID)
		}
		if got.TraceID != create.TraceID {
			t.Errorf("%s is in another trace entirely", stage)
		}
	}

	// The statement comes from the connection rather than from the generated
	// code, and it lands under the stage whose context issued it.
	insert, ok := spans["INSERT team"]
	if !ok {
		t.Fatalf("no span for the INSERT; got %v", names(spans))
	}
	if insert.ParentID != create.SpanID {
		t.Errorf("the INSERT is not under the create: parent %q", insert.ParentID)
	}
	if insert.Attributes["db.query.text"] == nil {
		t.Error("the statement itself is not on the span")
	}
}

// A hook that refuses is the failed span, and nothing after it happened. What a
// trace shows is where the write stopped, which is the question somebody has
// when a create came back 422.
func TestARefusedWriteStopsAtTheStageThatRefused(t *testing.T) {
	w := newTraced(t)

	refused := rigerr.Invalid("a team needs a name somebody will recognise")
	_, err := w.repos.Teams.Create(w.ctx, dbhook.Create[model.TeamCreateInput, model.Team]{
		Input: model.TeamCreateInput{Name: "Rovers", IsActive: true},
		Hooks: dbhook.CreateHooks[model.TeamCreateInput, model.Team]{
			Before: func(context.Context, tenancy.Claims, *model.TeamCreateInput) error {
				return refused
			},
			After: func(context.Context, tenancy.Claims, *model.Team) error {
				t.Error("After ran after Before refused")
				return nil
			},
		},
	})
	if !errors.Is(err, refused) {
		t.Fatalf("the refusal did not come back: %v", err)
	}

	spans := w.spans(t)

	before, ok := spans["repository.Team.Create.Before"]
	if !ok {
		t.Fatalf("no span for the stage that refused; got %v", names(spans))
	}
	if before.Status != "error" {
		t.Errorf("the refusing stage is not marked failed: %q", before.Status)
	}
	if !strings.Contains(before.Error, "recognise") {
		t.Errorf("the reason is not on the span: %q", before.Error)
	}

	if _, ok := spans["repository.Team.Create.After"]; ok {
		t.Error("a stage that never ran has a span")
	}
	if _, ok := spans["INSERT team"]; ok {
		t.Error("the write was refused before it began, and there is an INSERT span")
	}
}

func names(spans map[string]observe.SpanRecord) []string {
	out := make([]string, 0, len(spans))
	for name := range spans {
		out = append(out, name)
	}
	return out
}

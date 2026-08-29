//go:build docker

// What a shape endpoint answers when the sync service is not there, checked
// against what the sync service itself would have answered.
//
// This is the test the fallback needs and the unit tests cannot be: the claim is
// that a snapshot rig builds and a chunk the sync service builds decode to the
// same values, and only one of those two is rig's to produce.
package electrictest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/simonjanss/rig/runtime/electric"
)

// wideSchema is one column per type a generated model has a Go type for.
const wideSchema = `
DO $$ BEGIN
    CREATE TYPE wide_kind AS ENUM ('Original', 'Snapshot');
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS wide (
    id          uuid PRIMARY KEY,
    tenant_id   uuid NOT NULL,
    c_text      text,
    c_bool      boolean,
    c_int8      bigint,
    c_numeric   numeric(12,4),
    c_float8    double precision,
    c_tstz      timestamptz,
    c_date      date,
    c_time      time,
    c_uuid      uuid,
    c_jsonb     jsonb,
    c_enum      wide_kind,
    c_textarr   text[],
    c_bytea     bytea,
    c_boolarr   boolean[],
    c_int2      smallint,
    c_float4    real
);
`

// wideColumns is the projection, and the order does not matter to either side.
var wideColumns = []string{
	"id", "tenant_id", "c_text", "c_bool", "c_int8", "c_numeric", "c_float8",
	"c_tstz", "c_date", "c_time", "c_uuid", "c_jsonb", "c_enum", "c_textarr",
	"c_bytea", "c_boolarr", "c_int2", "c_float4",
}

// wideKey is what identifies a row of the shape, the way a generated shape
// carries its table's primary key.
var wideKey = []string{"id"}

// wideRow is what a generated model holds for those columns, with the Go types
// rig's own mapping picks.
type wideRow struct {
	ID       uuid.UUID
	TenantID uuid.UUID
	Text     *string
	Bool     *bool
	Int8     *int64
	Numeric  pgtype.Numeric
	Float8   *float64
	TSTZ     *time.Time
	Date     *time.Time
	Clock    *time.Time
	UUID     *uuid.UUID
	JSONB    json.RawMessage
	Enum     *string
	TextArr  []string
	Bytea    []byte
	BoolArr  []bool
	Int2     *int16
	Float4   *float32
}

// render is what the generated encoder does, by hand: one electric.Value per
// column, and DateOnly for the one Value cannot tell from a timestamp.
func (w wideRow) render() electric.Row {
	return electric.Row{
		Key: electric.RowKey("wide", fmt.Sprint(w.ID)),
		Value: map[string]any{
			"id":        electric.Value(w.ID),
			"tenant_id": electric.Value(w.TenantID),
			"c_text":    electric.Value(w.Text),
			"c_bool":    electric.Value(w.Bool),
			"c_int8":    electric.Value(w.Int8),
			"c_numeric": electric.Value(w.Numeric),
			"c_float8":  electric.Value(w.Float8),
			"c_tstz":    electric.Value(w.TSTZ),
			"c_date":    electric.DateOnly(w.Date),
			"c_time":    electric.TimeOnly(w.Clock),
			"c_uuid":    electric.Value(w.UUID),
			"c_jsonb":   electric.Value(w.JSONB),
			"c_enum":    electric.Value(w.Enum),
			"c_textarr": electric.Value(w.TextArr),
			"c_bytea":   electric.Value(w.Bytea),
			"c_boolarr": electric.Value(w.BoolArr),
			"c_int2":    electric.Value(w.Int2),
			"c_float4":  electric.Value(w.Float4),
		},
	}
}

// readWide is the read a fallback stands in for.
func readWide(ctx context.Context, p *pgxpool.Pool, tenant uuid.UUID) ([]wideRow, error) {
	rows, err := p.Query(ctx, `
		SELECT id, tenant_id, c_text, c_bool, c_int8, c_numeric, c_float8,
		       c_tstz, c_date, c_time, c_uuid, c_jsonb, c_enum, c_textarr, c_bytea,
		       c_boolarr, c_int2, c_float4
		FROM wide WHERE tenant_id = $1`, tenant)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []wideRow
	for rows.Next() {
		var w wideRow
		if err := rows.Scan(&w.ID, &w.TenantID, &w.Text, &w.Bool, &w.Int8,
			&w.Numeric, &w.Float8, &w.TSTZ, &w.Date, &w.Clock, &w.UUID,
			&w.JSONB, &w.Enum, &w.TextArr, &w.Bytea,
			&w.BoolArr, &w.Int2, &w.Float4); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// wideFront serves the shape, through a proxy the caller chooses: the real sync
// service, or one pointed nowhere. The mutator is what a generated handler would
// have put on the Shape — a Fallback, or the key and schema the proxy reads it
// with — and is nil for the shape as the sync service sees it.
func wideFront(t *testing.T, proxy *electric.Proxy, tenant uuid.UUID, on func(*electric.Shape)) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var where electric.Where
		where.Eq("tenant_id", tenant.String())

		s := electric.Shape{
			Table:   "wide",
			Where:   where.SQL(),
			Params:  where.Params(),
			Columns: wideColumns,
		}
		if on != nil {
			on(&s)
		}
		proxy.Serve(w, r, s)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// values reads a shape response into one map per row, keyed by the row's key.
func values(t *testing.T, srv *httptest.Server) (map[string]map[string]any, http.Header) {
	t.Helper()

	res, err := srv.Client().Get(srv.URL + "?offset=-1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d\n%s", res.StatusCode, body)
	}

	var out []struct {
		Key   string         `json:"key"`
		Value map[string]any `json:"value"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}

	rows := map[string]map[string]any{}
	for _, m := range out {
		if m.Key != "" {
			rows[m.Key] = m.Value
		}
	}
	return rows, res.Header
}

func insertWide(t *testing.T, p *pgxpool.Pool, tenant uuid.UUID) {
	t.Helper()

	// Half a second past the minute on c_time, because a time column keeps
	// microseconds and a layout that dropped them would agree with the sync
	// service on every whole second and nowhere else.

	if _, err := p.Exec(context.Background(), `
		INSERT INTO wide (id, tenant_id, c_text, c_bool, c_int8, c_numeric,
		    c_float8, c_tstz, c_date, c_time, c_uuid, c_jsonb, c_enum, c_textarr,
		    c_bytea, c_boolarr, c_int2, c_float4)
		VALUES ($1, $2, 'a "quoted" text', true, 9007199254740993, 12345.6789,
		    1.5, '2026-08-25 10:06:07.123456+02', '2026-08-25', '10:06:07.5', $3,
		    '{"z":1,"a":{"b":[1,2,null]}}', 'Snapshot',
		    ARRAY['one','two, with comma','has "quote"'], '\x00ff10',
		    ARRAY[true,false], -32768, 1.5)`,
		uuid.New(), tenant, uuid.New()); err != nil {
		t.Fatal(err)
	}
	// And a row that is null everywhere it can be, because a null is the value
	// most likely to be rendered as the string "null" by mistake.
	if _, err := p.Exec(context.Background(),
		`INSERT INTO wide (id, tenant_id) VALUES ($1, $2)`, uuid.New(), tenant); err != nil {
		t.Fatal(err)
	}
}

// The claim the whole feature rests on: for the same rows, a snapshot the proxy
// builds and a chunk of the real shape carry the same values.
//
// This is the one the fallback needs and the unit tests cannot be. Everything
// about rendering — an array's quoting, a numeric's scale, whether a boolean is
// t or true, what a bytea looks like — is the sync service's answer rather than
// this package's opinion, and only one of the two sides is rig's to produce. Both
// paths are checked: the read the proxy builds from the shape's own filter, which
// is what every project gets, and a hand-written Fallback, which is the escape
// hatch and the only thing that exercises electric.Value against real rows.
func TestAFallbackSnapshotAgreesWithTheSyncService(t *testing.T) {
	p, url := environment(t)
	ctx := context.Background()

	if _, err := p.Exec(ctx, wideSchema); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	insertWide(t, p, tenant)

	live, err := newProxy(electric.Config{URL: url})
	if err != nil {
		t.Fatal(err)
	}
	// Nothing answers on this address, which is what the sync service being
	// down looks like from inside the proxy.
	gone, err := newProxy(electric.Config{URL: "http://127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}

	fromSync, syncHeaders := values(t, wideFront(t, live, tenant, nil))
	schema := syncHeaders.Get("electric-schema")

	// A proxy given the database. Nothing on the shape but the key and the schema
	// — the filter it reads with is the one it would have sent upstream, which is
	// the whole claim.
	read, err := newProxy(electric.Config{URL: "http://127.0.0.1:1", DB: p})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		on   func(*electric.Shape)
	}{
		{"the read the proxy builds", func(s *electric.Shape) {
			s.Key = wideKey
			s.Schema = schema
		}},
		{"a hand-written fallback", func(s *electric.Shape) {
			s.Fallback = func(ctx context.Context) (electric.Snapshot, error) {
				rows, err := readWide(ctx, p, tenant)
				if err != nil {
					return electric.Snapshot{}, err
				}
				out := make([]electric.Row, 0, len(rows))
				for _, w := range rows {
					out = append(out, w.render())
				}
				return electric.Snapshot{Rows: out, Schema: schema}, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proxy := read
			if tc.name == "a hand-written fallback" {
				proxy = gone
			}
			fromFallback, fbHeaders := values(t, wideFront(t, proxy, tenant, tc.on))

			if fbHeaders.Get("X-Rig-Sync-Fallback") == "" {
				t.Fatal("this answer came from the sync service, so it proves nothing")
			}
			if got := fbHeaders.Get("electric-schema"); got != schema {
				t.Errorf("electric-schema:\n got %s\nwant %s", got, schema)
			}
			if len(fromSync) != 2 || len(fromFallback) != 2 {
				t.Fatalf("got %d rows from the sync service and %d from the fallback",
					len(fromSync), len(fromFallback))
			}

			for key, want := range fromSync {
				got, ok := fromFallback[key]
				if !ok {
					t.Errorf("the fallback did not send %s, so the keys disagree", key)
					continue
				}
				for _, col := range wideColumns {
					compare(t, key, col, want[col], got[col])
				}
			}
		})
	}
}

// A shape that opted out answers a sync outage the way it always did, even where
// the proxy has a database and every other shape is answered from it.
func TestAShapeThatOptedOutIsStillABadGateway(t *testing.T) {
	p, _ := environment(t)

	if _, err := p.Exec(context.Background(), wideSchema); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	insertWide(t, p, tenant)

	proxy, err := newProxy(electric.Config{URL: "http://127.0.0.1:1", DB: p})
	if err != nil {
		t.Fatal(err)
	}
	srv := wideFront(t, proxy, tenant, func(s *electric.Shape) {
		s.Key = wideKey
		s.NoFallback = true
	})

	res, err := srv.Client().Get(srv.URL + "?offset=-1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want %d", res.StatusCode, http.StatusBadGateway)
	}
}

// The bound refuses rather than truncating, and does it as a LIMIT — so the rows
// past it are never read. A subscriber cannot tell a short answer from a
// complete one, which is why a partial snapshot would be worse than none.
func TestAShapePastItsBoundIsRefusedByTheRead(t *testing.T) {
	p, _ := environment(t)

	if _, err := p.Exec(context.Background(), wideSchema); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	insertWide(t, p, tenant)

	// Two rows, and room for one.
	proxy, err := newProxy(electric.Config{URL: "http://127.0.0.1:1", DB: p, MaxSnapshotRows: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv := wideFront(t, proxy, tenant, func(s *electric.Shape) { s.Key = wideKey })

	res, err := srv.Client().Get(srv.URL + "?offset=-1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status %d, want %d — a snapshot past its bound is refused", res.StatusCode, http.StatusBadGateway)
	}
}

// compare is column equality, with the one column that is equal without being
// identical named and explained rather than skipped.
//
// One, now. A jsonb document used to be compared as JSON, on the grounds that
// Postgres prints it normalised and Go prints what it was handed — but both paths
// send the bytes Postgres printed, so there is nothing left for that tolerance to
// forgive, and a tolerance nothing needs is one that hides something.
func compare(t *testing.T, key, col string, want, got any) {
	t.Helper()

	if want == nil || got == nil {
		if want != got {
			t.Errorf("%s.%s: sync service %#v, fallback %#v", key, col, want, got)
		}
		return
	}

	// The sync service writes "2026-08-25 08:06:07.123456+00" and a snapshot
	// writes the RFC 3339 form the API sends. Both parse to the same instant,
	// which is what a subscriber ends up with, and only one of them is also what
	// the same column looks like over REST — so this is a decision rather than a
	// discrepancy, and electric.Value has the reasoning.
	if col == "c_tstz" {
		if a, b := instant(t, want), instant(t, got); !a.Equal(b) {
			t.Errorf("%s.%s: %s and %s are different moments", key, col, want, got)
		}
		return
	}

	if want != got {
		t.Errorf("%s.%s: sync service %#v, fallback %#v", key, col, want, got)
	}
}

func instant(t *testing.T, v any) time.Time {
	t.Helper()

	s, ok := v.(string)
	if !ok {
		t.Fatalf("%#v is not a timestamp", v)
	}
	// Postgres's own form: a space, and an offset of two digits.
	s = strings.Replace(s, " ", "T", 1)
	if len(s) > 3 && (s[len(s)-3] == '+' || s[len(s)-3] == '-') {
		s += ":00"
	}
	at, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return at
}

// How a subscriber that was served a snapshot gets back onto real sync. The
// proxy arranges none of it: a handle the sync service never issued is a
// must-refetch, which is the protocol's own way of saying start again.
func TestASubscriberRecoversOntoRealSyncAfterAFallback(t *testing.T) {
	p, url := environment(t)
	ctx := context.Background()

	if _, err := p.Exec(ctx, wideSchema); err != nil {
		t.Fatal(err)
	}
	tenant := uuid.New()
	insertWide(t, p, tenant)

	gone, _ := newProxy(electric.Config{URL: "http://127.0.0.1:1"})
	live, _ := newProxy(electric.Config{URL: url})

	// A hand-written one, so this test says nothing about where the rows came
	// from: what it is about is the handle protocol.
	fallback := func(s *electric.Shape) {
		s.Fallback = func(context.Context) (electric.Snapshot, error) {
			return electric.Snapshot{Rows: []electric.Row{{
				Key:   electric.RowKey("wide", "1"),
				Value: map[string]any{"id": "1"},
			}}}, nil
		}
	}

	// While the outage lasts: a snapshot, and a handle of this proxy's own.
	_, headers := values(t, wideFront(t, gone, tenant, fallback))
	handle := headers.Get("electric-handle")
	if !strings.HasPrefix(handle, "rig-fallback-") {
		t.Fatalf("electric-handle = %q", handle)
	}

	// While it still lasts, the poll that follows is asked to wait rather than
	// handed a second snapshot.
	res, err := wideFront(t, gone, tenant, fallback).Client().Get(
		wideFront(t, gone, tenant, fallback).URL + "?offset=0_inf&handle=" + handle + "&live=true")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("during the outage a resumed poll answered %d, want 503", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After")
	}

	// And once the sync service is reachable, the same poll is a must-refetch.
	srv := wideFront(t, live, tenant, fallback)
	res, err = srv.Client().Get(srv.URL + "?offset=0_inf&handle=" + handle + "&live=true")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want the sync service's 409\n%s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "must-refetch") {
		t.Errorf("the reset is not in the body: %s", body)
	}
}

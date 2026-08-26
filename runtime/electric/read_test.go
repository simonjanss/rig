package electric

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// The statement is most of the behaviour, and it needs no database to check.
// What a filter means is settled elsewhere — the docker suite compares a snapshot
// against what the sync service answers for the same shape — so these are about
// the read being the shape: this table, this projection, this filter, this bound.

func TestTheReadIsTheShapesOwnFilter(t *testing.T) {
	t.Parallel()

	w := &Where{}
	w.Eq("tenant_id", "t1").IsNull("deleted_at")

	got := selectRows(Shape{
		Table:   "todo",
		Columns: []string{"id", "title"},
		Key:     []string{"id"},
		Where:   w.SQL(),
	}, 0)

	want := `SELECT "id", "title" FROM "public"."todo" WHERE "tenant_id" = $1 AND "deleted_at" IS NULL`
	if got != want {
		t.Errorf("statement:\n got %s\nwant %s", got, want)
	}
}

func TestATableWithNoFilterReadsWithNoWhereClause(t *testing.T) {
	t.Parallel()

	// A table with no tenant column, no owner, no lifecycle columns and no scope.
	// `WHERE` with nothing after it does not parse, so the clause has to be
	// absent rather than empty.
	got := selectRows(Shape{Table: "setting", Columns: []string{"key"}, Key: []string{"key"}}, 0)

	if strings.Contains(got, "WHERE") {
		t.Errorf("a shape with no filter still wrote a WHERE clause: %s", got)
	}
	if want := `SELECT "key" FROM "public"."setting"`; got != want {
		t.Errorf("statement:\n got %s\nwant %s", got, want)
	}
}

func TestABareTableNameIsQualifiedTheWayTheSyncServiceQualifiesIt(t *testing.T) {
	t.Parallel()

	// Bare would be resolved by the connection's search_path. The sync service
	// resolves it to public, and a shape that read one table and streamed another
	// would be one shape answering two questions.
	for _, tc := range []struct{ table, want string }{
		{"todo", `"public"."todo"`},
		{"public.todo", `"public"."todo"`},
		{"billing.invoice", `"billing"."invoice"`},
	} {
		if got := qualified(tc.table); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.table, got, tc.want)
		}
	}
}

func TestAColumnNamedAfterAKeywordIsStillReadable(t *testing.T) {
	t.Parallel()

	got := selectRows(Shape{Table: "line", Columns: []string{"order", `we"ird`}, Key: []string{"order"}}, 0)

	if want := `SELECT "order", "we""ird" FROM "public"."line"`; got != want {
		t.Errorf("statement:\n got %s\nwant %s", got, want)
	}
}

func TestAKeyOutsideTheProjectionIsReadAndNotSent(t *testing.T) {
	t.Parallel()

	// Read, because RowKey is built from it and a row with no key is a row a
	// subscriber cannot address. Not sent, because the projection is the promise.
	s := Shape{Table: "todo", Columns: []string{"title"}, Key: []string{"id"}}

	if got, want := selectRows(s, 0), `SELECT "title", "id" FROM "public"."todo"`; got != want {
		t.Errorf("statement:\n got %s\nwant %s", got, want)
	}

	key, send, err := layout(s, fields("title", "id"))
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if len(key) != 1 || key[0] != 1 {
		t.Errorf("key columns: got %v, want [1]", key)
	}
	if len(send) != 1 || send[0] != 0 {
		t.Errorf("sent columns: got %v, want [0] — id was read for the key, not for the row", send)
	}
}

func TestAKeyAlreadyInTheProjectionIsNotReadTwice(t *testing.T) {
	t.Parallel()

	s := Shape{Table: "todo", Columns: []string{"id", "title"}, Key: []string{"id"}}

	if got, want := selectRows(s, 0), `SELECT "id", "title" FROM "public"."todo"`; got != want {
		t.Errorf("statement:\n got %s\nwant %s", got, want)
	}
}

func TestAnEmptyProjectionReadsAndSendsEveryColumn(t *testing.T) {
	t.Parallel()

	// Shape.Columns documents empty as every column, and the streaming path
	// honours it by sending no columns parameter at all.
	s := Shape{Table: "todo", Key: []string{"id"}}

	if got, want := selectRows(s, 0), `SELECT * FROM "public"."todo"`; got != want {
		t.Errorf("statement:\n got %s\nwant %s", got, want)
	}

	_, send, err := layout(s, fields("id", "title", "done"))
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if len(send) != 3 {
		t.Errorf("sent columns: got %v, want all three", send)
	}
}

func TestAShapeWithNoKeyAtAllIsAnError(t *testing.T) {
	t.Parallel()

	// RowKey with no parts is the table, so every row of the snapshot would be
	// called the same thing and a subscriber would hold one row. Refused for the
	// reason a null key part is.
	_, _, err := layout(Shape{Table: "todo", Columns: []string{"title"}}, fields("title"))
	if err == nil {
		t.Fatal("a shape that names no key columns was accepted")
	}
	if !strings.Contains(err.Error(), "todo") {
		t.Errorf("the error does not name the table: %v", err)
	}
}

func TestAKeyColumnTheResultDoesNotHaveIsAnError(t *testing.T) {
	t.Parallel()

	_, _, err := layout(Shape{Table: "todo", Key: []string{"id"}}, fields("title"))
	if err == nil {
		t.Fatal("a shape keyed by a column that is not there was accepted")
	}
	if !strings.Contains(err.Error(), "id") {
		t.Errorf("the error does not name the column: %v", err)
	}
}

func TestTheReadAsksForOneRowPastTheBound(t *testing.T) {
	t.Parallel()

	// One past, so that a shape too large to send is known to be too large
	// without the rows past the bound being read.
	p := &Proxy{maxRows: 3}
	if got, want := p.readLimit(), 4; got != want {
		t.Errorf("limit: got %d, want %d", got, want)
	}

	got := selectRows(Shape{Table: "todo", Columns: []string{"id"}}, p.readLimit())
	if !strings.HasSuffix(got, " LIMIT 4") {
		t.Errorf("statement does not end in the bound: %s", got)
	}
}

func TestNoBoundIsNoLimitClause(t *testing.T) {
	t.Parallel()

	// A negative MaxSnapshotRows is documented as no bound, and a LIMIT of 0
	// would be a bound of nothing.
	p := &Proxy{maxRows: -1}
	if got := p.readLimit(); got != 0 {
		t.Errorf("limit: got %d, want 0", got)
	}
	if got := selectRows(Shape{Table: "todo", Columns: []string{"id"}}, p.readLimit()); strings.Contains(got, "LIMIT") {
		t.Errorf("an unbounded read wrote a limit: %s", got)
	}
}

func TestTheBoundIsAskedWithoutReadingTheRows(t *testing.T) {
	t.Parallel()

	w := &Where{}
	w.Eq("tenant_id", "t1")

	// OFFSET past the bound: a row means too large, no row means it will send.
	// The alternative is building the snapshot and throwing it away, which is a
	// second scan per tab that was already streaming.
	got := selectExists(Shape{Table: "todo", Where: w.SQL()}, 20_000)
	want := `SELECT 1 FROM "public"."todo" WHERE "tenant_id" = $1 OFFSET 20000 LIMIT 1`
	if got != want {
		t.Errorf("statement:\n got %s\nwant %s", got, want)
	}
}

func TestTheFiltersValuesGoThroughAsText(t *testing.T) {
	t.Parallel()

	// As Go strings, which is what lets one filter compare against a column of
	// any type: pgx sends the characters unchanged and Postgres's own input
	// function parses them, the same way the sync service is given them.
	w := &Where{}
	w.Eq("tenant_id", "3f0f4b1e-0000-4000-8000-000000000000").Gt("count", "7")

	args := (&Proxy{}).args(Shape{Where: w.SQL(), Params: w.Params()})

	if _, ok := args[0].(pgx.QueryResultFormatsByOID); !ok {
		t.Fatalf("the result formats are not the first argument: %T", args[0])
	}
	for i, arg := range args[1:] {
		if _, ok := arg.(string); !ok {
			t.Errorf("parameter %d is %T, not a string", i+1, arg)
		}
	}
	if len(args) != 3 {
		t.Errorf("arguments: got %d, want the formats and two parameters", len(args))
	}
}

func TestTheColumnsReadInBinaryAreTheOnesTheSessionWouldOtherwiseDecide(t *testing.T) {
	t.Parallel()

	// A date and a timestamp are printed according to DateStyle, and an instant
	// according to TimeZone as well, so their text form is a fact about the
	// connection rather than about the row. Boolean is here for the other reason:
	// Postgres prints t and f where the sync service sends true and false.
	for _, oid := range []uint32{
		pgtype.BoolOID, pgtype.DateOID, pgtype.TimestampOID, pgtype.TimestamptzOID,
	} {
		if binaryFormats[oid] != pgx.BinaryFormatCode {
			t.Errorf("oid %d is read as text", oid)
		}
	}

	// Everything else is the text Postgres prints, which is the form a shape
	// response carries. A missing key is zero, and zero is the text format.
	for _, oid := range []uint32{
		pgtype.UUIDOID, pgtype.TextOID, pgtype.Int8OID, pgtype.NumericOID,
		pgtype.JSONBOID, pgtype.ByteaOID, pgtype.TextArrayOID,
		pgtype.TimeOID, pgtype.TimetzOID,
	} {
		if binaryFormats[oid] != pgx.TextFormatCode {
			t.Errorf("oid %d is read as binary", oid)
		}
	}
}

func TestATextColumnIsSentAsPostgresPrintedIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		oid  uint32
		raw  string
	}{
		{"a uuid", pgtype.UUIDOID, "3f0f4b1e-0000-4000-8000-000000000000"},
		{"a numeric keeps its scale", pgtype.NumericOID, "1234.5600"},
		{"an array keeps postgres's quoting", pgtype.TextArrayOID, `{one,"two, with comma"}`},
		{"a jsonb is the document", pgtype.JSONBOID, `{"a": 1}`},
		{"a bytea is hex", pgtype.ByteaOID, `\x00ff10`},
		{"a time of day keeps its fraction", pgtype.TimeOID, "10:06:07.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := column(field("c", tc.oid, pgx.TextFormatCode), []byte(tc.raw))
			if err != nil {
				t.Fatalf("column: %v", err)
			}
			if got != tc.raw {
				t.Errorf("got %v, want %s", got, tc.raw)
			}
		})
	}
}

func TestANullColumnIsNullAndNotAnEmptyString(t *testing.T) {
	t.Parallel()

	got, err := column(field("c", pgtype.TextOID, pgx.TextFormatCode), nil)
	if err != nil {
		t.Fatalf("column: %v", err)
	}
	if got != nil {
		t.Errorf("got %#v, want nil — an absent value is not an empty one", got)
	}
}

func TestABooleanIsNormalisedTheWayTheSyncServiceNormalisesIt(t *testing.T) {
	t.Parallel()

	// Postgres prints t and f. A subscriber is sent true and false, because that
	// is what the sync service sends it.
	for _, tc := range []struct {
		in   []byte
		want string
	}{
		{[]byte{1}, "true"},
		{[]byte{0}, "false"},
	} {
		got, err := column(field("done", pgtype.BoolOID, pgx.BinaryFormatCode), tc.in)
		if err != nil {
			t.Fatalf("column: %v", err)
		}
		if got != tc.want {
			t.Errorf("got %v, want %s", got, tc.want)
		}
	}
}

func TestADateIsTheDayAndAnInstantIsTheInstant(t *testing.T) {
	t.Parallel()

	// The two are the same Go type by the time a renderer sees one, which is why
	// DateOnly exists. Here the column's own type is what chooses, so nothing has
	// to be told and no generator has to know.
	day := mustEncode(t, pgtype.DateOID, time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC))
	got, err := column(field("due_on", pgtype.DateOID, pgx.BinaryFormatCode), day)
	if err != nil {
		t.Fatalf("column: %v", err)
	}
	if got != "2026-08-25" {
		t.Errorf("a date rendered as %v", got)
	}

	// And an instant is UTC whatever the session's TimeZone is, which is the whole
	// reason it is not read as text.
	at := time.Date(2026, 8, 25, 8, 6, 7, 123456000, time.FixedZone("CEST", 2*60*60))
	raw := mustEncode(t, pgtype.TimestamptzOID, at)
	got, err = column(field("created_at", pgtype.TimestamptzOID, pgx.BinaryFormatCode), raw)
	if err != nil {
		t.Fatalf("column: %v", err)
	}
	if got != "2026-08-25T06:06:07.123456Z" {
		t.Errorf("an instant rendered as %v", got)
	}
}

func TestAKeyColumnReadInBinaryIsNamedByItsPrintedValue(t *testing.T) {
	t.Parallel()

	// A key part is the value Postgres printed for every column read as text,
	// which is almost all of them. The four read in binary arrive as bytes that
	// are not a name at all — an eight-byte timestamp is mostly not valid UTF-8,
	// and encoding/json writes one U+FFFD per byte it cannot read, so two rows
	// whose keys differ only above 0x7F would go out under the same key.
	for _, tc := range []struct {
		name string
		oid  uint32
		in   any
		want string
	}{
		{"a date", pgtype.DateOID, time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), "2026-08-25"},
		{"an instant", pgtype.TimestamptzOID,
			time.Date(2026, 8, 25, 8, 6, 7, 123456000, time.UTC), "2026-08-25T08:06:07.123456Z"},
		{"a boolean", pgtype.BoolOID, true, "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := keyPart(field("k", tc.oid, pgx.BinaryFormatCode), mustEncode(t, tc.oid, tc.in))
			if err != nil {
				t.Fatalf("keyPart: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}

	// And a key of uuids, integers or text is the text it arrived as, untouched.
	id := "3f0f4b1e-0000-4000-8000-000000000000"
	got, err := keyPart(field("id", pgtype.UUIDOID, pgx.TextFormatCode), []byte(id))
	if err != nil {
		t.Fatalf("keyPart: %v", err)
	}
	if got != id {
		t.Errorf("got %q, want %q", got, id)
	}
}

// The wiring: which of the three sources answers a shape, and what a proxy with
// none of them does.

func TestAShapeWithNoDatabaseAndNoFallbackIsTheFiveOhTwoItAlwaysWas(t *testing.T) {
	t.Parallel()

	p := &Proxy{}
	if got := p.resolve(Shape{Table: "todo"}); got.Fallback != nil {
		t.Error("a proxy with no database invented a fallback")
	}
}

func TestAnExplicitFallbackWinsOverTheDatabase(t *testing.T) {
	t.Parallel()

	// An escape hatch a default overrode would not be one.
	sentinel := errors.New("mine")
	p := &Proxy{db: failingDB{}}

	got := p.resolve(Shape{
		Table:    "todo",
		Fallback: func(context.Context) (Snapshot, error) { return Snapshot{}, sentinel },
	})
	if _, err := got.Fallback(context.Background()); !errors.Is(err, sentinel) {
		t.Errorf("the shape's own fallback was not the one called: %v", err)
	}
	if got.probe != nil {
		t.Error("a hand-written fallback was given a probe it cannot answer")
	}
}

func TestAShapeThatOptedOutGetsNoFallbackEvenWithADatabase(t *testing.T) {
	t.Parallel()

	// Presence. A snapshot of who was here a moment ago, that then stops
	// updating, is worth less than an empty list — the feature is the freshness.
	p := &Proxy{db: failingDB{}}
	if got := p.resolve(Shape{Table: "rig_presence", NoFallback: true}); got.Fallback != nil {
		t.Error("a shape that opted out was given a fallback")
	}
}

func TestADatabaseAnswersEveryOtherShape(t *testing.T) {
	t.Parallel()

	p := &Proxy{db: failingDB{}}
	got := p.resolve(Shape{Table: "todo", Columns: []string{"id"}, Key: []string{"id"}})
	if got.Fallback == nil {
		t.Fatal("a shape with a database has no fallback")
	}
	if got.probe == nil {
		t.Fatal("a shape read from the database has no cheap probe")
	}

	// The failing handle is what proves it is the database being asked rather
	// than something invented.
	if _, err := got.Fallback(context.Background()); !errors.Is(err, errDB) {
		t.Errorf("the read did not reach the database: %v", err)
	}
	if err := got.probe(context.Background()); !errors.Is(err, errDB) {
		t.Errorf("the probe did not reach the database: %v", err)
	}
}

func TestTheProbeRefusalIsAnsweredWithoutBuildingASnapshot(t *testing.T) {
	t.Parallel()

	// A resuming subscriber is only sent to start again where there is something
	// to start again to. A refusal here keeps its rows where they are.
	p := &Proxy{db: failingDB{}, breaker: &breaker{threshold: -1}}
	s := p.resolve(Shape{Table: "todo", Columns: []string{"id"}, Key: []string{"id"}})

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/todo/_stream?offset=0_5&handle=electric-1", nil)

	if p.tryProbe(rec, r, s) {
		t.Fatal("a shape whose read fails was reported as having a snapshot")
	}
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusBadGateway)
	}
}

// errDB is what failingDB answers with, so a test can tell "the database was
// asked and said no" from "nothing asked the database".
var errDB = errors.New("no database here")

// failingDB is a DB that refuses. It is enough for the wiring tests: what they
// check is which handle gets asked, not what comes back.
type failingDB struct{}

func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errDB }

func (failingDB) QueryRow(context.Context, string, ...any) pgx.Row {
	panic("electric: nothing here reads a single row")
}

func (failingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("electric: nothing here writes")
}

// fields is a result description of text columns, for [layout], which only reads
// the names.
func fields(names ...string) []pgconn.FieldDescription {
	out := make([]pgconn.FieldDescription, 0, len(names))
	for _, n := range names {
		out = append(out, field(n, pgtype.TextOID, pgx.TextFormatCode))
	}
	return out
}

func field(name string, oid uint32, format int16) pgconn.FieldDescription {
	return pgconn.FieldDescription{Name: name, DataTypeOID: oid, Format: format}
}

// encoding serialises mustEncode. A pgtype.Map remembers the plan it worked out
// for an (oid, type) pair the first time it is asked, and remembering is a write
// to a map it holds — so two parallel tests encoding two types at once is a
// concurrent write and a fatal error. Only encoding memoises; the scan the read
// path does works its plan out each time.
var encoding sync.Mutex

// mustEncode is a value in the wire form Postgres would have sent it in, so a
// decoding test does not depend on a database to produce its input.
func mustEncode(t *testing.T, oid uint32, v any) []byte {
	t.Helper()

	encoding.Lock()
	defer encoding.Unlock()

	out, err := codecs.Encode(oid, pgx.BinaryFormatCode, v, nil)
	if err != nil {
		t.Fatalf("encoding for oid %d: %v", oid, err)
	}
	return out
}

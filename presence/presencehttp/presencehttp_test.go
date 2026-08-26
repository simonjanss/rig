package presencehttp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/simonjanss/rig/presence"
	"github.com/simonjanss/rig/presence/presencehttp"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// TestTheWireNamesAreCamelCase is the promise the package documentation makes:
// these routes answer camelCase whatever `api.json_case` a project sets, because
// the browser package is compiled against them once.
//
// A test rather than a comment because the failure is silent — a struct field
// renamed without its tag produces a response the shared package cannot parse,
// and nothing else in the build would notice.
func TestTheWireNamesAreCamelCase(t *testing.T) {
	t.Parallel()

	for _, v := range []any{presencehttp.Beat{}, presencehttp.Beaten{}, presencehttp.Person{}} {
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			f := typ.Field(i)
			tag := f.Tag.Get("json")
			if tag == "" {
				t.Errorf("%s.%s has no json tag, so its wire name is whatever Go called it",
					typ.Name(), f.Name)
				continue
			}
			name, _, _ := strings.Cut(tag, ",")
			if name == "" || name == "-" {
				t.Errorf("%s.%s has no wire name", typ.Name(), f.Name)
				continue
			}
			if c := name[0]; c < 'a' || c > 'z' {
				t.Errorf("%s.%s is %q on the wire; these routes answer camelCase",
					typ.Name(), f.Name, name)
			}
			if strings.ContainsAny(name, "_-") {
				t.Errorf("%s.%s is %q on the wire; these routes answer camelCase, not "+
					"the column names the stream carries", typ.Name(), f.Name, name)
			}
		}
	}
}

// TestThereIsNowhereToNameSomebodyElse is the whole authorization story on these
// routes, asserted rather than described: the wire shape has no account field, so
// a client that tried to set one is refused by the decoder.
func TestThereIsNowhereToNameSomebodyElse(t *testing.T) {
	t.Parallel()

	if _, ok := reflect.TypeOf(presencehttp.Beat{}).FieldByName("AccountID"); ok {
		t.Fatal("Beat carries an account, so a client can say somebody else is editing something")
	}

	rec := beat(t, someone(), `{"sessionKey":"tab-1","scope":"board","accountId":"`+uuid.New().String()+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a body naming an account answered %d, want %d — it should be refused rather "+
			"than silently ignored, because the field it reaches for is the one this package "+
			"exists to make unreachable", rec.Code, http.StatusBadRequest)
	}
}

func TestBeatRefusals(t *testing.T) {
	t.Parallel()

	row := uuid.New()
	for _, tc := range []struct {
		name   string
		claims tenancy.Claims
		body   string
		want   int
	}{
		{
			// An API key and a system credential both have no account behind
			// them, and there is nobody for their presence to be about.
			name:   "a credential with no account",
			claims: tenancy.Claims{TenantID: uuid.New(), Subject: tenancy.SubjectAPIKey},
			body:   `{"sessionKey":"tab-1","scope":"board"}`,
			want:   http.StatusForbidden,
		},
		{
			name:   "no session key",
			claims: someone(),
			body:   `{"scope":"board"}`,
			want:   http.StatusUnprocessableEntity,
		},
		{
			name:   "no scope",
			claims: someone(),
			body:   `{"sessionKey":"tab-1"}`,
			want:   http.StatusUnprocessableEntity,
		},
		{
			name:   "a field with no row to name it on",
			claims: someone(),
			body:   `{"sessionKey":"tab-1","scope":"board","targetField":"title"}`,
			want:   http.StatusUnprocessableEntity,
		},
		{
			name:   "a row with no table to find it in",
			claims: someone(),
			body:   `{"sessionKey":"tab-1","scope":"board","targetId":"` + row.String() + `"}`,
			want:   http.StatusUnprocessableEntity,
		},
		{
			name:   "an activity nothing recognises",
			claims: someone(),
			body:   `{"sessionKey":"tab-1","scope":"board","activity":"lurking"}`,
			want:   http.StatusUnprocessableEntity,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := beat(t, tc.claims, tc.body).Code; got != tc.want {
				t.Errorf("answered %d, want %d", got, tc.want)
			}
		})
	}
}

// TestATableOutsideTheDocumentIsRefused is the typo boundary. It is not a
// security boundary — target_table reaches no statement — but without it the
// column is untrusted text and every reader has to treat it that way.
func TestATableOutsideTheDocumentIsRefused(t *testing.T) {
	t.Parallel()

	h := handler(presence.Config{DB: stubDB{}, Targets: []string{"todo"}}, someone())

	ok := send(h, http.MethodPut, "/presence",
		`{"sessionKey":"t","scope":"board","targetTable":"todo","targetId":"`+uuid.New().String()+`"}`)
	// The stub reaches no database, so a target this service accepts fails later
	// and differently. What matters is that it is not the 422 below.
	if ok.Code == http.StatusUnprocessableEntity {
		t.Errorf("a table the document names was refused as a typo: %s", ok.Body)
	}

	bad := send(h, http.MethodPut, "/presence",
		`{"sessionKey":"t","scope":"board","targetTable":"nonesuch","targetId":"`+uuid.New().String()+`"}`)
	if bad.Code != http.StatusUnprocessableEntity {
		t.Errorf("a table nothing in the document names answered %d, want %d",
			bad.Code, http.StatusUnprocessableEntity)
	}
}

func TestLeaveNeedsASessionKey(t *testing.T) {
	t.Parallel()

	h := handler(presence.Config{DB: stubDB{}}, someone())
	if got := send(h, http.MethodDelete, "/presence", `{}`).Code; got != http.StatusUnprocessableEntity {
		t.Errorf("a leave naming no session answered %d, want %d", got, http.StatusUnprocessableEntity)
	}
}

// TestAFailingClaimsFunctionIsNotAnEmptyRoom: a handler that could not identify
// its caller must refuse, not write every tab in the building into one row.
func TestAFailingClaimsFunctionIsNotAnEmptyRoom(t *testing.T) {
	t.Parallel()

	svc := presence.NewService(presence.Config{DB: stubDB{}})
	h := presencehttp.New(svc, presencehttp.Options{
		Claims: func(*http.Request) (tenancy.Claims, error) {
			return tenancy.Claims{}, errors.New("no")
		},
	})
	mux := http.NewServeMux()
	h.Mount(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/presence",
		strings.NewReader(`{"sessionKey":"t","scope":"board"}`)))
	if rec.Code == http.StatusOK {
		t.Fatal("a request whose caller could not be identified was recorded as present")
	}
}

func TestNewPanicsWithoutClaims(t *testing.T) {
	t.Parallel()

	defer func() {
		if recover() == nil {
			t.Fatal("a handler with no way to identify its caller was accepted")
		}
	}()
	presencehttp.New(presence.NewService(presence.Config{DB: stubDB{}}), presencehttp.Options{})
}

// TestTheAnswerCarriesTheIntervals is why a browser reads the response at all:
// the TTL and the heartbeat come back on every beat, so changing either is a
// deploy of the server rather than a release of the front end.
func TestTheAnswerCarriesTheIntervals(t *testing.T) {
	t.Parallel()

	// Read off the `here` route, which needs no write to answer.
	h := handler(presence.Config{DB: emptyDB{}}, someone())
	rec := send(h, http.MethodGet, "/presence", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("answered %d: %s", rec.Code, rec.Body)
	}

	var body struct {
		TTLSeconds       int `json:"ttlSeconds"`
		HeartbeatSeconds int `json:"heartbeatSeconds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if want := int(presence.DefaultTTL.Seconds()); body.TTLSeconds != want {
		t.Errorf("ttlSeconds = %d, want %d", body.TTLSeconds, want)
	}
	if want := int(presence.DefaultHeartbeat.Seconds()); body.HeartbeatSeconds != want {
		t.Errorf("heartbeatSeconds = %d, want %d", body.HeartbeatSeconds, want)
	}
}

// A body over the limit is refused rather than read.
//
// Until these routes used [httpx.Decode] there was no limit here at all — the
// only route in rig that would read an unbounded request body. Authenticated, so
// never an anonymous hole, but a signed-in client streaming forever into a
// heartbeat is not a threat model worth keeping.
func TestABodyOverTheLimitIsRefused(t *testing.T) {
	t.Parallel()

	// A syntactically valid object whose one string runs well past the 64 KiB
	// bound, so what refuses it is the limit and not the parser.
	huge := `{"sessionKey":"` + strings.Repeat("x", 1<<17) + `","scope":"board"}`

	rec := beat(t, someone(), huge)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a body of %d bytes answered %d, want 400", len(huge), rec.Code)
	}
}

// The envelope is flat and the same one every other rig route answers with.
//
// It used to be nested under an `error` key, which neither of rig's client
// libraries can read: the flat struct they decode into comes out all-zero, so
// every error predicate answers false and the caller is left with the status.
func TestTheFallbackEnvelopeIsFlat(t *testing.T) {
	t.Parallel()

	// No Fail supplied, so this is the package's own fallback.
	h := presencehttp.New(presence.NewService(presence.Config{DB: stubDB{}}),
		presencehttp.Options{
			Claims: func(*http.Request) (tenancy.Claims, error) {
				return tenancy.Claims{}, rigerr.Forbidden("not for you")
			},
		})
	mux := http.NewServeMux()
	h.Mount(mux)

	rec := send(mux, http.MethodGet, "/presence", "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, nested := raw["error"]; nested {
		t.Error(`the envelope has an "error" key, which neither client library reads`)
	}
	if raw["code"] != string(rigerr.CodeForbidden) {
		t.Errorf("code = %v, want %q", raw["code"], rigerr.CodeForbidden)
	}
	if raw["message"] != "not for you" {
		t.Errorf("message = %v", raw["message"])
	}
}

// Claims that are well formed but carry no tenant are refused here rather than
// written into a row. The project's own Claims function is not asked to have
// checked: [tenancy.FromContext] is, and it is the one source the handlers read.
func TestClaimsWithNoTenantAreRefused(t *testing.T) {
	t.Parallel()

	rec := beat(t, tenancy.Claims{AccountID: uuid.New(), Subject: tenancy.SubjectAccount},
		`{"sessionKey":"tab-1","scope":"board"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("claims with no tenant answered %d, want 401", rec.Code)
	}
}

// --- helpers -----------------------------------------------------------------

func someone() tenancy.Claims {
	return tenancy.Claims{
		TenantID:  uuid.New(),
		AccountID: uuid.New(),
		Subject:   tenancy.SubjectAccount,
	}
}

func handler(cfg presence.Config, claims tenancy.Claims) *http.ServeMux {
	h := presencehttp.New(presence.NewService(cfg), presencehttp.Options{
		Claims: func(*http.Request) (tenancy.Claims, error) { return claims, nil },
	})
	mux := http.NewServeMux()
	h.Mount(mux)
	return mux
}

func beat(t *testing.T, claims tenancy.Claims, body string) *httptest.ResponseRecorder {
	t.Helper()
	return send(handler(presence.Config{DB: stubDB{}}, claims), http.MethodPut, "/presence", body)
}

func send(mux *http.ServeMux, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

// stubDB reaches no database and fails anything that tries.
type stubDB struct{}

var errStub = errors.New("presencehttp: the stub database was asked for something")

func (stubDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errStub }
func (stubDB) QueryRow(context.Context, string, ...any) pgx.Row        { return stubRow{} }
func (stubDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errStub
}

type stubRow struct{}

func (stubRow) Scan(...any) error { return errStub }

// emptyDB answers a read with no rows, so a route that only lists can be
// exercised without a database.
type emptyDB struct{ stubDB }

func (emptyDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return noRows{}, nil }

type noRows struct{ pgx.Rows }

func (noRows) Next() bool { return false }
func (noRows) Err() error { return nil }
func (noRows) Close()     {}

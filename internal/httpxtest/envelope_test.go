// Package httpxtest asserts that the three hand-written HTTP surfaces rig ships
// answer a failure with the same bytes.
//
// It is in the root module because that is the only one that can import
// `rig/auth`, `rig/notify` and `rig/presence` at once — they are separate modules
// and none of them depends on another. There was nowhere a test could stand and
// see all three, which is a large part of why they drifted: `notifyhttp` and
// `presencehttp` answered a nested `{"error":{code,message}}` that neither of
// rig's client libraries can parse, and `authhttp` answered a flat one missing
// the per-field detail. Each package's own tests were happy.
package httpxtest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/auth/apikey"
	"github.com/simonjanss/rig/auth/authhttp"
	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/auth/password"
	"github.com/simonjanss/rig/auth/session"
	"github.com/simonjanss/rig/notify/notifyhttp"
	"github.com/simonjanss/rig/presence"
	"github.com/simonjanss/rig/presence/presencehttp"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
	"github.com/simonjanss/rig/runtime/throttle"
)

// The property, stated once: every one of these surfaces answers an
// unestablished caller with the same status, the same Content-Type, and the same
// set of JSON keys carrying the same values.
//
// Provoked through the claims function, because that is the one failure every one
// of the three can be made to produce without a database — and because it is the
// failure a browser sees most often.
func TestTheThreeSurfacesAnswerOneEnvelope(t *testing.T) {
	t.Parallel()

	type answer struct {
		status      int
		contentType string
		keys        []string
		body        map[string]any
	}

	got := map[string]answer{}
	for name, mux := range map[string]*http.ServeMux{
		"notifyhttp":   notifyMux(t),
		"presencehttp": presenceMux(t),
		"authhttp":     authMux(t),
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, routes[name], nil))

		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: the body is not JSON (%v): %s", name, err, rec.Body)
		}
		keys := make([]string, 0, len(body))
		for k := range body {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		got[name] = answer{
			status:      rec.Code,
			contentType: rec.Header().Get("Content-Type"),
			keys:        keys,
			body:        body,
		}
	}

	// Asserted against a literal rather than against one of the three, so no one
	// of them can drag the others along by being wrong.
	//
	// The message is each surface's own words and is deliberately not compared:
	// "this request carries no credential" and "no session" are the same answer
	// phrased for their own route. What a client branches on is the code, and what
	// a client parses is the key set.
	const (
		wantStatus      = http.StatusUnauthorized
		wantContentType = "application/json; charset=utf-8"
		wantCode        = "Unauthorized"
	)
	wantKeys := []string{"code", "message"}

	for name, a := range got {
		if a.status != wantStatus {
			t.Errorf("%s: status = %d, want %d", name, a.status, wantStatus)
		}
		if a.contentType != wantContentType {
			t.Errorf("%s: Content-Type = %q, want %q", name, a.contentType, wantContentType)
		}
		if !reflect.DeepEqual(a.keys, wantKeys) {
			t.Errorf("%s: keys = %v, want %v — a client cannot branch on a shape that "+
				"differs by route", name, a.keys, wantKeys)
		}
		if a.body["code"] != wantCode {
			t.Errorf("%s: code = %v, want %q", name, a.body["code"], wantCode)
		}
		if msg, _ := a.body["message"].(string); msg == "" {
			t.Errorf("%s: no message: %v", name, a.body)
		}
	}
}

// And the shape a client actually decodes into. Both of rig's client libraries
// read a flat struct; against a nested envelope every field comes out zero, so
// `IsUnauthorized` answers false and the caller sees a status and nothing else.
func TestEveryEnvelopeDecodesIntoTheClientsShape(t *testing.T) {
	t.Parallel()

	// rigclient/error.go's errorBody, and ts/packages/client's, spelled here so
	// the assertion is about the shape those parse rather than about ours.
	type clientView struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}

	for name, mux := range map[string]*http.ServeMux{
		"notifyhttp":   notifyMux(t),
		"presencehttp": presenceMux(t),
		"authhttp":     authMux(t),
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, routes[name], nil))

		var view clientView
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if view.Code == "" {
			t.Errorf("%s: a client decoding this gets no code, so every error "+
				"predicate answers false: %s", name, rec.Body)
		}
		if view.Message == "" {
			t.Errorf("%s: a client decoding this gets no message: %s", name, rec.Body)
		}
	}
}

// routes is a GET on each surface that goes through the claims function.
var routes = map[string]string{
	"notifyhttp":   "/notifications",
	"presencehttp": "/presence",
	"authhttp":     "/auth/sessions",
}

// refuse is the claims function notifyhttp and presencehttp are given: a caller
// who cannot be established, which is the failure every one of these surfaces can
// produce with no database and the one a browser sees most often. authhttp needs
// no equivalent — it reads the Authorization header, and the request carries none.
func refuse(*http.Request) (tenancy.Claims, error) {
	return tenancy.Claims{}, rigerr.Unauthorized("no session")
}

// unreachableDB is a pool nothing reaches: every one of these cases is refused
// before a statement is run, and a database that answered would hide that.
type unreachableDB struct{}

func (unreachableDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errUnreachable
}
func (unreachableDB) QueryRow(context.Context, string, ...any) pgx.Row { return unreachableRow{} }
func (unreachableDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errUnreachable
}

type unreachableRow struct{}

func (unreachableRow) Scan(...any) error { return errUnreachable }

var errUnreachable = errors.New("httpxtest: the database was asked for something")

func notifyMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	notifyhttp.New(nil, notifyhttp.Options{Claims: refuse}).Mount(mux)
	return mux
}

func presenceMux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	svc := presence.NewService(presence.Config{DB: unreachableDB{}})
	presencehttp.New(svc, presencehttp.Options{Claims: refuse}).Mount(mux)
	return mux
}

// authMux builds the real handler, which needs the whole service graph. The
// in-memory stores are what make that cheap — see SIMPLIFICATIONS B4, where
// keeping them was decided partly so that a test like this one can exist without
// a database.
func authMux(t *testing.T) *http.ServeMux {
	t.Helper()

	now := func() time.Time { return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC) }
	counter := throttle.NewMemory()
	tokens := session.NewMemoryStore()

	sessions, err := session.New(session.Config{Store: tokens, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	identities, err := session.NewIdentity(session.IdentityConfig{Store: tokens, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	keys, err := apikey.New(apikey.Config{Store: apikey.NewMemoryStore(), Now: now})
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := account.New(account.Config{
		Store:      account.NewMemoryStore(),
		Sessions:   sessions,
		Identities: identities,
		Hasher:     password.New(password.Params{Memory: 8 * 1024, Iterations: 1, Parallelism: 1}),
		Notifier:   account.NoNotifier{},
		Limiter:    throttle.New(counter).WithClock(now),
		Now:        now,
		Sleep:      func(context.Context, time.Duration) {},
	})
	if err != nil {
		t.Fatal(err)
	}

	// No claims hook to inject: authhttp establishes the caller from the
	// Authorization header itself, so the refusal is provoked by sending none.
	h, err := authhttp.New(authhttp.Config{
		Accounts:   accounts,
		Sessions:   sessions,
		Identities: identities,
		APIKeys:    keys,
		AuditLog:   authlog.NewMemory(),
		Tenant:     func(*http.Request) (uuid.UUID, error) { return uuid.New(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	return mux
}

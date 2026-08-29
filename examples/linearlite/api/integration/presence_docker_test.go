//go:build docker

package integration

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/examples/linearlite/internal/app"
)

// Presence, over the three hand-written routes under /presence.
//
// The read path is not here: presence is read over its live shape, which needs
// the sync service and is asserted in electric_docker_test.go. What this covers
// is the half a browser writes — and the two things about it that are structural
// rather than checked, because a test is how a structural claim stays true.
func TestPresence(t *testing.T) {
	api := newServer(t)
	tenant := api.seed(t)
	token := api.login(t, app.SeedEmail)
	demo := api.accountID(t, tenant, app.SeedEmail)

	// A tab has to name itself. Two tabs of one account are two rows, so the
	// key is what keeps them from overwriting each other on every beat.
	const tabA = "test-tab-a"
	const tabB = "test-tab-b"

	item := api.someTodo(t, tenant)

	beat := func(t *testing.T, sessionKey string, body map[string]any) response {
		t.Helper()
		body["sessionKey"] = sessionKey
		return api.do(t, request{
			method: http.MethodPut, path: "/presence", token: token, body: body,
		})
	}

	t.Run("a beat answers with the two numbers the browser is not told twice", func(t *testing.T) {
		res := beat(t, tabA, map[string]any{
			"scope":       "board",
			"targetTable": "todo",
			"targetId":    item.String(),
			"targetField": "title",
			"activity":    "editing",
		})
		if res.status != http.StatusOK {
			t.Fatalf("beat: %d %s", res.status, res.body)
		}

		var out struct {
			ID               uuid.UUID `json:"id"`
			SeenAt           string    `json:"seenAt"`
			TTLSeconds       int       `json:"ttlSeconds"`
			HeartbeatSeconds int       `json:"heartbeatSeconds"`
		}
		res.decode(t, &out)

		// The TTL and the heartbeat come back on every beat rather than being
		// compiled into web/: changing `presence:` in rig.yaml is a deploy of
		// this binary and not a release of the front end, and there is no copy
		// of either number in the browser to disagree with these.
		if out.TTLSeconds != 60 || out.HeartbeatSeconds != 20 {
			t.Errorf("ttl %ds, heartbeat %ds; rig.yaml leaves both at their defaults",
				out.TTLSeconds, out.HeartbeatSeconds)
		}
		// The write's own clock reading, which is the only one a browser gets —
		// and what makes a client-side freshness test possible without an
		// offset, a header or a second request.
		if out.SeenAt == "" {
			t.Error("no seenAt: there is nothing for a subscriber to measure against")
		}
	})

	t.Run("who is present comes from the credential", func(t *testing.T) {
		here := api.here(t, token)
		mine := here.find(tabA)
		if mine == nil {
			t.Fatalf("tab A is not present: %+v", here.Data)
		}
		// There is no account field anywhere in the request, so "you may only
		// write your own presence" is a sentence a client cannot phrase rather
		// than a rule a handler enforces.
		if mine.AccountID != demo {
			t.Errorf("presence is for %s, want the signed-in account %s", mine.AccountID, demo)
		}
		if mine.TargetField != "title" || mine.Activity != "editing" {
			t.Errorf("target came back as %q/%q, want title/editing", mine.TargetField, mine.Activity)
		}
	})

	t.Run("two tabs are two presences", func(t *testing.T) {
		if res := beat(t, tabB, map[string]any{"scope": "board"}); res.status != http.StatusOK {
			t.Fatalf("beat from a second tab: %d %s", res.status, res.body)
		}

		here := api.here(t, token)
		if here.find(tabA) == nil || here.find(tabB) == nil {
			t.Fatalf("one account's two tabs collapsed into one row: %+v", here.Data)
		}
		// And it is the reader's job to collapse them, which web/src/presence
		// does — the server keeps them apart because reading in one tab and
		// editing in another is the ordinary case.
	})

	t.Run("a leave takes one tab and not the other", func(t *testing.T) {
		leave := api.do(t, request{
			method: http.MethodDelete, path: "/presence", token: token,
			body: map[string]any{"sessionKey": tabB},
		})
		if leave.status != http.StatusNoContent {
			t.Fatalf("leave: %d %s", leave.status, leave.body)
		}

		here := api.here(t, token)
		if here.find(tabB) != nil {
			t.Error("the tab that left is still present")
		}
		if here.find(tabA) == nil {
			t.Error("the tab that stayed was taken with it")
		}

		// Again, and still 204. The leave goes out on pagehide with
		// `keepalive`, so a retry of a request whose answer was lost is the
		// ordinary case here and must not look like a failure.
		again := api.do(t, request{
			method: http.MethodDelete, path: "/presence", token: token,
			body: map[string]any{"sessionKey": tabB},
		})
		if again.status != http.StatusNoContent {
			t.Errorf("a second leave: %d %s, want 204", again.status, again.body)
		}
	})

	t.Run("a target table this API never heard of is refused", func(t *testing.T) {
		// PresenceTargets() is written from the compiled document, so this is a
		// typo boundary rather than a security one: target_table reaches no SQL
		// statement. What it buys is that a reader can trust the value means a
		// table.
		res := beat(t, tabA, map[string]any{
			"scope":       "board",
			"targetTable": "tickets",
			"targetId":    item.String(),
		})
		if res.status != http.StatusUnprocessableEntity && res.status != http.StatusBadRequest {
			t.Errorf("a made-up table: %d %s, want a refusal", res.status, res.body)
		}
	})

	t.Run("a subscriber has to be somebody", func(t *testing.T) {
		res := api.do(t, request{
			method: http.MethodPut, path: "/presence",
			body: map[string]any{"sessionKey": tabA, "scope": "board"},
		})
		if res.status != http.StatusUnauthorized {
			t.Errorf("an anonymous beat: %d %s, want 401", res.status, res.body)
		}
	})
}

// presencePerson is one row as GET /presence answers it.
//
// camelCase whatever api.json_case says, for the reason /auth/* is: these
// routes are rig's, they are identical in every project, and @rig/presence is
// compiled against them once. The live shape carries the same row under
// Postgres column names, which is the seam that package exists to hide.
type presencePerson struct {
	AccountID   uuid.UUID `json:"accountId"`
	SessionKey  string    `json:"sessionKey"`
	Scope       string    `json:"scope"`
	TargetTable string    `json:"targetTable"`
	TargetField string    `json:"targetField"`
	Activity    string    `json:"activity"`
	SeenAt      string    `json:"seenAt"`
}

// presenceHere is the whole answer: the people, and the two numbers every
// response carries.
type presenceHere struct {
	Data             []presencePerson `json:"data"`
	TTLSeconds       int              `json:"ttlSeconds"`
	HeartbeatSeconds int              `json:"heartbeatSeconds"`
}

// find answers the row for one tab, or nil.
func (h presenceHere) find(sessionKey string) *presencePerson {
	for i := range h.Data {
		if h.Data[i].SessionKey == sessionKey {
			return &h.Data[i]
		}
	}
	return nil
}

// here reads GET /presence for the board scope.
func (s *server) here(t *testing.T, token string) presenceHere {
	t.Helper()
	res := s.do(t, request{method: http.MethodGet, path: "/presence?scope=board", token: token})
	if res.status != http.StatusOK {
		t.Fatalf("here: %d %s", res.status, res.body)
	}
	var out presenceHere
	res.decode(t, &out)
	return out
}

// someTodo answers any seeded item, for a presence to point at.
func (s *server) someTodo(t *testing.T, tenant uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := s.pool.QueryRow(context.Background(),
		`SELECT id FROM todo WHERE tenant_id = $1 AND deleted_at IS NULL LIMIT 1`,
		tenant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

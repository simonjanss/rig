package rigtest

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Outcome is how an event went.
//
// The two spellings are named here because getting one wrong is not a failed
// assertion: rig_auth_outcome is a Postgres enum, so a query comparing against
// "Success" is an error at runtime from a database that will not say which of
// your queries it meant.
const (
	Succeeded = "Succeeded"
	Failed    = "Failed"
)

// LogEntry is one row of rig_auth_log — both the audit trail and what rig's rate
// limits count, deliberately the same table.
//
// That second job is worth knowing before a suite is built on it. A limit counts
// these rows over a rolling window, so a test database that outlives the run
// carries the previous run's budget: a suite signing in repeatedly as one address
// spends that address's hourly allowance and the next run is a wall of 429s
// saying nothing about the code under test. Delete the rows your own fixtures
// wrote, and only those.
type LogEntry struct {
	ID           uuid.UUID
	TenantID     *uuid.UUID
	CreatedAt    time.Time
	Event        string
	Outcome      string
	AccountID    *uuid.UUID
	EmailAddress string
	Detail       map[string]any
}

// Reason is what the entry's detail says went wrong, when it says anything.
//
// rig records the distinguishing fact here rather than in the response, because
// several refusals deliberately answer identically from outside — every way a
// code can be wrong is one error, and an address nobody has gets the same 204 as
// one somebody does. So this is where a test asks which of them actually
// happened.
func (e LogEntry) Reason() string {
	if s, ok := e.Detail["reason"].(string); ok {
		return s
	}
	return ""
}

// LogQuery narrows the trail. A zero value is everything.
type LogQuery struct {
	Event        string
	Outcome      string
	EmailAddress string
	TenantID     *uuid.UUID
}

// AuthLog reads the trail, oldest first.
func (r *Rig) AuthLog(tb testing.TB, q LogQuery) []LogEntry {
	tb.Helper()

	var (
		where []string
		args  []any
	)
	add := func(clause string, arg any) {
		args = append(args, arg)
		where = append(where, clause+"$"+strconv.Itoa(len(args)))
	}
	if q.Event != "" {
		add("event = ", q.Event)
	}
	if q.Outcome != "" {
		add("outcome = ", q.Outcome)
	}
	if q.EmailAddress != "" {
		add("lower(email_address) = lower(", q.EmailAddress)
		where[len(where)-1] += ")"
	}
	if q.TenantID != nil {
		add("tenant_id = ", *q.TenantID)
	}

	sql := `SELECT id, tenant_id, created_at, event::text, outcome::text,
	               account_id, coalesce(email_address, ''), detail
	          FROM rig_auth_log`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY created_at, id"

	rows, err := r.Pool.Query(context.Background(), sql, args...)
	if err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
	defer rows.Close()

	var out []LogEntry
	for rows.Next() {
		var (
			e      LogEntry
			detail []byte
		)
		if err := rows.Scan(&e.ID, &e.TenantID, &e.CreatedAt, &e.Event,
			&e.Outcome, &e.AccountID, &e.EmailAddress, &detail); err != nil {
			tb.Fatalf("rigtest: %v", err)
		}
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				tb.Fatalf("rigtest: %s detail is not an object: %v", e.Event, err)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		tb.Fatalf("rigtest: %v", err)
	}
	return out
}

// Events counts matching entries, for the assertion that is only about how many.
func (r *Rig) Events(tb testing.TB, q LogQuery) int {
	tb.Helper()
	return len(r.AuthLog(tb, q))
}

// ForgetAuthLog deletes one address's entries for one event, and nothing else.
//
// This is the escape from the budget [LogEntry] describes, and it is narrow on
// purpose. Everything else in that table is somebody's assertion or somebody
// else's limit, so a suite clears the rows its own repeated sign-ins wrote —
// by address and by event — rather than truncating and wondering later why an
// unrelated test went green.
func (r *Rig) ForgetAuthLog(tb testing.TB, event, emailAddress string) {
	tb.Helper()

	r.exec(tb, `
		DELETE FROM rig_auth_log
		 WHERE event = $1::rig_auth_event AND lower(email_address) = lower($2)`,
		event, emailAddress)
}

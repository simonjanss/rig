package authpg

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/authlog"
	"github.com/simonjanss/rig/runtime/dbx"
)

// Log is rig_auth_log: the writer, the reader, and the prune.
//
// One type for three contracts that a caller sees separately, because they are
// one table and splitting them here would mean three structs holding the same
// connection. Which of the three a caller is given is the interface it takes:
// [authlog.Log] for the twenty call sites that record, [authlog.Reader] for the
// endpoint that shows the trail, [authlog.Pruner] for the retention task.
type Log struct{ db dbx.Conn }

var (
	_ authlog.Log    = (*Log)(nil)
	_ authlog.Reader = (*Log)(nil)
	_ authlog.Pruner = (*Log)(nil)
)

// logColumns is the row a reader gets back. The write path names its columns
// itself, since it also writes created_at.
const logColumns = `id, tenant_id, created_at, event, outcome, account_id,
	api_key_id, email_address, api_key_ref, ip_address, user_agent,
	token_root_id, detail`

// Write implements [authlog.Log].
//
// A failure here is swallowed, deliberately. A login that succeeded and then
// returned 500 because the log was unreachable has turned an observability
// problem into an outage — and the rate limiter reading a slightly incomplete
// log is a smaller harm than the alternative.
func (l *Log) Write(ctx context.Context, e authlog.Entry) {
	// An entry with no tenant is written, not dropped. It used to be dropped on
	// the grounds that every row is tenant-scoped, and that was a mistake worth
	// naming: a sign-in that names no tenant and a guess at an address nobody
	// has both resolve to no tenant, and both are exactly what the lockout counts.
	// Dropping them removed the rate limit for the attempts that most need one.
	id, err := uuid.NewV7()
	if err != nil {
		return
	}

	// Deliberately not through conn(): an entry describing a failed attempt
	// must survive the rollback of whatever transaction noticed it.
	_, _ = l.db.Exec(ctx, `
		INSERT INTO rig_auth_log
			(id, tenant_id, created_at, event, outcome, account_id, api_key_id,
			 email_address, api_key_ref, ip_address, user_agent, token_root_id, detail)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		id, e.TenantID, e.At, e.Event, string(e.Outcome), e.AccountID, e.APIKeyID,
		nullable(e.EmailAddress), nullable(e.APIKeyRef), addrValue(e.IPAddress),
		nullable(e.UserAgent), e.TokenRootID, e.Detail)
}

// Read implements [authlog.Reader].
//
// Two statements, the page and the count, and the count is the expensive half:
// it is `count(*)` over the tenant's slice, which is what a client asking for
// the total is asking for. The generated list endpoints pay the same price for
// the same reason, so this is not a new cost, only one worth knowing about on a
// table that grows with every login.
//
// The default page rides `rig_auth_log_tenant_created_idx`. Narrowing by event
// scans the tenant's slice instead — the index that would fix it,
// `(tenant_id, event, created_at DESC)`, is a write cost on the hottest write
// path in the system for the benefit of a screen somebody opens twice a month,
// so it is not there. Add it when a real log argues for it.
func (l *Log) Read(ctx context.Context, q authlog.Query) ([]authlog.Record, int64, error) {
	where, args := logFilters(q)
	db := conn(ctx, l.db)

	var total int64
	if err := db.QueryRow(ctx,
		`SELECT count(*) FROM rig_auth_log WHERE `+strings.Join(where, " AND "), args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("authpg: count auth log: %w", err)
	}
	// Nothing matched, so there is no page to fetch and no ordering to do.
	if total == 0 {
		return nil, 0, nil
	}

	// created_at is not unique — a login writes its entry and the session's in
	// the same instant — so id breaks the tie. Without it two pages of one
	// query can show the same row twice and never show another.
	sql := `SELECT ` + logColumns + ` FROM rig_auth_log WHERE ` +
		strings.Join(where, " AND ") + ` ORDER BY created_at DESC, id DESC`
	sql += fmt.Sprintf(" LIMIT $%d OFFSET $%d", len(args)+1, len(args)+2)
	args = append(args, q.Limit, q.Offset)

	rows, err := db.Query(ctx, sql, args...)
	if err != nil {
		return nil, 0, fmt.Errorf("authpg: read auth log: %w", err)
	}
	defer rows.Close()

	out := make([]authlog.Record, 0, q.Limit)
	for rows.Next() {
		var (
			r         authlog.Record
			outcome   string
			email     *string
			keyRef    *string
			ip        *netip.Addr
			userAgent *string
		)
		if err := rows.Scan(&r.ID, &r.TenantID, &r.At, &r.Event, &outcome,
			&r.AccountID, &r.APIKeyID, &email, &keyRef, &ip, &userAgent,
			&r.TokenRootID, &r.Detail); err != nil {
			return nil, 0, fmt.Errorf("authpg: scan auth log: %w", err)
		}

		r.Outcome = authlog.Outcome(outcome)
		r.EmailAddress = derefString(email)
		r.APIKeyRef = derefString(keyRef)
		r.IPAddress = addrString(ip)
		r.UserAgent = derefString(userAgent)
		// UTC out of the database, the same rule every other instant follows —
		// and this one is rendered into JSON, so a process in another zone would
		// otherwise report the whole trail shifted.
		r.At = dbx.UTC(r.At)

		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("authpg: read auth log: %w", err)
	}
	return out, total, nil
}

// logFilters renders the predicates and their arguments.
//
// The tenant is first and unconditional. There is no code path here that reads
// another tenant's rows or the rows belonging to none, which is the whole
// security property of this reader — see [authlog.Query].
func logFilters(q authlog.Query) ([]string, []any) {
	where := []string{"tenant_id = $1"}
	args := []any{q.TenantID}

	add := func(clause string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(clause, len(args)))
	}

	if q.AccountID != nil {
		add("account_id = $%d", *q.AccountID)
	}
	if q.Event != "" {
		add("event = $%d", q.Event)
	}
	if q.Outcome != "" {
		add("outcome = $%d", string(q.Outcome))
	}
	if !q.Since.IsZero() {
		add("created_at >= $%d", q.Since)
	}
	if !q.Until.IsZero() {
		add("created_at < $%d", q.Until)
	}
	return where, args
}

// pruneBatch bounds one statement, so a table nobody has pruned in a year does
// not become a single DELETE holding a connection and a lock for an hour.
const pruneBatch = 5000

// Prune implements [authlog.Pruner].
//
// In batches, and it keeps going until a batch comes back short. The subselect
// is what makes each statement bounded; ordering it lets the index do the work
// rather than the planner choosing whichever rows it happens to reach first.
func (l *Log) Prune(ctx context.Context, olderThan time.Time) (int, error) {
	total := 0
	for {
		tag, err := conn(ctx, l.db).Exec(ctx, `
			DELETE FROM rig_auth_log
			 WHERE id IN (
			       SELECT id FROM rig_auth_log
			        WHERE created_at < $1
			        ORDER BY created_at
			        LIMIT $2
			 )`, olderThan, pruneBatch)
		if err != nil {
			return total, fmt.Errorf("authpg: prune auth log: %w", err)
		}

		n := int(tag.RowsAffected())
		total += n
		if n < pruneBatch {
			return total, nil
		}
		// A cancelled context between batches stops the job rather than being
		// noticed only when the next statement fails.
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}

// derefString reads back a column that is NULL when the value was unknown.
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

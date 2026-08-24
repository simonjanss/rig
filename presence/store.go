package presence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// DB is the pool the presence table lives in.
//
// It is the two dbx interfaces together rather than *pgxpool.Pool, so that a
// test can hand this a transaction and so the module never imports pgxpool.
type DB interface {
	dbx.Conn
	dbx.Beginner
}

// columns is what every read selects, in one place so the scan below cannot
// drift from it.
const columns = `id, tenant_id, account_id, session_key, scope,
	target_table, target_id, target_field, activity, created_at, seen_at`

// upsert writes one heartbeat.
//
// ON CONFLICT rather than a read followed by a branch, and the conflict target
// is the unique index the migration creates. That is what makes a beat one
// round trip and one statement — the client does not know whether its row exists
// (it may never have, or may have been swept while the tab slept, or may have
// been written by a beat whose answer went missing), and with an upsert it does
// not have to.
//
// `id` is generated on every call and used only when the insert wins. A UUIDv7 is
// cheap and the alternative is reading the row first to find out whether one is
// needed, which is the round trip this statement exists to avoid.
func (s *Service) upsert(
	ctx context.Context,
	claims tenancy.Claims,
	b Beat,
	activity Activity,
	now time.Time,
) (*Presence, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, rigerr.Internal(err, "cannot generate an identifier")
	}

	const q = `
		INSERT INTO ` + Table + `
			(id, tenant_id, account_id, session_key, scope,
			 target_table, target_id, target_field, activity, created_at, seen_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
		ON CONFLICT (tenant_id, account_id, session_key) DO UPDATE SET
			scope        = EXCLUDED.scope,
			target_table = EXCLUDED.target_table,
			target_id    = EXCLUDED.target_id,
			target_field = EXCLUDED.target_field,
			activity     = EXCLUDED.activity,
			seen_at      = EXCLUDED.seen_at
		RETURNING ` + columns

	row := s.cfg.DB.QueryRow(ctx, q,
		id, claims.TenantID, claims.AccountID, b.SessionKey, b.Scope,
		nullString(b.Target.Table), nullUUID(b.Target.ID), nullString(b.Target.Field),
		string(activity), now,
	)

	p, err := scan(row)
	if err != nil {
		return nil, rigerr.Internal(err, "cannot record presence")
	}
	return p, nil
}

// remove deletes one session's row.
//
// Scoped by tenant *and* account as well as by key, so the statement cannot
// reach somebody else's row even if a key were guessed. It is not the only thing
// in the way — no route takes an account — and it is the one that survives
// somebody adding one.
func (s *Service) remove(ctx context.Context, claims tenancy.Claims, sessionKey string) error {
	const q = `DELETE FROM ` + Table + `
		WHERE tenant_id = $1 AND account_id = $2 AND session_key = $3`

	if _, err := s.cfg.DB.Exec(ctx, q, claims.TenantID, claims.AccountID, sessionKey); err != nil {
		return rigerr.Internal(err, "cannot end presence")
	}
	return nil
}

// list reads who is present, fresher than `since`.
func (s *Service) list(
	ctx context.Context,
	claims tenancy.Claims,
	q Query,
	since time.Time,
) ([]*Presence, error) {
	var b strings.Builder
	b.WriteString(`SELECT ` + columns + ` FROM ` + Table + ` WHERE tenant_id = $1 AND seen_at > $2`)
	args := []any{claims.TenantID, since}

	// Built by appending a placeholder per condition rather than by interpolating
	// anything, for the reason every other query in rig gives.
	add := func(clause string, v any) {
		args = append(args, v)
		fmt.Fprintf(&b, " AND %s = $%d", clause, len(args))
	}
	if q.Scope != "" {
		add("scope", q.Scope)
	}
	if q.Target.Table != "" {
		add("target_table", q.Target.Table)
	}
	if q.Target.ID != uuid.Nil {
		add("target_id", q.Target.ID)
	}
	if q.Target.Field != "" {
		add("target_field", q.Target.Field)
	}
	// Freshest first, then by identifier so the order is total: without the
	// second term two reads can disagree about two rows written in the same
	// millisecond.
	b.WriteString(" ORDER BY seen_at DESC, id")

	rows, err := s.cfg.DB.Query(ctx, b.String(), args...)
	if err != nil {
		return nil, rigerr.Internal(err, "cannot read presence")
	}
	defer rows.Close()

	var out []*Presence
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, rigerr.Internal(err, "cannot read presence")
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, rigerr.Internal(err, "cannot read presence")
	}
	return out, nil
}

// sweep deletes what has expired, up to `limit` rows, and reports how many.
func (s *Service) sweep(ctx context.Context, before time.Time, limit int) (int, error) {
	// A subselect with a LIMIT rather than a bare DELETE, so one pass is bounded
	// and a table that had a bad week does not become a single statement holding
	// a connection while it rewrites every page.
	const q = `DELETE FROM ` + Table + ` WHERE id IN (
		SELECT id FROM ` + Table + ` WHERE seen_at < $1 LIMIT $2)`

	tag, err := s.cfg.DB.Exec(ctx, q, before, limit)
	if err != nil {
		return 0, fmt.Errorf("presence: sweep: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// scanner is what both a single row and a row of a result set satisfy.
type scanner interface{ Scan(dest ...any) error }

// scan reads one row in the order [columns] names.
func scan(r scanner) (*Presence, error) {
	var (
		p           Presence
		targetTable *string
		targetID    *uuid.UUID
		targetField *string
		activity    string
	)
	err := r.Scan(
		&p.ID, &p.TenantID, &p.AccountID, &p.SessionKey, &p.Scope,
		&targetTable, &targetID, &targetField, &activity,
		&p.CreatedAt, &p.SeenAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, err
	}
	if targetTable != nil {
		p.Target.Table = *targetTable
	}
	if targetID != nil {
		p.Target.ID = *targetID
	}
	if targetField != nil {
		p.Target.Field = *targetField
	}
	p.Activity = Activity(activity)
	return &p, nil
}

// nullString writes an empty string as SQL NULL.
//
// The three target columns are nullable because the levels of a target genuinely
// narrow — a null table is somebody in the scope and nowhere in particular — and
// an empty string would be a second spelling of the same absence that every
// reader would have to check for as well.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullUUID writes the nil UUID as SQL NULL, for the reason above.
func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

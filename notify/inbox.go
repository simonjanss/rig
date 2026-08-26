package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/dbx"
	"github.com/simonjanss/rig/runtime/rigerr"
	"github.com/simonjanss/rig/runtime/tenancy"
)

// The inbox: what a person can ask about their own notifications, and nothing
// about anybody else's.
//
// Every read here narrows to `tenant_id = $1 AND account_id = $2` and none of
// them takes a scope parameter. There is no widening for an inbox, because "read
// everybody's notifications" is not a thing an application means — and the
// deletes are against the recipient row rather than the notification, because
// one person clearing their inbox must not change what anybody else sees.
//
// A project that wants the filter grammar, the sort keys and a generated client
// over the same rows turns `notifications.expose` on and gets all of them.
// Both stay, and the difference between them is the point: these are what a
// project gets without thinking about it.

// DefaultPageSize is how many lines a page holds when the caller says nothing.
const DefaultPageSize = 50

// MaxPageSize bounds what a caller can ask for.
const MaxPageSize = 200

// InboxQuery is what a caller may narrow their own inbox by.
//
// Deliberately small. This is the bell icon's query, not a reporting surface,
// and every field here is one somebody taps rather than types.
type InboxQuery struct {
	// UnreadOnly is the filter the badge and the list share.
	UnreadOnly bool
	// Kind narrows to one kind of event. Empty is all of them.
	Kind string

	// Before pages backwards through created_at, which is the order the list is
	// in. A cursor rather than an offset, because an inbox gains rows at the top
	// while somebody is reading it and an offset would show them a line twice.
	Before *time.Time
	Limit  int
}

func (q InboxQuery) limit() int {
	switch {
	case q.Limit <= 0:
		return DefaultPageSize
	case q.Limit > MaxPageSize:
		return MaxPageSize
	default:
		return q.Limit
	}
}

// Inbox reads the caller's own notifications, newest first.
func (s *Service) Inbox(ctx context.Context, q InboxQuery) ([]*Recipient, error) {
	claims, err := s.reader(ctx)
	if err != nil {
		return nil, err
	}

	const sql = `SELECT ` + recipientColumns + ` FROM ` + RecipientTable + `
		WHERE tenant_id = $1 AND account_id = $2 AND deleted_at IS NULL
		  AND ($3::boolean IS NOT TRUE OR read_at IS NULL)
		  AND ($4::text IS NULL OR kind = $4)
		  AND ($5::timestamptz IS NULL OR created_at < $5)
		ORDER BY created_at DESC, id DESC
		LIMIT $6`

	rows, err := s.store.conn(ctx).Query(ctx, sql,
		claims.TenantID, claims.AccountID, q.UnreadOnly, dbx.Null(q.Kind), q.Before, q.limit())
	if err != nil {
		return nil, fmt.Errorf("notify: read inbox: %w", err)
	}
	defer rows.Close()

	var out []*Recipient
	for rows.Next() {
		r, err := scanRecipient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UnreadCount is the badge: one number, and the only thing most applications ask
// for on every page load.
func (s *Service) UnreadCount(ctx context.Context) (int, error) {
	claims, err := s.reader(ctx)
	if err != nil {
		return 0, err
	}

	const sql = `SELECT count(*) FROM ` + RecipientTable + `
		WHERE tenant_id = $1 AND account_id = $2 AND read_at IS NULL AND deleted_at IS NULL`

	var n int
	if err := s.store.conn(ctx).QueryRow(ctx, sql, claims.TenantID, claims.AccountID).Scan(&n); err != nil {
		return 0, fmt.Errorf("notify: unread count: %w", err)
	}
	return n, nil
}

// MarkRead marks one line read. Marking a line that is already read is not an
// error: the caller asked for it to be read, and it is.
func (s *Service) MarkRead(ctx context.Context, id uuid.UUID) error {
	claims, err := s.reader(ctx)
	if err != nil {
		return err
	}

	const sql = `UPDATE ` + RecipientTable + ` SET read_at = coalesce(read_at, $4), updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND account_id = $3 AND deleted_at IS NULL`

	tag, err := s.store.conn(ctx).Exec(ctx, sql, id, claims.TenantID, claims.AccountID, s.cfg.now())
	if err != nil {
		return fmt.Errorf("notify: mark read: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkAllRead marks what the caller can currently see, taking the same filter
// the list took.
//
// The filter is not a detail. "Mark all read" on a filtered inbox that silently
// cleared the unfiltered one is the interaction people complain about, and it is
// the reason this takes a query at all rather than being a bare statement.
func (s *Service) MarkAllRead(ctx context.Context, q InboxQuery) (int, error) {
	claims, err := s.reader(ctx)
	if err != nil {
		return 0, err
	}

	const sql = `UPDATE ` + RecipientTable + ` SET read_at = $4, updated_at = now()
		WHERE tenant_id = $1 AND account_id = $2 AND read_at IS NULL AND deleted_at IS NULL
		  AND ($3::text IS NULL OR kind = $3)`

	tag, err := s.store.conn(ctx).Exec(ctx, sql,
		claims.TenantID, claims.AccountID, dbx.Null(q.Kind), s.cfg.now())
	if err != nil {
		return 0, fmt.Errorf("notify: mark all read: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Dismiss removes one line from the caller's inbox.
//
// A soft delete against the recipient row, and the notification is untouched.
// One person clearing their inbox must not change what anybody else sees, which
// is the one structural argument for the recipient row existing separately at
// all beyond the collapse index.
func (s *Service) Dismiss(ctx context.Context, id uuid.UUID) error {
	claims, err := s.reader(ctx)
	if err != nil {
		return err
	}

	const sql = `UPDATE ` + RecipientTable + ` SET
			deleted_at = $4, deleted_by_account_id = $3, updated_at = now()
		WHERE id = $1 AND tenant_id = $2 AND account_id = $3 AND deleted_at IS NULL`

	tag, err := s.store.conn(ctx).Exec(ctx, sql, id, claims.TenantID, claims.AccountID, s.cfg.now())
	if err != nil {
		return fmt.Errorf("notify: dismiss: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// reader is the claims an inbox read needs, refusing the ones that cannot have
// an inbox.
//
// An API key and a system credential both have a nil AccountID, and a query
// narrowed by one matches nothing *silently* — which is the wrong kind of
// correct, because an empty inbox and an inbox nobody could have looked at read
// the same to whoever asked.
func (s *Service) reader(ctx context.Context) (tenancy.Claims, error) {
	claims, err := tenancy.FromContext(ctx)
	if err != nil {
		return claims, err
	}
	if claims.AccountID == uuid.Nil {
		return claims, rigerr.Forbidden(
			"an inbox belongs to an account, and this credential is not one")
	}
	return claims, nil
}

func scanRecipient(row dbx.Scanner) (*Recipient, error) {
	var r Recipient
	err := row.Scan(&r.ID, &r.TenantID, &r.NotificationID, &r.AccountID, &r.Kind,
		&r.GroupKey, &r.EventCount, &r.ReadAt, &r.CreatedAt, &r.DeletedAt)
	if err != nil {
		return nil, fmt.Errorf("notify: scan inbox line: %w", err)
	}
	r.CreatedAt = r.CreatedAt.UTC()
	return &r, nil
}

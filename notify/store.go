package notify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/runtime/dbx"
)

// DB is the pool the notification tables and their subjects live in.
//
// It is the two dbx interfaces together rather than *pgxpool.Pool, so that a
// test can hand this a transaction and so the module never imports pgxpool.
type DB interface {
	dbx.Conn
	dbx.Beginner
}

// The two managed tables. Spelled once, here, because they are the only names in
// this package a migration also has to know.
const (
	Table          = "rig_notification"
	RecipientTable = "rig_notification_recipient"
)

// notificationColumns is what every read of a notification selects, in one place
// so the scan below cannot drift from it.
const notificationColumns = `id, tenant_id, kind, state, payload, deliver_at, resolved_at,
	created_at, group_key, account_ids`

// recipientColumns is the same for an inbox line.
const recipientColumns = `id, tenant_id, notification_id, account_id, kind, group_key,
	event_count, read_at, created_at, deleted_at`

// fanOutBatch bounds how many inbox lines one statement writes.
//
// A fan-out is not one query and this package should not pretend it is: an
// announcement to a group of ten thousand is ten thousand upserts, and the bound
// belongs here for the reason the file sweeper's does — so a tenant with a bad
// week does not become a single statement holding a connection for an hour.
const fanOutBatch = 500

// store is the SQL half of the module, and nothing that decides anything.
//
// Hand-written rather than generated, for the reason the foundation already
// gives about rig_file: these tables are the same in every project, no project
// migration ever alters them, and there is nothing for a generator to vary. That
// is also what makes it safe to hand-write the routes over them.
type store struct{ db DB }

// conn is the transaction on the context, or the pool.
//
// Every statement goes through it, which is what lets an announcement be written
// inside the transaction that caused it — and what makes the delete propagation
// part of the delete rather than a follow-up.
func (s store) conn(ctx context.Context) dbx.Conn {
	if tx, ok := dbx.Tx(ctx); ok {
		return tx
	}
	return s.db
}

// insert writes the notification and its link row.
//
// Both, or neither. The link is what makes a notification findable from the row
// it is about and what makes the delete propagation possible at all, so a
// notification committed without one is a row nothing can ever clean up.
func (s store) insert(ctx context.Context, n *Notification, subject Subject) error {
	conn := s.conn(ctx)

	const q = `INSERT INTO ` + Table + `
		(id, tenant_id, kind, state, payload, deliver_at, group_key, account_ids)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`
	if _, err := conn.Exec(ctx, q,
		n.ID, n.TenantID, n.Kind, string(n.State), payloadOrEmpty(n.Payload), n.DeliverAt,
		n.GroupKey, accountsOrNil(n.AccountIDs)); err != nil {
		return fmt.Errorf("notify: insert notification: %w", err)
	}

	// The tenant is written into the link row as well as into both sides,
	// because the composite foreign key the documentation recommends is what
	// makes pointing at another tenant's row a constraint violation rather than
	// something a hook has to remember.
	link := fmt.Sprintf(
		`INSERT INTO %s (tenant_id, %s, notification_id) VALUES ($1, $2, $3)`,
		subject.LinkTable, subject.Column)
	if _, err := conn.Exec(ctx, link, n.TenantID, subject.ID, n.ID); err != nil {
		return fmt.Errorf("notify: link %s: %w", subject.LinkTable, err)
	}
	return nil
}

// due reads the notifications whose time has come.
//
// Ordered by deliver_at so the oldest goes first, and bounded, for the reason
// every sweep here is bounded.
func (s store) due(ctx context.Context, now time.Time, limit int) ([]*Notification, error) {
	const q = `SELECT ` + notificationColumns + ` FROM ` + Table + `
		WHERE state = 'Pending' AND deliver_at <= $1
		ORDER BY deliver_at
		LIMIT $2`

	rows, err := s.conn(ctx).Query(ctx, q, now, limit)
	if err != nil {
		return nil, fmt.Errorf("notify: read due notifications: %w", err)
	}
	defer rows.Close()

	var out []*Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// subjectsOf reads which rows a notification is about, from every link table it
// could be in.
//
// A notification is about one row in practice, and the query is written for the
// general case anyway: the link table is a many-to-many in both directions, so
// nothing in the schema says a notification cannot be about two.
func (s store) subjectsOf(ctx context.Context, n *Notification, links []Subject) ([]Subject, error) {
	var out []Subject
	for _, link := range links {
		q := fmt.Sprintf(
			`SELECT %s FROM %s WHERE tenant_id = $1 AND notification_id = $2`,
			link.Column, link.LinkTable)

		rows, err := s.conn(ctx).Query(ctx, q, n.TenantID, n.ID)
		if err != nil {
			return nil, fmt.Errorf("notify: read %s: %w", link.LinkTable, err)
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("notify: scan %s: %w", link.LinkTable, err)
			}
			found := link
			found.ID = id
			out = append(out, found)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("notify: read %s: %w", link.LinkTable, err)
		}
	}
	return out, nil
}

// fanOut writes one inbox line per account, and is safe to run twice.
//
// Two statements per account, in this order, and the order is the whole of the
// collapsing behaviour.
//
// **Collapse first.** If the person already has an unread line for this kind and
// this group, the event joins it: event_count goes up and the line points at the
// newest notification. Ten comments on one post become one line saying ten.
//
// **Insert if nothing was collapsed.** A first event, a group nobody set, or a
// group whose line has already been read — read it and the next comment starts a
// fresh one, which falls out of the index predicate rather than out of a rule,
// so there is nothing to get wrong about when a group ends.
//
// The insert conflicts on (notification_id, account_id) and does nothing, which
// is what makes a repeated fan-out harmless. That index is the whole reason the
// Audience contract can be "a pure read that may be called again" rather than "a
// read that had better only run once", and it is the difference between a system
// that recovers from a crash and one that duplicates somebody's inbox after it.
//
// The batching is what keeps an announcement to a large group from being one
// statement holding a connection while it runs. A fan-out is not one query and
// this should not pretend it is.
func (s store) fanOut(ctx context.Context, n *Notification, accounts []uuid.UUID) (written, collapsed int, err error) {
	conn := s.conn(ctx)

	for start := 0; start < len(accounts); start += fanOutBatch {
		end := min(start+fanOutBatch, len(accounts))

		for _, account := range accounts[start:end] {
			if n.GroupKey != nil {
				joined, err := s.collapse(ctx, n, *n.GroupKey, account)
				if err != nil {
					return written, collapsed, err
				}
				if joined {
					collapsed++
					continue
				}
			}

			const q = `INSERT INTO ` + RecipientTable + `
				(id, tenant_id, notification_id, account_id, kind, group_key)
				VALUES ($1, $2, $3, $4, $5, $6)
				ON CONFLICT (notification_id, account_id) DO NOTHING`
			tag, err := conn.Exec(ctx, q, uuid.New(), n.TenantID, n.ID, account, n.Kind, n.GroupKey)
			if err != nil {
				return written, collapsed, fmt.Errorf("notify: fan out: %w", err)
			}
			written += int(tag.RowsAffected())
		}
	}
	return written, collapsed, nil
}

// collapse folds this event into an existing unread line for the same group.
//
// Separate from the insert rather than one upsert, because the two conflicts sit
// on different indexes and Postgres takes one ON CONFLICT target per statement.
//
// The `notification_id <> $1` clause is what keeps a repeat idempotent: the
// second time this notification is resolved the line already points at it, so
// nothing is collapsed and the insert below finds its own conflict.
func (s store) collapse(ctx context.Context, n *Notification, group string, account uuid.UUID) (bool, error) {
	const q = `UPDATE ` + RecipientTable + ` SET
			event_count = event_count + 1,
			notification_id = $1,
			updated_at = now()
		WHERE account_id = $2 AND kind = $3 AND group_key = $4
		  AND read_at IS NULL AND deleted_at IS NULL
		  AND notification_id <> $1`

	tag, err := s.conn(ctx).Exec(ctx, q, n.ID, account, n.Kind, group)
	if err != nil {
		return false, fmt.Errorf("notify: collapse: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// recipientID finds the inbox line for one account, which is what a delivery
// row points at.
//
// False rather than an error when there is none: a fan-out that collapsed into
// an existing line wrote no row for this notification, and the copies that line
// is owed were written when it was.
func (s store) recipientID(ctx context.Context, notificationID, accountID uuid.UUID) (uuid.UUID, bool, error) {
	const q = `SELECT id FROM ` + RecipientTable + `
		WHERE notification_id = $1 AND account_id = $2`

	var id uuid.UUID
	err := s.conn(ctx).QueryRow(ctx, q, notificationID, accountID).Scan(&id)
	if err != nil {
		if dbx.IsNoRows(err) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, fmt.Errorf("notify: read inbox line: %w", err)
	}
	return id, true, nil
}

// resolve marks a notification's audience as computed.
//
// It does not mean anything was sent. What it means is that the inbox lines
// exist, which is the only claim this milestone's state machine makes.
func (s store) resolve(ctx context.Context, id uuid.UUID, now time.Time) error {
	const q = `UPDATE ` + Table + ` SET state = 'Resolved', resolved_at = $2, updated_at = now()
		WHERE id = $1 AND state = 'Pending'`
	if _, err := s.conn(ctx).Exec(ctx, q, id, now); err != nil {
		return fmt.Errorf("notify: resolve: %w", err)
	}
	return nil
}

// reschedule moves a pending notification's due time.
//
// This is what a publish_at that moved reaches: the notification travels with
// it, and there is no hook for anybody to remember.
func (s store) reschedule(ctx context.Context, ids []uuid.UUID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE ` + Table + ` SET deliver_at = $2, updated_at = now()
		WHERE id = ANY($1) AND state = 'Pending'`
	if _, err := s.conn(ctx).Exec(ctx, q, ids, at); err != nil {
		return fmt.Errorf("notify: reschedule: %w", err)
	}
	return nil
}

func scanNotification(row interface{ Scan(...any) error }) (*Notification, error) {
	var (
		n     Notification
		state string
	)
	err := row.Scan(&n.ID, &n.TenantID, &n.Kind, &state, &n.Payload,
		&n.DeliverAt, &n.ResolvedAt, &n.CreatedAt, &n.GroupKey, &n.AccountIDs)
	if err != nil {
		if errors.Is(err, errNoRows) || dbx.IsNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("notify: scan notification: %w", err)
	}
	n.State = State(state)
	n.DeliverAt = n.DeliverAt.UTC()
	n.CreatedAt = n.CreatedAt.UTC()
	n.ResolvedAt = dbx.UTCPtr(n.ResolvedAt)
	return &n, nil
}

// errNoRows is a sentinel the scan compares against so this package does not
// import pgx for one error value.
var errNoRows = errors.New("no rows in result set")

// payloadOrEmpty keeps the column NOT NULL without making every caller say so.
// A notification with nothing to add to its kind is the ordinary one.
func payloadOrEmpty(p []byte) []byte {
	if len(p) == 0 {
		return []byte("{}")
	}
	return p
}

// accountsOrNil writes null rather than an empty array, so that "no list was
// captured" and "a list was captured and it was empty" stay different states.
// The second one is a real answer — an announcement whose audience turned out to
// be nobody — and reading it as the first would send it to everybody.
func accountsOrNil(ids []uuid.UUID) any {
	if ids == nil {
		return nil
	}
	return ids
}

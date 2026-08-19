package notify

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// The delete propagation: what happens to a row's notifications when the row
// itself goes.
//
// "Somebody commented on ⟨deleted⟩" is the failure mode of every notification
// system, and the link table does not fix it on its own — the link row's foreign
// key restricts, so a hard delete of the subject fails on 23503 until something
// clears it, which moves the problem rather than solving it.
//
// So rig generates the calls into the subject's own writer, on both sides of the
// lifecycle, all inside the transaction that deletes the row. Nothing for the
// developer to implement and nothing to forget, which is the point: this is the
// one part of a notification system that is pure bookkeeping, and pure
// bookkeeping is exactly what a generator should own.

// Deleting is what the subject's writer calls when one of its rows is being
// soft-deleted.
//
// Cancel what is still pending about it, and soft-delete the inbox lines of what
// was already resolved. The link rows stay, because they are what says which
// lines to bring back.
func (s *Service) Deleting(ctx context.Context, subject Subject) error {
	ids, err := s.about(ctx, subject)
	if err != nil || len(ids) == 0 {
		return err
	}
	if err := s.cancel(ctx, ids); err != nil {
		return err
	}

	const q = `UPDATE ` + RecipientTable + ` SET deleted_at = $2, updated_at = now()
		WHERE notification_id = ANY($1) AND deleted_at IS NULL`
	if _, err := s.store.conn(ctx).Exec(ctx, q, ids, s.cfg.now()); err != nil {
		return fmt.Errorf("notify: retire inbox lines: %w", err)
	}
	return nil
}

// Restoring is the other side: the row is coming back, so its inbox lines do.
//
// A notification cancelled while its deliver_at went past stays cancelled, which
// is why only the recipient rows are restored and not the state. Reviving it
// would announce something that was gone when it was due.
func (s *Service) Restoring(ctx context.Context, subject Subject) error {
	ids, err := s.about(ctx, subject)
	if err != nil || len(ids) == 0 {
		return err
	}

	const q = `UPDATE ` + RecipientTable + ` SET deleted_at = NULL, updated_at = now()
		WHERE notification_id = ANY($1) AND deleted_at IS NOT NULL AND deleted_by_account_id IS NULL`
	if _, err := s.store.conn(ctx).Exec(ctx, q, ids); err != nil {
		return fmt.Errorf("notify: restore inbox lines: %w", err)
	}
	return nil
}

// Deleted is what the writer calls when a row is being removed outright.
//
// Two things go: the link rows, which is what lets the subject's delete succeed
// at all rather than failing on 23503, and the inbox lines, because "somebody
// commented on ⟨deleted⟩" is the failure this exists to prevent.
//
// The notification row itself stays, and that is deliberate rather than an
// oversight. A notification can be about rows in two tables, and deleting it
// here would fail on the other table's link — aborting a delete that had already
// succeeded. What is left is a resolved notification nothing points at, which is
// precisely the second thing the retention sweep is for.
func (s *Service) Deleted(ctx context.Context, subject Subject) error {
	ids, err := s.about(ctx, subject)
	if err != nil {
		return err
	}

	link := fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, subject.LinkTable, subject.Column)
	if _, err := s.store.conn(ctx).Exec(ctx, link, subject.ID); err != nil {
		return fmt.Errorf("notify: unlink %s: %w", subject.LinkTable, err)
	}
	if len(ids) == 0 {
		return nil
	}

	const orphans = `DELETE FROM ` + RecipientTable + ` WHERE notification_id = ANY($1)`
	if _, err := s.store.conn(ctx).Exec(ctx, orphans, ids); err != nil {
		return fmt.Errorf("notify: delete inbox lines: %w", err)
	}
	return nil
}

// cancel stops what has not been resolved yet.
//
// Pending only, always. Mail that is out cannot be recalled, and a state
// transition that pretended otherwise would be a lie the schema tells.
func (s *Service) cancel(ctx context.Context, ids []uuid.UUID) error {
	const q = `UPDATE ` + Table + ` SET state = 'Cancelled', updated_at = now()
		WHERE id = ANY($1) AND state = 'Pending'`
	if _, err := s.store.conn(ctx).Exec(ctx, q, ids); err != nil {
		return fmt.Errorf("notify: cancel: %w", err)
	}
	return nil
}

// about reads every notification linked to a row.
func (s *Service) about(ctx context.Context, subject Subject) ([]uuid.UUID, error) {
	q := fmt.Sprintf(`SELECT notification_id FROM %s WHERE %s = $1`,
		subject.LinkTable, subject.Column)
	return s.ids(ctx, q, subject.ID)
}

// pendingAbout is [Service.about] narrowed to what can still be moved.
func (s *Service) pendingAbout(ctx context.Context, subject Subject) ([]uuid.UUID, error) {
	q := fmt.Sprintf(
		`SELECT l.notification_id FROM %s l
		 JOIN %s n ON n.id = l.notification_id
		 WHERE l.%s = $1 AND n.state = 'Pending'`,
		subject.LinkTable, Table, subject.Column)
	return s.ids(ctx, q, subject.ID)
}

func (s *Service) ids(ctx context.Context, q string, args ...any) ([]uuid.UUID, error) {
	rows, err := s.store.conn(ctx).Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("notify: read links: %w", err)
	}
	defer rows.Close()

	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("notify: scan link: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

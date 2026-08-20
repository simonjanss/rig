package notify

import (
	"context"
	"fmt"
	"time"
)

// DefaultRetention is how long a read and dismissed inbox line is kept.
//
// Ninety days: long enough that "what was I told in the spring" has an answer,
// and short enough that the busiest table in the schema does not grow forever.
const DefaultRetention = 90 * 24 * time.Hour

// pruneBatch bounds one pass, for the reason every sweep here is bounded.
const pruneBatch = 500

// PruneReport is what one retention pass removed.
//
// Every count including the zeros, for the reason the file sweeper's report
// gives: a pass that removed nothing is the ordinary case and still worth
// seeing, because the absence of a line cannot be told from the job not running.
type PruneReport struct {
	// Lines are inbox lines that were read and dismissed, past retention.
	Lines int
	// Deliveries are the copies of them, which have to go first: a delivery
	// points at a line.
	Deliveries int
	// Notifications are the ones left with no inbox line and no link pointing at
	// them — which is what a hard-deleted subject leaves behind, deliberately.
	Notifications int
}

// String is the one line a pass is worth in a log.
func (r PruneReport) String() string {
	return fmt.Sprintf("notify: pruned %d lines, %d deliveries, %d notifications",
		r.Lines, r.Deliveries, r.Notifications)
}

// Prune removes what nothing will read again.
//
// rig prunes almost nothing else — the auth log grows forever, expired session
// tokens are never deleted — and this milestone adds three tables to a schema
// that already does not clean up after itself, the busiest of them getting a row
// per person per channel per event. So this is the one sweep that exists, and it
// has two rules and no third:
//
// **Inbox lines that were read and dismissed**, past `retention`, with their
// deliveries. Read but not dismissed is somebody's history and stays; dismissed
// but unread is somebody's decision and also stays until the window passes.
//
// **Notifications nothing points at any more.** A hard-deleted subject leaves
// exactly this behind on purpose: the delete could not remove the notification
// itself, because it might be about a row in another table whose link would
// refuse, so it leaves a resolved row for this pass.
//
// It runs in the dispatch task rather than on its own schedule, the way the file
// sweeper's two rules share one.
func (e *Engine) Prune(ctx context.Context, retention time.Duration) (PruneReport, error) {
	var report PruneReport
	if retention <= 0 {
		retention = DefaultRetention
	}
	before := e.cfg.now().Add(-retention)

	// Deliveries first: one points at a line, and the foreign key would refuse
	// the other order.
	const deliveries = `DELETE FROM ` + DeliveryTable + ` WHERE id IN (
			SELECT d.id FROM ` + DeliveryTable + ` d
			JOIN ` + RecipientTable + ` r ON r.id = d.recipient_id
			WHERE r.deleted_at IS NOT NULL AND r.read_at IS NOT NULL AND r.deleted_at < $1
			LIMIT $2)`
	tag, err := e.store.conn(ctx).Exec(ctx, deliveries, before, pruneBatch)
	if err != nil {
		return report, fmt.Errorf("notify: prune deliveries: %w", err)
	}
	report.Deliveries = int(tag.RowsAffected())

	const lines = `DELETE FROM ` + RecipientTable + ` WHERE id IN (
			SELECT id FROM ` + RecipientTable + `
			WHERE deleted_at IS NOT NULL AND read_at IS NOT NULL AND deleted_at < $1
			  AND NOT EXISTS (SELECT 1 FROM ` + DeliveryTable + ` d WHERE d.recipient_id = ` +
		RecipientTable + `.id)
			LIMIT $2)`
	tag, err = e.store.conn(ctx).Exec(ctx, lines, before, pruneBatch)
	if err != nil {
		return report, fmt.Errorf("notify: prune inbox lines: %w", err)
	}
	report.Lines = int(tag.RowsAffected())

	// And the notifications nothing points at. Bounded by the same batch and by
	// the same age, so a notification resolved this morning is not swept because
	// its one recipient dismissed it.
	notifications, err := e.pruneOrphans(ctx, before)
	if err != nil {
		return report, err
	}
	report.Notifications = notifications

	return report, nil
}

// pruneOrphans removes notifications with no inbox line and no link.
//
// The link half is the part that needs the subquery over every link table, and
// it is written from the engine's own list rather than from the catalogue: the
// table names come from the compiled document, which is what makes it safe to
// build a statement out of them.
func (e *Engine) pruneOrphans(ctx context.Context, before time.Time) (int, error) {
	q := `DELETE FROM ` + Table + ` n
		WHERE n.created_at < $1
		  AND n.state <> 'Pending'
		  AND NOT EXISTS (SELECT 1 FROM ` + RecipientTable + ` r WHERE r.notification_id = n.id)`

	for _, link := range e.links {
		q += fmt.Sprintf(
			"\n\t\t  AND NOT EXISTS (SELECT 1 FROM %s l WHERE l.notification_id = n.id)",
			link.LinkTable)
	}

	tag, err := e.store.conn(ctx).Exec(ctx, q, before)
	if err != nil {
		return 0, fmt.Errorf("notify: prune notifications: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

package authpg

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/simonjanss/rig/auth/account"
	"github.com/simonjanss/rig/runtime/dbx"
)

// The mail queue's SQL, and the shape it is in is the one notify's dispatcher
// argues for at length in its own file header: a claim is a lease rather than a
// row lock, because a lock lives as long as its transaction and this one would be
// held across a call into somebody's mail provider.
//
// The statements are close enough to rig_notification_delivery's to be recognised
// on sight, and that is the point of the table sharing its shape. They are
// duplicated rather than shared because rig/auth cannot import rig/notify, and
// the alternative — a queue package in runtime/ that interpolates table names
// supplied by its callers — is a worse thing to have than a second copy of
// thirty-five lines. The one invariant that must not drift between the two is the
// claim-TTL against send-timeout refusal, and account.resolveMail names
// notify.NewEngine where it makes it.

// deliveryTable is the queue.
const deliveryTable = "rig_identity_verification_delivery"

// OutboxStore is an [account.Outbox] over the foundation's tables.
type OutboxStore struct {
	db dbx.Conn
}

// Enqueue implements [account.Outbox].
//
// Through conn, so it joins the transaction that wrote the verification row. The
// two belong together: a link with no delivery is one nobody will ever mail, and
// it is invisible except as an invitation in a listing that was never sent.
func (s *OutboxStore) Enqueue(ctx context.Context, d *account.Delivery) error {
	const q = `INSERT INTO ` + deliveryTable + `
			(id, verification_id, kind, state, deliver_at)
		VALUES ($1, $2, $3, $4, $5)`
	_, err := dbx.ConnFor(ctx, s.db).Exec(ctx, q,
		d.ID, d.VerificationID, string(d.Kind), string(account.DeliveryPending), d.DeliverAt)
	if err != nil {
		return fmt.Errorf("authpg: enqueue a link: %w", err)
	}
	return nil
}

// Claim implements [account.Outbox], in the one statement that makes this
// contention-free.
//
// The inner SELECT does the work: SKIP LOCKED walks past whatever another
// claimant is taking rather than queueing behind it, and the outer UPDATE stamps
// the lease. `claimed_at IS NULL OR claimed_at < now() - ttl` is what makes a
// crashed process recoverable at all — the row is still Pending with a stale
// claim, and the next dispatcher past it takes it.
func (s *OutboxStore) Claim(ctx context.Context, by uuid.UUID, now time.Time, ttl time.Duration, limit int) ([]account.Delivery, error) {
	const q = `
		UPDATE ` + deliveryTable + ` SET
			claimed_at = $1, claimed_by = $2, attempts = attempts + 1, updated_at = now()
		WHERE id IN (
			SELECT id FROM ` + deliveryTable + `
			WHERE state = 'Pending'
			  AND deliver_at <= $1
			  AND (claimed_at IS NULL OR claimed_at < $3)
			ORDER BY deliver_at
			LIMIT $4
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, verification_id, kind, state, deliver_at, attempts`

	rows, err := dbx.ConnFor(ctx, s.db).Query(ctx, q, now, by, now.Add(-ttl), limit)
	if err != nil {
		return nil, fmt.Errorf("authpg: claim links: %w", err)
	}
	defer rows.Close()

	var out []account.Delivery
	for rows.Next() {
		var (
			d           account.Delivery
			kind, state string
		)
		if err := rows.Scan(&d.ID, &d.VerificationID, &kind, &state, &d.DeliverAt,
			&d.Attempts); err != nil {
			return nil, fmt.Errorf("authpg: scan a claim: %w", err)
		}
		d.Kind = account.VerificationKind(kind)
		d.State = account.DeliveryState(state)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RotateToken implements [account.Outbox].
//
// One statement, because the check and the write cannot be two: between reading
// "not consumed" and writing a hash, another pass could redeem the link, and both
// would then believe they held the live token. The WHERE clause is the check.
func (s *OutboxStore) RotateToken(ctx context.Context, verificationID uuid.UUID, hash []byte, expiresAt time.Time) (bool, error) {
	const q = `UPDATE rig_identity_verification SET token_hash = $2, expires_at = $3
		WHERE id = $1 AND consumed_at IS NULL AND revoked_at IS NULL
		RETURNING id`

	var id uuid.UUID
	err := dbx.ConnFor(ctx, s.db).QueryRow(ctx, q, verificationID, hash, expiresAt).Scan(&id)
	if err != nil {
		if dbx.IsNoRows(err) {
			// Consumed or withdrawn between being queued and being sent, which is
			// a Skipped delivery rather than a failed one.
			return false, nil
		}
		return false, fmt.Errorf("authpg: rotate a link's token: %w", err)
	}
	return true, nil
}

// MarkSent implements [account.Outbox].
func (s *OutboxStore) MarkSent(ctx context.Context, id uuid.UUID, at time.Time) error {
	const q = `UPDATE ` + deliveryTable + ` SET
			state = 'Sent', sent_at = $2, claimed_at = NULL, updated_at = now()
		WHERE id = $1`
	if _, err := dbx.ConnFor(ctx, s.db).Exec(ctx, q, id, at); err != nil {
		return fmt.Errorf("authpg: mark a link sent: %w", err)
	}
	return nil
}

// MarkFailed implements [account.Outbox].
func (s *OutboxStore) MarkFailed(ctx context.Context, id uuid.UUID, reason string, _ time.Time) error {
	const q = `UPDATE ` + deliveryTable + ` SET
			state = 'Failed', failed_reason = $2, claimed_at = NULL, updated_at = now()
		WHERE id = $1`
	if _, err := dbx.ConnFor(ctx, s.db).Exec(ctx, q, id, reason); err != nil {
		return fmt.Errorf("authpg: mark a link failed: %w", err)
	}
	return nil
}

// MarkSkipped implements [account.Outbox].
func (s *OutboxStore) MarkSkipped(ctx context.Context, id uuid.UUID, reason string, _ time.Time) error {
	const q = `UPDATE ` + deliveryTable + ` SET
			state = 'Skipped', failed_reason = $2, claimed_at = NULL, updated_at = now()
		WHERE id = $1`
	if _, err := dbx.ConnFor(ctx, s.db).Exec(ctx, q, id, reason); err != nil {
		return fmt.Errorf("authpg: mark a link skipped: %w", err)
	}
	return nil
}

// Retry implements [account.Outbox].
func (s *OutboxStore) Retry(ctx context.Context, id uuid.UUID, at time.Time, reason string, _ time.Time) error {
	const q = `UPDATE ` + deliveryTable + ` SET
			deliver_at = $2, failed_reason = $3, claimed_at = NULL, updated_at = now()
		WHERE id = $1`
	if _, err := dbx.ConnFor(ctx, s.db).Exec(ctx, q, id, at, reason); err != nil {
		return fmt.Errorf("authpg: schedule a retry: %w", err)
	}
	return nil
}

// Abandon implements [account.Outbox].
//
// The attempt is given back with the claim, and that is the part worth spelling
// out. Claim charges every row in the batch an attempt before anything is sent, so
// a row released without a send would have paid for one it never got — and
// MaxAttempts of those, on a provider slow enough to abandon the tail of every
// batch, would fail a delivery no notifier had ever been asked about.
//
// claimed_by is in the WHERE clause for the same reason: past a lease that expired
// anyway, the row may be somebody else's now, and the attempt to give back would
// be theirs.
func (s *OutboxStore) Abandon(ctx context.Context, ids []uuid.UUID, by uuid.UUID, _ time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE ` + deliveryTable + ` SET
			attempts = greatest(attempts - 1, 0), claimed_at = NULL, updated_at = now()
		WHERE id = ANY($1) AND claimed_by = $2 AND state = 'Pending'`
	if _, err := dbx.ConnFor(ctx, s.db).Exec(ctx, q, ids, by); err != nil {
		return fmt.Errorf("authpg: abandon links: %w", err)
	}
	return nil
}

// ReleaseClaims implements [account.Outbox].
//
// The attempts stay where they are, which is the one thing Abandon does that this
// deliberately does not: the send it is giving up on was made.
func (s *OutboxStore) ReleaseClaims(ctx context.Context, by uuid.UUID, _ time.Time) (int, error) {
	const q = `UPDATE ` + deliveryTable + ` SET claimed_at = NULL, updated_at = now()
		WHERE claimed_by = $1 AND claimed_at IS NOT NULL AND state = 'Pending'`
	tag, err := dbx.ConnFor(ctx, s.db).Exec(ctx, q, by)
	if err != nil {
		return 0, fmt.Errorf("authpg: release link claims: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// Prune deletes deliveries that are done and older than olderThan, in the same
// task that dispatches them.
//
// Only the terminal states, and never a Pending row: a schema that grows forever
// is the state most of rig's tables are still in, and this one is written at the
// rate people forget their passwords. The verification rows are left alone — they
// are the audit trail of who was invited and when, and pruning those is a decision
// this queue does not get to make.
func (s *OutboxStore) Prune(ctx context.Context, olderThan time.Duration, now time.Time) (int, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	const q = `DELETE FROM ` + deliveryTable + `
		WHERE state <> 'Pending' AND coalesce(updated_at, created_at) < $1`
	tag, err := dbx.ConnFor(ctx, s.db).Exec(ctx, q, now.Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("authpg: prune links: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

package throttle

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/simonjanss/rig/runtime/dbx"
)

// Tally counts into rig_throttle.
//
// It is the [Recorder] half of this package, for the limits whose event is not
// worth a row of its own — which is every ordinary API call. Where [Postgres]
// reads a trail somebody was keeping anyway, this keeps a number, and the
// difference in cost is the reason the two exist separately.
//
// The window slides by weighting: a limit's window is the bucket size, and the
// count is the current bucket plus however much of the previous one is still
// inside the window. That is an approximation of a true sliding window and it
// is a deliberate one — the exact answer is a row per event, which is the shape
// this table exists to avoid. It is much closer than plain fixed buckets, which
// let twice the limit through either side of a boundary.
type Tally struct {
	db  dbx.Conn
	cfg TallyConfig
}

// TallyConfig says where the counters are.
type TallyConfig struct {
	// Table is the counter table. Empty means rig_throttle.
	Table string
}

// DefaultTallyConfig is the table runtime's foundation creates.
func DefaultTallyConfig() TallyConfig { return TallyConfig{Table: "rig_throttle"} }

// NewTally builds a recorder over a connection.
func NewTally(db dbx.Conn, cfg TallyConfig) *Tally {
	if cfg.Table == "" {
		cfg = DefaultTallyConfig()
	}
	return &Tally{db: db, cfg: cfg}
}

// Incr implements [Recorder].
func (t *Tally) Incr(ctx context.Context, limit Limit, key Key, now time.Time, delta int) (int, time.Time, error) {
	if limit.Window <= 0 {
		// A zero window is a limit nobody finished configuring. Counting into a
		// bucket of no width would divide by zero below, and defaulting to some
		// window would enforce a number nobody chose.
		return 0, time.Time{}, fmt.Errorf("throttle: limit %s has no window", limit.Name)
	}
	if delta < 0 {
		return 0, time.Time{}, fmt.Errorf("throttle: limit %s incremented by %d", limit.Name, delta)
	}

	// UTC first: Truncate divides absolute time, so the bucket an instant falls
	// in is the same either way, but the value that reaches Postgres should not
	// depend on the server's zone. dbx has the same argument the other way for
	// reads.
	now = now.UTC()
	bucket := now.Truncate(limit.Window)
	prev := bucket.Add(-limit.Window)

	// How much of the previous bucket is still inside a window ending now. At
	// the instant of a rollover this is all of it, and it falls to nothing by
	// the end of the bucket — which is what makes the count slide rather than
	// step.
	weight := 1 - float64(now.Sub(bucket))/float64(limit.Window)

	var total int
	err := t.db.QueryRow(ctx, t.sql(), limit.Name, key.Kind, key.Value, bucket, weight, prev, delta).Scan(&total)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("throttle: count %s: %w", limit.Name, err)
	}

	// The end of the current bucket: the earliest moment the answer can change
	// in the caller's favour, which is not the same as the moment it will. A
	// caller far enough over is sent back early and refused again, which is the
	// same way round Allow is deliberately wrong and for the same reason —
	// pinning it exactly would cost a second query on the requests least worth
	// spending one on.
	return total, bucket.Add(limit.Window), nil
}

// sql builds the counting statement.
//
// One statement, because two would race: a separate read and write could
// straddle another replica's increment and answer with a count that never
// existed. The insert both spends the slot and reports the total, so the number
// a decision is made from is the number the write produced.
func (t *Tally) sql() string {
	return fmt.Sprintf(`
WITH bumped AS (
    INSERT INTO %[1]s (limit_name, key_kind, key_value, bucket_at, n)
    VALUES ($1, $2, $3, $4, $7)
    ON CONFLICT (limit_name, key_kind, key_value, bucket_at)
    DO UPDATE SET n = %[1]s.n + EXCLUDED.n
    RETURNING n
)
SELECT ((SELECT n FROM bumped)
      + coalesce((SELECT round(n * $5::float8) FROM %[1]s
                  WHERE limit_name = $1 AND key_kind = $2
                    AND key_value = $3 AND bucket_at = $6), 0))::int`, t.cfg.Table)
}

// Sweep deletes tallies whose window has passed, and returns how many.
//
// Unlike the auth log there is no retention window to respect here and nothing
// to get wrong by pruning too eagerly: a bucket older than the longest limit
// cannot be counted by anything, so deleting it cannot free a caller who is
// still over. That is most of what this table buys over counting the audit log,
// where the same sweep would quietly clear a lockout.
func (t *Tally) Sweep(ctx context.Context, olderThan time.Duration, now time.Time) (int64, error) {
	if olderThan <= 0 {
		return 0, errors.New("throttle: sweep needs a positive age")
	}
	tag, err := t.db.Exec(ctx,
		fmt.Sprintf("DELETE FROM %s WHERE bucket_at < $1", t.cfg.Table),
		now.UTC().Add(-olderThan))
	if err != nil {
		return 0, fmt.Errorf("throttle: sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}

package files

import (
	"context"
	"errors"
	"fmt"

	"github.com/simonjanss/rig/files/blob"
)

// sweepBatch bounds one pass, so a bucket with a bad week does not become a
// single query holding a connection for an hour.
const sweepBatch = 500

// SweepReport is what one pass did, for the log line the task writes.
type SweepReport struct {
	// Abandoned is the number of rows reaped because their bytes never arrived.
	Abandoned int
	// Expired is the number reaped because their restore window closed.
	Expired int
	// Remarked is the number of deleted objects the bucket had not been told
	// about, brought back into step.
	Remarked int
}

func (r SweepReport) String() string {
	return fmt.Sprintf("files: swept %d abandoned, %d expired, re-marked %d",
		r.Abandoned, r.Expired, r.Remarked)
}

// Sweep is the whole of rig's file housekeeping: two rules, and no third.
//
// **Abandoned uploads.** A row with a null uploaded_at is either an upload in
// flight or the remains of a request that died. Past `files.abandoned_after` it
// is the second, and both the object and the row go.
//
// **Trash past the window.** A soft-deleted file stays restorable for
// `files.restore_window`, and its bytes have to outlive the delete for exactly
// that long — a restore inside the window that handed back a row pointing at
// nothing would be worse than refusing. Past it, both go.
//
// There is deliberately **no unreferenced-file reaper**. Finding those means
// enumerating every foreign key pointing at rig_file, and the failure mode of
// getting that wrong is deleting somebody's data. A file is immutable and is
// never deleted because the thing referencing it changed: a table with snapshots
// copies the whole row on every update, so after three picture changes the first
// file is referenced by three version rows, and reaping it on replace would
// corrupt the history.
//
// It is a task rather than a goroutine, so it is a subcommand in a cron job
// rather than something racing itself in every replica.
func (s *Service) Sweep(ctx context.Context) (SweepReport, error) {
	var report SweepReport
	if s.cfg.Store == nil {
		return report, nil
	}
	now := s.cfg.now()

	abandoned, err := s.store.pending(ctx, now.Add(-s.cfg.abandonedAfter()), sweepBatch)
	if err != nil {
		return report, err
	}
	for _, f := range abandoned {
		if err := s.reap(ctx, f); err != nil {
			return report, err
		}
		report.Abandoned++
	}

	expired, err := s.store.expired(ctx, now.Add(-s.cfg.restoreWindow()), sweepBatch)
	if err != nil {
		return report, err
	}
	for _, f := range expired {
		if err := s.reap(ctx, f); err != nil {
			return report, err
		}
		report.Expired++
	}

	// And the reconciliation the mark exists for. A delete commits the row and
	// then marks the object, best-effort — so a mark that failed leaves the two
	// out of step in the safe direction, and this is what brings them back.
	marker, ok := s.cfg.Store.(blob.Marker)
	if !ok {
		return report, nil
	}
	trashed, err := s.store.trashed(ctx, now.Add(-s.cfg.restoreWindow()), sweepBatch)
	if err != nil {
		return report, err
	}
	for _, f := range trashed {
		if err := marker.Mark(ctx, f.StorageKey, blob.StateDeleted, *f.DeletedAt); err != nil {
			return report, fmt.Errorf("files: re-mark %s: %w", f.ID, err)
		}
		report.Remarked++
	}

	return report, nil
}

// reap removes the object and then the row.
//
// The object first. A row removed before its object leaves bytes nothing points
// at, which needs a bucket scan to find; an object removed before its row leaves
// a row the next pass will try again, which needs one query. Delete is
// idempotent for that reason.
func (s *Service) reap(ctx context.Context, f *File) error {
	if err := s.cfg.Store.Delete(ctx, f.StorageKey); err != nil && !errors.Is(err, blob.ErrNotFound) {
		return fmt.Errorf("files: remove object for %s: %w", f.ID, err)
	}
	return s.store.hardDelete(ctx, f.ID)
}

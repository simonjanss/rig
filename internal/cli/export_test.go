package cli

import (
	"io"
	"time"
)

// MainAt is [Main] with a clock of the caller's choosing.
//
// The revision only moves when the API surface does, and the way to test that
// is to run twice on different days without waiting for one. This file is
// compiled into the test binary and nowhere else, so the clock is a seam for
// the suite rather than part of what rig offers.
func MainAt(args []string, stdout, stderr io.Writer, now time.Time) int {
	return execute(&env{
		out:    stdout,
		errOut: stderr,
		now:    func() time.Time { return now },
	}, args)
}

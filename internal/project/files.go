package project

import (
	"reflect"
	"slices"
	"time"

	"github.com/simonjanss/rig/internal/diag"
)

// The backends a project can keep its bytes in.
const (
	// BackendMemory keeps objects in a map. For tests and `go run`, and not
	// durable — every upload is lost on a restart.
	BackendMemory = "memory"
	// BackendS3 is S3 and anything speaking its API.
	BackendS3 = "s3"
)

// Backends is every accepted value, for the schema and for the diagnostic.
func Backends() []string { return []string{BackendMemory, BackendS3} }

// Configured reports whether the files block says anything beyond how the
// managed table is treated, the same way [Auth.Configured] does.
func (f Files) Configured() bool {
	bare := Files{Enabled: f.Enabled, Expose: f.Expose}
	return !reflect.DeepEqual(f, bare)
}

// RestoreWindowDays is [Files.RestoreWindow] in whole days, which is the
// granularity a generated restore window is written in.
//
// rig_file has no `restore_window_days` of its own — this is where the number
// the compiler puts on the resource comes from — so the conversion happens
// here rather than in a table configuration nobody writes.
//
// Truncated rather than rounded up: the row has to stop being restorable
// before the bytes behind it go, and a window that outlived them would hand a
// caller back a row pointing at nothing.
func (f Files) RestoreWindowDays() int {
	return int(f.RestoreWindow.Duration() / (24 * time.Hour))
}

// checkFiles validates what the JSON Schema cannot: values that only make sense
// relative to each other, and a block nothing reads.
func (p *Project) checkFiles() diag.List {
	var diags diag.List
	f := p.Config.Files

	if !f.Enabled {
		// The same failure mode the auth block has, and worth refusing for the
		// same reason: a byte cap somebody set and believed in, silently unread.
		if f.Configured() {
			diags.Add(diag.CodeConfigInvalid, p.At("files", "enabled"),
				"files is configured but files.enabled is false, so none of it is read; "+
					"set `enabled: true` or remove the block")
		}
		return diags
	}

	if !slices.Contains(Backends(), f.Backend) {
		diags.Add(diag.CodeConfigInvalid, p.At("files", "backend"),
			"files.backend is %q; it has to be one of %v", f.Backend, Backends())
	}

	if f.Backend == BackendS3 && f.S3.Bucket == "" && f.S3.BucketEnv == "" {
		diags.Add(diag.CodeConfigInvalid, p.At("files", "s3", "bucket"),
			"files.backend is s3, so one of files.s3.bucket and files.s3.bucket_env has to say which bucket")
	}

	// Only the cap is checked for sign. The durations cannot be wrong in this
	// direction and there is nothing here pretending otherwise: the schema's
	// pattern refuses a negative one outright, and a zero is read as "left out"
	// and defaulted, the way every duration in rig.yaml is. A check for either
	// would be a check that never runs, which is worse than no check — it reads
	// like coverage.
	if f.MaxBytes < 0 {
		diags.Add(diag.CodeConfigInvalid, p.At("files", "max_bytes"),
			"files.max_bytes is %d; a negative cap accepts nothing", f.MaxBytes)
	}

	// The two windows are the only durations here that bound the same thing —
	// how long bytes nobody is using stay in the bucket — so they are the pair
	// worth comparing. An abandoned_after longer than restore_window means the
	// remains of an upload that never finished sit there longer than a file
	// somebody deliberately deleted, which is legal and is almost always the
	// two numbers having been set at different times.
	if f.AbandonedAfter.Duration() > 0 && f.RestoreWindow.Duration() > 0 &&
		f.AbandonedAfter.Duration() > f.RestoreWindow.Duration() {
		diags.AddSeverity(diag.CodeConfigInvalid, diag.SeverityWarning,
			p.At("files", "abandoned_after"),
			"files.abandoned_after (%s) is longer than files.restore_window (%s), so an abandoned "+
				"upload outlives a deleted file; that is legal and probably not what was meant",
			f.AbandonedAfter, f.RestoreWindow)
	}

	// Exposing rig_file gives it a generated restore endpoint, and that window
	// is a whole number of days — there is no `restore_window_days` on this
	// table to say otherwise, so [Files.RestoreWindowDays] is what the
	// generators get. Under a day truncates to zero, which is a restore
	// endpoint that refuses everything.
	if f.Expose && f.RestoreWindow.Duration() > 0 && f.RestoreWindowDays() < 1 {
		diags.Add(diag.CodeConfigInvalid, p.At("files", "restore_window"),
			"files.restore_window is %s, and rig_file's generated restore window is a whole "+
				"number of days, so with `files.expose: true` it has to be at least 24h",
			f.RestoreWindow)
	}

	if f.CookieDownloads && !p.Config.Auth.Enabled {
		diags.Add(diag.CodeConfigInvalid, p.At("files", "cookie_downloads"),
			"files.cookie_downloads accepts the session cookie on download routes, and there are "+
				"no sessions without `auth.enabled: true`")
	}

	return diags
}

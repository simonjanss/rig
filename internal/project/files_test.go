package project_test

import (
	"strings"
	"testing"
	"time"

	"github.com/simonjanss/rig/internal/project"
)

// parseFiles is the whole loop: text in, resolved configuration and diagnostics
// out. Going through Parse rather than building a Config by hand is the point —
// the defaults are applied there, and a test that skipped it would be testing a
// struct nobody uses.
func parseFiles(t *testing.T, body string) (*project.Project, string) {
	t.Helper()

	src := "project:\n  name: demo\n  module: example.com/demo\n" + body
	p, diags := project.Parse("rig.yaml", []byte(src))
	return p, diags.String()
}

func TestFilesDefaults(t *testing.T) {
	p, out := parseFiles(t, "files:\n  enabled: true\n")
	if out != "" {
		t.Fatalf("a bare enabled block should be valid:\n%s", out)
	}

	f := p.Config.Files
	if f.Backend != project.BackendMemory {
		t.Errorf("backend = %q, want memory", f.Backend)
	}
	if f.MaxBytes != project.DefaultFilesMaxBytes {
		t.Errorf("max_bytes = %d, want %d", f.MaxBytes, project.DefaultFilesMaxBytes)
	}
	if f.RestoreWindow.Duration() != project.DefaultFilesRestoreWindow {
		t.Errorf("restore_window = %s, want %s", f.RestoreWindow, project.DefaultFilesRestoreWindow)
	}
	if f.AbandonedAfter.Duration() != project.DefaultFilesAbandonedAfter {
		t.Errorf("abandoned_after = %s, want %s", f.AbandonedAfter, project.DefaultFilesAbandonedAfter)
	}
	if len(f.InlineTypes) == 0 {
		t.Error("no inline types, so every image would download instead of rendering")
	}
}

// The list is short on purpose, and these three are the ones somebody adds
// without thinking about where the bytes are served from.
func TestNothingScriptableIsServedInline(t *testing.T) {
	for _, dangerous := range []string{"text/html", "image/svg+xml", "application/xhtml+xml"} {
		for _, got := range project.DefaultInlineTypes() {
			if got == dangerous {
				t.Errorf("%s is served inline by default; a file served inline from the API "+
					"origin runs on that origin", dangerous)
			}
		}
	}
}

// Nothing is resolved for a project that never turned the block on, so an
// unfinished block reads as unfinished rather than as a full configuration.
func TestFilesDefaultsAreNotAppliedWhenOff(t *testing.T) {
	p, out := parseFiles(t, "")
	if out != "" {
		t.Fatalf("no files block at all should be valid:\n%s", out)
	}
	if p.Config.Files.Backend != "" || p.Config.Files.MaxBytes != 0 {
		t.Errorf("a project with no files block got defaults anyway: %+v", p.Config.Files)
	}
}

func TestFilesDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			// The failure mode worth refusing outright: every value silently
			// unread, including a cap somebody believed they had set.
			name: "configured but never enabled",
			body: "files:\n  max_bytes: 100\n",
			want: "files.enabled is false",
		},
		{
			// Caught by the schema before the project is built at all, which is
			// the better place: an editor pointed at the schema says so while
			// the line is being typed.
			name: "unknown backend",
			body: "files:\n  enabled: true\n  backend: ftp\n",
			want: "must be one of 'memory', 's3'",
		},
		{
			name: "s3 with no bucket",
			body: "files:\n  enabled: true\n  backend: s3\n",
			want: "which bucket",
		},
		{
			name: "a cap that accepts nothing",
			body: "files:\n  enabled: true\n  max_bytes: -1\n",
			want: "negative cap",
		},
		{
			name: "an abandoned upload outliving a deleted file",
			body: "files:\n  enabled: true\n  abandoned_after: 48h\n  restore_window: 24h\n",
			want: "outlives a deleted file",
		},
		{
			// There are no sessions to read a cookie from.
			name: "cookie downloads without auth",
			body: "files:\n  enabled: true\n  cookie_downloads: true\n",
			want: "no sessions",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, out := parseFiles(t, tc.body)
			if !strings.Contains(out, tc.want) {
				t.Errorf("diagnostics do not mention %q:\n%s", tc.want, out)
			}
		})
	}
}

func TestFilesAcceptsAWorkingConfiguration(t *testing.T) {
	p, out := parseFiles(t, `auth:
  enabled: true
files:
  enabled: true
  expose: true
  backend: s3
  cookie_downloads: true
  max_bytes: 1048576
  restore_window: 168h
  abandoned_after: 1h
  s3:
    bucket_env: UPLOADS_BUCKET
    region: eu-north-1
`)
	if out != "" {
		t.Fatalf("this configuration should be accepted:\n%s", out)
	}

	f := p.Config.Files
	if f.MaxBytes != 1<<20 {
		t.Errorf("max_bytes = %d, want %d", f.MaxBytes, 1<<20)
	}
	if f.RestoreWindow.Duration() != 168*time.Hour {
		t.Errorf("restore_window = %s, want 168h", f.RestoreWindow)
	}
	// The credentials default to the SDK's own variable names, so a deployment
	// that already has them in the environment configures nothing.
	if f.S3.AccessKeyEnv != project.DefaultAccessKeyEnv {
		t.Errorf("access_key_env = %q, want %q", f.S3.AccessKeyEnv, project.DefaultAccessKeyEnv)
	}
	if f.S3.Bucket != "" {
		t.Errorf("bucket = %q; bucket_env was given instead and nothing should invent one", f.S3.Bucket)
	}
}
